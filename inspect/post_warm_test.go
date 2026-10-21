package inspect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
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
