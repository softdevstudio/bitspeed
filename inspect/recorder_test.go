package inspect

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/softdevstudio/bitspeed/internal/fixture"
)

func testOptions() Options { o := DefaultOptions(); o.Samples = 1; return o }
func checkedRecord(t *testing.T, ctx context.Context, target string, o Options) Recording {
	t.Helper()
	r, err := Record(ctx, target, o)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Encode(&out, r); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(&out); err != nil {
		t.Fatal("recorder produced invalid JSON:", err)
	}
	return r
}
func event(h Hop, kind string) *Event {
	for i := range h.Events {
		if h.Events[i].Kind == kind {
			return &h.Events[i]
		}
	}
	return nil
}

func TestRecorderFixtures(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{HeaderDelay: 20 * time.Millisecond, ChunkDelay: 5 * time.Millisecond}))
	defer s.Close()
	for _, path := range []string{"/headers", "/body", "/empty", "/redirect", "/disconnect", "/missing"} {
		t.Run(path, func(t *testing.T) {
			r := checkedRecord(t, context.Background(), s.URL+path, testOptions())
			h := r.Samples[0].Hops[0]
			if event(h, "hop_done") == nil {
				t.Fatal("no hop termination")
			}
			if h.ConnectionReused == nil || *h.ConnectionReused || h.RemoteAddress == nil || h.WinningConnectAttempt == nil {
				t.Fatal("fresh connection evidence absent")
			}
			if path == "/disconnect" {
				if h.BodyBytes != 5 || h.BodyComplete || h.ErrorCode != "body_failed" || RecordingSucceeded(r) {
					t.Fatalf("bad partial evidence: %+v", h)
				}
			} else if !RecordingSucceeded(r) {
				t.Fatalf("unexpected failure: %+v", h)
			}
			if path == "/redirect" && len(r.Samples[0].Hops) != 2 {
				t.Fatal("redirect hops merged")
			}
			if path == "/empty" && h.BodyBytes != 0 {
				t.Fatal("empty response counted bytes")
			}
			if path == "/body" && h.BodyBytes != 32 {
				t.Fatalf("wrong stream count %d", h.BodyBytes)
			}
			if path == "/missing" && *h.StatusCode != 404 {
				t.Fatal("HTTP status misclassified as transport failure")
			}
		})
	}
}

func TestRecorderTimeoutsAndCancellation(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{HeaderDelay: time.Hour, ChunkDelay: time.Hour}))
	defer s.Close()
	for _, mode := range []string{"headers", "body", "total", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			o := testOptions()
			path := "/timeout"
			ctx := context.Background()
			var cancel context.CancelFunc = func() {}
			switch mode {
			case "headers":
				o.HeaderTimeout = 25 * time.Millisecond
				path = "/headers"
			case "body":
				o.BodyTimeout = 25 * time.Millisecond
				path = "/body"
			case "total":
				o.Timeout = 25 * time.Millisecond
			case "cancel":
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			defer cancel()
			r := checkedRecord(t, ctx, s.URL+path, o)
			h := r.Samples[0].Hops[0]
			want := "timeout"
			if mode == "cancel" {
				want = "cancelled"
			}
			if h.ErrorCode != want || h.BodyComplete {
				t.Fatalf("%s: %+v", want, h)
			}
		})
	}
}

func TestRedirectLimitsAndPrivacy(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/invalid":
			w.Header().Set("Location", "https://user:secret@example.org/")
			w.WriteHeader(302)
		case "/loop":
			w.Header().Set("Location", "/loop?token=secret")
			w.WriteHeader(302)
		default:
			if r.URL.Query().Get("token") != "secret" {
				t.Error("request lost original query")
			}
			w.WriteHeader(204)
		}
	}))
	defer s.Close()
	for _, path := range []string{"/?token=secret", "/invalid", "/loop"} {
		o := testOptions()
		o.MaxRedirects = 1
		r := checkedRecord(t, context.Background(), s.URL+path, o)
		var out bytes.Buffer
		Encode(&out, r)
		if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "user:") {
			t.Fatal("secret leaked into recording")
		}
		last := r.Samples[0].Hops[len(r.Samples[0].Hops)-1]
		switch path {
		case "/invalid":
			if last.ErrorCode != "invalid_redirect" || !last.BodyComplete {
				t.Fatal("invalid redirect not preserved")
			}
		case "/loop":
			if len(r.Samples[0].Hops) != 2 || last.ErrorCode != "redirect_limit" {
				t.Fatal("redirect limit failed")
			}
		}
	}
}

func TestTLSFailure(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	defer s.Close()
	r := checkedRecord(t, context.Background(), s.URL, testOptions())
	h := r.Samples[0].Hops[0]
	if h.ErrorCode != "tls_failed" || h.StatusCode != nil || event(h, "tls_done") == nil || event(h, "tls_done").Outcome != "failed" {
		t.Fatalf("lost TLS failure: %+v", h)
	}
}

func TestConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	r := checkedRecord(t, context.Background(), "http://"+address, testOptions())
	if h := r.Samples[0].Hops[0]; h.ErrorCode != "connect_failed" || h.StatusCode != nil || h.WinningConnectAttempt != nil {
		t.Fatalf("bad connection failure: %+v", h)
	}
}

func TestRepeatedSamplesUseFreshConnections(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{}))
	defer s.Close()
	o := testOptions()
	o.Samples = 3
	o.Interval = time.Millisecond
	r := checkedRecord(t, context.Background(), s.URL+"/empty", o)
	if len(r.Samples) != 3 {
		t.Fatal("missing samples")
	}
	for _, sample := range r.Samples {
		h := sample.Hops[0]
		if h.ConnectionReused == nil || *h.ConnectionReused {
			t.Fatal("connection reused")
		}
	}
}

func TestStreamedLargeBody(t *testing.T) {
	const size = 32 << 20
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 32<<10)
		for n := 0; n < size; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer s.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r := checkedRecord(t, context.Background(), s.URL, testOptions())
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("streamed_bytes=%d total_allocated_bytes=%d", size, allocated)
	if allocated > 16<<20 {
		t.Fatal("allocations unexpectedly scale with a full response body")
	}
	if r.Samples[0].Hops[0].BodyBytes != size || !RecordingSucceeded(r) {
		t.Fatal("large stream incomplete")
	}
}

func TestInvalidOptions(t *testing.T) {
	for _, mutate := range []func(*Options){func(o *Options) { o.Samples = 0 }, func(o *Options) { o.Samples = 101 }, func(o *Options) { o.MaxRedirects = 11 }, func(o *Options) { o.Timeout = 0 }, func(o *Options) { o.BodyTimeout = -1 }, func(o *Options) { o.Interval = -1 }, func(o *Options) { o.LocationLabel = strings.Repeat("x", 257) }} {
		o := testOptions()
		mutate(&o)
		if _, err := Record(context.Background(), "https://example.org/", o); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	for _, target := range []string{"file:///tmp/a", "https://u:p@example.org/", "http://:80/"} {
		if _, err := Record(context.Background(), target, testOptions()); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
}

func TestTracePairingAndLateCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	l := &traceLog{start: time.Now(), events: []Event{}, cancel: cancel, connect: map[string][]string{}, successful: map[string][]string{}}
	tr := l.trace()
	tr.ConnectStart("tcp", "127.0.0.1:1")
	tr.ConnectStart("tcp", "127.0.0.1:2")
	tr.ConnectDone("tcp", "127.0.0.1:2", nil)
	tr.ConnectDone("tcp", "127.0.0.1:1", context.DeadlineExceeded)
	tr.TLSHandshakeStart()
	tr.TLSHandshakeDone(tls.ConnectionState{}, context.Canceled)
	var h Hop
	l.finish(&h)
	before := len(h.Events)
	tr.GotFirstResponseByte()
	if len(h.Events) != before || len(l.events) != before {
		t.Fatal("late callback mutated frozen evidence")
	}
	starts := map[string]string{}
	for _, e := range h.Events {
		if e.Kind == "connect_start" {
			starts[e.Address] = e.Attempt
		}
		if e.Kind == "connect_done" && starts[e.Address] != e.Attempt {
			t.Fatal("attempt pairing lost")
		}
	}
	for i := 0; i < MaxEvents*2; i++ {
		l.add("first_byte", "", "", "")
	}
	if ctx.Err() != nil {
		t.Fatal("frozen callbacks cancelled completed hop")
	}
}

func TestTraceDNSAndLimit(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	l := &traceLog{start: time.Now(), events: []Event{}, cancel: cancel, connect: map[string][]string{}, successful: map[string][]string{}}
	tr := l.trace()
	tr.DNSStart(httptrace.DNSStartInfo{Host: "private.example"})
	tr.DNSDone(httptrace.DNSDoneInfo{Err: &net.DNSError{Err: "raw secret", Name: "private.example"}})
	if l.events[0].Attempt != l.events[1].Attempt || l.events[1].Outcome != "failed" {
		t.Fatal("DNS evidence lost")
	}
	for i := 0; i < MaxEvents*2; i++ {
		tr.ConnectStart("tcp", "127.0.0.1:1")
	}
	var h Hop
	l.finish(&h)
	if len(h.Events) > MaxEvents || !errors.Is(context.Cause(ctx), errEventLimit) {
		t.Fatal("trace not bounded")
	}
	if code := classifyError(context.Background(), &net.DNSError{Err: "raw secret"}, false); code != "dns_failed" {
		t.Fatal(code)
	}
}

func TestUpgradeRejectedWithoutReadingStream(t *testing.T) {
	closed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, out, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(closed)
		fmt.Fprint(out, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: private\r\n\r\n")
		out.Flush()
		var b [1]byte
		conn.Read(b[:])
	}))
	defer s.Close()
	o := testOptions()
	o.Timeout = 100 * time.Millisecond
	o.BodyTimeout = 25 * time.Millisecond
	r := checkedRecord(t, context.Background(), s.URL, o)
	h := r.Samples[0].Hops[0]
	if h.ErrorCode != "unsupported_upgrade" || h.BodyComplete || *h.StatusCode != 101 {
		t.Fatalf("unexpected upgrade evidence: %+v", h)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upgraded connection not closed")
	}
}

func TestRecordedEncodingClassification(t *testing.T) {
	for _, encoding := range []string{"", "gzip", "GZip", "br", "gzip, br", "private-secret"} {
		t.Run(encoding, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				w.Write([]byte("raw"))
			}))
			defer s.Close()
			r := checkedRecord(t, context.Background(), s.URL, testOptions())
			h := r.Samples[0].Hops[0]
			want := strings.ToLower(encoding)
			if want == "" {
				want = "identity"
			}
			if want == "gzip, br" || want == "private-secret" {
				want = "other"
			}
			if h.ContentEncoding == nil || *h.ContentEncoding != want || h.BodyBytes != 3 {
				t.Fatalf("bad raw encoding evidence %+v", h)
			}
			var out bytes.Buffer
			Encode(&out, r)
			if strings.Contains(out.String(), "private-secret") {
				t.Fatal("raw encoding value leaked")
			}
		})
	}
}
