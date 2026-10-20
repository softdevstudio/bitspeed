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
		Request: Request{SamplesRequested: 1, MaxRedirects: 10, TimeoutNS: 1000000000, ConnectTimeoutNS: 1000000000, HeaderTimeoutNS: 1000000000, BodyTimeoutNS: 1000000000, Method: "GET", URL: "https://example.org/", ConnectionPolicy: "fresh"},
		Samples: []Sample{{Index: 0, Outcome: "complete", ElapsedNS: 20,
			Hops: []Hop{{URL: "https://example.org/", Outcome: "complete", ElapsedNS: 20,
				StatusCode: &status, BodyComplete: true,
				Events: []Event{{Kind: "first_byte", OffsetNS: 0}, {Kind: "body_done", OffsetNS: 20}, {Kind: "hop_done", OffsetNS: 20}}}}}},
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
		"unknown content encoding": func(r *Recording) { value := "private-raw-value"; r.Samples[0].Hops[0].ContentEncoding = &value },
		"future schema":            func(r *Recording) { r.SchemaVersion++ },
		"missing timestamp":        func(r *Recording) { r.StartedAt = time.Time{} },
		"missing tool version":     func(r *Recording) { r.ToolVersion = "" },
		"no samples":               func(r *Recording) { r.Samples = nil },
		"method":                   func(r *Recording) { r.Request.Method = "POST" },
		"policy":                   func(r *Recording) { r.Request.ConnectionPolicy = "warm" },
		"raw secret":               func(r *Recording) { r.Request.URL += "?token=secret" },
		"sample index":             func(r *Recording) { r.Samples[0].Index = 1 },
		"sample outcome":           func(r *Recording) { r.Samples[0].Outcome = "unknown" },
		"negative time":            func(r *Recording) { r.Samples[0].ElapsedNS = -1 },
		"missing hops":             func(r *Recording) { r.Samples[0].Hops = nil },
		"hop beyond sample":        func(r *Recording) { r.Samples[0].Hops[0].ElapsedNS = 21 },
		"negative bytes":           func(r *Recording) { r.Samples[0].Hops[0].BodyBytes = -1 },
		"missing status":           func(r *Recording) { r.Samples[0].Hops[0].StatusCode = nil },
		"incomplete body":          func(r *Recording) { r.Samples[0].Hops[0].BodyComplete = false },
		"error in success":         func(r *Recording) { r.Samples[0].Hops[0].ErrorCode = "timeout" },
		"unordered":                func(r *Recording) { r.Samples[0].Hops[0].Events[0].OffsetNS = 21 },
		"unknown event":            func(r *Recording) { r.Samples[0].Hops[0].Events[0].Kind = "database_slow" },
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
	h.Events = []Event{{Kind: "hop_done", OffsetNS: h.ElapsedNS}}
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
	r.ToolVersion = strings.Repeat("x", MaxRecordingBytes)
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

func TestStrictJSONFields(t *testing.T) {
	var out bytes.Buffer
	if err := Encode(&out, example()); err != nil {
		t.Fatal(err)
	}
	raw := out.String()
	cases := []string{
		strings.Replace(raw, `"schema_version": 1`, `"schema_version": 1, "schema_version": 1`, 1),
		strings.Replace(raw, `"schema_version": 1`, `"Schema_Version": 1`, 1),
		strings.Replace(raw, `"decompression": false`, `"decompression": null`, 1),
		strings.Replace(raw, `"decompression": false`, `"location_label": ""`, 1),
		strings.Replace(raw, `"body_bytes": 0,`, ``, 1),
		strings.Replace(raw, `"offset_ns": 0`, `"offset_ns": 0, "offset_ns": 1`, 1),
		strings.Replace(raw, `"connection_reused": null,`, ``, 1),
	}
	for i, candidate := range cases {
		if candidate == raw {
			t.Fatalf("case %d did not mutate JSON", i)
		}
		if _, err := Decode(strings.NewReader(candidate)); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestImportedConfigurationBounds(t *testing.T) {
	for _, mutate := range []func(*Recording){
		func(r *Recording) { r.Request.Decompression = true },
		func(r *Recording) { r.Request.TimeoutNS = int64(2 * time.Hour) },
		func(r *Recording) { r.Request.LocationLabel = strings.Repeat("x", 257) },
		func(r *Recording) { reused := true; r.Samples[0].Hops[0].ConnectionReused = &reused },
		func(r *Recording) { r.Samples[0].Hops = append(r.Samples[0].Hops, r.Samples[0].Hops[0]) },
	} {
		r := example()
		mutate(&r)
		if r.Validate() == nil {
			t.Fatal("impossible configuration accepted")
		}
	}
}
