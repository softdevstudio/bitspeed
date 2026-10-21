package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testReceiver(t *testing.T) (*Receiver, *httptest.Server) {
	t.Helper()
	r, e := NewReceiver(ReceiverConfig{Root: filepath.Join(t.TempDir(), "receiver"), Token: testToken})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(r)
	t.Cleanup(func() { server.Close(); r.Close() })
	return r, server
}
func dataManifest(data []byte) Manifest {
	m := manifest(int64(len(data)))
	m.ChunkSize = MinChunkSize
	m.ChunkSHA256 = []string{}
	sum := sha256.Sum256(data)
	m.FileSHA256 = hex.EncodeToString(sum[:])
	for i := 0; i < len(data); i += int(MinChunkSize) {
		sum = sha256.Sum256(data[i:min(i+int(MinChunkSize), len(data))])
		m.ChunkSHA256 = append(m.ChunkSHA256, hex.EncodeToString(sum[:]))
	}
	return m
}
func perform(r *Receiver, method, path, kind string, body io.Reader, length int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+testToken)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	req.ContentLength = length
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func admitTest(t *testing.T, r *Receiver, m Manifest) {
	t.Helper()
	data, _ := m.CanonicalJSON()
	w := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(data), int64(len(data)))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestWireManifestStrict(t *testing.T) {
	m := dataManifest([]byte("data"))
	good, _ := m.CanonicalJSON()
	if _, e := DecodeManifest(append([]byte(" \n"), good...)); e != nil {
		t.Fatal("whitespace rejected", e)
	}
	cases := [][]byte{[]byte(`null`), []byte(`{}`), append(append([]byte{}, good...), []byte(` {}`)...), bytes.Replace(good, []byte(`"protocol_version":1`), []byte(`"protocol_version":1,"protocol_version":1`), 1), bytes.Replace(good, []byte(`"file_size":4`), []byte(`"file_size":4e0`), 1), bytes.Replace(good, []byte(`"file_size":4`), []byte(`"File_Size":4`), 1), bytes.Replace(good, []byte(`"file_size":4`), []byte(`"file_size":null`), 1), bytes.Replace(good, []byte(`"file_size":4`), []byte(`"file_size":{},"other":4`), 1), bytes.Repeat([]byte(" "), MaxManifestBytes+1)}
	for i, data := range cases {
		if _, e := DecodeManifest(data); e == nil {
			t.Fatal("accepted malformed wire manifest", i)
		}
	}
	// Hash arrays must stop before allocating more than the protocol count.
	fields := map[string]any{}
	json.Unmarshal(good, &fields)
	fields["chunk_sha256"] = make([]string, MaxChunks+1)
	for i := range fields["chunk_sha256"].([]string) {
		fields["chunk_sha256"].([]string)[i] = strings.Repeat("a", 64)
	}
	data, _ := json.Marshal(fields)
	if _, e := DecodeManifest(data); e == nil {
		t.Fatal("accepted excessive hash count")
	}
}

type unreadableBody struct{ called bool }

func (b *unreadableBody) Read([]byte) (int, error) {
	b.called = true
	return 0, errors.New("body should not be read")
}
func TestAuthenticationBeforeBodiesAndPaths(t *testing.T) {
	r, _ := testReceiver(t)
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + strings.Repeat("f", 64)} {
		body := &unreadableBody{}
		req := httptest.NewRequest("POST", "/v1/transfers", body)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 401 || body.called || w.Body.String() != "{\"protocol_version\":1,\"error_code\":\"unauthorized\",\"retryable\":false}\n" {
			t.Fatal("authentication leaked or read body", w.Code, w.Body.String())
		}
	}
	entries, _ := os.ReadDir(filepath.Join(r.root, "sessions"))
	if len(entries) != 0 {
		t.Fatal("authfailure wrote a session")
	}
}
func TestReceiverAdmissionChunkAndDuplicate(t *testing.T) {
	r, _ := testReceiver(t)
	data := bytes.Repeat([]byte{9}, int(MinChunkSize)+17)
	m := dataManifest(data)
	admitTest(t, r, m)
	manifestData, _ := m.CanonicalJSON()
	same := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(manifestData), int64(len(manifestData)))
	if same.Code != 200 {
		t.Fatal(same.Body.String())
	}
	conflicting := m
	conflicting.FileSHA256 = strings.Repeat("c", 64)
	different, _ := conflicting.CanonicalJSON()
	w := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(different), int64(len(different)))
	if w.Code != 409 {
		t.Fatal("manifest rebound", w.Body.String())
	}
	for i := range m.ChunkSHA256 {
		chunk := data[int64(i)*m.ChunkSize : min(int64(len(data)), int64(i+1)*m.ChunkSize)]
		path := "/v1/transfers/" + m.TransferID + "/chunks/" + fmt.Sprint(i)
		for attempt := 0; attempt < 2; attempt++ {
			w = perform(r, "PUT", path, "application/octet-stream", bytes.NewReader(chunk), int64(len(chunk)))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		bad := append([]byte{}, chunk...)
		bad[0] ^= 1
		w = perform(r, "PUT", path, "application/octet-stream", bytes.NewReader(bad), int64(len(bad)))
		if w.Code != 422 {
			t.Fatal("duplicate skipped body validation", w.Body.String())
		}
	}
	w = perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	s, e := decodeStatus(w.Body.Bytes(), m.TransferID)
	if e != nil || !statusMatches(s, m) || s.VerifiedBytes != int64(len(data)) || len(s.MissingChunks) != 0 || s.Receipt != nil || s.State != Uploading {
		t.Fatal("false or incorrect progress", e, w.Body.String())
	}
	files, _ := os.ReadDir(filepath.Join(r.root, "files"))
	if len(files) != 0 {
		t.Fatal("staging published a destination")
	}
}
func TestRequestBoundsAndInvalidPaths(t *testing.T) {
	r, _ := testReceiver(t)
	m := dataManifest([]byte("data"))
	admitTest(t, r, m)
	base := "/v1/transfers/" + m.TransferID
	tests := []struct {
		method, path, kind, encoding string
		length                       int64
		want                         int
	}{
		{"GET", "/v1/transfers", "", "", 0, 405}, {"GET", base + "?SECRET=secret", "", "", 0, 400}, {"GET", base + "/", "", "", 0, 400}, {"GET", "/v1/transfers/../secret", "", "", 0, 400}, {"GET", base, "", "", 1, 400}, {"PUT", base + "/chunks/00", "application/octet-stream", "", 4, 400}, {"PUT", base + "/chunks/0", "application/octet-stream", "gzip", 4, 415}, {"PUT", base + "/chunks/0", "text/plain", "", 4, 415}, {"PUT", base + "/chunks/0", "application/octet-stream", "", -1, 411}, {"PUT", base + "/chunks/0", "application/octet-stream", "", MaxChunkSize + 1, 413}, {"PUT", base + "/chunks/0", "application/octet-stream", "", 3, 422}, {"POST", "/v1/transfers", "application/json", "", MaxManifestBytes + 1, 413},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, strings.NewReader("data"))
		req.RemoteAddr = "127.0.0.1:1"
		req.ContentLength = tt.length
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", tt.kind)
		if tt.encoding != "" {
			req.Header.Set("Content-Encoding", tt.encoding)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tt.want || strings.Contains(w.Body.String(), "SECRET") {
			t.Fatal(tt, w.Code, w.Body.String())
		}
	}
	w := perform(r, "PUT", base+"/chunks/0", "application/octet-stream", strings.NewReader("dat"), 4)
	if w.Code != 422 {
		t.Fatal("truncated body accepted")
	}
	w = perform(r, "PUT", base+"/chunks/0", "application/octet-stream", strings.NewReader("dataEXTRA"), 4)
	if w.Code != 422 {
		t.Fatal("excess bytes accepted")
	}
	r.mu.Lock()
	w = perform(r, "PUT", base+"/chunks/0", "application/octet-stream", strings.NewReader("data"), 4)
	r.mu.Unlock()
	if w.Code != 409 {
		t.Fatal("concurrent mutation not rejected")
	}
}
func TestReceiverQuotaLockAndRecoveryGate(t *testing.T) {
	r, _ := testReceiver(t)
	if _, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken}); !errors.Is(e, ErrBusy) {
		t.Fatal("root lock not exclusive", e)
	}
	r.maxSessions = 1
	m := dataManifest([]byte("data"))
	admitTest(t, r, m)
	second := m
	second.TransferID = strings.Repeat("b", 64)
	data, _ := second.CanonicalJSON()
	w := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(data), int64(len(data)))
	if w.Code != 503 {
		t.Fatal("quota bypassed")
	}
	path := filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")
	os.WriteFile(path, []byte("bad!"), 0600)
	w = perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"verified_chunks":0`) || !strings.Contains(w.Body.String(), `"missing_chunks":[0]`) {
		t.Fatal("corrupt chunk reported progress")
	}
}
func TestReceiverStorageFailureGates(t *testing.T) {
	for _, operation := range []string{"write", "short write", "corrupt write", "sync", "close", "link", "recovery sync"} {
		t.Run(operation, func(t *testing.T) {
			r, _ := testReceiver(t)
			m := dataManifest([]byte("data"))
			admitTest(t, r, m)
			switch operation {
			case "write":
				r.ops.write = func(*os.File, []byte) (int, error) { return 0, errors.New("disk SECRET") }
			case "short write":
				r.ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:1]) }
			case "corrupt write":
				r.ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(bytes.Repeat([]byte{0}, len(b))) }
			case "sync":
				real := r.ops.sync
				r.ops.sync = func(f *os.File) error {
					if filepath.Base(f.Name()) == "incoming.tmp" {
						return errors.New("sync")
					}
					return real(f)
				}
			case "close":
				real := r.ops.close
				r.ops.close = func(f *os.File) error {
					e := real(f)
					if filepath.Base(f.Name()) == "incoming.tmp" {
						return errors.New("close")
					}
					return e
				}
			case "link":
				r.ops.link = func(string, string) error { return errors.New("link") }
			case "recovery sync":
				r.ops.sync = func(*os.File) error { return errors.New("sync") }
			}
			w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", strings.NewReader("data"), 4)
			if w.Code != 503 || strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), r.root) {
				t.Fatal("unsafe acknowledgement or raw error", w.Code, w.Body.String())
			}
			if _, e := os.Lstat(filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")); !os.IsNotExist(e) {
				t.Fatal("bad chunk published", e)
			}
		})
	}
}
func TestAdmissionFailuresAndRetry(t *testing.T) {
	for _, kind := range []string{"space", "write", "short", "corrupt", "close", "sync", "rename"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := testReceiver(t)
			m := dataManifest([]byte("data"))
			switch kind {
			case "space":
				r.ops.space = func(string) (int64, error) { return 0, nil }
			case "write":
				r.ops.write = func(*os.File, []byte) (int, error) { return 0, errors.New("disk") }
			case "short":
				r.ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:1]) }
			case "corrupt":
				r.ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(bytes.Repeat([]byte{0}, len(b))) }
			case "close":
				real := r.ops.close
				r.ops.close = func(f *os.File) error { _ = real(f); return errors.New("close") }
			case "sync":
				r.ops.sync = func(*os.File) error { return errors.New("disk") }
			case "rename":
				r.ops.rename = func(string, string) error { return errors.New("disk") }
			}
			data, _ := m.CanonicalJSON()
			w := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(data), int64(len(data)))
			if w.Code != 503 {
				t.Fatal("failed admission acknowledged", w.Code, w.Body.String())
			}
			if _, e := os.Lstat(filepath.Join(r.root, "sessions", m.TransferID)); !os.IsNotExist(e) {
				t.Fatal("partial admission published")
			}
		})
	}
}
func TestConcurrentAdmission(t *testing.T) {
	r, _ := testReceiver(t)
	m := dataManifest([]byte("data"))
	data, _ := m.CanonicalJSON()
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(data), int64(len(data))).Code
		}()
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == 201 {
			created++
		} else if code != 200 && code != 409 {
			t.Fatal(code)
		}
	}
	if created != 1 {
		t.Fatal("admission not exclusive", created)
	}
}

func TestPhysicalReservationsIncludeExistingAdmissions(t *testing.T) {
	r, _ := testReceiver(t)
	r.ops.space = func(string) (int64, error) { return 80 << 20, nil }
	m := manifest(10 << 20)
	admitTest(t, r, m)
	m.TransferID = strings.Repeat("b", 64)
	data, _ := m.CanonicalJSON()
	w := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(data), int64(len(data)))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "capacity_exhausted") {
		t.Fatal("existing physical reservation ignored", w.Code, w.Body.String())
	}
}

func TestPublishedChunkRecoverySyncGate(t *testing.T) {
	r, _ := testReceiver(t)
	m := dataManifest([]byte("data"))
	admitTest(t, r, m)
	real := r.ops.sync
	count := 0
	r.ops.sync = func(f *os.File) error {
		if filepath.Base(f.Name()) == "chunks" {
			count++
			if count >= 2 {
				return errors.New("directory sync")
			}
		}
		return real(f)
	}
	w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", strings.NewReader("data"), 4)
	if w.Code != 503 {
		t.Fatal("acknowledged without directory sync", w.Code)
	}
	if _, e := os.Stat(filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")); e != nil {
		t.Fatal("published chunk missing", e)
	}
	w = perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	if w.Code != 503 {
		t.Fatal("recovery sync bypassed")
	}
	r.ops.sync = real
	w = perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	if w.Code != 200 {
		t.Fatal("not recoverable", w.Body.String())
	}
}

type zeroStream struct{}

func (zeroStream) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestReceiverStreamingAllocation(t *testing.T) {
	r, _ := testReceiver(t)
	size := int64(16 << 20)
	hash := sha256.New()
	io.CopyBuffer(hash, io.LimitReader(zeroStream{}, size), make([]byte, 32<<10))
	digest := hex.EncodeToString(hash.Sum(nil))
	m := Manifest{1, strings.Repeat("a", 64), size, size, []string{digest}, digest}
	admitTest(t, r, m)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	w := perform(r, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", io.LimitReader(zeroStream{}, size), size)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("streamed_chunk_bytes=%d total_allocated_bytes=%d", size, allocated)
	if w.Code != 200 || allocated > 8<<20 {
		t.Fatal("chunk was not bounded streaming", w.Code, allocated, w.Body.String())
	}
}
