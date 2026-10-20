package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/softdevstudio/bitspeed/internal/fixture"
)

func timedRecording() Recording {
	r := example()
	status := 200
	r.Samples[0].Hops[0].StatusCode = &status
	h := &r.Samples[0].Hops[0]
	h.ElapsedNS = 1100
	r.Samples[0].ElapsedNS = 1100
	h.BodyBytes = 60
	remote := "127.0.0.1:80"
	winner := "connect-1"
	reused := false
	h.RemoteAddress = &remote
	h.WinningConnectAttempt = &winner
	h.ConnectionReused = &reused
	h.Events = []Event{
		{Kind: "connect_start", OffsetNS: 0, Attempt: winner, Address: remote},
		{Kind: "connect_done", OffsetNS: 50, Attempt: winner, Address: remote, Outcome: "success"},
		{Kind: "connection_acquired", OffsetNS: 100, Address: remote, Outcome: "success"},
		{Kind: "request_written", OffsetNS: 200, Outcome: "success"},
		{Kind: "first_byte", OffsetNS: 800},
		{Kind: "body_done", OffsetNS: 1100},
		{Kind: "hop_done", OffsetNS: 1100},
	}
	return r
}
func metric(t *testing.T, h HopReport, name string) Calculation {
	t.Helper()
	for _, c := range h.Calculations {
		if c.Name == name {
			return c
		}
	}
	t.Fatal("missing calculation", name)
	return Calculation{}
}
func explained(t *testing.T, r Recording) Report {
	t.Helper()
	report, err := Explain(r)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
func valueEquals(t *testing.T, h HopReport, name string, want float64) {
	t.Helper()
	c := metric(t, h, name)
	if c.Value == nil || *c.Value != want {
		t.Fatalf("%s got %+v want %v", name, c, want)
	}
}

func TestExplainExactIntervalsAndEvidence(t *testing.T) {
	r := timedRecording()
	before, _ := json.Marshal(r)
	report := explained(t, r)
	h := report.Samples[0].Hops[0]
	valueEquals(t, h, "tcp_connect", 50)
	valueEquals(t, h, "connection_setup", 100)
	valueEquals(t, h, "first_byte_wait", 600)
	valueEquals(t, h, "response_read", 300)
	valueEquals(t, h, "observed_payload_rate", 2e8)
	for _, name := range []string{"dns_resolution", "tls_handshake"} {
		if metric(t, h, name).Value != nil {
			t.Fatal("invented missing phase")
		}
	}
	if len(h.Interpretations) != 1 || !strings.HasPrefix(h.Interpretations[0].Text, "First-byte waiting") {
		t.Fatalf("wrong interpretation %+v", h.Interpretations)
	}
	after, _ := json.Marshal(r)
	if !bytes.Equal(before, after) {
		t.Fatal("source recording mutated")
	}
	again := explained(t, r)
	if !reflect.DeepEqual(report, again) {
		t.Fatal("report is not deterministic")
	}
	verifyEvidence(t, r, report)
}

func TestExplainMissingAndAmbiguousEvidence(t *testing.T) {
	for _, mode := range []string{"missing_write", "failed_write", "reversed_write", "duplicate_write", "missing_winner", "duplicate_first_byte", "zero_read", "partial", "tie"} {
		t.Run(mode, func(t *testing.T) {
			r := timedRecording()
			h := &r.Samples[0].Hops[0]
			wantUnavailable := "first_byte_wait"
			switch mode {
			case "missing_write":
				h.Events = append(h.Events[:3], h.Events[4:]...)
			case "failed_write":
				h.Events[3].Outcome = "failed"
			case "reversed_write":
				h.Events[3], h.Events[4] = h.Events[4], h.Events[3]
				h.Events[3].OffsetNS = 200
				h.Events[4].OffsetNS = 800
			case "duplicate_write":
				h.Events = append(h.Events[:4], append([]Event{h.Events[3]}, h.Events[4:]...)...)
			case "missing_winner":
				h.WinningConnectAttempt = nil
				wantUnavailable = "tcp_connect"
			case "duplicate_first_byte":
				h.Events = append(h.Events[:5], append([]Event{h.Events[4]}, h.Events[5:]...)...)
				wantUnavailable = "response_read"
			case "zero_read":
				h.Events[4].OffsetNS = 1100
				wantUnavailable = "observed_payload_rate"
			case "partial":
				h.Outcome = "failed"
				r.Samples[0].Outcome = "failed"
				h.ErrorCode = "body_failed"
				h.BodyComplete = false
				h.Events = append(h.Events[:5], h.Events[6:]...)
				wantUnavailable = "observed_payload_rate"
			case "tie":
				h.Events[3].OffsetNS = 500
				h.Events[4].OffsetNS = 800
				wantUnavailable = ""
			}
			report := explained(t, r)
			hr := report.Samples[0].Hops[0]
			if wantUnavailable != "" {
				c := metric(t, hr, wantUnavailable)
				if c.Value != nil || c.UnavailableReason == "" {
					t.Fatalf("unsafe calculation %+v", c)
				}
			}
			for _, inference := range hr.Interpretations {
				if inference.RuleID == "largest_interval_v1" && mode != "zero_read" {
					t.Fatal("selected largest interval from insufficient, overlapping, partial or tied evidence")
				}
			}
			verifyEvidence(t, r, report)
		})
	}
}

func TestExplainSuccessfulAndFailedOperations(t *testing.T) {
	r := timedRecording()
	h := &r.Samples[0].Hops[0]
	dns := []Event{{Kind: "dns_start", OffsetNS: 0, Attempt: "dns-1"}, {Kind: "dns_done", OffsetNS: 0, Attempt: "dns-1", Outcome: "success"}}
	tls := []Event{{Kind: "tls_start", OffsetNS: 60, Attempt: "tls-1"}, {Kind: "tls_done", OffsetNS: 90, Attempt: "tls-1", Outcome: "success"}}
	h.Events = append(dns, h.Events...)
	h.Events = append(h.Events[:4], append(tls, h.Events[4:]...)...)
	hr := explained(t, r).Samples[0].Hops[0]
	valueEquals(t, hr, "dns_resolution", 0)
	valueEquals(t, hr, "tls_handshake", 30)
	h.Events[5].Outcome = "failed"
	hr = explained(t, r).Samples[0].Hops[0]
	if metric(t, hr, "tls_handshake").Value != nil {
		t.Fatal("failed TLS counted as successful phase")
	}
}

func TestExplainControlledDelays(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{HeaderDelay: 80 * time.Millisecond, ChunkDelay: 30 * time.Millisecond}))
	defer s.Close()
	for _, path := range []string{"/headers", "/body"} {
		r := checkedRecord(t, context.Background(), s.URL+path, testOptions())
		hr := explained(t, r).Samples[0].Hops[0]
		waiting, response := metric(t, hr, "first_byte_wait"), metric(t, hr, "response_read")
		if waiting.Value == nil || response.Value == nil {
			t.Fatal("fixture timing evidence unavailable")
		}
		if path == "/headers" && *waiting.Value < float64(60*time.Millisecond) {
			t.Fatal("header delay attributed elsewhere")
		}
		if path == "/body" && *response.Value < float64(90*time.Millisecond) {
			t.Fatal("streamed body delay attributed elsewhere")
		}
		t.Logf("fixture=%s first_byte_wait_ns=%.0f response_read_ns=%.0f", path, *waiting.Value, *response.Value)
	}
}

func TestExplainFailuresRedactionAndIncompleteBatch(t *testing.T) {
	r := timedRecording()
	r.Request.URL = "https://example.org/?x=%5Bredacted%5D"
	r.Request.SamplesRequested = 2
	h := &r.Samples[0].Hops[0]
	h.Outcome = "failed"
	r.Samples[0].Outcome = "failed"
	h.ErrorCode = "redirect_limit"
	report := explained(t, r)
	if len(report.Interpretations) != 2 {
		t.Fatal("missing batch/redaction interpretation")
	}
	if len(report.Samples[0].Hops[0].Interpretations) == 0 {
		t.Fatal("policy failure unexplained")
	}
	verifyEvidence(t, r, report)
	for _, code := range []string{"dns_failed", "connect_failed", "tls_failed", "request_write_failed", "response_failed", "body_failed", "timeout", "cancelled", "redirect_limit", "invalid_redirect", "event_limit", "unsupported_upgrade"} {
		if failureExplanation(code) == "" {
			t.Fatal("empty failure explanation")
		}
	}
}

func TestExplainRejectsInvalidInputAndRendersSafely(t *testing.T) {
	r := timedRecording()
	r.SchemaVersion++
	if _, err := Explain(r); err == nil {
		t.Fatal("invalid recording explained")
	}
	r = timedRecording()
	r.Request.LocationLabel = "\x1b[2J\nspoof\u202e"
	report := explained(t, r)
	var text, jsonOut bytes.Buffer
	if err := WriteReportText(&text, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.String(), "\x1b") || strings.Contains(text.String(), "\u202e") {
		t.Fatal("terminal control characters rendered")
	}
	for _, label := range []string{"Measured", "Calculated", "Interpretation"} {
		if !strings.Contains(text.String(), label) {
			t.Fatal("evidence types not labelled")
		}
	}
	if err := WriteReportJSON(&jsonOut, report); err != nil || !json.Valid(jsonOut.Bytes()) {
		t.Fatalf("invalid JSON report: %v", err)
	}
	if WriteReportText(brokenWriter{}, report) == nil || WriteReportJSON(brokenWriter{}, report) == nil {
		t.Fatal("output errors ignored")
	}
	if !errors.Is(WriteReportText(shortWriter{}, report), io.ErrShortWrite) || !errors.Is(WriteReportJSON(shortWriter{}, report), io.ErrShortWrite) {
		t.Fatal("short write ignored")
	}
	if writeReportBytes(io.Discard, make([]byte, MaxReportBytes+1)) == nil {
		t.Fatal("oversized report accepted")
	}
}

type shortWriter struct{}

func (shortWriter) Write(b []byte) (int, error) { return len(b) / 2, nil }

// Check every generated pointer, including references from interpretations to calculations.
func verifyEvidence(t *testing.T, r Recording, report Report) {
	t.Helper()
	rb, _ := json.Marshal(r)
	pb, _ := json.Marshal(report)
	var recordingDoc, reportDoc any
	json.Unmarshal(rb, &recordingDoc)
	json.Unmarshal(pb, &reportDoc)
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case []any:
			for _, x := range n {
				walk(x)
			}
		case map[string]any:
			if document, ok := n["document"].(string); ok {
				pointer, ok := n["pointer"].(string)
				if !ok {
					t.Fatal("missing evidence pointer")
				}
				target := recordingDoc
				if document == "report" {
					target = reportDoc
				} else if document != "recording" {
					t.Fatal("unknown evidence document")
				}
				for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
					switch value := target.(type) {
					case map[string]any:
						var exists bool
						target, exists = value[part]
						if !exists {
							t.Fatal("missing evidence", pointer)
						}
					case []any:
						i, err := strconv.Atoi(part)
						if err != nil || i < 0 || i >= len(value) {
							t.Fatal("invalid evidence index", pointer)
						}
						target = value[i]
					default:
						t.Fatal("evidence pointer passed through scalar", pointer)
					}
				}
			}
			for _, x := range n {
				walk(x)
			}
		}
	}
	walk(reportDoc)
	if report.SourceSHA256 == "" {
		t.Fatal(fmt.Errorf("missing source hash"))
	}
}

func TestExplainUsesWinningDial(t *testing.T) {
	r := timedRecording()
	h := &r.Samples[0].Hops[0]
	loserStart := Event{Kind: "connect_start", Attempt: "loser", OffsetNS: 0, Address: "127.0.0.1:81"}
	loserDone := Event{Kind: "connect_done", Attempt: "loser", OffsetNS: 250, Address: "127.0.0.1:81", Outcome: "failed"}
	h.Events = append([]Event{loserStart}, h.Events...)
	h.Events = append(h.Events[:5], append([]Event{loserDone}, h.Events[5:]...)...)
	hr := explained(t, r).Samples[0].Hops[0]
	valueEquals(t, hr, "tcp_connect", 50)
	for _, ref := range metric(t, hr, "tcp_connect").Evidence {
		if ref.Pointer == "/samples/0/hops/0/events/5" {
			t.Fatal("losing dial used for TCP duration")
		}
	}
}

func TestTLSLifecycleEvidenceDependencies(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		r := timedRecording()
		h := &r.Samples[0].Hops[0]
		starts := Event{Kind: "tls_start", OffsetNS: 60, Attempt: "tls-1"}
		ends := Event{Kind: "tls_done", OffsetNS: 90, Attempt: "tls-1", Outcome: "success"}
		h.Events = append(h.Events[:2], append([]Event{starts, ends}, h.Events[2:]...)...)
		if overlap {
			h.Events[3], h.Events[4] = h.Events[4], h.Events[3]
			h.Events[4].OffsetNS = 110
		}
		report := explained(t, r)
		hr := report.Samples[0].Hops[0]
		tlsMetric, setup := metric(t, hr, "tls_handshake"), metric(t, hr, "connection_setup")
		if (tlsMetric.Value == nil) != overlap || (setup.Value == nil) != overlap {
			t.Fatal("TLS lifecycle decision incorrect")
		}
		for _, c := range []Calculation{tlsMetric, setup} {
			required := []string{"/samples/0/hops/0/remote_address", "/samples/0/hops/0/winning_connect_attempt", "/samples/0/hops/0/events/0", "/samples/0/hops/0/events/1", "/samples/0/hops/0/events/2", "/samples/0/hops/0/events/3", "/samples/0/hops/0/events/4"}
			for _, pointer := range required {
				found := false
				for _, ref := range c.Evidence {
					if ref.Document == "recording" && ref.Pointer == pointer {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s lacks dependency %s", c.Name, pointer)
				}
			}
		}
		verifyEvidence(t, r, report)
	}
}

func TestAmbiguousBodyCompletionReason(t *testing.T) {
	r := timedRecording()
	h := &r.Samples[0].Hops[0]
	h.Events = append(h.Events[:6], append([]Event{h.Events[5]}, h.Events[6:]...)...)
	c := metric(t, explained(t, r).Samples[0].Hops[0], "response_read")
	if c.Value != nil || !strings.Contains(c.UnavailableReason, "body-completion") {
		t.Fatalf("misleading unavailable reason: %+v", c)
	}
}
