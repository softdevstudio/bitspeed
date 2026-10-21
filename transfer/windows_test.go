//go:build windows

package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise the real Windows calls behind docs/windows-persistence.md.

func TestWindowsPrivateDirectoriesAndFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if e := makePrivateDir(dir); e != nil {
		t.Fatal(e)
	}
	if e := makePrivateDir(dir); !os.IsExist(e) {
		t.Fatal("second create must report an existing directory", e)
	}
	if e := privateDir(dir); e != nil {
		t.Fatal("own directory not private", e)
	}
	file := filepath.Join(dir, "file")
	if e := os.WriteFile(file, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	info, _ := os.Lstat(file)
	if !privateFileOK(file, info) || !sealedOK(file, info) {
		t.Fatal("inherited file DACL not private")
	}
	inner := filepath.Join(dir, "inner")
	if e := os.Mkdir(inner, 0700); e != nil {
		t.Fatal(e)
	}
	if e := privateDir(inner); e != nil {
		t.Fatal("inherited directory DACL not private", e)
	}
	for _, sid := range []string{"*S-1-1-0", "*S-1-5-11", "*S-1-5-32-545"} {
		icacls(t, inner, "/grant", sid+":(OI)(CI)(R)")
		if privateDir(inner) == nil {
			t.Fatal("directory readable by", sid, "accepted")
		}
		icacls(t, inner, "/remove:g", sid)
		if e := privateDir(inner); e != nil {
			t.Fatal("restored directory refused", e)
		}
	}
	icacls(t, inner, "/deny", "*S-1-1-0:(W)")
	if e := privateDir(inner); e != nil {
		t.Fatal("deny entry must not break privacy", e)
	}
	exposeFile(t, file)
	info, _ = os.Lstat(file)
	if privateFileOK(file, info) {
		t.Fatal("exposed file accepted")
	}
	if privateDirOK(file, info) || privateFileOK(dir, info) {
		t.Fatal("kind mismatch accepted")
	}
}

func TestWindowsFlushesReadHandlesAndDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "flush")
	if e := makePrivateDir(dir); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(dir, "data")
	if e := os.WriteFile(file, []byte("data"), 0600); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(file)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("plain Sync on a read handle: %v", f.Sync())
	if e = syncHandle(f); e != nil {
		t.Fatal("read handle flush", e)
	}
	f.Close()
	d, e := os.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("plain Sync on a directory handle: %v", d.Sync())
	if e = syncHandle(d); e != nil {
		t.Fatal("directory flush", e)
	}
	d.Close()
	if e = syncDir(dir, realSnapshotOps()); e != nil {
		t.Fatal(e)
	}
}

func TestWindowsRenameAndNoReplaceLink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "names")
	if e := makePrivateDir(dir); e != nil {
		t.Fatal(e)
	}
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	os.WriteFile(a, []byte("a"), 0600)
	os.WriteFile(b, []byte("b"), 0600)
	if e := os.Link(a, b); !os.IsExist(e) {
		t.Fatal("link replaced or failed oddly", e)
	}
	if e := os.Link(a, c); e != nil {
		t.Fatal(e)
	}
	ia, _ := os.Lstat(a)
	ic, _ := os.Lstat(c)
	if !os.SameFile(ia, ic) {
		t.Fatal("hard link identity")
	}
	if e := renamePath(a, b); e != nil {
		t.Fatal("replace rename", e)
	}
	if data, _ := os.ReadFile(b); string(data) != "a" {
		t.Fatal("rename content", string(data))
	}
	from := filepath.Join(dir, "from")
	makePrivateDir(from)
	if e := renamePath(from, filepath.Join(dir, "to")); e != nil {
		t.Fatal("directory rename", e)
	}
}

func TestWindowsSourceWritersAreRefusedAndBlocked(t *testing.T) {
	src, spool := snapshotPaths(t, []byte("stable source"))
	writer, e := os.OpenFile(src, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); !errors.Is(e, ErrSource) {
		t.Fatal("source open for writing was accepted", e)
	}
	writer.Close()
	held, e := openSource(src)
	if e != nil {
		t.Fatal(e)
	}
	if w, e := os.OpenFile(src, os.O_WRONLY, 0); e == nil {
		w.Close()
		t.Fatal("writer allowed during preparation")
	}
	if e = os.Rename(src, src+".moved"); e == nil {
		t.Fatal("rename allowed during preparation")
	}
	held.Close()
	if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); e != nil {
		t.Fatal(e)
	}
}

func TestWindowsLockAndFilesystem(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	ops := realSnapshotOps()
	unlock, e := openSpool(spool, ops)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = openSpool(spool, ops); !errors.Is(e, ErrBusy) {
		t.Fatal("second lock holder", e)
	}
	unlock()
	unlock, e = openSpool(spool, ops)
	if e != nil {
		t.Fatal("lock not released", e)
	}
	unlock()
	if e = receiverFilesystem(spool); e != nil {
		t.Fatal("runner temp volume should be local NTFS", e)
	}
	if free, e := freeSpace(spool); e != nil || free <= 0 {
		t.Fatal("free space", free, e)
	}
}
