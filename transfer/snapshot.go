package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrUnsupported  = errors.New("snapshot preparation is unsupported on this platform")
	ErrStorage      = errors.New("snapshot storage unavailable")
	ErrInvalidSpool = errors.New("invalid or damaged sender spool")
	ErrSource       = errors.New("source is not a stable regular file")
	ErrCapacity     = errors.New("insufficient snapshot capacity")
	ErrBusy         = errors.New("sender spool is busy")
	ErrSnapshot     = errors.New("snapshot verification failed")
)

const spoolHeadroom int64 = 32 << 20

type Progress struct {
	Stage        string
	Bytes, Total int64
}
type PrepareOptions struct {
	ChunkSize int64
	Progress  func(Progress) error
}

// Per-operation dependencies permit deterministic disk failure tests without globals.
type snapshotOps struct {
	space  func(string) (int64, error)
	write  func(*os.File, []byte) (int, error)
	sync   func(*os.File) error
	close  func(*os.File) error
	rename func(string, string) error
}

func realSnapshotOps() snapshotOps {
	return snapshotOps{freeSpace, func(f *os.File, b []byte) (int, error) { return f.Write(b) }, (*os.File).Sync, (*os.File).Close, os.Rename}
}

// Prepare copies a quiescent source into a private, locked local spool. No network
// is used. Only a fully verified and synchronized ID directory is reusable.
func Prepare(ctx context.Context, source, spool string, options PrepareOptions) (Manifest, error) {
	return prepare(ctx, source, spool, options, realSnapshotOps())
}

func prepare(ctx context.Context, source, spool string, o PrepareOptions, ops snapshotOps) (result Manifest, err error) {
	if !spoolSupported() {
		return result, ErrUnsupported
	}
	spool = filepath.Clean(spool)
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if o.ChunkSize == 0 {
		o.ChunkSize = DefaultChunkSize
	}
	if o.ChunkSize < MinChunkSize || o.ChunkSize > MaxChunkSize {
		return result, ErrSource
	}
	// Inspect and open the same regular file; path identity is checked again later.
	before, e := os.Lstat(source)
	if e != nil || !before.Mode().IsRegular() {
		return result, ErrSource
	}
	src, e := openSource(source)
	if e != nil {
		return result, ErrSource
	}
	defer src.Close()
	opened, e := src.Stat()
	if e != nil || !sameSource(before, opened) {
		return result, ErrSource
	}
	count := before.Size() / o.ChunkSize
	if before.Size()%o.ChunkSize != 0 {
		count++
	}
	if before.Size() < 0 || before.Size() > MaxFileSize || count > MaxChunks {
		return result, ErrSource
	}
	unlock, e := openSpool(spool, ops)
	if e != nil {
		return result, e
	}
	defer unlock()
	if e = cleanPreparing(spool, ops); e != nil {
		return result, e
	}
	entries, e := boundedEntries(spool, 64)
	if e != nil {
		return result, ErrStorage
	}
	if len(entries) >= 64 {
		return result, ErrCapacity
	}
	available, e := ops.space(spool)
	if e != nil {
		return result, ErrStorage
	}
	if available < before.Size()+spoolHeadroom {
		return result, ErrCapacity
	}
	var random [32]byte
	if _, e = rand.Read(random[:]); e != nil {
		return result, ErrStorage
	}
	id := hex.EncodeToString(random[:])
	pending := filepath.Join(spool, ".preparing-"+id)
	final := filepath.Join(spool, id)
	if _, e = os.Lstat(final); !os.IsNotExist(e) {
		return result, ErrInvalidSpool
	}
	if e = os.Mkdir(pending, 0700); e != nil {
		return result, ErrStorage
	}
	if e = privateDir(pending); e != nil {
		return result, e
	}
	published := false
	defer func() {
		if !published {
			_ = removePreparing(pending, ops)
		}
	}()
	dst, e := os.OpenFile(filepath.Join(pending, "snapshot"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return result, ErrStorage
	}
	defer dst.Close()
	buf := make([]byte, 32<<10)
	var copied int64
	copiedHash := sha256.New()
	for {
		if e = ctx.Err(); e != nil {
			return result, e
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if copied+int64(n) > before.Size() {
				return result, ErrSource
			}
			written, we := ops.write(dst, buf[:n])
			if we != nil || written != n {
				return result, ErrStorage
			}
			copied += int64(n)
			copiedHash.Write(buf[:n])
			if e = notify(o.Progress, Progress{"copying", copied, before.Size()}); e != nil {
				return result, e
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return result, ErrSource
		}
	}
	after, e := src.Stat()
	if e != nil || copied != before.Size() || !sameSource(before, after) {
		return result, ErrSource
	}
	pathAfter, e := os.Lstat(source)
	if e != nil || !sameSource(before, pathAfter) {
		return result, ErrSource
	}
	if e = ops.close(src); e != nil {
		return result, ErrStorage
	}
	if e = dst.Chmod(0400); e != nil {
		return result, ErrStorage
	}
	if e = ops.sync(dst); e != nil {
		return result, ErrStorage
	}
	if e = ops.close(dst); e != nil {
		return result, ErrStorage
	}
	m, e := hashSnapshot(ctx, filepath.Join(pending, "snapshot"), id, before.Size(), o.ChunkSize, o.Progress, ops)
	if e != nil {
		return result, e
	}
	if m.FileSHA256 != hex.EncodeToString(copiedHash.Sum(nil)) {
		return result, ErrSnapshot
	}
	data, e := m.CanonicalJSON()
	if e != nil {
		return result, ErrSnapshot
	}
	meta, e := os.OpenFile(filepath.Join(pending, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return result, ErrStorage
	}
	defer meta.Close()
	n, e := ops.write(meta, data)
	if e != nil || n != len(data) {
		return result, ErrStorage
	}
	if e = meta.Chmod(0400); e != nil {
		return result, ErrStorage
	}
	if e = ops.sync(meta); e != nil {
		return result, ErrStorage
	}
	if e = ops.close(meta); e != nil {
		return result, ErrStorage
	}
	if e = syncDir(pending, ops); e != nil {
		return result, e
	}
	if e = ctx.Err(); e != nil {
		return result, e
	}
	if e = ops.rename(pending, final); e != nil {
		return result, ErrStorage
	}
	published = true
	if e = syncDir(spool, ops); e != nil {
		return result, e
	}
	if e = ctx.Err(); e != nil {
		return result, e
	}
	return m, nil
}

func sameSource(a, b os.FileInfo) bool {
	return b.Mode().IsRegular() && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}
func notify(callback func(Progress) error, p Progress) error {
	if callback != nil {
		return callback(p)
	}
	return nil
}
func syncDir(path string, ops snapshotOps) error {
	f, e := os.Open(path)
	if e != nil {
		return ErrStorage
	}
	defer f.Close()
	if e = ops.sync(f); e != nil {
		return ErrStorage
	}
	if e = ops.close(f); e != nil {
		return ErrStorage
	}
	return nil
}

// Reuse revalidates saved bytes and completes synchronization again, including
// recovery after a crash between publishing an ID directory and syncing its parent.
func Reuse(ctx context.Context, spool, id string) (Manifest, error) {
	return reuse(ctx, spool, id, realSnapshotOps())
}
func reuse(ctx context.Context, spool, id string, ops snapshotOps) (Manifest, error) {
	var empty Manifest
	if !spoolSupported() {
		return empty, ErrUnsupported
	}
	spool = filepath.Clean(spool)
	if !hex256(id) {
		return empty, ErrInvalidSpool
	}
	if e := ctx.Err(); e != nil {
		return empty, e
	}
	unlock, e := openSpool(spool, ops)
	if e != nil {
		return empty, e
	}
	defer unlock()
	if e = cleanPreparing(spool, ops); e != nil {
		return empty, e
	}
	dir := filepath.Join(spool, id)
	if e = privateDir(dir); e != nil {
		return empty, e
	}
	entries, e := boundedEntries(dir, 2)
	if e != nil || len(entries) != 2 {
		return empty, ErrInvalidSpool
	}
	for _, entry := range entries {
		if entry.Name() != "manifest.json" && entry.Name() != "snapshot" {
			return empty, ErrInvalidSpool
		}
	}
	path := filepath.Join(dir, "manifest.json")
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() > MaxManifestBytes {
		return empty, ErrInvalidSpool
	}
	f, e := os.Open(path)
	if e != nil {
		return empty, ErrInvalidSpool
	}
	data, e := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	ce := ops.close(f)
	if e != nil || ce != nil || len(data) > MaxManifestBytes {
		return empty, ErrInvalidSpool
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil {
		return empty, ErrInvalidSpool
	}
	canonical, e := m.CanonicalJSON()
	if e != nil || !bytes.Equal(canonical, data) || m.TransferID != id {
		return empty, ErrInvalidSpool
	}
	actual, e := hashSnapshot(ctx, filepath.Join(dir, "snapshot"), id, m.FileSize, m.ChunkSize, nil, ops)
	if e != nil {
		return empty, e
	}
	encoded, e := actual.CanonicalJSON()
	if e != nil || !bytes.Equal(encoded, data) {
		return empty, ErrSnapshot
	}
	if e = ctx.Err(); e != nil {
		return empty, e
	}
	// Sync metadata as well as data before reporting a reusable snapshot.
	f, e = os.Open(path)
	if e != nil {
		return empty, ErrStorage
	}
	defer f.Close()
	if e = ops.sync(f); e != nil {
		return empty, ErrStorage
	}
	if e = ops.close(f); e != nil {
		return empty, ErrStorage
	}
	if e = ctx.Err(); e != nil {
		return empty, e
	}
	if e = syncDir(dir, ops); e != nil {
		return empty, e
	}
	if e = syncDir(spool, ops); e != nil {
		return empty, e
	}
	if e = ctx.Err(); e != nil {
		return empty, e
	}
	return m, nil
}

func hashSnapshot(ctx context.Context, path, id string, size, chunkSize int64, progress func(Progress) error, ops snapshotOps) (Manifest, error) {
	var empty Manifest
	before, e := os.Lstat(path)
	if e != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0400 || before.Size() != size {
		return empty, ErrSnapshot
	}
	f, e := os.Open(path)
	if e != nil {
		return empty, ErrSnapshot
	}
	defer f.Close()
	m := Manifest{ProtocolVersion: ProtocolVersion, TransferID: id, FileSize: size, ChunkSize: chunkSize, ChunkSHA256: []string{}}
	full := sha256.New()
	chunk := sha256.New()
	buf := make([]byte, 32<<10)
	var total, inChunk int64
	for total < size {
		if e = ctx.Err(); e != nil {
			return empty, e
		}
		want := int64(len(buf))
		if remain := chunkSize - inChunk; want > remain {
			want = remain
		}
		if remain := size - total; want > remain {
			want = remain
		}
		n, re := io.ReadFull(f, buf[:want])
		if re != nil {
			return empty, ErrSnapshot
		}
		full.Write(buf[:n])
		chunk.Write(buf[:n])
		total += int64(n)
		inChunk += int64(n)
		if inChunk == chunkSize || total == size {
			m.ChunkSHA256 = append(m.ChunkSHA256, hex.EncodeToString(chunk.Sum(nil)))
			chunk.Reset()
			inChunk = 0
		}
		if e = notify(progress, Progress{"verifying", total, size}); e != nil {
			return empty, e
		}
	}
	var extra [1]byte
	if n, re := f.Read(extra[:]); n != 0 || re != io.EOF {
		return empty, ErrSnapshot
	}
	after, e := f.Stat()
	if e != nil || !sameSource(before, after) {
		return empty, ErrSnapshot
	}
	if e = ctx.Err(); e != nil {
		return empty, e
	}
	if e = ops.sync(f); e != nil {
		return empty, ErrStorage
	}
	if e = ops.close(f); e != nil {
		return empty, ErrStorage
	}
	m.FileSHA256 = hex.EncodeToString(full.Sum(nil))
	return m, nil
}

func privateDir(path string) error {
	info, e := os.Lstat(path)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return ErrInvalidSpool
	}
	return nil
}
func openSpool(path string, ops snapshotOps) (func(), error) {
	if e := os.Mkdir(path, 0700); e != nil && !os.IsExist(e) {
		return nil, ErrInvalidSpool
	}
	if e := privateDir(path); e != nil {
		return nil, e
	}
	lockPath := filepath.Join(path, ".lock")
	info, e := os.Lstat(lockPath)
	if e == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
		return nil, ErrInvalidSpool
	}
	if e != nil && !os.IsNotExist(e) {
		return nil, ErrInvalidSpool
	}
	f, e := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, ErrStorage
	}
	opened, e := f.Stat()
	if e != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		f.Close()
		return nil, ErrInvalidSpool
	}
	if e = lockSpool(f); e != nil {
		f.Close()
		return nil, ErrBusy
	}
	if e = syncDir(filepath.Dir(path), ops); e != nil {
		f.Close()
		return nil, e
	}
	return func() { _ = f.Close() }, nil
}
func cleanPreparing(spool string, ops snapshotOps) error {
	entries, e := boundedEntries(spool, 64)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".lock" {
			continue
		}
		if strings.HasPrefix(name, ".preparing-") && hex256(strings.TrimPrefix(name, ".preparing-")) {
			if e = removePreparing(filepath.Join(spool, name), ops); e != nil {
				return e
			}
			continue
		}
		if !hex256(name) || !entry.IsDir() {
			return ErrInvalidSpool
		}
	}
	return syncDir(spool, ops)
}
func removePreparing(path string, ops snapshotOps) error {
	if e := privateDir(path); e != nil {
		return e
	}
	entries, e := boundedEntries(path, 2)
	if e != nil || len(entries) > 2 {
		return ErrInvalidSpool
	}
	for _, entry := range entries {
		if entry.Name() != "snapshot" && entry.Name() != "manifest.json" {
			return ErrInvalidSpool
		}
		info, e := os.Lstat(filepath.Join(path, entry.Name()))
		if e != nil || !info.Mode().IsRegular() {
			return ErrInvalidSpool
		}
	}
	for _, entry := range entries {
		if e = os.Remove(filepath.Join(path, entry.Name())); e != nil {
			return ErrStorage
		}
	}
	if e = os.Remove(path); e != nil {
		return ErrStorage
	}
	return syncDir(filepath.Dir(path), ops)
}

// Read at most one entry beyond the contract, even in damaged directories.
func boundedEntries(path string, limit int) ([]os.DirEntry, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrStorage
	}
	entries, e := f.ReadDir(limit + 1)
	ce := f.Close()
	if e != nil && e != io.EOF || ce != nil {
		return nil, ErrStorage
	}
	if len(entries) > limit {
		return nil, ErrInvalidSpool
	}
	return entries, nil
}
