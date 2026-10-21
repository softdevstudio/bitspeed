package inspect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type seen struct {
	mu       sync.Mutex
	requests []string
}

func (s *seen) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(body))
	s.mu.Unlock()
}

func postServer(t *testing.T) (*httptest.Server, *seen) {
	t.Helper()
	log := &seen{}
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) { log.add(r); io.WriteString(w, "ok") })
	mux.HandleFunc("/r307", func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		http.Redirect(w, r, "/echo", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/r303", func(w http.ResponseWriter, r *http.Request) {
		log.add(r)
		http.Redirect(w, r, "/echo", http.StatusSeeOther)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, log
}

func postOptions(body string) Options {
	o := DefaultOptions()
	o.Samples = 3
	o.Method = "POST"
	o.Body = []byte(body)
	o.ContentType = "application/json"
	return o
}

func TestRecordPostKeepsBodyOutOfRecording(t *testing.T) {
	s, log := postServer(t)
	secret := `{"card":"SECRET-4242"}`
	r, e := Record(context.Background(), s.URL+"/echo", postOptions(secret))
	if e != nil || !RecordingSucceeded(r) {
		t.Fatal(r, e)
	}
	sum := sha256.Sum256([]byte(secret))
	q := r.Request
	if r.SchemaVersion != 2 || q.Method != "POST" || q.RequestBodyBytes != int64(len(secret)) || q.RequestBodySHA256 != hex.EncodeToString(sum[:]) || q.ContentType != "application/json" {
		t.Fatal("wrong request facts", q)
	}
	for _, sample := range r.Samples {
		if sample.Hops[0].Method != "POST" {
			t.Fatal("hop method", sample.Hops[0])
		}
	}
	if len(log.requests) != 3 || log.requests[0] != "POST /echo application/json "+secret {
		t.Fatal("server did not receive the body", log.requests)
	}
	var out bytes.Buffer
	if e = Encode(&out, r); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("request body leaked into the recording")
	}
	back, e := Decode(&out)
	if e != nil || back.Request != r.Request {
		t.Fatal("schema 2 round trip", e)
	}
	if _, e = Explain(back); e != nil {
		t.Fatal(e)
	}
}

func TestRecordPostRedirectMethods(t *testing.T) {
	s, log := postServer(t)
	o := postOptions("payload")
	o.Samples = 1
	r, e := Record(context.Background(), s.URL+"/r307", o)
	if e != nil || len(r.Samples[0].Hops) != 2 || r.Samples[0].Hops[1].Method != "POST" {
		t.Fatal("307 must keep POST", r, e)
	}
	if log.requests[1] != "POST /echo application/json payload" {
		t.Fatal("307 must resend the body", log.requests)
	}
	log.requests = nil
	r, e = Record(context.Background(), s.URL+"/r303", o)
	if e != nil || r.Samples[0].Hops[1].Method != "GET" {
		t.Fatal("303 must switch to GET", r, e)
	}
	if log.requests[1] != "GET /echo  " {
		t.Fatal("303 must drop the body", log.requests)
	}
}

func TestRecordWarmReusesConnections(t *testing.T) {
	s, _ := postServer(t)
	o := DefaultOptions()
	o.Samples = 4
	o.Warm = true
	r, e := Record(context.Background(), s.URL+"/echo", o)
	if e != nil || r.SchemaVersion != 2 || r.Request.ConnectionPolicy != "warm" || r.Samples[0].Hops[0].Method != "GET" {
		t.Fatal(r.Request, e)
	}
	if reused := r.Samples[0].Hops[0].ConnectionReused; reused == nil || *reused {
		t.Fatal("the first warm sample opens the connection")
	}
	for _, sample := range r.Samples[1:] {
		if reused := sample.Hops[0].ConnectionReused; reused == nil || !*reused {
			t.Fatal("later warm samples must reuse the connection", sample.Index)
		}
	}
	if _, e = Explain(r); e != nil {
		t.Fatal(e)
	}
	fresh := DefaultOptions()
	fresh.Samples = 2
	f, e := Record(context.Background(), s.URL+"/echo", fresh)
	if e != nil || f.SchemaVersion != 1 {
		t.Fatal("plain GET with fresh connections must stay schema 1", f.SchemaVersion, e)
	}
	var out bytes.Buffer
	Encode(&out, f)
	for _, key := range []string{"request_body", "content_type", "connection_policy\": \"warm"} {
		if strings.Contains(out.String(), key) {
			t.Fatal("schema 1 output gained a schema 2 field", key)
		}
	}
	for _, sample := range f.Samples {
		if sample.Hops[0].Method != "" {
			t.Fatal("schema 1 hops carry no method")
		}
	}
	c, e := Compare(f, r)
	if e != nil || !hasCheck(c, "request_setting_connection_policy_v1") {
		t.Fatal("fresh against warm must be blocked", e)
	}
}

func hasCheck(c Comparison, id string) bool {
	for _, k := range c.Checks {
		if k.RuleID == id && k.Severity == "blocking" {
			return true
		}
	}
	return false
}

func TestComparePostBodiesAndValidation(t *testing.T) {
	s, _ := postServer(t)
	a, e := Record(context.Background(), s.URL+"/echo", postOptions(`{"a":1}`))
	if e != nil {
		t.Fatal(e)
	}
	b, e := Record(context.Background(), s.URL+"/echo", postOptions(`{"a":2}`))
	if e != nil {
		t.Fatal(e)
	}
	c, _ := Compare(a, b)
	if !hasCheck(c, "request_setting_request_body_sha256_v1") {
		t.Fatal("different bodies must block")
	}
	ct := postOptions(`{"a":1}`)
	ct.ContentType = "text/plain"
	d, _ := Record(context.Background(), s.URL+"/echo", ct)
	if c, _ = Compare(a, d); !hasCheck(c, "request_setting_content_type_v1") {
		t.Fatal("different content types must block")
	}
	if c, _ = Compare(a, a); len(c.Checks) != 0 {
		t.Fatal("identical POST recordings must be comparable", c.Checks)
	}
	bad := map[string]func(*Recording){
		"plain GET as schema 2": func(r *Recording) { r.Request.Method, r.Request.ConnectionPolicy = "GET", "fresh" },
		"POST without digest":   func(r *Recording) { r.Request.RequestBodySHA256 = "" },
		"oversized body":        func(r *Recording) { r.Request.RequestBodyBytes = MaxRequestBodyBytes + 1 },
		"wrong hop method":      func(r *Recording) { r.Samples[0].Hops[0].Method = "GET" },
		"unknown method":        func(r *Recording) { r.Request.Method = "PUT" },
		"schema 1 POST":         func(r *Recording) { r.SchemaVersion = 1 },
	}
	for name, change := range bad {
		r := a
		r.Samples = append([]Sample(nil), a.Samples...)
		r.Samples[0].Hops = append([]Hop(nil), a.Samples[0].Hops...)
		change(&r)
		if r.Validate() == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, o := range []Options{{Method: "PUT"}, {Body: []byte("x")}, {ContentType: "x"}, {Method: "POST", Body: make([]byte, MaxRequestBodyBytes+1)}, {Method: "POST", ContentType: "a\r\nb"}} {
		full := DefaultOptions()
		full.Method, full.Body, full.ContentType = o.Method, o.Body, o.ContentType
		if _, e := Record(context.Background(), s.URL+"/echo", full); e == nil {
			t.Fatal("invalid options accepted", o.Method, len(o.Body), o.ContentType)
		}
	}
}

// staleServer answers the first request on each connection, keeps the
// connection open, then reads the next request and closes without answering,
// the way a server with a short keep-alive timeout looks to a client.
func staleServer(t *testing.T) string {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				if req, err := http.ReadRequest(br); err == nil {
					io.Copy(io.Discard, req.Body)
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
				}
				if req, err := http.ReadRequest(br); err == nil {
					io.Copy(io.Discard, req.Body)
				}
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/"
}

func TestWarmRecordingSurvivesStaleConnections(t *testing.T) {
	url := staleServer(t)
	o := DefaultOptions()
	o.Samples = 4
	o.Warm = true
	r, e := Record(context.Background(), url, o)
	if e != nil || !RecordingSucceeded(r) {
		t.Fatal("a retried request on a stale connection must still be recorded", e)
	}
	retried := 0
	for _, s := range r.Samples {
		h := s.Hops[0]
		if len(eventsNamed(h, "connection_acquired")) > 1 {
			retried++
			if h.WinningConnectAttempt != nil {
				t.Fatal("a retried hop cannot claim one winning connection")
			}
			if h.ConnectionReused == nil || *h.ConnectionReused {
				t.Fatal("the connection that served the retry was new")
			}
		}
	}
	if retried == 0 {
		t.Fatal("the stale server should have forced at least one retry")
	}
	report, e := Explain(r)
	if e != nil {
		t.Fatal(e)
	}
	_ = report
	// POST is not replayed by the transport, so a stale connection fails the sample honestly.
	p := postOptions("x")
	p.Warm, p.Samples = true, 3
	if r, e = Record(context.Background(), url, p); e != nil || RecordingSucceeded(r) {
		t.Fatal("a POST on a stale connection must be recorded as failed, not retried", e)
	}
}

func eventsNamed(h Hop, kind string) []Event {
	var out []Event
	for _, e := range h.Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestReviewFixesForPostAndWarm(t *testing.T) {
	s, log := postServer(t)
	other, otherLog := postServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/echo", http.StatusTemporaryRedirect)
	})
	away := httptest.NewServer(mux)
	defer away.Close()
	o := postOptions("secret-body")
	o.Samples = 1
	r, e := Record(context.Background(), away.URL+"/away", o)
	if e != nil || len(r.Samples[0].Hops) != 1 || r.Samples[0].Hops[0].ErrorCode != "cross_origin_body_redirect" || len(otherLog.requests) != 0 {
		t.Fatal("a 307 to another origin must not resend the body", r.Samples, e)
	}
	_ = log
	get := r
	get.Samples = append([]Sample(nil), r.Samples...)
	get.Samples[0].Hops = append([]Hop(nil), r.Samples[0].Hops...)
	get.Samples[0].Hops[0].Method = "GET"
	if get.Validate() == nil {
		t.Fatal("a GET hop cannot carry cross_origin_body_redirect")
	}

	for _, ct := range []string{"a\x01b", "a\x7fb", "a\nb"} {
		bad := postOptions("x")
		bad.ContentType = ct
		if _, e := Record(context.Background(), s.URL+"/echo", bad); e == nil {
			t.Fatalf("content type %q accepted", ct)
		}
	}
	empty, e := Record(context.Background(), s.URL+"/echo", postOptions(""))
	if e != nil || empty.Request.RequestBodySHA256 != emptySHA256 {
		t.Fatal(e)
	}
	forged := empty
	forged.Request.RequestBodySHA256 = strings.Repeat("a", 64)
	if forged.Validate() == nil {
		t.Fatal("a zero-byte body with another digest was accepted")
	}

	// A later hop that doesn't follow a redirect is invalid in either schema.
	two, e := Record(context.Background(), s.URL+"/r307", o)
	if e != nil {
		t.Fatal(e)
	}
	status := 200
	two.Samples[0].Hops[0].StatusCode = &status
	if two.Validate() == nil {
		t.Fatal("hop after a 200 accepted")
	}

	// Two warm runs where one server closes connections: reuse differs, so a verdict is cautioned.
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		io.WriteString(w, "ok")
	}))
	defer closing.Close()
	w := DefaultOptions()
	w.Samples, w.Warm = 4, true
	kept, e := Record(context.Background(), s.URL+"/echo", w)
	if e != nil {
		t.Fatal(e)
	}
	closed, e := Record(context.Background(), closing.URL+"/echo", w)
	if e != nil {
		t.Fatal(e)
	}
	closed.Request.URL, closed.Samples = kept.Request.URL, retarget(closed.Samples, kept.Samples[0].Hops[0].URL)
	c, _ := Compare(kept, closed)
	found := false
	for _, k := range c.Checks {
		if k.RuleID == "connection_reuse_v1" && k.Severity == "caution" {
			found = true
		}
	}
	if !found {
		t.Fatal("different reuse patterns must be cautioned", c.Checks)
	}
	if c, _ = Compare(kept, kept); len(c.Checks) != 0 {
		t.Fatal(c.Checks)
	}
}

func retarget(samples []Sample, url string) []Sample {
	out := make([]Sample, len(samples))
	for i, s := range samples {
		s.Hops = append([]Hop(nil), s.Hops...)
		for j := range s.Hops {
			s.Hops[j].URL = url
		}
		out[i] = s
	}
	return out
}
