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
	"strings"
	"sync"
	"time"
)

const MaxReceiverSessions = 16
const MaxReservedBytes int64 = 256 << 30

var ErrRecovery = errors.New("receiver recovery required")
var ErrConfig = errors.New("invalid Transfer configuration")

type ReceiverConfig struct {
	Root, Token      string
	MaxSessions      int
	MaxReservedBytes int64
}
type receiverOps struct {
	snapshotOps
	link func(string, string) error
}

func realReceiverOps() receiverOps { return receiverOps{realSnapshotOps(), os.Link} }

// MaxInflightChunks bounds chunk uploads one session may have streaming at once.
const MaxInflightChunks = 8

// lockWait is how long a request waits for the receiver lock before it is
// refused as session_busy.
var lockWait = 30 * time.Second

type Receiver struct {
	mu          sync.Mutex
	root, token string
	unlock      func()
	closed      bool
	maxSessions int
	maxReserved int64
	ops         receiverOps
	// inflight holds, per session, the chunk indexes whose bodies are being
	// received without the lock. Guarded by mu.
	inflight map[string]map[int]bool
}

// acquire waits for the receiver lock, at most lockWait and never past ctx.
func (r *Receiver) acquire(ctx context.Context) bool {
	deadline := time.Now().Add(lockWait)
	pause := time.Millisecond
	for {
		if r.mu.TryLock() {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(pause)
		if pause < 20*time.Millisecond {
			pause *= 2
		}
	}
}
func (r *Receiver) busy(id string) bool { return len(r.inflight[id]) != 0 }

func NewReceiver(c ReceiverConfig) (*Receiver, error) { return newReceiver(c, realReceiverOps()) }
func newReceiver(c ReceiverConfig, ops receiverOps) (*Receiver, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return newReceiverContext(ctx, c, ops)
}

// NewReceiverContext bounds startup recovery as well as validating stored progress.
func NewReceiverContext(ctx context.Context, c ReceiverConfig) (*Receiver, error) {
	return newReceiverContext(ctx, c, realReceiverOps())
}
func newReceiverContext(ctx context.Context, c ReceiverConfig, ops receiverOps) (*Receiver, error) {
	if !hex256(c.Token) {
		return nil, ErrConfig
	}
	return openReceiver(ctx, c, ops, false)
}

// openReceiver takes the root lock and runs every startup recovery check.
// Offline maintenance uses it without a token because it serves no requests,
// and skips the write probe that only serving needs, so it can run on a full disk.
func openReceiver(ctx context.Context, c ReceiverConfig, ops receiverOps, offline bool) (*Receiver, error) {
	if !spoolSupported() {
		return nil, ErrUnsupported
	}
	if c.Root == "" {
		return nil, ErrConfig
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = MaxReceiverSessions
	}
	if c.MaxReservedBytes == 0 {
		c.MaxReservedBytes = MaxReservedBytes
	}
	if c.MaxSessions < 1 || c.MaxSessions > MaxReceiverSessions || c.MaxReservedBytes < spoolHeadroom || c.MaxReservedBytes > MaxReservedBytes {
		return nil, ErrConfig
	}
	c.Root = filepath.Clean(c.Root)
	check := c.Root
	if _, e := os.Lstat(check); os.IsNotExist(e) {
		check = filepath.Dir(check)
	}
	if e := receiverFilesystem(check); e != nil {
		return nil, e
	}
	unlock, e := openSpool(c.Root, ops.snapshotOps)
	if e != nil {
		return nil, e
	}
	r := &Receiver{root: c.Root, token: c.Token, unlock: unlock, maxSessions: c.MaxSessions, maxReserved: c.MaxReservedBytes, ops: ops}
	ok := false
	defer func() {
		if !ok {
			unlock()
		}
	}()
	entries, e := boundedEntries(c.Root, 3+MaxReceiverSessions)
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		if entry.Name() != ".lock" && entry.Name() != "sessions" && entry.Name() != "files" && !probeName(entry.Name()) {
			return nil, ErrRecovery
		}
	}
	for _, name := range []string{"sessions", "files"} {
		path := filepath.Join(c.Root, name)
		if e = makePrivateDir(path); e != nil && !os.IsExist(e) {
			return nil, ErrStorage
		}
		if e = privateDir(path); e != nil {
			return nil, e
		}
	}
	if e = r.cleanProbes(ctx); e != nil {
		return nil, e
	}
	if e = r.cleanAdmissions(ctx); e != nil {
		return nil, e
	}
	files, fe := boundedEntries(filepath.Join(c.Root, "files"), MaxReceiverSessions)
	if fe != nil {
		return nil, ErrRecovery
	}
	for _, file := range files {
		id, ok := strings.CutSuffix(file.Name(), ".data")
		if !ok || !hex256(id) {
			return nil, ErrRecovery
		}
		if _, e = r.load(id); e != nil {
			return nil, e
		}
	}
	if e = syncDir(c.Root, ops.snapshotOps); e != nil {
		return nil, e
	}
	if !offline {
		if e = r.probe(); e != nil {
			return nil, e
		}
	}
	if _, _, e = r.inventory(); e != nil {
		return nil, e
	}
	sessions, e := boundedEntries(filepath.Join(r.root, "sessions"), MaxReceiverSessions)
	if e != nil {
		return nil, ErrRecovery
	}
	for _, entry := range sessions {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		m, le := r.load(entry.Name())
		if le != nil {
			return nil, le
		}
		if _, e = r.status(ctx, m); e != nil {
			return nil, e
		}
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	ok = true
	return r, nil
}
func (r *Receiver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.unlock()
	}
	return nil
}
func (r *Receiver) probe() error {
	dir, e := os.MkdirTemp(r.root, ".probe-")
	if e != nil {
		return ErrStorage
	}
	a := filepath.Join(dir, "source")
	b := filepath.Join(r.root, "files", filepath.Base(dir))
	defer func() { _ = os.Remove(b); _ = os.Remove(a); _ = os.Remove(dir) }()
	f, e := os.OpenFile(a, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return ErrStorage
	}
	if n, we := r.ops.write(f, []byte("probe")); we != nil || n != 5 {
		f.Close()
		return ErrStorage
	}
	if e = r.ops.sync(f); e != nil {
		f.Close()
		return ErrStorage
	}
	if e = r.ops.close(f); e != nil {
		return ErrStorage
	}
	if e = r.ops.link(a, b); e != nil {
		return ErrUnsupported
	}
	if e = r.ops.link(a, b); !os.IsExist(e) {
		return ErrUnsupported
	}
	if e = syncDir(filepath.Dir(b), r.ops.snapshotOps); e != nil {
		return e
	}
	if e = os.Remove(b); e != nil {
		return ErrStorage
	}
	if e = os.Remove(a); e != nil {
		return ErrStorage
	}
	if e = os.Remove(dir); e != nil {
		return ErrStorage
	}
	if e = syncDir(filepath.Join(r.root, "files"), r.ops.snapshotOps); e != nil {
		return e
	}
	return syncDir(r.root, r.ops.snapshotOps)
}
func (r *Receiver) inventory() (int, int64, error) {
	entries, e := boundedEntries(filepath.Join(r.root, "sessions"), MaxReceiverSessions)
	if e != nil {
		return 0, 0, ErrRecovery
	}
	var reserved int64
	for _, entry := range entries {
		if !hex256(entry.Name()) || !entry.IsDir() {
			return 0, 0, ErrRecovery
		}
		m, e := r.load(entry.Name())
		if e != nil {
			return 0, 0, e
		}
		reserved += reservation(m)
	}
	if len(entries) > r.maxSessions || reserved > r.maxReserved {
		return 0, 0, ErrRecovery
	}
	return len(entries), reserved, nil
}
func readRegular(path string, limit int64) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || !privateFileOK(path, info) || info.Size() > limit {
		return nil, ErrRecovery
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrRecovery
	}
	data, e := io.ReadAll(io.LimitReader(f, limit+1))
	ce := f.Close()
	if e != nil || ce != nil || int64(len(data)) > limit {
		return nil, ErrRecovery
	}
	return data, nil
}
func (r *Receiver) load(id string) (Manifest, error) {
	var m Manifest
	dir := filepath.Join(r.root, "sessions", id)
	if e := privateDir(dir); e != nil {
		return m, ErrRecovery
	}
	entries, e := boundedEntries(dir, 8)
	if e != nil || len(entries) < 3 {
		return m, ErrRecovery
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "manifest.json", "journal.json", "chunks", "journal.json.tmp", "completion.json", "completion.json.tmp", "assembly.tmp", "ready":
		default:
			return m, ErrRecovery
		}
	}
	data, e := readRegular(filepath.Join(dir, "manifest.json"), MaxManifestBytes)
	if e != nil {
		return m, e
	}
	m, e = DecodeManifest(data)
	if e != nil || m.TransferID != id {
		return m, ErrRecovery
	}
	canonical, _ := m.CanonicalJSON()
	if !bytes.Equal(data, canonical) {
		return m, ErrRecovery
	}
	if _, e = r.readJournal(m); e != nil {
		return m, e
	}
	if found, e := exists(filepath.Join(dir, "completion.json")); e != nil {
		return m, e
	} else if found {
		if _, e = r.savedReceipt(m); e != nil {
			return m, e
		}
	}
	if e = privateDir(filepath.Join(dir, "chunks")); e != nil {
		return m, ErrRecovery
	}
	return m, nil
}
func (r *Receiver) writeFile(path string, data []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return ErrStorage
	}
	defer f.Close()
	n, e := r.ops.write(f, data)
	if e != nil || n != len(data) {
		return ErrStorage
	}
	if e = r.ops.sync(f); e != nil {
		return ErrStorage
	}
	if e = r.ops.close(f); e != nil {
		return ErrStorage
	}
	saved, e := readRegular(path, MaxManifestBytes)
	if e != nil || !bytes.Equal(saved, data) {
		return ErrRecovery
	}
	return nil
}
func (r *Receiver) admit(ctx context.Context, m Manifest) (StoredStatus, bool, error) {
	dir := filepath.Join(r.root, "sessions", m.TransferID)
	if _, e := os.Lstat(dir); e == nil {
		existing, e := r.load(m.TransferID)
		if e != nil {
			return StoredStatus{}, false, e
		}
		a, _ := existing.Identity()
		b, _ := m.Identity()
		if a != b {
			return StoredStatus{}, false, failure(409, "manifest_conflict", false)
		}
		s, e := r.status(ctx, m)
		if e == nil && s.State == Failed {
			return StoredStatus{}, false, failure(409, "transfer_failed", false)
		}
		return s, false, e
	} else if !os.IsNotExist(e) {
		return StoredStatus{}, false, ErrStorage
	}
	count, reserved, e := r.inventory()
	if e != nil {
		return StoredStatus{}, false, e
	}
	budget := reservation(m)
	free, e := r.ops.space(r.root)
	if e != nil {
		return StoredStatus{}, false, ErrStorage
	}
	if count >= r.maxSessions || budget > r.maxReserved-reserved || free < reserved+budget {
		return StoredStatus{}, false, failure(503, "capacity_exhausted", true)
	}
	pending := filepath.Join(r.root, "sessions", ".admitting-"+m.TransferID)
	if e = makePrivateDir(pending); e != nil {
		return StoredStatus{}, false, ErrRecovery
	}
	// Failed admission is retained as evidence and blocks further admission until
	// session-nine recovery/offline inspection; no partial session is advertised.
	if e = privateDir(pending); e != nil {
		return StoredStatus{}, false, e
	}
	if e = makePrivateDir(filepath.Join(pending, "chunks")); e != nil {
		return StoredStatus{}, false, ErrStorage
	}
	data, _ := m.CanonicalJSON()
	if e = r.writeFile(filepath.Join(pending, "manifest.json"), data); e != nil {
		return StoredStatus{}, false, e
	}
	identity, _ := m.Identity()
	data, _ = json.Marshal(journal{1, identity, Uploading, nil})
	if e = r.writeFile(filepath.Join(pending, "journal.json"), data); e != nil {
		return StoredStatus{}, false, e
	}
	if e = syncDir(filepath.Join(pending, "chunks"), r.ops.snapshotOps); e != nil {
		return StoredStatus{}, false, e
	}
	if e = syncDir(pending, r.ops.snapshotOps); e != nil {
		return StoredStatus{}, false, e
	}
	if e = ctx.Err(); e != nil {
		return StoredStatus{}, false, e
	}
	if e = r.ops.rename(pending, dir); e != nil {
		return StoredStatus{}, false, ErrStorage
	}
	if e = syncDir(filepath.Dir(dir), r.ops.snapshotOps); e != nil {
		return StoredStatus{}, false, e
	}
	s, e := r.status(ctx, m)
	if e == nil && s.State == Failed {
		return StoredStatus{}, false, failure(409, "transfer_failed", false)
	}
	return s, true, e
}
func chunkSizeAt(m Manifest, index int) int64 {
	return min(m.ChunkSize, m.FileSize-int64(index)*m.ChunkSize)
}
func (r *Receiver) verifyFile(ctx context.Context, path string, length int64, digest string) error {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || !privateFileOK(path, info) || info.Size() > length {
		return ErrRecovery
	}
	if info.Size() != length {
		return errChunkCorrupt
	}
	f, e := os.Open(path)
	if e != nil {
		return ErrStorage
	}
	defer f.Close()
	corrupt := func() error {
		if e := r.ops.close(f); e != nil {
			return ErrStorage
		}
		return errChunkCorrupt
	}
	hash := sha256.New()
	buf := make([]byte, 32<<10)
	var n int64
	for n < length {
		if e = ctx.Err(); e != nil {
			return e
		}
		count, re := f.Read(buf[:min(int64(len(buf)), length-n)])
		if count > 0 {
			hash.Write(buf[:count])
			n += int64(count)
		}
		if re != nil && re != io.EOF {
			return ErrStorage
		}
		if count == 0 {
			return corrupt()
		}
	}
	var extra [1]byte
	if count, re := f.Read(extra[:]); re != nil && re != io.EOF {
		return ErrStorage
	} else if count != 0 || re != io.EOF {
		return corrupt()
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return corrupt()
	}
	if e = r.ops.sync(f); e != nil {
		return ErrStorage
	}
	if e = r.ops.close(f); e != nil {
		return ErrStorage
	}
	return ctx.Err()
}
func (r *Receiver) uploadStatus(ctx context.Context, m Manifest) (StoredStatus, error) {
	identity, _ := m.Identity()
	s := StoredStatus{ProtocolVersion: 1, TransferID: m.TransferID, ManifestSHA256: identity, State: Uploading, MissingChunks: []int{}}
	dir := filepath.Join(r.root, "sessions", m.TransferID)
	chunks := filepath.Join(dir, "chunks")
	entries, e := boundedEntries(chunks, 2*MaxChunks+MaxInflightChunks+1)
	if e != nil {
		return s, ErrRecovery
	}
	for _, entry := range entries {
		if temp, index := incomingName(entry.Name(), len(m.ChunkSHA256)); temp {
			if index >= 0 && r.inflight[m.TransferID][index] {
				continue
			}
			info, e := os.Lstat(filepath.Join(chunks, entry.Name()))
			if e != nil || !info.Mode().IsRegular() || !privateFileOK(filepath.Join(chunks, entry.Name()), info) || info.Size() > MaxChunkSize {
				return s, ErrRecovery
			}
			if e = os.Remove(filepath.Join(chunks, entry.Name())); e != nil {
				return s, ErrStorage
			}
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".bad")
		i, e := strconv.Atoi(name)
		if e != nil || i < 0 || i >= len(m.ChunkSHA256) || strconv.Itoa(i) != name {
			return s, ErrRecovery
		}
		if strings.HasSuffix(entry.Name(), ".bad") {
			if e = privateChunk(filepath.Join(chunks, entry.Name()), chunkSizeAt(m, i)); e != nil {
				return s, e
			}
		}
	}
	for i, digest := range m.ChunkSHA256 {
		path := filepath.Join(chunks, strconv.Itoa(i))
		if _, e = os.Lstat(path); os.IsNotExist(e) {
			s.MissingChunks = append(s.MissingChunks, i)
			continue
		} else if e != nil {
			return s, ErrStorage
		}
		if e = r.verifyFile(ctx, path, chunkSizeAt(m, i), digest); errors.Is(e, errChunkCorrupt) {
			if e = r.quarantine(path, chunkSizeAt(m, i)); e != nil {
				return s, e
			}
			s.MissingChunks = append(s.MissingChunks, i)
			continue
		} else if e != nil {
			return s, e
		}
		s.VerifiedChunks++
		s.VerifiedBytes += chunkSizeAt(m, i)
	}
	if e = r.syncSession(ctx, m); e != nil {
		return s, e
	}
	return s, nil
}
func (r *Receiver) syncSession(ctx context.Context, m Manifest) error {
	dir := filepath.Join(r.root, "sessions", m.TransferID)
	for _, name := range []string{"manifest.json", "journal.json"} {
		if e := ctx.Err(); e != nil {
			return e
		}
		path := filepath.Join(dir, name)
		if _, e := readRegular(path, MaxManifestBytes); e != nil {
			return e
		}
		f, e := os.Open(path)
		if e != nil {
			return ErrStorage
		}
		se := r.ops.sync(f)
		ce := r.ops.close(f)
		if se != nil || ce != nil {
			return ErrStorage
		}
	}
	for _, path := range []string{filepath.Join(dir, "chunks"), dir, filepath.Dir(dir), r.root, filepath.Dir(r.root)} {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := syncDir(path, r.ops.snapshotOps); e != nil {
			return e
		}
	}
	return ctx.Err()
}

// incomingName reports whether name is a chunk temp file: the 0.4 name
// incoming.tmp (index -1) or incoming-INDEX.tmp for an index in the manifest.
func incomingName(name string, count int) (bool, int) {
	if name == "incoming.tmp" {
		return true, -1
	}
	middle, ok := strings.CutPrefix(name, "incoming-")
	if !ok {
		return false, 0
	}
	middle, ok = strings.CutSuffix(middle, ".tmp")
	if !ok {
		return false, 0
	}
	index, valid := validIndex(middle, count)
	if !valid {
		return false, 0
	}
	return true, index
}

// pendingChunk is one chunk upload between its two locked steps.
type pendingChunk struct {
	m         Manifest
	index     int
	want      int64
	temp      string
	target    string
	duplicate bool
	f         *os.File
}

// beginStore runs under the lock: it checks the target, discards a stale temp
// file and opens a fresh one. The caller has reserved the index.
func (r *Receiver) beginStore(ctx context.Context, m Manifest, index int, length int64) (*pendingChunk, error) {
	want := chunkSizeAt(m, index)
	if length != want {
		return nil, failure(422, "chunk_length_mismatch", false)
	}
	chunks := filepath.Join(r.root, "sessions", m.TransferID, "chunks")
	p := &pendingChunk{m: m, index: index, want: want, target: filepath.Join(chunks, strconv.Itoa(index)), temp: filepath.Join(chunks, "incoming-"+strconv.Itoa(index)+".tmp")}
	if _, e := os.Lstat(p.target); e == nil {
		if e = r.verifyFile(ctx, p.target, want, m.ChunkSHA256[index]); errors.Is(e, errChunkCorrupt) {
			if e = r.quarantine(p.target, want); e != nil {
				return nil, e
			}
		} else if e != nil {
			return nil, e
		} else {
			p.duplicate = true
		}
	} else if !os.IsNotExist(e) {
		return nil, ErrStorage
	}
	if _, e := os.Lstat(p.temp); e == nil {
		if e := privateChunk(p.temp, MaxChunkSize); e != nil {
			return nil, e
		}
		if e = os.Remove(p.temp); e != nil {
			return nil, ErrStorage
		}
	} else if !os.IsNotExist(e) {
		return nil, ErrStorage
	}
	if !p.duplicate {
		f, e := os.OpenFile(p.temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, ErrStorage
		}
		p.f = f
	}
	return p, nil
}

// discard closes and removes the temp file. Windows cannot remove a file that
// is still open, so close first. Safe to call more than once.
func (p *pendingChunk) discard() {
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
	if !p.duplicate {
		_ = os.Remove(p.temp)
	}
}

// receive streams the body without the lock: the index is reserved, so no
// other request touches this temp file or target.
func (r *Receiver) receive(ctx context.Context, p *pendingChunk, body io.Reader) error {
	hash := sha256.New()
	buf := make([]byte, 32<<10)
	var received int64
	for received < p.want {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, re := body.Read(buf[:min(int64(len(buf)), p.want-received)])
		if n > 0 {
			hash.Write(buf[:n])
			if p.f != nil {
				written, we := r.ops.write(p.f, buf[:n])
				if we != nil || written != n {
					return ErrStorage
				}
			}
			received += int64(n)
		}
		if re != nil && re != io.EOF {
			return failure(422, "chunk_length_mismatch", false)
		}
		if n == 0 {
			return failure(422, "chunk_length_mismatch", false)
		}
	}
	var extra [1]byte
	if n, re := body.Read(extra[:]); n != 0 || re != io.EOF {
		return failure(422, "chunk_length_mismatch", false)
	}
	if hex.EncodeToString(hash.Sum(nil)) != p.m.ChunkSHA256[p.index] {
		return failure(422, "chunk_digest_mismatch", false)
	}
	if p.f != nil {
		f := p.f
		if e := r.ops.sync(f); e != nil {
			return ErrStorage
		}
		p.f = nil
		if e := r.ops.close(f); e != nil {
			return ErrStorage
		}
		if e := r.verifyFile(ctx, p.temp, p.want, p.m.ChunkSHA256[p.index]); e != nil {
			return e
		}
	}
	return nil
}

// commitStore runs under the lock: it publishes the verified temp file and
// syncs the chunk folder before the chunk is acknowledged.
func (r *Receiver) commitStore(ctx context.Context, p *pendingChunk) (chunkAck, error) {
	identity, _ := p.m.Identity()
	ack := chunkAck{1, p.m.TransferID, identity, p.index, p.m.ChunkSHA256[p.index], true}
	j, e := r.readJournal(p.m)
	if e != nil {
		return ack, e
	}
	if j.State != Uploading {
		return ack, failure(409, "invalid_state", false)
	}
	if !p.duplicate {
		if e = r.ops.link(p.temp, p.target); e != nil {
			return ack, ErrStorage
		}
		if e = os.Remove(p.temp); e != nil {
			return ack, ErrStorage
		}
	}
	if e = syncDir(filepath.Dir(p.target), r.ops.snapshotOps); e != nil {
		return ack, e
	}
	if e = ctx.Err(); e != nil {
		return ack, e
	}
	return ack, nil
}

// store runs all three steps while the caller already holds the lock. Tests
// and single-request paths use it; ServeHTTP releases the lock while the body
// streams.
func (r *Receiver) store(ctx context.Context, m Manifest, index int, body io.Reader, length int64) (chunkAck, error) {
	p, e := r.beginStore(ctx, m, index, length)
	if e != nil {
		return chunkAck{}, e
	}
	defer p.discard()
	if e = r.receive(ctx, p, body); e != nil {
		return chunkAck{}, e
	}
	return r.commitStore(ctx, p)
}

func receiverError(err error) *protocolError {
	var p *protocolError
	if errors.As(err, &p) {
		return p
	}
	if errors.Is(err, ErrRecovery) || errors.Is(err, ErrInvalidSpool) {
		return &protocolError{503, "recovery_required", true}
	}
	return &protocolError{503, "storage_unavailable", true}
}
func validIndex(value string, count int) (int, bool) {
	i, e := strconv.Atoi(value)
	return i, e == nil && i >= 0 && i < count && strconv.Itoa(i) == value
}
func safeIDPath(path string) (string, string) {
	parts := strings.Split(path, "/")
	if len(parts) >= 4 && parts[0] == "" && parts[1] == "v1" && parts[2] == "transfers" && hex256(parts[3]) {
		return parts[3], strings.Join(parts[4:], "/")
	}
	return "", ""
}

func (r *Receiver) sessionExists(id string) (bool, error) {
	_, e := os.Lstat(filepath.Join(r.root, "sessions", id))
	if os.IsNotExist(e) {
		return false, failure(404, "unknown_transfer", false)
	}
	if e != nil {
		return false, ErrStorage
	}
	return true, nil
}
