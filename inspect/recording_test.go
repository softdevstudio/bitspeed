package inspect

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func example() Recording {
	status := 204
	return Recording{
		SchemaVersion: SchemaVersion, ToolVersion: Version, StartedAt: time.Now().UTC(),
		Request: Request{Method: "GET", URL: "https://example.org/", ConnectionPolicy: "fresh"},
		Samples: []Sample{{Index: 0, Outcome: "complete", ElapsedNS: 20,
			Hops: []Hop{{URL: "https://example.org/", Outcome: "complete", ElapsedNS: 20,
				StatusCode: &status, BodyComplete: true,
				Events: []Event{{Kind: "first_byte", OffsetNS: 0}, {Kind: "body_done", OffsetNS: 20}}}}}},
	}
}

func TestRoundTripPreservesUnavailableAndZero(t *testing.T) {
	var out bytes.Buffer
	if err := Encode(&out, example()); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(&out)
	if err != nil {
		t.Fatal(err)
	}
	h := got.Samples[0].Hops[0]
	if h.ConnectionReused != nil || h.Events[0].OffsetNS != 0 || h.BodyBytes != 0 {
		t.Fatal("missing evidence and measured zero were conflated")
	}
}

func TestInvalidRecordings(t *testing.T) {
	tests := map[string]func(*Recording){
		"future schema":        func(r *Recording) { r.SchemaVersion++ },
		"missing timestamp":    func(r *Recording) { r.StartedAt = time.Time{} },
		"missing tool version": func(r *Recording) { r.ToolVersion = "" },
		"no samples":           func(r *Recording) { r.Samples = nil },
		"method":               func(r *Recording) { r.Request.Method = "POST" },
		"policy":               func(r *Recording) { r.Request.ConnectionPolicy = "warm" },
		"raw secret":           func(r *Recording) { r.Request.URL += "?token=secret" },
		"sample index":         func(r *Recording) { r.Samples[0].Index = 1 },
		"sample outcome":       func(r *Recording) { r.Samples[0].Outcome = "unknown" },
		"negative time":        func(r *Recording) { r.Samples[0].ElapsedNS = -1 },
		"missing hops":         func(r *Recording) { r.Samples[0].Hops = nil },
		"hop beyond sample":    func(r *Recording) { r.Samples[0].Hops[0].ElapsedNS = 21 },
		"negative bytes":       func(r *Recording) { r.Samples[0].Hops[0].BodyBytes = -1 },
		"missing status":       func(r *Recording) { r.Samples[0].Hops[0].StatusCode = nil },
		"incomplete body":      func(r *Recording) { r.Samples[0].Hops[0].BodyComplete = false },
		"error in success":     func(r *Recording) { r.Samples[0].Hops[0].ErrorCode = "timeout" },
		"unordered":            func(r *Recording) { r.Samples[0].Hops[0].Events[0].OffsetNS = 21 },
		"unknown event":        func(r *Recording) { r.Samples[0].Hops[0].Events[0].Kind = "database_slow" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := example()
			mutate(&r)
			if r.Validate() == nil {
				t.Fatal("invalid evidence accepted")
			}
			if Encode(io.Discard, r) == nil {
				t.Fatal("invalid evidence encoded")
			}
		})
	}
}

func TestFailedHopWithoutStatus(t *testing.T) {
	r := example()
	r.Samples[0].Outcome = "failed"
	h := &r.Samples[0].Hops[0]
	h.Outcome, h.StatusCode, h.BodyComplete, h.ErrorCode = "failed", nil, false, "connect_failed"
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	h.ErrorCode = ""
	if r.Validate() == nil {
		t.Fatal("failure without reason accepted")
	}
}

func TestSafeURL(t *testing.T) {
	got, err := SafeURL("https://example.org/path?token=secret&x=one&x=two#secret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "one") || strings.Contains(got, "two") {
		t.Fatal("query values or fragment leaked")
	}
	if safe, err := SafeURL(got); err != nil || safe != got {
		t.Fatal("redaction is not stable")
	}
	for _, raw := range []string{"file:///tmp/a", "/relative", "https://u:p@example.org/", "https://example.org/?x=%zz", "https://:443/"} {
		if _, err := SafeURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestDecodeBoundaries(t *testing.T) {
	var out bytes.Buffer
	if err := Encode(&out, example()); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"", "null", "[]", out.String() + " {}", out.String() + " garbage",
		strings.Replace(out.String(), "\"schema_version\": 1", "\"schema_version\": 1, \"extra\": 1", 1),
		strings.Repeat(" ", MaxRecordingBytes+1),
	} {
		if _, err := Decode(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	if _, err := Decode(brokenReader{}); err == nil {
		t.Fatal("reader error ignored")
	}
	if err := Encode(brokenWriter{}, example()); err == nil {
		t.Fatal("writer error ignored")
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestEncodeRejectsOversizedRecording(t *testing.T) {
	r := example()
	r.Request.LocationLabel = strings.Repeat("x", MaxRecordingBytes)
	var out bytes.Buffer
	if Encode(&out, r) == nil || out.Len() != 0 {
		t.Fatal("oversized recording must fail without partial output")
	}
}

func TestErrorCodesCannotContainRawErrors(t *testing.T) {
	r := example()
	r.Samples[0].Outcome = "failed"
	h := &r.Samples[0].Hops[0]
	h.Outcome, h.BodyComplete = "failed", false
	h.ErrorCode = "Get https://example.org/?token=secret: timeout"
	if r.Validate() == nil {
		t.Fatal("raw network error accepted")
	}
}
