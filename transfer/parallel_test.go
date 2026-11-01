package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldPut starts a chunk PUT whose body is held open until release is called,
// so the request stays in flight with its index reserved.
type heldPut struct {
	done    chan *httptest.ResponseRecorder
	writer  *io.PipeWriter
	payload []byte
}

func startHeldPut(t *testing.T, r *Receiver, m Manifest, data []byte, index int) *heldPut {
	t.Helper()
	start := index * int(m.ChunkSize)
	payload := data[start:min(start+int(m.ChunkSize), len(data))]
	reader, writer := io.Pipe()
	h := &heldPut{done: make(chan *httptest.ResponseRecorder, 1), writer: writer, payload: payload}
	go func() {
		h.done <- perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/"+strconv.Itoa(index), "application/octet-stream", reader, int64(len(payload)))
	}()
	// Write all but the last byte: the receiver is now streaming the body.
	if _, e := writer.Write(payload[:len(payload)-1]); e != nil {
		t.Fatal(e)
	}
	return h
}
func (h *heldPut) finish(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	if _, e := h.writer.Write(h.payload[len(h.payload)-1:]); e != nil {
		t.Fatal(e)
	}
	h.writer.Close()
	select {
	case w := <-h.done:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("held upload did not finish")
	}
	return nil
}
func waitInflight(t *testing.T, r *Receiver, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.inflight[id])
		r.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("in-flight count never reached", n)
}
func chunked(n int) []byte {
	data := make([]byte, n*int(MinChunkSize)+11)
	for i := range data {
		data[i] = byte(i*7 + i/int(MinChunkSize))
	}
	return data
}

func TestParallelChunksStreamWithoutTheLock(t *testing.T) {
	r, _ := testReceiver(t)
	data := chunked(3)
	m := dataManifest(data)
	admitTest(t, r, m)
	held := startHeldPut(t, r, m, data, 0)
	waitInflight(t, r, m.TransferID, 1)
	// A second chunk is accepted and stored while the first is still streaming.
	w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/1", "application/octet-stream", bytes.NewReader(data[MinChunkSize:2*MinChunkSize]), MinChunkSize)
	if w.Code != 200 {
		t.Fatal("second chunk blocked by an in-flight upload", w.Code, w.Body.String())
	}
	// Status works too, reports the streaming chunk as missing and leaves its temp file.
	w = perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"missing_chunks":[0,2,3]`) {
		t.Fatal("status during upload", w.Code, w.Body.String())
	}
	if _, e := os.Lstat(filepath.Join(r.root, "sessions", m.TransferID, "chunks", "incoming-0.tmp")); e != nil {
		t.Fatal("status removed an in-flight temp file", e)
	}
	// Finalize must wait for in-flight chunks.
	body := []byte(`{"manifest_sha256":"` + mustIdentity(t, m) + `"}`)
	w = perform(r, "POST", "/v1/transfers/"+m.TransferID+"/finalize", "application/json", bytes.NewReader(body), int64(len(body)))
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"session_busy"`) || !strings.Contains(w.Body.String(), `"retryable":true`) {
		t.Fatal("finalize during upload", w.Code, w.Body.String())
	}
	if w = held.finish(t); w.Code != 200 {
		t.Fatal("held chunk", w.Code, w.Body.String())
	}
	waitInflight(t, r, m.TransferID, 0)
	if _, e := os.Lstat(filepath.Join(r.root, "sessions", m.TransferID, "chunks", "incoming-0.tmp")); !os.IsNotExist(e) {
		t.Fatal("temp file left after commit", e)
	}
	w = perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	if !strings.Contains(w.Body.String(), `"missing_chunks":[2,3]`) {
		t.Fatal(w.Body.String())
	}
}

func mustIdentity(t *testing.T, m Manifest) string {
	t.Helper()
	id, e := m.Identity()
	if e != nil {
		t.Fatal(e)
	}
	return id
}

func TestSameChunkTwiceAndInflightCap(t *testing.T) {
	r, _ := testReceiver(t)
	data := chunked(MaxInflightChunks + 1)
	m := dataManifest(data)
	admitTest(t, r, m)
	first := startHeldPut(t, r, m, data, 0)
	waitInflight(t, r, m.TransferID, 1)
	w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", bytes.NewReader(data[:MinChunkSize]), MinChunkSize)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "session_busy") {
		t.Fatal("same index accepted twice", w.Code, w.Body.String())
	}
	held := []*heldPut{first}
	for i := 1; i < MaxInflightChunks; i++ {
		held = append(held, startHeldPut(t, r, m, data, i))
	}
	waitInflight(t, r, m.TransferID, MaxInflightChunks)
	index := MaxInflightChunks
	payload := data[index*int(MinChunkSize) : min((index+1)*int(MinChunkSize), len(data))]
	w = perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/"+strconv.Itoa(index), "application/octet-stream", bytes.NewReader(payload), int64(len(payload)))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "session_busy") {
		t.Fatal("in-flight cap not enforced", w.Code, w.Body.String())
	}
	for _, h := range held {
		if w := h.finish(t); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	waitInflight(t, r, m.TransferID, 0)
}

func TestFailedStreamReleasesItsIndex(t *testing.T) {
	r, _ := testReceiver(t)
	data := chunked(1)
	m := dataManifest(data)
	admitTest(t, r, m)
	held := startHeldPut(t, r, m, data, 0)
	waitInflight(t, r, m.TransferID, 1)
	// The client goes away mid-body: the upload fails and frees the index.
	held.writer.CloseWithError(io.ErrUnexpectedEOF)
	if w := <-held.done; w.Code != 422 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitInflight(t, r, m.TransferID, 0)
	if _, e := os.Lstat(filepath.Join(r.root, "sessions", m.TransferID, "chunks", "incoming-0.tmp")); !os.IsNotExist(e) {
		t.Fatal("failed upload left its temp file", e)
	}
	w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", bytes.NewReader(data[:MinChunkSize]), MinChunkSize)
	if w.Code != 200 {
		t.Fatal("index stayed reserved", w.Code, w.Body.String())
	}
}

func TestParallelSendDeliversAndCountsProgress(t *testing.T) {
	if !spoolSupported() {
		t.Skip("sender unsupported on this platform")
	}
	data := chunked(13)
	r, url, spool, m := preparedOnly(t, data)
	var events []SendProgress
	var running, peak atomic.Int64
	// Count how many chunk bodies are on the wire at once.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "PUT" {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			defer running.Add(-1)
		}
		r.ServeHTTP(w, q)
	}))
	defer server.Close()
	_ = url
	s, e := SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 6, Progress: func(p SendProgress) error {
		events = append(events, p)
		return nil
	}})
	if e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	if peak.Load() < 2 || peak.Load() > 6 {
		t.Fatal("window not used or exceeded", peak.Load())
	}
	n := len(m.ChunkSHA256)
	if len(events) != n+2 || events[0].Stage != "resume" || events[n+1].Stage != "finalize" {
		t.Fatal("event shape", events)
	}
	seen := []int{}
	var last int64
	for i := 1; i <= n; i++ {
		p := events[i]
		if p.Stage != "chunk" || p.Sent != i || p.BytesSent <= last || p.BytesSent > p.BytesToSend {
			t.Fatal("chunk event", i, p)
		}
		last = p.BytesSent
		seen = append(seen, p.ChunkIndex)
	}
	sort.Ints(seen)
	for i, index := range seen {
		if index != i {
			t.Fatal("chunks reported", seen)
		}
	}
	if last != m.FileSize {
		t.Fatal("final bytes", last)
	}
	out, e := os.ReadFile(filepath.Join(r.root, "files", m.TransferID+".data"))
	sum := sha256.Sum256(out)
	if e != nil || hex.EncodeToString(sum[:]) != m.FileSHA256 {
		t.Fatal("destination", e)
	}
}

// A 0.4 receiver refuses any request while another holds its lock. The
// sender retries session_busy, so a parallel send still completes.
func TestParallelSendToSerialReceiver(t *testing.T) {
	if !spoolSupported() {
		t.Skip("sender unsupported on this platform")
	}
	data := chunked(9)
	r, _, spool, m := preparedOnly(t, data)
	var serial sync.Mutex
	var refused atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if !serial.TryLock() {
			refused.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			io.WriteString(w, `{"protocol_version":1,"error_code":"session_busy","retryable":true}`)
			return
		}
		defer serial.Unlock()
		time.Sleep(5 * time.Millisecond)
		r.ServeHTTP(w, q)
	}))
	defer server.Close()
	s, e := SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: MaxParallel})
	if e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	if refused.Load() == 0 {
		t.Fatal("test did not exercise busy replies")
	}
}

func TestParallelSendStopsOnFirstError(t *testing.T) {
	if !spoolSupported() {
		t.Skip("sender unsupported on this platform")
	}
	data := chunked(12)
	r, _, spool, m := preparedOnly(t, data)
	var puts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "PUT" && puts.Add(1) == 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			io.WriteString(w, `{"protocol_version":1,"error_code":"storage_unavailable","retryable":true}`)
			return
		}
		r.ServeHTTP(w, q)
	}))
	defer server.Close()
	acked := 0
	_, e := UploadOnlyWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 3, Progress: func(p SendProgress) error {
		if p.Stage == "chunk" {
			acked++
		}
		return nil
	}})
	if e == nil || !strings.Contains(e.Error(), "storage_unavailable") {
		t.Fatal("error not returned", e)
	}
	s, e := QueryStatus(context.Background(), server.URL, testToken, m.TransferID)
	if e != nil || s.VerifiedChunks < acked || s.VerifiedChunks == len(m.ChunkSHA256) {
		t.Fatal("acknowledged chunks must stay verified and the send must stop", acked, s, e)
	}
	// Resuming sends only what is missing.
	if s, e = SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 4}); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
}

func TestParallelOptionBounds(t *testing.T) {
	for _, n := range []int{-1, MaxParallel + 1} {
		if _, e := UploadOnlyWithOptions(context.Background(), "http://127.0.0.1:1", testToken, t.TempDir(), testToken, SendOptions{Parallel: n}); e != ErrConfig {
			t.Fatal(n, e)
		}
	}
}

func TestKilledParallelUploadResume(t *testing.T) {
	for _, point := range []string{"chunk write", "chunk link", "before acknowledgement"} {
		t.Run(point, func(t *testing.T) {
			data := chunked(7)
			src, spool := snapshotPaths(t, data)
			m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
			if e != nil {
				t.Fatal(e)
			}
			root := filepath.Join(t.TempDir(), "receiver")
			receiver := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=receiver", "BITSPEED_RECOVERY_ROOT="+root, "BITSPEED_RECOVERY_POINT="+point)
			url := childLine(t, receiver, "url")
			sender := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=sender", "BITSPEED_RECOVERY_URL="+url, "BITSPEED_RECOVERY_SPOOL="+spool, "BITSPEED_RECOVERY_ID="+m.TransferID, "BITSPEED_RECOVERY_PARALLEL=4")
			childLine(t, receiver, "ready")
			// Let the other workers run into the stopped receiver too.
			time.Sleep(100 * time.Millisecond)
			sender.cmd.Process.Kill()
			sender.cmd.Wait()
			receiver.cmd.Process.Kill()
			receiver.cmd.Wait()
			restarted, e := NewReceiver(ReceiverConfig{Root: root, Token: testToken})
			if e != nil {
				t.Fatal("restart", e)
			}
			server := httptest.NewServer(restarted)
			defer func() { server.Close(); restarted.Close() }()
			entries, e := os.ReadDir(filepath.Join(root, "sessions", m.TransferID, "chunks"))
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "incoming") {
					t.Fatal("startup kept a temp file", entry.Name())
				}
			}
			s, e := QueryStatus(context.Background(), server.URL, testToken, m.TransferID)
			if e != nil || s.VerifiedChunks == len(m.ChunkSHA256) || s.State != Uploading {
				t.Fatal("status after kill", s, e)
			}
			if s, e = SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 4}); e != nil || s.State != Completed {
				t.Fatal("resume", s, e)
			}
			out, e := os.ReadFile(filepath.Join(root, "files", m.TransferID+".data"))
			if e != nil || !bytes.Equal(out, data) {
				t.Fatal("destination differs", e)
			}
		})
	}
}
