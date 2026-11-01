package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A tree is one transfer payload that carries a folder: the contents of its
// files in index order, then the canonical JSON index, then a 16-byte footer
// holding the index length (big-endian) and treeMagic. The transfer protocol
// treats it as an ordinary file, so resume, parallel chunks and the completion
// receipt are unchanged, and the receipt's digest binds every file through the
// index. See docs/transfer-tree.md.
const (
	TreeFormat        = "bitspeed-tree"
	TreeVersion       = 1
	MaxTreeEntries    = 100000
	MaxTreeIndexBytes = 16 << 20
	MaxTreePathBytes  = 4096
	MaxTreeNameBytes  = 255
	treeMagic         = "BSTREE01"
	treeFooterSize    = 16
)

var (
	ErrTree     = errors.New("invalid directory tree")
	ErrNotTree  = errors.New("transfer is not a directory tree")
	ErrUnpack   = errors.New("unpack output is not usable")
	ErrNotReady = errors.New("transfer is not completed on this receiver")
)

// TreeEntry is one folder or file. Folders have size 0 and an empty digest.
type TreeEntry struct {
	Type   string `json:"type"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// TreeIndex lists every folder and file below the tree root, sorted by path.
type TreeIndex struct {
	Format  string      `json:"format"`
	Version int         `json:"version"`
	Entries []TreeEntry `json:"entries"`
}

// TreeSummary counts what a tree holds.
type TreeSummary struct {
	Files   int   `json:"files"`
	Folders int   `json:"folders"`
	Bytes   int64 `json:"bytes"`
}

func (t TreeIndex) summary() TreeSummary {
	var s TreeSummary
	for _, e := range t.Entries {
		if e.Type == "dir" {
			s.Folders++
		} else {
			s.Files++
			s.Bytes += e.Size
		}
	}
	return s
}

// validTreePath accepts a relative, slash-separated, clean UTF-8 path.
func validTreePath(p string) bool {
	if p == "" || len(p) > MaxTreePathBytes || !utf8.ValidString(p) {
		return false
	}
	for _, name := range strings.Split(p, "/") {
		if name == "" || name == "." || name == ".." || len(name) > MaxTreeNameBytes {
			return false
		}
		for _, c := range name {
			if c < 0x20 || c == 0x7f || c == '\\' || c == utf8.RuneError {
				return false
			}
		}
	}
	return true
}

// windowsName reports whether every segment of p can be created on Windows
// without changing meaning: no reserved characters, no trailing dot or space
// and no reserved device names.
func windowsName(p string) bool {
	for _, name := range strings.Split(p, "/") {
		if strings.ContainsAny(name, `<>:"|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
			return false
		}
		base := strings.ToUpper(name)
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		base = strings.TrimRight(base, " ")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return false
		}
		for _, device := range []string{"COM", "LPT"} {
			if digit, ok := strings.CutPrefix(base, device); ok && len(digit) > 0 && strings.Contains("0123456789¹²³", digit) && utf8.RuneCountInString(digit) == 1 {
				return false
			}
		}
	}
	return true
}

// Validate checks shape, order and limits. Every entry's parent folder must be
// listed before it, paths are unique and sorted by byte order.
func (t TreeIndex) Validate() error {
	if t.Format != TreeFormat || t.Version != TreeVersion || t.Entries == nil || len(t.Entries) > MaxTreeEntries {
		return ErrTree
	}
	dirs := map[string]bool{}
	var total int64
	for i, e := range t.Entries {
		if !validTreePath(e.Path) || i > 0 && t.Entries[i-1].Path >= e.Path {
			return ErrTree
		}
		if parent := path.Dir(e.Path); parent != "." && !dirs[parent] {
			return ErrTree
		}
		switch e.Type {
		case "dir":
			if e.Size != 0 || e.SHA256 != "" {
				return ErrTree
			}
			dirs[e.Path] = true
		case "file":
			if e.Size < 0 || e.Size > MaxFileSize || !hex256(e.SHA256) {
				return ErrTree
			}
			if e.Size == 0 {
				empty := sha256.Sum256(nil)
				if e.SHA256 != hex.EncodeToString(empty[:]) {
					return ErrTree
				}
			}
			total += e.Size
			if total > MaxFileSize {
				return ErrTree
			}
		default:
			return ErrTree
		}
	}
	return nil
}

// CanonicalJSON validates and encodes the index with fixed field order and no
// whitespace.
func (t TreeIndex) CanonicalJSON() ([]byte, error) {
	if e := t.Validate(); e != nil {
		return nil, e
	}
	data, e := json.Marshal(t)
	if e != nil || len(data) > MaxTreeIndexBytes {
		return nil, ErrTree
	}
	return data, nil
}

// decodeTreeIndex accepts only the exact canonical encoding.
func decodeTreeIndex(data []byte) (TreeIndex, error) {
	var t TreeIndex
	if len(data) > MaxTreeIndexBytes {
		return t, ErrTree
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&t) != nil || d.More() {
		return t, ErrTree
	}
	canonical, e := t.CanonicalJSON()
	if e != nil || !bytes.Equal(canonical, data) {
		return t, ErrTree
	}
	return t, nil
}

func treeFooter(indexLength int) []byte {
	footer := make([]byte, treeFooterSize)
	binary.BigEndian.PutUint64(footer, uint64(indexLength))
	copy(footer[8:], treeMagic)
	return footer
}

// treeSource is one walked entry with the stat result used to detect change.
type treeSource struct {
	entry TreeEntry
	info  os.FileInfo
}

func treeSourceError(rel, why string) error {
	return fmt.Errorf("%w: %s %s", ErrSource, strconv.Quote(rel), why)
}

// walkTree lists every entry below root without following symlinks. Anything
// other than folders and regular files is refused.
func walkTree(ctx context.Context, root string) ([]treeSource, error) {
	var out []treeSource
	e := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		rel, re := filepath.Rel(root, p)
		if walkErr != nil || re != nil {
			return treeSourceError(filepath.ToSlash(rel), "cannot be read")
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if len(out) >= MaxTreeEntries {
			return fmt.Errorf("%w: more than %d entries", ErrSource, MaxTreeEntries)
		}
		if !validTreePath(rel) {
			return treeSourceError(rel, "has a name a tree cannot carry")
		}
		info, ie := os.Lstat(p)
		if ie != nil {
			return treeSourceError(rel, "cannot be read")
		}
		switch {
		case info.IsDir():
			out = append(out, treeSource{TreeEntry{Type: "dir", Path: rel}, info})
		case info.Mode().IsRegular():
			out = append(out, treeSource{TreeEntry{Type: "file", Path: rel, Size: info.Size()}, info})
		case info.Mode()&os.ModeSymlink != 0:
			return treeSourceError(rel, "is a symbolic link; trees carry folders and regular files only")
		default:
			return treeSourceError(rel, "is not a folder or regular file")
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	sort.Slice(out, func(i, j int) bool { return out[i].entry.Path < out[j].entry.Path })
	return out, nil
}

func sameWalk(a, b []treeSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.entry.Path != y.entry.Path || x.entry.Type != y.entry.Type || !os.SameFile(x.info, y.info) {
			return false
		}
		if x.entry.Type == "file" && (x.info.Size() != y.info.Size() || !x.info.ModTime().Equal(y.info.ModTime())) {
			return false
		}
	}
	return true
}

func inside(child, parent string) bool {
	rel, e := filepath.Rel(parent, child)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolved returns an absolute path with symlinks in its existing part resolved.
func resolved(p string) (string, error) {
	p, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	rest := ""
	for {
		if real, e := filepath.EvalSymlinks(p); e == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", errors.New("no existing ancestor")
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// PrepareTree snapshots a quiet folder into the spool as one tree payload.
// It refuses symlinks and special files, keeps no permissions, owners or
// times, and fails if the folder changes while it is copied.
func PrepareTree(ctx context.Context, source, spool string, options PrepareOptions) (Manifest, TreeSummary, error) {
	return prepareTree(ctx, source, spool, options, realSnapshotOps())
}

func prepareTree(ctx context.Context, source, spool string, o PrepareOptions, ops snapshotOps) (Manifest, TreeSummary, error) {
	var empty Manifest
	if !spoolSupported() {
		return empty, TreeSummary{}, ErrUnsupported
	}
	if e := ctx.Err(); e != nil {
		return empty, TreeSummary{}, e
	}
	if o.ChunkSize == 0 {
		o.ChunkSize = DefaultChunkSize
	}
	if o.ChunkSize < MinChunkSize || o.ChunkSize > MaxChunkSize {
		return empty, TreeSummary{}, ErrSource
	}
	info, e := os.Lstat(source)
	if e != nil || !info.IsDir() {
		return empty, TreeSummary{}, ErrSource
	}
	realSource, se := resolved(source)
	realSpool, pe := resolved(spool)
	if se != nil || pe != nil || inside(realSpool, realSource) || inside(realSource, realSpool) {
		return empty, TreeSummary{}, fmt.Errorf("%w: the spool and the source folder must not contain each other", ErrSource)
	}
	walked, e := walkTree(ctx, source)
	if e != nil {
		return empty, TreeSummary{}, e
	}
	// The index length is known before copying: every digest is 64 hex digits.
	index := TreeIndex{Format: TreeFormat, Version: TreeVersion, Entries: make([]TreeEntry, len(walked))}
	var content int64
	placeholder := strings.Repeat("0", 64)
	for i, w := range walked {
		index.Entries[i] = w.entry
		if w.entry.Type == "file" {
			index.Entries[i].SHA256 = placeholder
			content += w.entry.Size
			if content > MaxFileSize {
				return empty, TreeSummary{}, ErrSource
			}
		}
	}
	probe, e := json.Marshal(index)
	if e != nil || len(probe) > MaxTreeIndexBytes {
		return empty, TreeSummary{}, fmt.Errorf("%w: the index would exceed %d bytes", ErrSource, MaxTreeIndexBytes)
	}
	size := content + int64(len(probe)) + treeFooterSize
	m, e := prepareInto(ctx, spool, o, ops, size, func(dst *os.File) (string, error) {
		full := sha256.New()
		write := func(b []byte) error {
			n, we := ops.write(dst, b)
			if we != nil || n != len(b) {
				return ErrStorage
			}
			full.Write(b)
			return nil
		}
		buf := make([]byte, 32<<10)
		var copied int64
		for i, w := range walked {
			if w.entry.Type != "file" {
				continue
			}
			sum, e := copyTreeFile(ctx, filepath.Join(source, filepath.FromSlash(w.entry.Path)), w, buf, write, func(n int64) error {
				copied += n
				return notify(o.Progress, Progress{"copying", copied, size})
			}, ops)
			if e != nil {
				return "", e
			}
			index.Entries[i].SHA256 = sum
		}
		again, e := walkTree(ctx, source)
		if e != nil {
			return "", e
		}
		if !sameWalk(walked, again) {
			return "", fmt.Errorf("%w: the folder changed while it was copied", ErrSource)
		}
		data, e := index.CanonicalJSON()
		if e != nil || len(data) != len(probe) {
			return "", ErrSnapshot
		}
		if e = write(data); e != nil {
			return "", e
		}
		if e = write(treeFooter(len(data))); e != nil {
			return "", e
		}
		return hex.EncodeToString(full.Sum(nil)), nil
	})
	if e != nil {
		return empty, TreeSummary{}, e
	}
	return m, index.summary(), nil
}

func copyTreeFile(ctx context.Context, p string, w treeSource, buf []byte, write func([]byte) error, progress func(int64) error, ops snapshotOps) (string, error) {
	src, e := openSource(p)
	if e != nil {
		return "", treeSourceError(w.entry.Path, "cannot be opened")
	}
	defer src.Close()
	opened, e := src.Stat()
	if e != nil || !sameSource(w.info, opened) {
		return "", treeSourceError(w.entry.Path, "changed while it was copied")
	}
	hash := sha256.New()
	var copied int64
	for {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		n, re := src.Read(buf)
		if n > 0 {
			if copied+int64(n) > w.entry.Size {
				return "", treeSourceError(w.entry.Path, "changed while it was copied")
			}
			if e := write(buf[:n]); e != nil {
				return "", e
			}
			hash.Write(buf[:n])
			copied += int64(n)
			if e := progress(int64(n)); e != nil {
				return "", e
			}
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			return "", treeSourceError(w.entry.Path, "cannot be read")
		}
	}
	after, e := src.Stat()
	if e != nil || copied != w.entry.Size || !sameSource(w.info, after) {
		return "", treeSourceError(w.entry.Path, "changed while it was copied")
	}
	if pathAfter, e := os.Lstat(p); e != nil || !sameSource(w.info, pathAfter) {
		return "", treeSourceError(w.entry.Path, "changed while it was copied")
	}
	if e = ops.close(src); e != nil {
		return "", ErrStorage
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// UnpackResult reports a written tree.
type UnpackResult struct {
	TransferID string `json:"transfer_id"`
	Out        string `json:"out"`
	TreeSummary
}

// readTreeIndex reads and validates the footer and index of a payload of the
// given size. The caller still checks the whole stream against the receipt.
func readTreeIndex(f io.ReaderAt, size int64) (TreeIndex, []byte, error) {
	var t TreeIndex
	if size < treeFooterSize {
		return t, nil, ErrNotTree
	}
	footer := make([]byte, treeFooterSize)
	if _, e := f.ReadAt(footer, size-treeFooterSize); e != nil {
		return t, nil, ErrStorage
	}
	if string(footer[8:]) != treeMagic {
		return t, nil, ErrNotTree
	}
	length := binary.BigEndian.Uint64(footer)
	if length > MaxTreeIndexBytes || int64(length) > size-treeFooterSize {
		return t, nil, ErrTree
	}
	data := make([]byte, length)
	if _, e := f.ReadAt(data, size-treeFooterSize-int64(length)); e != nil {
		return t, nil, ErrStorage
	}
	t, e := decodeTreeIndex(data)
	if e != nil {
		return t, nil, e
	}
	if t.summary().Bytes != size-treeFooterSize-int64(length) {
		return t, nil, ErrTree
	}
	return t, append(data, footer...), nil
}

// Unpack writes a completed tree transfer from a receiver root into out, a
// folder that must not exist yet and must be outside the root. It takes no
// lock and only reads the root, so it can run while the receiver serves. Every
// file is checked against its index digest and the whole payload against the
// completion receipt before out appears; out is created by renaming a fully
// written and synchronised sibling folder.
func Unpack(ctx context.Context, root, id, out string) (UnpackResult, error) {
	return unpack(ctx, root, id, out, realSnapshotOps())
}

func unpack(ctx context.Context, root, id, out string, ops snapshotOps) (result UnpackResult, err error) {
	if !spoolSupported() {
		return result, ErrUnsupported
	}
	if root == "" || out == "" || !hex256(id) {
		return result, ErrConfig
	}
	root, out = filepath.Clean(root), filepath.Clean(out)
	r := &Receiver{root: root, ops: receiverOps{snapshotOps: ops}}
	if _, e := r.sessionExists(id); e != nil {
		return result, ErrNotReady
	}
	m, e := r.load(id)
	if e != nil {
		return result, e
	}
	j, e := r.readJournal(m)
	if e != nil {
		return result, e
	}
	if j.State != Completed {
		return result, ErrNotReady
	}
	receipt, e := r.savedReceipt(m)
	if e != nil {
		return result, e
	}
	if receipt == nil || *receipt != expectedReceipt(m) {
		return result, ErrRecovery
	}
	data := r.output(m)
	info, e := os.Lstat(data)
	if e != nil || !info.Mode().IsRegular() || info.Size() != m.FileSize {
		return result, ErrRecovery
	}
	f, e := os.Open(data)
	if e != nil {
		return result, ErrStorage
	}
	defer f.Close()
	index, trailer, e := readTreeIndex(f, m.FileSize)
	if e != nil {
		return result, e
	}
	if runtime.GOOS == "windows" {
		for _, entry := range index.Entries {
			if !windowsName(entry.Path) {
				return result, fmt.Errorf("%w: %s cannot be created on Windows", ErrTree, strconv.Quote(entry.Path))
			}
		}
	}
	// out must be new, its parent must exist, and it must not land in the root.
	if _, e := os.Lstat(out); !os.IsNotExist(e) {
		return result, fmt.Errorf("%w: the output folder already exists", ErrUnpack)
	}
	parent := filepath.Dir(out)
	if info, e := os.Stat(parent); e != nil || !info.IsDir() {
		return result, fmt.Errorf("%w: the output folder's parent does not exist", ErrUnpack)
	}
	realRoot, re := resolved(root)
	realOut, oe := resolved(out)
	if re != nil || oe != nil || inside(realOut, realRoot) {
		return result, fmt.Errorf("%w: the output must be outside the receiver root", ErrUnpack)
	}
	temp, e := os.MkdirTemp(parent, ".bitspeed-unpack-")
	if e != nil {
		return result, fmt.Errorf("%w: cannot create a folder next to the output", ErrUnpack)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(temp)
		}
	}()
	full := sha256.New()
	stream := io.TeeReader(f, full)
	buf := make([]byte, 32<<10)
	var dirs []string
	for _, entry := range index.Entries {
		if e := ctx.Err(); e != nil {
			return result, e
		}
		target := filepath.Join(temp, filepath.FromSlash(entry.Path))
		if entry.Type == "dir" {
			if e := os.Mkdir(target, 0700); e != nil {
				return result, fmt.Errorf("%w: cannot create %s (a name clash on this filesystem?)", ErrUnpack, strconv.Quote(entry.Path))
			}
			dirs = append(dirs, target)
			continue
		}
		if e := writeTreeFile(ctx, target, entry, stream, buf, ops); e != nil {
			return result, e
		}
	}
	rest := make([]byte, len(trailer))
	if _, e := io.ReadFull(stream, rest); e != nil || !bytes.Equal(rest, trailer) {
		return result, ErrRecovery
	}
	var extra [1]byte
	if n, e := f.Read(extra[:]); n != 0 || e != io.EOF {
		return result, ErrRecovery
	}
	if hex.EncodeToString(full.Sum(nil)) != receipt.FileSHA256 {
		return result, ErrRecovery
	}
	// Sync folders deepest first, then the new folder itself.
	for i := len(dirs) - 1; i >= 0; i-- {
		if e := syncDir(dirs[i], ops); e != nil {
			return result, e
		}
	}
	if e := syncDir(temp, ops); e != nil {
		return result, e
	}
	if e := ctx.Err(); e != nil {
		return result, e
	}
	if _, e := os.Lstat(out); !os.IsNotExist(e) {
		return result, fmt.Errorf("%w: the output folder already exists", ErrUnpack)
	}
	if e := ops.rename(temp, out); e != nil {
		return result, fmt.Errorf("%w: cannot rename the unpacked folder into place", ErrUnpack)
	}
	published = true
	if e := syncDir(parent, ops); e != nil {
		return result, e
	}
	abs, _ := filepath.Abs(out)
	return UnpackResult{TransferID: id, Out: abs, TreeSummary: index.summary()}, nil
}

func writeTreeFile(ctx context.Context, target string, entry TreeEntry, stream io.Reader, buf []byte, ops snapshotOps) error {
	out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fmt.Errorf("%w: cannot create %s (a name clash on this filesystem?)", ErrUnpack, strconv.Quote(entry.Path))
	}
	defer out.Close()
	hash := sha256.New()
	var written int64
	for written < entry.Size {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, re := io.ReadFull(stream, buf[:min(int64(len(buf)), entry.Size-written)])
		if re != nil {
			return ErrRecovery
		}
		if w, we := ops.write(out, buf[:n]); we != nil || w != n {
			return ErrStorage
		}
		hash.Write(buf[:n])
		written += int64(n)
	}
	if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("%w: %s does not match its index digest", ErrRecovery, strconv.Quote(entry.Path))
	}
	if e = ops.sync(out); e != nil {
		return ErrStorage
	}
	if e = ops.close(out); e != nil {
		return ErrStorage
	}
	return nil
}
