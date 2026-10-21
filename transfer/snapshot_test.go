package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func snapshotPaths(t *testing.T, data []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if e := os.WriteFile(src, data, 0600); e != nil {
		t.Fatal(e)
	}
	return src, filepath.Join(dir, "spool")
}
func readyIDs(t *testing.T, spool string) []string {
	t.Helper()
	entries, e := os.ReadDir(spool)
	if e != nil {
		t.Fatal(e)
	}
	var ids []string
	for _, entry := range entries {
		if hex256(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	return ids
}
func TestPrepareAndReuse(t *testing.T) {
	for _, size := range []int{0, 1, int(MinChunkSize), int(MinChunkSize) + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte{7}, size)
			src, spool := snapshotPaths(t, data)
			m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
			if e != nil {
				t.Fatal(e)
			}
			sum := sha256.Sum256(data)
			if m.FileSHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("wrong snapshot digest")
			}
			if len(m.ChunkSHA256) > 0 {
				sum = sha256.Sum256(data[:min(size, int(MinChunkSize))])
				if m.ChunkSHA256[0] != hex.EncodeToString(sum[:]) {
					t.Fatal("wrong chunk")
				}
			}
			os.Remove(src) // Reuse must not consult the original source, even if gone.
			again, e := Reuse(context.Background(), spool, m.TransferID)
			if e != nil {
				t.Fatal(e)
			}
			a, _ := m.Identity()
			b, _ := again.Identity()
			if a != b {
				t.Fatal("identity changed")
			}
			sealed := filepath.Join(spool, m.TransferID, "snapshot")
			info, e := os.Stat(sealed)
			if e != nil || !sealedOK(sealed, info) {
				t.Fatal("not read-only", e)
			}
		})
	}
}

func TestPreparationFailuresPublishNothing(t *testing.T) {
	for _, kind := range []string{"capacity", "space error", "write", "short write", "corrupt write", "sync", "close", "rename", "cancel", "source change", "source replace"} {
		t.Run(kind, func(t *testing.T) {
			src, spool := snapshotPaths(t, bytes.Repeat([]byte{4}, int(MinChunkSize)+1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o := PrepareOptions{ChunkSize: MinChunkSize}
			ops := realSnapshotOps()
			switch kind {
			case "capacity":
				ops.space = func(string) (int64, error) { return 0, nil }
			case "space error":
				ops.space = func(string) (int64, error) { return 0, errors.New("disk") }
			case "write":
				ops.write = func(*os.File, []byte) (int, error) { return 0, errors.New("disk") }
			case "short write":
				ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) }
			case "corrupt write":
				ops.write = func(f *os.File, b []byte) (int, error) {
					copyData := append([]byte(nil), b...)
					if len(copyData) > 0 {
						copyData[0] ^= 1
					}
					return f.Write(copyData)
				}
			case "sync":
				real := ops.sync
				ops.sync = func(f *os.File) error {
					if f.Name() == filepath.Join(spool, ".lock") {
						return real(f)
					}
					if filepath.Base(f.Name()) == "snapshot" {
						return errors.New("disk")
					}
					return real(f)
				}
			case "close":
				real := ops.close
				ops.close = func(f *os.File) error {
					e := real(f)
					if filepath.Base(f.Name()) == "snapshot" {
						return errors.New("disk")
					}
					return e
				}
			case "rename":
				ops.rename = func(string, string) error { return errors.New("disk") }
			case "cancel":
				o.Progress = func(Progress) error { cancel(); return ctx.Err() }
			case "source change":
				o.Progress = func(p Progress) error {
					if p.Stage == "copying" {
						return os.WriteFile(src, []byte("changed"), 0600)
					}
					return nil
				}
			case "source replace":
				done := false
				o.Progress = func(p Progress) error {
					if !done && p.Stage == "copying" {
						done = true
						os.Rename(src, src+".old")
						return os.WriteFile(src, []byte("replacement"), 0600)
					}
					return nil
				}
			}
			if _, e := prepare(ctx, src, spool, o, ops); e == nil {
				t.Fatal("failure reported success")
			}
			if len(readyIDs(t, spool)) != 0 {
				t.Fatal("published failed preparation")
			}
		})
	}
}

func TestEverySynchronizationAndCloseBoundary(t *testing.T) {
	// Discover and then fail each persistence boundary in turn, including metadata
	// and parent synchronization after the ID directory has been published.
	for _, operation := range []string{"sync", "close", "write"} {
		src, spool := snapshotPaths(t, []byte("payload"))
		ops := realSnapshotOps()
		calls := 0
		if operation == "sync" {
			real := ops.sync
			ops.sync = func(f *os.File) error { calls++; return real(f) }
		}
		if operation == "close" {
			real := ops.close
			ops.close = func(f *os.File) error { calls++; return real(f) }
		}
		if operation == "write" {
			real := ops.write
			ops.write = func(f *os.File, b []byte) (int, error) { calls++; return real(f, b) }
		}
		if _, e := prepare(context.Background(), src, spool, PrepareOptions{}, ops); e != nil {
			t.Fatal(e)
		}
		for failure := 1; failure <= calls; failure++ {
			t.Run(fmt.Sprintf("%s-%d", operation, failure), func(t *testing.T) {
				src, spool := snapshotPaths(t, []byte("payload"))
				ops := realSnapshotOps()
				n := 0
				if operation == "sync" {
					real := ops.sync
					ops.sync = func(f *os.File) error {
						n++
						if n == failure {
							return errors.New("sync")
						}
						return real(f)
					}
				}
				if operation == "close" {
					real := ops.close
					ops.close = func(f *os.File) error {
						n++
						e := real(f)
						if n == failure {
							return errors.New("close")
						}
						return e
					}
				}
				if operation == "write" {
					real := ops.write
					ops.write = func(f *os.File, b []byte) (int, error) {
						n++
						if n == failure {
							return 0, errors.New("write")
						}
						return real(f, b)
					}
				}
				if _, e := prepare(context.Background(), src, spool, PrepareOptions{}, ops); e == nil {
					t.Fatal("accepted failed persistence stage")
				}
				for _, id := range readyIDs(t, spool) {
					if _, e := Reuse(context.Background(), spool, id); e != nil {
						t.Fatal("published directory not safely recoverable", e)
					}
				}
			})
		}
	}
}

func TestReuseRejectsDamage(t *testing.T) {
	for _, kind := range []string{"bytes", "length", "manifest", "duplicate", "unknown", "symlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			src, spool := snapshotPaths(t, []byte("original"))
			m, e := Prepare(context.Background(), src, spool, PrepareOptions{})
			if e != nil {
				t.Fatal(e)
			}
			dir := filepath.Join(spool, m.TransferID)
			snap := filepath.Join(dir, "snapshot")
			meta := filepath.Join(dir, "manifest.json")
			switch kind {
			case "bytes":
				unseal(snap)
				os.WriteFile(snap, []byte("modified"), 0400)
				reseal(snap)
			case "length":
				unseal(snap)
				os.WriteFile(snap, []byte("x"), 0400)
				reseal(snap)
			case "manifest":
				unseal(meta)
				os.WriteFile(meta, []byte("{}"), 0400)
				reseal(meta)
			case "duplicate":
				b, _ := os.ReadFile(meta)
				b = bytes.Replace(b, []byte(`{"protocol_version":1,`), []byte(`{"protocol_version":1,"protocol_version":1,`), 1)
				unseal(meta)
				os.WriteFile(meta, b, 0400)
				reseal(meta)
			case "unknown":
				os.WriteFile(filepath.Join(dir, "other"), nil, 0600)
			case "symlink":
				os.Remove(snap)
				os.Symlink(src, snap)
			case "mode":
				breakSeal(t, snap)
			}
			if _, e = Reuse(context.Background(), spool, m.TransferID); e == nil {
				t.Fatal("accepted damaged spool")
			}
		})
	}
}

func TestSpoolLockAndUnsafePaths(t *testing.T) {
	src, spool := snapshotPaths(t, nil)
	ops := realSnapshotOps()
	unlock, e := openSpool(spool, ops)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); !errors.Is(e, ErrBusy) {
		t.Fatal("lock not exclusive", e)
	}
	unlock()
	if _, e = Reuse(context.Background(), spool, "../outside"); !errors.Is(e, ErrInvalidSpool) {
		t.Fatal(e)
	}
	pending := filepath.Join(spool, ".preparing-"+strings.Repeat("a", 64))
	os.Mkdir(pending, 0700)
	os.Symlink(src, filepath.Join(pending, "snapshot"))
	if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); !errors.Is(e, ErrInvalidSpool) {
		t.Fatal("followed temporary symlink", e)
	}
	data, _ := os.ReadFile(src)
	if len(data) != 0 {
		t.Fatal("modified source")
	}
}

func TestSnapshotMemoryBound(t *testing.T) {
	src, spool := snapshotPaths(t, nil)
	f, e := os.OpenFile(src, os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(32 << 20); e != nil {
		t.Fatal(e)
	}
	f.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); e != nil {
		t.Fatal(e)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("32 MiB sparse source snapshot preparation allocated %d bytes", allocated)
	if allocated > 8<<20 {
		t.Fatal("allocation scales with complete file")
	}
}

func TestKilledPreparationRecovery(t *testing.T) {
	if os.Getenv("BITSPEED_SNAPSHOT_CHILD") == "1" {
		ctx := context.Background()
		_, _ = Prepare(ctx, os.Getenv("BITSPEED_SNAPSHOT_SOURCE"), os.Getenv("BITSPEED_SNAPSHOT_SPOOL"), PrepareOptions{Progress: func(Progress) error { os.Stdout.Write([]byte("ready\n")); select {} }})
		os.Exit(2)
	}
	src, spool := snapshotPaths(t, bytes.Repeat([]byte{1}, 1<<20))
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledPreparationRecovery$")
	cmd.Env = append(os.Environ(), "BITSPEED_SNAPSHOT_CHILD=1", "BITSPEED_SNAPSHOT_SOURCE="+src, "BITSPEED_SNAPSHOT_SPOOL="+spool)
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer cmd.Process.Kill()
	signal := make([]byte, 6)
	if _, e = out.Read(signal); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	if len(readyIDs(t, spool)) != 0 {
		t.Fatal("incomplete preparation visible")
	}
	if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); e != nil {
		t.Fatal("failed restart after kill", e)
	}
	entries, _ := os.ReadDir(spool)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".preparing-") {
			t.Fatal("temporary preparation not discarded")
		}
	}
}

func TestParentSyncRetriedAndReuseCancellation(t *testing.T) {
	src, spool := snapshotPaths(t, []byte("data"))
	ops := realSnapshotOps()
	parent := filepath.Dir(spool)
	real := ops.sync
	ops.sync = func(f *os.File) error {
		if f.Name() == parent {
			return errors.New("parent sync")
		}
		return real(f)
	}
	if _, e := prepare(context.Background(), src, spool, PrepareOptions{}, ops); e == nil {
		t.Fatal("ignored parent sync")
	}
	ops = realSnapshotOps()
	seen := false
	real = ops.sync
	ops.sync = func(f *os.File) error {
		if f.Name() == parent {
			seen = true
		}
		return real(f)
	}
	m, e := prepare(context.Background(), src, spool, PrepareOptions{}, ops)
	if e != nil || !seen {
		t.Fatal("parent not synchronized on retry", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ops = realSnapshotOps()
	real = ops.sync
	ops.sync = func(f *os.File) error {
		e := real(f)
		if filepath.Base(f.Name()) == "manifest.json" {
			cancel()
		}
		return e
	}
	if _, e = reuse(ctx, spool, m.TransferID, ops); !errors.Is(e, context.Canceled) {
		t.Fatal("ignored cancellation", e)
	}
}

func TestSpoolTrailingSymlinkRejected(t *testing.T) {
	src, spool := snapshotPaths(t, nil)
	os.Mkdir(spool, 0700)
	link := spool + "-link"
	if e := os.Symlink(spool, link); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{link, link + "/", link + "/."} {
		if _, e := Prepare(context.Background(), src, path, PrepareOptions{}); !errors.Is(e, ErrInvalidSpool) {
			t.Fatal("followed symlink spool", path, e)
		}
	}
}

func TestKilledPublicationRecovery(t *testing.T) {
	stage := os.Getenv("BITSPEED_SNAPSHOT_PUBLICATION_CHILD")
	if stage != "" {
		ops := realSnapshotOps()
		wait := func() { fmt.Println("ready"); select {} }
		if stage == "manifest" {
			real := ops.write
			ops.write = func(f *os.File, b []byte) (int, error) {
				n, e := real(f, b)
				if filepath.Base(f.Name()) == "manifest.json" {
					wait()
				}
				return n, e
			}
		}
		if stage == "rename" {
			real := ops.rename
			ops.rename = func(a, b string) error {
				e := real(a, b)
				if e == nil {
					wait()
				}
				return e
			}
		}
		_, _ = prepare(context.Background(), os.Getenv("BITSPEED_SNAPSHOT_SOURCE"), os.Getenv("BITSPEED_SNAPSHOT_SPOOL"), PrepareOptions{}, ops)
		os.Exit(2)
	}
	for _, stage := range []string{"manifest", "rename"} {
		t.Run(stage, func(t *testing.T) {
			src, spool := snapshotPaths(t, []byte("saved data"))
			cmd := exec.Command(os.Args[0], "-test.run=^TestKilledPublicationRecovery$")
			cmd.Env = append(os.Environ(), "BITSPEED_SNAPSHOT_PUBLICATION_CHILD="+stage, "BITSPEED_SNAPSHOT_SOURCE="+src, "BITSPEED_SNAPSHOT_SPOOL="+spool)
			out, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			defer cmd.Process.Kill()
			signal := make([]byte, 6)
			if _, e = out.Read(signal); e != nil {
				t.Fatal(e)
			}
			cmd.Process.Kill()
			_ = cmd.Wait()
			ids := readyIDs(t, spool)
			if stage == "manifest" {
				if len(ids) != 0 {
					t.Fatal("unfinished metadata visible")
				}
				if _, e = Prepare(context.Background(), src, spool, PrepareOptions{}); e != nil {
					t.Fatal(e)
				}
			} else {
				if len(ids) != 1 {
					t.Fatal("published ID lost")
				}
				ops := realSnapshotOps()
				real := ops.sync
				ops.sync = func(f *os.File) error {
					if f.Name() == spool {
						return errors.New("recovery sync")
					}
					return real(f)
				}
				if _, e = reuse(context.Background(), spool, ids[0], ops); e == nil {
					t.Fatal("accepted failed recovery sync")
				}
				if _, e = Reuse(context.Background(), spool, ids[0]); e != nil {
					t.Fatal("published snapshot not recoverable", e)
				}
			}
		})
	}
}

func TestBoundedDirectoryScans(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 100; i++ {
		if e := os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := boundedEntries(dir, 2); !errors.Is(e, ErrInvalidSpool) {
		t.Fatal("unbounded directory accepted", e)
	}
	if _, e := boundedEntries(dir, 64); !errors.Is(e, ErrInvalidSpool) {
		t.Fatal("unbounded root accepted", e)
	}
}
