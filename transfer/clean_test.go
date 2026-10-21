package transfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func chunkFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestCleanReceiverRemovesOnlyCompletedChunks(t *testing.T) {
	if !spoolSupported() {
		t.Skip("receiver unsupported on this platform")
	}
	data := bytes.Repeat([]byte("done"), int(MinChunkSize)/2+9)
	r, server, spool, m := stagedFile(t, data)
	if s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	src, spool2 := snapshotPaths(t, bytes.Repeat([]byte("open"), int(MinChunkSize)/2+3))
	m2, e := Prepare(context.Background(), src, spool2, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = UploadOnly(context.Background(), server.URL, testToken, spool2, m2.TransferID); e != nil {
		t.Fatal(e)
	}
	root := r.root
	done := filepath.Join(root, "sessions", m.TransferID, "chunks")
	open := filepath.Join(root, "sessions", m2.TransferID, "chunks")
	evidence := filepath.Join(done, "0.bad")
	if e = os.WriteFile(evidence, data[:MinChunkSize], 0600); e != nil {
		t.Fatal(e)
	}

	if _, e = CleanReceiver(context.Background(), root, false); !errors.Is(e, ErrBusy) {
		t.Fatal("clean must refuse a running receiver", e)
	}
	server.Close()
	r.Close()

	before := len(chunkFiles(t, done))
	results, e := CleanReceiver(context.Background(), root, true)
	if e != nil || len(results) != 2 || len(chunkFiles(t, done)) != before {
		t.Fatal("dry run must not remove chunks", results, e)
	}
	results, e = CleanReceiver(context.Background(), root, false)
	if e != nil || len(results) != 2 {
		t.Fatal(results, e)
	}
	for _, result := range results {
		switch result.TransferID {
		case m.TransferID:
			if !result.Cleaned || result.ChunksRemoved != len(m.ChunkSHA256) || result.BytesFreed != m.FileSize || result.EvidenceKept != 1 {
				t.Fatal("completed session not cleaned exactly", result)
			}
		case m2.TransferID:
			if result.Cleaned || result.ChunksRemoved != 0 || result.SkippedBecause == "" {
				t.Fatal("unfinished session must be skipped", result)
			}
		default:
			t.Fatal("unexpected session", result)
		}
	}
	if names := chunkFiles(t, done); len(names) != 1 || names[0] != "0.bad" {
		t.Fatal("only evidence may remain", names)
	}
	if len(chunkFiles(t, open)) != len(m2.ChunkSHA256) {
		t.Fatal("unfinished chunks were touched")
	}
	if saved, e := os.ReadFile(filepath.Join(root, "files", m.TransferID+".data")); e != nil || !bytes.Equal(saved, data) {
		t.Fatal("destination changed", e)
	}

	// Cleaning again is a no-op, and the receiver still serves both sessions.
	if results, e = CleanReceiver(context.Background(), root, false); e != nil || results[0].ChunksRemoved+results[1].ChunksRemoved != 0 {
		t.Fatal("repeat clean", results, e)
	}
	r2, e := NewReceiver(ReceiverConfig{Root: root, Token: testToken})
	if e != nil {
		t.Fatal("cleaned root must open", e)
	}
	defer r2.Close()
	s, e := r2.status(context.Background(), m)
	if e != nil || s.State != Completed || s.Receipt == nil || *s.Receipt != expectedReceipt(m) {
		t.Fatal("completed status after clean", s, e)
	}
}

func TestCleanReceiverPartialCleanupStillOpens(t *testing.T) {
	if !spoolSupported() {
		t.Skip("receiver unsupported on this platform")
	}
	data := bytes.Repeat([]byte{7}, 3*int(MinChunkSize)+11)
	r, server, spool, m := stagedFile(t, data)
	if s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	server.Close()
	r.Close()
	// A crash part way through clean leaves some chunks removed and some not.
	chunks := filepath.Join(r.root, "sessions", m.TransferID, "chunks")
	if e := os.Remove(filepath.Join(chunks, strconv.Itoa(1))); e != nil {
		t.Fatal(e)
	}
	results, e := CleanReceiver(context.Background(), r.root, false)
	if e != nil || len(results) != 1 || results[0].ChunksRemoved != len(m.ChunkSHA256)-1 {
		t.Fatal(results, e)
	}
	r2, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken})
	if e != nil {
		t.Fatal(e)
	}
	defer r2.Close()
	if s, e := r2.status(context.Background(), m); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
}

func TestCleanReceiverRejectsBadRoots(t *testing.T) {
	if !spoolSupported() {
		t.Skip("receiver unsupported on this platform")
	}
	if _, e := CleanReceiver(context.Background(), "", false); !errors.Is(e, ErrConfig) {
		t.Fatal(e)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, e := CleanReceiver(context.Background(), missing, false); !errors.Is(e, ErrConfig) {
		t.Fatal(e)
	}
	if _, e := os.Lstat(missing); !os.IsNotExist(e) {
		t.Fatal("clean must not create a root")
	}
	// A destination with no session binding blocks startup, so clean removes nothing.
	data := bytes.Repeat([]byte{3}, int(MinChunkSize)+5)
	r, server, spool, m := stagedFile(t, data)
	if s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	server.Close()
	r.Close()
	stray := filepath.Join(r.root, "files", "unknown.data")
	if e := os.WriteFile(stray, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	chunks := filepath.Join(r.root, "sessions", m.TransferID, "chunks")
	before := len(chunkFiles(t, chunks))
	if _, e := CleanReceiver(context.Background(), r.root, false); !errors.Is(e, ErrRecovery) {
		t.Fatal("damaged root must stop clean", e)
	}
	if len(chunkFiles(t, chunks)) != before {
		t.Fatal("clean removed chunks from a root that failed recovery")
	}
}

func completedRoot(t *testing.T, size int) (string, Manifest) {
	t.Helper()
	data := bytes.Repeat([]byte{5}, size)
	r, server, spool, m := stagedFile(t, data)
	if s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	server.Close()
	r.Close()
	return r.root, m
}

func TestCleanReceiverKeepsChunksOfDamagedDestination(t *testing.T) {
	if !spoolSupported() {
		t.Skip("receiver unsupported on this platform")
	}
	root, m := completedRoot(t, 2*int(MinChunkSize)+3)
	output := filepath.Join(root, "files", m.TransferID+".data")
	f, e := os.OpenFile(output, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.WriteAt([]byte{9}, 0); e != nil {
		t.Fatal(e)
	}
	f.Close()
	chunks := filepath.Join(root, "sessions", m.TransferID, "chunks")
	before := len(chunkFiles(t, chunks))
	results, e := CleanReceiver(context.Background(), root, false)
	if e != nil || len(results) != 1 || results[0].State != Failed || results[0].Cleaned || results[0].ChunksRemoved != 0 {
		t.Fatal("damaged destination must be marked failed and keep its chunks", results, e)
	}
	if len(chunkFiles(t, chunks)) != before {
		t.Fatal("chunks removed from a failed session")
	}
}

func TestCleanReceiverWorksWithoutWriteSpace(t *testing.T) {
	if !spoolSupported() {
		t.Skip("receiver unsupported on this platform")
	}
	root, m := completedRoot(t, int(MinChunkSize)+1)
	ops := realReceiverOps()
	ops.write = func(*os.File, []byte) (int, error) { return 0, errors.New("no space") }
	if _, e := newReceiver(ReceiverConfig{Root: root, Token: testToken}, ops); e == nil {
		t.Fatal("serve startup should need its write probe")
	}
	results, e := cleanReceiver(context.Background(), root, false, ops)
	if e != nil || len(results) != 1 || results[0].ChunksRemoved != len(m.ChunkSHA256) {
		t.Fatal("clean must not need to write on a completed root", results, e)
	}
}

func TestCleanReceiverStorageAndCancellation(t *testing.T) {
	if !spoolSupported() {
		t.Skip("receiver unsupported on this platform")
	}
	root, m := completedRoot(t, 2*int(MinChunkSize))
	chunks := filepath.Join(root, "sessions", m.TransferID, "chunks")
	ops := realReceiverOps()
	ops.sync = func(f *os.File) error {
		if filepath.Clean(f.Name()) == chunks {
			return errors.New("sync")
		}
		return f.Sync()
	}
	if _, e := cleanReceiver(context.Background(), root, false, ops); !errors.Is(e, ErrStorage) {
		t.Fatal("a failed directory sync must be reported", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := CleanReceiver(ctx, root, false); e == nil {
		t.Fatal("cancelled clean must fail")
	}
	results, e := CleanReceiver(context.Background(), root, false)
	if e != nil || len(results) != 1 || !results[0].Cleaned {
		t.Fatal("clean must finish after earlier failures", results, e)
	}
	if names := chunkFiles(t, chunks); len(names) != 0 {
		t.Fatal(names)
	}
}
