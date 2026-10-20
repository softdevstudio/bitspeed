package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Receipt identifies one verified destination, with no receiver-private paths.
type Receipt struct {
	TransferID     string `json:"transfer_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	FileSize       int64  `json:"file_size"`
	FileSHA256     string `json:"file_sha256"`
	OutputName     string `json:"output_name"`
}

func expectedReceipt(m Manifest) Receipt {
	identity, _ := m.Identity()
	return Receipt{m.TransferID, identity, m.FileSize, m.FileSHA256, m.TransferID + ".data"}
}
func reservation(m Manifest) int64 { return 3*m.FileSize + spoolHeadroom }
func terminalCode(code string) bool {
	return code == "destination_conflict" || code == "final_digest_mismatch" || code == "storage_corrupt"
}
func (r *Receiver) sessionDir(m Manifest) string {
	return filepath.Join(r.root, "sessions", m.TransferID)
}
func (r *Receiver) output(m Manifest) string {
	return filepath.Join(r.root, "files", m.TransferID+".data")
}
func (r *Receiver) readJournal(m Manifest) (journal, error) {
	var j journal
	data, e := readRegular(filepath.Join(r.sessionDir(m), "journal.json"), MaxManifestBytes)
	if e != nil {
		return j, e
	}
	if json.Unmarshal(data, &j) != nil {
		return j, ErrRecovery
	}
	identity, _ := m.Identity()
	if j.ProtocolVersion != 1 || j.ManifestSHA256 != identity {
		return j, ErrRecovery
	}
	switch j.State {
	case Uploading, Finalizing, Completed:
		if j.ErrorCode != nil {
			return j, ErrRecovery
		}
	case Failed:
		if j.ErrorCode == nil || !terminalCode(*j.ErrorCode) {
			return j, ErrRecovery
		}
	default:
		return j, ErrRecovery
	}
	canonical, _ := json.Marshal(j)
	if !bytes.Equal(data, canonical) {
		return j, ErrRecovery
	}
	return j, nil
}
func (r *Receiver) discardTemp(path string, limit int64) error {
	if _, e := os.Lstat(path); os.IsNotExist(e) {
		return nil
	} else if e != nil {
		return ErrStorage
	}
	if e := privateChunk(path, limit); e != nil {
		return e
	}
	if e := os.Remove(path); e != nil {
		return ErrStorage
	}
	return syncDir(filepath.Dir(path), r.ops.snapshotOps)
}
func (r *Receiver) metadata(path string, value any) error {
	temp := path + ".tmp"
	if e := r.discardTemp(temp, MaxManifestBytes); e != nil {
		return e
	}
	data, e := json.Marshal(value)
	if e != nil {
		return ErrStorage
	}
	if e = r.writeFile(temp, data); e != nil {
		return e
	}
	if e = r.ops.rename(temp, path); e != nil {
		return ErrStorage
	}
	return syncDir(filepath.Dir(path), r.ops.snapshotOps)
}
func (r *Receiver) setState(m Manifest, state State, code *string) error {
	identity, _ := m.Identity()
	return r.metadata(filepath.Join(r.sessionDir(m), "journal.json"), journal{1, identity, state, code})
}
func (r *Receiver) fail(m Manifest, code string) error {
	if e := r.setState(m, Failed, &code); e != nil {
		return e
	}
	if code == "final_digest_mismatch" {
		return failure(422, code, false)
	}
	if code == "storage_corrupt" {
		return failure(409, "transfer_failed", false)
	}
	return failure(409, code, false)
}
func (r *Receiver) savedReceipt(m Manifest) (*Receipt, error) {
	data, e := readRegular(filepath.Join(r.sessionDir(m), "completion.json"), MaxManifestBytes)
	if e != nil {
		return nil, e
	}
	want := expectedReceipt(m)
	canonical, _ := json.Marshal(want)
	if !bytes.Equal(data, canonical) {
		return nil, ErrRecovery
	}
	return &want, nil
}
func exists(path string) (bool, error) {
	_, e := os.Lstat(path)
	if e == nil {
		return true, nil
	}
	if os.IsNotExist(e) {
		return false, nil
	}
	return false, ErrStorage
}

func (r *Receiver) status(ctx context.Context, m Manifest) (StoredStatus, error) {
	j, e := r.readJournal(m)
	if e != nil {
		return StoredStatus{}, e
	}
	for _, name := range []string{"journal.json.tmp", "completion.json.tmp"} {
		if e = r.discardTemp(filepath.Join(r.sessionDir(m), name), MaxManifestBytes); e != nil {
			return StoredStatus{}, e
		}
	}
	switch j.State {
	case Uploading:
		if found, e := exists(r.output(m)); e != nil {
			return StoredStatus{}, e
		} else if found {
			if e = r.setState(m, Failed, strptr("destination_conflict")); e != nil {
				return StoredStatus{}, e
			}
			return r.status(ctx, m)
		}
		// Neither a witness nor a destination can belong to an uploading session.
		for _, path := range []string{filepath.Join(r.sessionDir(m), "ready"), filepath.Join(r.sessionDir(m), "completion.json"), r.output(m)} {
			if found, e := exists(path); e != nil {
				return StoredStatus{}, e
			} else if found {
				return StoredStatus{}, ErrRecovery
			}
		}
		return r.uploadStatus(ctx, m)
	case Finalizing:
		s, e := r.finalize(ctx, m)
		var protocol *protocolError
		if errors.As(e, &protocol) {
			if protocol.code == "chunks_missing" {
				return r.status(ctx, m)
			}
			if current, je := r.readJournal(m); je == nil && current.State == Failed {
				return r.status(ctx, m)
			}
		}
		return s, e
	case Completed:
		receipt, e := r.savedReceipt(m)
		if e != nil {
			return StoredStatus{}, e
		}
		if e = r.syncFile(filepath.Join(r.sessionDir(m), "completion.json")); e != nil {
			return StoredStatus{}, e
		}
		ready := filepath.Join(r.sessionDir(m), "ready")
		a, ae := os.Lstat(ready)
		b, be := os.Lstat(r.output(m))
		if ae != nil || be != nil || !a.Mode().IsRegular() || !b.Mode().IsRegular() || !os.SameFile(a, b) {
			if (ae != nil && !os.IsNotExist(ae)) || (be != nil && !os.IsNotExist(be)) {
				return StoredStatus{}, ErrStorage
			}
			if e = r.setState(m, Failed, strptr("storage_corrupt")); e != nil {
				return StoredStatus{}, e
			}
			return r.status(ctx, m)
		}
		e = r.verifyFile(ctx, r.output(m), m.FileSize, m.FileSHA256)
		if errors.Is(e, errChunkCorrupt) || errors.Is(e, ErrRecovery) {
			if e = r.setState(m, Failed, strptr("storage_corrupt")); e != nil {
				return StoredStatus{}, e
			}
			return r.status(ctx, m)
		} else if e != nil {
			return StoredStatus{}, e
		}
		if e = syncDir(filepath.Join(r.root, "files"), r.ops.snapshotOps); e != nil {
			return StoredStatus{}, e
		}
		s, e := r.uploadStatus(ctx, m)
		if e != nil {
			return s, e
		}
		s.State = Completed
		s.Receipt = receipt
		return s, nil
	case Failed:
		s, e := r.uploadStatus(ctx, m)
		s.State = Failed
		s.ErrorCode = j.ErrorCode
		return s, e
	}
	return StoredStatus{}, ErrRecovery
}
func strptr(s string) *string { return &s }

func (r *Receiver) finalize(ctx context.Context, m Manifest) (StoredStatus, error) {
	empty := StoredStatus{}
	if e := ctx.Err(); e != nil {
		return empty, e
	}
	j, e := r.readJournal(m)
	if e != nil {
		return empty, e
	}
	if j.State == Completed {
		s, e := r.status(ctx, m)
		if e != nil {
			return s, e
		}
		if s.State != Completed {
			return StoredStatus{}, failure(409, "transfer_failed", false)
		}
		return s, nil
	}
	if j.State == Failed {
		return empty, failure(409, "transfer_failed", false)
	}
	dir := r.sessionDir(m)
	ready := filepath.Join(dir, "ready")
	output := r.output(m)
	if j.State == Uploading {
		s, e := r.status(ctx, m)
		if e != nil {
			return empty, e
		}
		if s.State == Failed {
			return empty, failure(409, "transfer_failed", false)
		}
		if len(s.MissingChunks) != 0 {
			return empty, failure(409, "chunks_missing", true)
		}
		// Reconstructed three-copy reservations include sessions from older builds.
		if _, _, e = r.inventory(); e != nil {
			return empty, e
		}
		free, e := r.ops.space(r.root)
		if e != nil {
			return empty, ErrStorage
		}
		if free < m.FileSize+spoolHeadroom {
			return empty, failure(503, "capacity_exhausted", true)
		}
		if e = r.setState(m, Finalizing, nil); e != nil {
			return empty, e
		}
	}
	hasReady, e := exists(ready)
	if e != nil {
		return empty, e
	}
	hasOutput, e := exists(output)
	if e != nil {
		return empty, e
	}
	hasReceipt, e := exists(filepath.Join(dir, "completion.json"))
	if e != nil {
		return empty, e
	}
	if hasReceipt {
		if _, e = r.savedReceipt(m); e != nil {
			return empty, e
		}
		if !hasReady || !hasOutput {
			return empty, r.fail(m, "storage_corrupt")
		}
	}
	if !hasReady {
		if hasOutput {
			return empty, ErrRecovery
		}
		if e = r.discardTemp(filepath.Join(dir, "assembly.tmp"), m.FileSize); e != nil {
			return empty, e
		}
		s, e := r.uploadStatus(ctx, m)
		if e != nil {
			return empty, e
		}
		if len(s.MissingChunks) != 0 {
			if e = r.setState(m, Uploading, nil); e != nil {
				return empty, e
			}
			return empty, failure(409, "chunks_missing", true)
		}
		if e = r.assemble(ctx, m); e != nil {
			return empty, e
		}
	}
	if e = r.verifyFile(ctx, ready, m.FileSize, m.FileSHA256); e != nil {
		if errors.Is(e, errChunkCorrupt) || errors.Is(e, ErrRecovery) {
			if hasReceipt {
				return empty, r.fail(m, "storage_corrupt")
			}
			return empty, ErrRecovery
		}
		return empty, e
	}
	if e = syncDir(dir, r.ops.snapshotOps); e != nil {
		return empty, e
	}
	if hasOutput {
		a, ae := os.Lstat(ready)
		b, be := os.Lstat(output)
		if ae != nil || be != nil {
			return empty, ErrStorage
		}
		if !b.Mode().IsRegular() || !os.SameFile(a, b) {
			code := "destination_conflict"
			if hasReceipt {
				code = "storage_corrupt"
			}
			return empty, r.fail(m, code)
		}
	} else if e = r.ops.link(ready, output); e != nil {
		if os.IsExist(e) {
			return empty, r.fail(m, "destination_conflict")
		}
		return empty, ErrStorage
	}
	if e = ctx.Err(); e != nil {
		return empty, e
	}
	if e = syncDir(filepath.Dir(output), r.ops.snapshotOps); e != nil {
		return empty, e
	}
	receipt := expectedReceipt(m)
	if !hasReceipt {
		if e = r.metadata(filepath.Join(dir, "completion.json"), receipt); e != nil {
			return empty, e
		}
	}
	if e = r.setState(m, Completed, nil); e != nil {
		return empty, e
	}
	return r.status(ctx, m)
}
func (r *Receiver) assemble(ctx context.Context, m Manifest) error {
	path := filepath.Join(r.sessionDir(m), "assembly.tmp")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return ErrStorage
	}
	defer f.Close()
	full := sha256.New()
	buf := make([]byte, 32<<10)
	for i, digest := range m.ChunkSHA256 {
		source := filepath.Join(r.sessionDir(m), "chunks", strconv.Itoa(i))
		if e = r.verifyFile(ctx, source, chunkSizeAt(m, i), digest); e != nil {
			return e
		}
		in, e := os.Open(source)
		if e != nil {
			return ErrStorage
		}
		chunk := sha256.New()
		var n int64
		for n < chunkSizeAt(m, i) {
			if e = ctx.Err(); e != nil {
				in.Close()
				return e
			}
			count, re := in.Read(buf[:min(int64(len(buf)), chunkSizeAt(m, i)-n)])
			if re != nil && re != io.EOF {
				in.Close()
				return ErrStorage
			}
			if count == 0 {
				in.Close()
				return ErrRecovery
			}
			written, we := r.ops.write(f, buf[:count])
			if we != nil || written != count {
				in.Close()
				return ErrStorage
			}
			full.Write(buf[:count])
			chunk.Write(buf[:count])
			n += int64(count)
		}
		if e = r.ops.close(in); e != nil {
			return ErrStorage
		}
		if hex.EncodeToString(chunk.Sum(nil)) != digest {
			return ErrRecovery
		}
	}
	if e = r.ops.sync(f); e != nil {
		return ErrStorage
	}
	if e = r.ops.close(f); e != nil {
		return ErrStorage
	}
	if hex.EncodeToString(full.Sum(nil)) != m.FileSHA256 {
		return r.fail(m, "final_digest_mismatch")
	}
	if e = r.verifyFile(ctx, path, m.FileSize, m.FileSHA256); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = r.ops.rename(path, filepath.Join(r.sessionDir(m), "ready")); e != nil {
		return ErrStorage
	}
	return syncDir(r.sessionDir(m), r.ops.snapshotOps)
}

func (r *Receiver) syncFile(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return ErrStorage
	}
	se := r.ops.sync(f)
	ce := r.ops.close(f)
	if se != nil || ce != nil {
		return ErrStorage
	}
	return nil
}
