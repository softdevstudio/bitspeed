package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUploadAndAuthenticatedStatus(t *testing.T) {
	for _, size := range []int{0, 1, int(MinChunkSize) + 17} {
		t.Run(string(rune('a'+size%10)), func(t *testing.T) {
			r, server := testReceiver(t)
			src, spool := snapshotPaths(t, bytes.Repeat([]byte{3}, size))
			m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
			if e != nil {
				t.Fatal(e)
			}
			os.Remove(src)
			t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			for attempt := 0; attempt < 2; attempt++ {
				s, e := UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID)
				if e != nil || s.VerifiedBytes != int64(size) || s.Receipt != nil || s.State != Uploading || len(s.MissingChunks) != 0 {
					t.Fatal("bad staging result", s, e)
				}
			}
			s, e := QueryStatus(context.Background(), server.URL, testToken, m.TransferID)
			if e != nil || !statusMatches(s, m) {
				t.Fatal(e)
			}
			files, _ := os.ReadDir(filepath.Join(r.root, "files"))
			if len(files) != 0 {
				t.Fatal("published incomplete transfer")
			}
		})
	}
}
func TestClientRejectsRedirectAndUntrustedTLS(t *testing.T) {
	var leaked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	id := strings.Repeat("a", 64)
	if _, e := QueryStatus(context.Background(), redirect.URL, testToken, id); e == nil {
		t.Fatal("followed redirect")
	}
	if leaked.Load() != 0 {
		t.Fatal("token request forwarded")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer tlsServer.Close()
	if _, e := QueryStatus(context.Background(), tlsServer.URL, testToken, id); e == nil {
		t.Fatal("accepted untrusted TLS")
	}
	if leaked.Load() != 0 {
		t.Fatal("untrusted TLS delivered auth")
	}
	for _, base := range []string{"http://example.org", "http://localhost:80", "https://user:SECRET@example.org", "https://example.org/path", "https://example.org?token=SECRET", "https://example.org/#SECRET"} {
		if _, e := QueryStatus(context.Background(), base, testToken, id); e == nil || strings.Contains(e.Error(), "SECRET") {
			t.Fatal("unsafe endpoint", e)
		}
	}
}
func TestTokenFileRules(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	for _, body := range []string{testToken, testToken + "\n", testToken + "\r\n"} {
		os.WriteFile(file, []byte(body), 0600)
		token, e := LoadToken(file)
		if e != nil || token != testToken {
			t.Fatal(e)
		}
	}
	for _, body := range []string{testToken + "\n\n", testToken + "\r\n\n", testToken + "\n\r\n", testToken + "\r", strings.ToUpper(testToken), " " + testToken, "SECRET"} {
		os.WriteFile(file, []byte(body), 0600)
		if _, e := LoadToken(file); e == nil || strings.Contains(e.Error(), "SECRET") {
			t.Fatal("invalid token accepted or exposed", e)
		}
	}
	os.WriteFile(file, []byte(testToken), 0600)
	exposeFile(t, file)
	if _, e := LoadToken(file); e == nil {
		t.Fatal("public token accepted")
	}
	unexposeFile(t, file)
	link := filepath.Join(dir, "link")
	os.Symlink(file, link)
	if _, e := LoadToken(link); e == nil {
		t.Fatal("token symlink accepted")
	}
}
func TestClientRejectsMalformedResponses(t *testing.T) {
	m := dataManifest([]byte("x"))
	identity, _ := m.Identity()
	valid := StoredStatus{1, m.TransferID, identity, Uploading, 0, 0, []int{0}, nil, nil}
	good, _ := json.Marshal(valid)
	invalid := [][]byte{[]byte(`{}`), append(append([]byte{}, good...), []byte(` {}`)...), bytes.Replace(good, []byte(`"protocol_version":1`), []byte(`"protocol_version":1,"protocol_version":1`), 1), bytes.Replace(good, []byte(`"receipt":null`), []byte(`"receipt":{}`), 1), bytes.Replace(good, []byte(`"state":"uploading"`), []byte(`"state":"completed"`), 1), bytes.Replace(good, []byte(`"missing_chunks":[0]`), []byte(`"missing_chunks":[0,0]`), 1), bytes.Replace(good, []byte(`"verified_bytes":0`), []byte(`"verified_bytes":0.0`), 1)}
	for _, data := range invalid {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
		}))
		if _, e := QueryStatus(context.Background(), server.URL, testToken, m.TransferID); e == nil {
			t.Fatal("malformed status trusted", string(data))
		}
		server.Close()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		w.Write([]byte(`{"protocol_version":1,"error_code":"SECRET","retryable":true}`))
	}))
	defer server.Close()
	if _, e := QueryStatus(context.Background(), server.URL, testToken, m.TransferID); e == nil || strings.Contains(e.Error(), "SECRET") {
		t.Fatal("untrusted diagnostic reflected", e)
	}
}
func TestClientRefusesInventedProgressAndAck(t *testing.T) {
	for _, kind := range []string{"progress", "ack"} {
		t.Run(kind, func(t *testing.T) {
			src, spool := snapshotPaths(t, []byte("x"))
			m, e := Prepare(context.Background(), src, spool, PrepareOptions{})
			if e != nil {
				t.Fatal(e)
			}
			identity, _ := m.Identity()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "PUT" {
					json.NewEncoder(w).Encode(chunkAck{1, m.TransferID, identity, 0, m.ChunkSHA256[0], false})
					return
				}
				s := StoredStatus{1, m.TransferID, identity, Uploading, 0, 0, []int{0}, nil, nil}
				if kind == "progress" {
					s.VerifiedBytes = 1
				}
				json.NewEncoder(w).Encode(s)
			}))
			defer server.Close()
			if _, e := UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID); e == nil {
				t.Fatal("trusted invented progress or false ack")
			}
		})
	}
}
