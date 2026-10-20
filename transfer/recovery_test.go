//go:build linux || darwin

package transfer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveryRepairAndReservation(t *testing.T) {
	r, server := testReceiver(t)
	src, spool := snapshotPaths(t, bytes.Repeat([]byte{7}, int(MinChunkSize)+17))
	m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	os.Remove(src)
	chunks := filepath.Join(r.root, "sessions", m.TransferID, "chunks")
	if e = os.WriteFile(filepath.Join(chunks, "0"), []byte("damaged"), 0600); e != nil {
		t.Fatal(e)
	}
	server.Close()
	r.Close()
	restarted, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken, MaxSessions: 1, MaxReservedBytes: 2*m.FileSize + spoolHeadroom})
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	count, reserved, e := restarted.inventory()
	if e != nil || count != 1 || reserved != 2*m.FileSize+spoolHeadroom {
		t.Fatal(count, reserved, e)
	}
	s, e := restarted.status(context.Background(), m)
	if e != nil || s.VerifiedChunks != 1 || len(s.MissingChunks) != 1 || s.MissingChunks[0] != 0 {
		t.Fatal(s, e)
	}
	var puts atomic.Int64
	resumed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "PUT" {
			puts.Add(1)
		}
		restarted.ServeHTTP(w, q)
	}))
	defer resumed.Close()
	s, e = UploadOnly(context.Background(), resumed.URL, testToken, spool, m.TransferID)
	if e != nil || s.VerifiedChunks != 2 || puts.Load() != 1 {
		t.Fatal(s, e, puts.Load())
	}
	if data, e := os.ReadFile(filepath.Join(chunks, "0.bad")); e != nil || string(data) != "damaged" {
		t.Fatal("lost corrupt evidence", e)
	}
	// A second independent corruption must preserve both pieces and fail closed.
	os.WriteFile(filepath.Join(chunks, "0"), []byte("again"), 0600)
	if _, e = restarted.status(context.Background(), m); !errors.Is(e, ErrRecovery) {
		t.Fatal("replaced evidence", e)
	}
}

func TestRecoveryRejectsUnexpectedEvidence(t *testing.T) {
	for _, kind := range []string{"pending unknown", "pending symlink", "pending data", "bad journal", "oversize chunk", "chunk symlink", "bad quarantine", "unknown root", "foreign probe link"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := testReceiver(t)
			m := dataManifest([]byte("data"))
			admitTest(t, r, m)
			dir := filepath.Join(r.root, "sessions", m.TransferID)
			chunks := filepath.Join(dir, "chunks")
			pending := filepath.Join(r.root, "sessions", ".admitting-"+strings.Repeat("b", 64))
			switch kind {
			case "pending unknown", "pending symlink", "pending data":
				os.Mkdir(pending, 0700)
				if kind == "pending unknown" {
					os.WriteFile(filepath.Join(pending, "unexpected"), []byte("keep"), 0600)
				}
				if kind == "pending symlink" {
					os.Symlink(filepath.Join(dir, "manifest.json"), filepath.Join(pending, "manifest.json"))
				}
				if kind == "pending data" {
					os.Mkdir(filepath.Join(pending, "chunks"), 0700)
					os.WriteFile(filepath.Join(pending, "chunks", "0"), []byte("keep"), 0600)
				}
			case "bad journal":
				os.WriteFile(filepath.Join(dir, "journal.json"), []byte("{}"), 0600)
			case "oversize chunk":
				os.WriteFile(filepath.Join(chunks, "0"), []byte("oversize"), 0600)
			case "chunk symlink":
				os.Symlink(filepath.Join(dir, "manifest.json"), filepath.Join(chunks, "0"))
			case "bad quarantine":
				os.Symlink(filepath.Join(dir, "manifest.json"), filepath.Join(chunks, "0.bad"))
			case "unknown root":
				os.WriteFile(filepath.Join(r.root, "unknown"), []byte("keep"), 0600)
			case "foreign probe link":
				probe := filepath.Join(r.root, ".probe-123")
				os.Mkdir(probe, 0700)
				os.WriteFile(filepath.Join(probe, "source"), []byte("probe"), 0600)
				os.WriteFile(filepath.Join(r.root, "files", ".probe-123"), []byte("probe"), 0600)
			}
			r.Close()
			if again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken}); e == nil {
				again.Close()
				t.Fatal("unexpected evidence accepted")
			}
			if kind == "foreign probe link" {
				if _, e := os.Stat(filepath.Join(r.root, "files", ".probe-123")); e != nil {
					t.Fatal("foreign evidence deleted", e)
				}
			}
		})
	}
}

func TestRecoveryQuarantineFailureGates(t *testing.T) {
	for _, op := range []string{"link", "sync", "close", "after link", "read sync"} {
		t.Run(op, func(t *testing.T) {
			r, _ := testReceiver(t)
			m := dataManifest([]byte("data"))
			admitTest(t, r, m)
			path := filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")
			os.WriteFile(path, []byte("bad!"), 0600)
			real := r.ops
			switch op {
			case "link":
				r.ops.link = func(string, string) error { return errors.New("injected") }
			case "sync":
				r.ops.sync = func(*os.File) error { return errors.New("injected") }
			case "close":
				r.ops.close = func(f *os.File) error { f.Close(); return errors.New("injected") }
			case "after link":
				r.ops.link = func(a, b string) error {
					if e := os.Link(a, b); e != nil {
						return e
					}
					return errors.New("lost result")
				}
			case "read sync":
				os.WriteFile(path, []byte("data"), 0600)
				r.ops.sync = func(*os.File) error { return errors.New("injected") }
			}
			if _, e := r.status(context.Background(), m); e == nil {
				t.Fatal("failed persistence advertised")
			}
			r.ops = real
			s, e := r.status(context.Background(), m)
			if e != nil {
				t.Fatal(e)
			}
			if op == "read sync" {
				if s.VerifiedChunks != 1 {
					t.Fatal(s)
				}
			} else if s.VerifiedChunks != 0 || len(s.MissingChunks) != 1 {
				t.Fatal(s)
			}
		})
	}
}

// The receiver and sender helpers are real separate processes. Parent kills both
// at injected I/O/response boundaries, then starts new processes on the same
// receiver storage and sender snapshot. No product crash hooks are required.
func TestRecoveryProcessHelper(t *testing.T) {
	mode := os.Getenv("BITSPEED_RECOVERY_HELPER")
	if mode == "" {
		return
	}
	if mode == "sender" {
		_, e := UploadOnly(context.Background(), os.Getenv("BITSPEED_RECOVERY_URL"), testToken, os.Getenv("BITSPEED_RECOVERY_SPOOL"), os.Getenv("BITSPEED_RECOVERY_ID"))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(3)
		}
		os.Exit(0)
	}
	point := os.Getenv("BITSPEED_RECOVERY_POINT")
	stop := func() {
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	ops := realReceiverOps()
	ops.write = func(f *os.File, b []byte) (int, error) {
		n, e := f.Write(b)
		if (point == "chunk write" && filepath.Base(f.Name()) == "incoming.tmp") || (point == "admission write" && filepath.Base(f.Name()) == "manifest.json") {
			stop()
		}
		return n, e
	}
	ops.rename = func(a, b string) error {
		e := os.Rename(a, b)
		if e == nil && point == "admission rename" {
			stop()
		}
		return e
	}
	ops.link = func(a, b string) error {
		e := os.Link(a, b)
		if e == nil && point == "chunk link" && filepath.Base(a) == "incoming.tmp" {
			stop()
		}
		return e
	}
	r, e := newReceiver(ReceiverConfig{Root: os.Getenv("BITSPEED_RECOVERY_ROOT"), Token: testToken}, ops)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(4)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "PUT" && point == "before acknowledgement" {
			response := httptest.NewRecorder()
			r.ServeHTTP(response, q)
			if response.Code == 200 {
				stop()
			}
			for k, v := range response.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(response.Code)
			w.Write(response.Body.Bytes())
			return
		}
		if q.Method == "PUT" {
			fmt.Println("put")
		}
		r.ServeHTTP(w, q)
	}))
	fmt.Println(server.URL)
	for {
		time.Sleep(time.Hour)
	}
}

type recoveryChild struct {
	cmd    *exec.Cmd
	lines  chan string
	stderr bytes.Buffer
}

func startRecoveryChild(t *testing.T, env ...string) *recoveryChild {
	t.Helper()
	c := &recoveryChild{cmd: exec.Command(os.Args[0], "-test.run=^TestRecoveryProcessHelper$"), lines: make(chan string, 32)}
	c.cmd.Env = append(os.Environ(), env...)
	c.cmd.Stderr = &c.stderr
	out, e := c.cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = c.cmd.Start(); e != nil {
		t.Fatal(e)
	}
	go func() {
		scan := bufio.NewScanner(out)
		for scan.Scan() {
			c.lines <- scan.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() { c.cmd.Process.Kill(); c.cmd.Wait() })
	return c
}
func childLine(t *testing.T, c *recoveryChild, want string) string {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				t.Fatal("child ended before", want)
			}
			if want == "url" && strings.HasPrefix(line, "http://") {
				return line
			}
			if line == want {
				return line
			}
		case <-timer.C:
			t.Fatal("child did not reach", want)
		}
	}
}
func TestKilledUploadResume(t *testing.T) {
	for _, point := range []string{"admission write", "admission rename", "chunk write", "chunk link", "before acknowledgement"} {
		t.Run(point, func(t *testing.T) {
			src, spool := snapshotPaths(t, bytes.Repeat([]byte{9}, int(MinChunkSize)+17))
			m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
			if e != nil {
				t.Fatal(e)
			}
			os.Remove(src)
			root := filepath.Join(t.TempDir(), "receiver")
			receiver := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=receiver", "BITSPEED_RECOVERY_ROOT="+root, "BITSPEED_RECOVERY_POINT="+point)
			url := childLine(t, receiver, "url")
			sender := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=sender", "BITSPEED_RECOVERY_URL="+url, "BITSPEED_RECOVERY_SPOOL="+spool, "BITSPEED_RECOVERY_ID="+m.TransferID)
			childLine(t, receiver, "ready")
			sender.cmd.Process.Kill()
			sender.cmd.Wait()
			receiver.cmd.Process.Kill()
			receiver.cmd.Wait()
			// The link kill happens before directory sync. Even valid bytes cannot
			// be reported until a restarted receiver completes its recovery gate.
			if point == "chunk link" {
				ops := realReceiverOps()
				chunk := filepath.Join(root, "sessions", m.TransferID, "chunks", "0")
				ops.sync = func(f *os.File) error {
					if f.Name() == chunk {
						return errors.New("recovery sync injected")
					}
					return f.Sync()
				}
				if failed, e := newReceiver(ReceiverConfig{Root: root, Token: testToken}, ops); e == nil {
					failed.Close()
					t.Fatal("acknowledged killed publication despite failed recovery sync")
				}
				if _, e := os.Stat(chunk); e != nil {
					t.Fatal("lost published evidence", e)
				}
			}
			restarted := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=receiver", "BITSPEED_RECOVERY_ROOT="+root)
			url = childLine(t, restarted, "url")
			s, e := QueryStatus(context.Background(), url, testToken, m.TransferID)
			expected := 2
			if point == "chunk link" || point == "before acknowledgement" {
				expected = 1
			}
			if point != "admission write" && (e != nil || len(s.MissingChunks) != expected) {
				t.Fatal(s, e)
			}
			again := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=sender", "BITSPEED_RECOVERY_URL="+url, "BITSPEED_RECOVERY_SPOOL="+spool, "BITSPEED_RECOVERY_ID="+m.TransferID)
			done := make(chan error, 1)
			go func() { done <- again.cmd.Wait() }()
			select {
			case e = <-done:
				if e != nil {
					t.Fatal(e, again.stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("sender did not resume")
			}
			s, e = QueryStatus(context.Background(), url, testToken, m.TransferID)
			if e != nil || s.VerifiedChunks != 2 || s.VerifiedBytes != m.FileSize || s.Receipt != nil {
				t.Fatal(s, e)
			}
			for i := 0; i < expected; i++ {
				childLine(t, restarted, "put")
			}
			select {
			case extra := <-restarted.lines:
				t.Fatal("unexpected repeated upload", extra)
			default:
			}
			files, e := os.ReadDir(filepath.Join(root, "files"))
			if e != nil || len(files) != 0 {
				t.Fatal("published incomplete file", e)
			}
		})
	}
}

func TestLostChunkResponseResume(t *testing.T) {
	r, _ := testReceiver(t)
	src, spool := snapshotPaths(t, bytes.Repeat([]byte{4}, int(MinChunkSize)+17))
	m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	var puts atomic.Int64
	var dropped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "PUT" {
			puts.Add(1)
			if !dropped.Swap(true) {
				response := httptest.NewRecorder()
				r.ServeHTTP(response, q)
				if response.Code != 200 {
					t.Error(response.Code, response.Body.String())
				}
				conn, _, e := w.(http.Hijacker).Hijack()
				if e == nil {
					conn.Close()
				}
				return
			}
		}
		r.ServeHTTP(w, q)
	}))
	defer server.Close()
	if _, e = UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID); e == nil {
		t.Fatal("lost response accepted")
	}
	s, e := UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID)
	if e != nil || s.VerifiedChunks != 2 || puts.Load() != 2 {
		t.Fatal("retransmitted substantiated chunk", s, e, puts.Load())
	}
}

func TestInterruptedProbeRecovery(t *testing.T) {
	for _, phase := range []string{"empty", "partial", "linked"} {
		t.Run(phase, func(t *testing.T) {
			r, _ := testReceiver(t)
			r.Close()
			dir := filepath.Join(r.root, ".probe-1234")
			os.Mkdir(dir, 0700)
			if phase != "empty" {
				os.WriteFile(filepath.Join(dir, "source"), []byte("pro"), 0600)
			}
			if phase == "linked" {
				os.Link(filepath.Join(dir, "source"), filepath.Join(r.root, "files", ".probe-1234"))
			}
			again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken})
			if e != nil {
				t.Fatal(e)
			}
			again.Close()
			if _, e = os.Lstat(dir); !os.IsNotExist(e) {
				t.Fatal("probe not cleaned", e)
			}
		})
	}
}

func TestRecoveryStartupPersistenceAndCancellation(t *testing.T) {
	for _, operation := range []string{"sync", "close", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			r, _ := testReceiver(t)
			m := dataManifest([]byte("data"))
			admitTest(t, r, m)
			chunks := filepath.Join(r.root, "sessions", m.TransferID, "chunks")
			os.WriteFile(filepath.Join(chunks, "0"), []byte("data"), 0600)
			r.Close()
			ops := realReceiverOps()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if operation == "cancel" {
				cancel()
			}
			if operation == "sync" {
				ops.sync = func(f *os.File) error {
					if f.Name() == filepath.Join(chunks, "0") {
						return errors.New("injected")
					}
					return f.Sync()
				}
			}
			if operation == "close" {
				ops.close = func(f *os.File) error {
					e := f.Close()
					if f.Name() == filepath.Join(chunks, "0") {
						return errors.New("injected")
					}
					return e
				}
			}
			if again, e := newReceiverContext(ctx, ReceiverConfig{Root: r.root, Token: testToken}, ops); e == nil {
				again.Close()
				t.Fatal("failed startup reported success")
			}
			again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken})
			if e != nil {
				t.Fatal("failed startup leaked lock", e)
			}
			again.Close()
		})
	}
}

func TestCorruptChunkCloseFailurePreservesEvidence(t *testing.T) {
	r, _ := testReceiver(t)
	m := dataManifest([]byte("data"))
	admitTest(t, r, m)
	path := filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")
	os.WriteFile(path, []byte("bad!"), 0600)
	real := r.ops.close
	r.ops.close = func(f *os.File) error {
		e := f.Close()
		if f.Name() == path {
			return errors.New("close injected")
		}
		return e
	}
	if _, e := r.status(context.Background(), m); !errors.Is(e, ErrStorage) {
		t.Fatal("ignored corrupt file close failure", e)
	}
	if _, e := os.Stat(path); e != nil {
		t.Fatal("quarantined despite close failure", e)
	}
	if _, e := os.Lstat(path + ".bad"); !os.IsNotExist(e) {
		t.Fatal("created quarantine despite close failure", e)
	}
	r.ops.close = real
	if s, e := r.status(context.Background(), m); e != nil || len(s.MissingChunks) != 1 {
		t.Fatal(s, e)
	}
}

func TestDirectUploadRepairsCorruptChunk(t *testing.T) {
	r, _ := testReceiver(t)
	m := dataManifest([]byte("data"))
	admitTest(t, r, m)
	path := filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")
	os.WriteFile(path, []byte("bad!"), 0600)
	w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", strings.NewReader("data"), 4)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if data, e := os.ReadFile(path); e != nil || string(data) != "data" {
		t.Fatal(e)
	}
	if data, e := os.ReadFile(path + ".bad"); e != nil || string(data) != "bad!" {
		t.Fatal("lost evidence", e)
	}
}
