package inspect

import (
	"os"
	"path/filepath"
	"testing"
)

func loadRecording(t *testing.T, path string) Recording {
	t.Helper()
	f, e := os.Open(filepath.Join("..", path))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r, e := Decode(f)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func gateOn(t *testing.T, base, cand string, g Gate) GateResult {
	t.Helper()
	c, e := Compare(loadRecording(t, base), loadRecording(t, cand))
	if e != nil {
		t.Fatal(e)
	}
	r, e := EvaluateGate(c, g)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestGateOnSavedRecordings(t *testing.T) {
	before, after := "examples/recordings/before.json", "examples/recordings/after.json"
	fb := []string{"first_byte_wait"}
	if r := gateOn(t, before, after, Gate{Metrics: fb}); r.Outcome != "pass" || r.Findings[0].Verdict != "lower_in_samples" {
		t.Fatal("an improvement must pass", r)
	}
	r := gateOn(t, after, before, Gate{Metrics: fb})
	if r.Outcome != "regressed" || r.Findings[0].Outcome != "regressed" || *r.Findings[0].AbsoluteChange < 150e6 {
		t.Fatal("a non-overlapping slowdown must regress", r)
	}
	if r = gateOn(t, after, before, Gate{Metrics: fb, MinChangeNS: 200e6}); r.Outcome != "pass" {
		t.Fatal("a slowdown below the minimum change must pass", r)
	}
	if r = gateOn(t, after, before, Gate{Metrics: fb, MinPercent: 1000}); r.Outcome != "pass" {
		t.Fatal("a slowdown below the minimum percent must pass", r)
	}
	if r = gateOn(t, after, before, Gate{Metrics: fb, MinChangeNS: 100e6, MinPercent: 100}); r.Outcome != "regressed" {
		t.Fatal("a slowdown above both thresholds must regress", r)
	}
	// Overlapping ranges are inconclusive and never fail the gate, even with a large percent change.
	if r = gateOn(t, after, before, Gate{Metrics: []string{"tcp_connect", "response_read"}}); r.Outcome != "pass" {
		t.Fatal("inconclusive intervals must pass", r)
	}
	// DNS never resolved over loopback, so the gate can't judge it.
	if r = gateOn(t, before, after, Gate{Metrics: []string{"dns_resolution"}}); r.Outcome != "undecided" {
		t.Fatal("unavailable intervals must be undecided", r)
	}
	if r = gateOn(t, before, after, Gate{Metrics: []string{"dns_resolution"}, AllowUndecided: true}); r.Outcome != "pass" {
		t.Fatal("allow-undecided must pass", r)
	}
	// A regression wins over undecided findings.
	if r = gateOn(t, after, before, Gate{Metrics: []string{"dns_resolution", "first_byte_wait"}}); r.Outcome != "regressed" {
		t.Fatal(r)
	}
}

func TestGateBlockedComparisonIsUndecided(t *testing.T) {
	// Two different public sites: comparability checks block every verdict.
	base, cand := "examples/public/results/github-ubuntu/example-com.json", "examples/public/results/github-ubuntu/bitspeed-home.json"
	r := gateOn(t, base, cand, Gate{Metrics: []string{"first_byte_wait"}})
	if r.Outcome != "undecided" || r.Findings[0].Verdict != "not_comparable" {
		t.Fatal("a blocked comparison must not pass silently", r)
	}
}

func TestGateRejectsBadInput(t *testing.T) {
	for _, list := range []string{"", "observed_payload_rate", "partial_response_read", "first_byte_wait,nope", "FIRST_BYTE_WAIT"} {
		if _, e := ParseGateMetrics(list); e == nil {
			t.Fatal("accepted", list)
		}
	}
	if m, e := ParseGateMetrics("first_byte_wait, first_byte_wait,tcp_connect"); e != nil || len(m) != 2 {
		t.Fatal(m, e)
	}
	c := Comparison{}
	for _, g := range []Gate{{}, {Metrics: []string{"tcp_connect"}, MinChangeNS: -1}, {Metrics: []string{"tcp_connect"}, MinPercent: -1}} {
		if _, e := EvaluateGate(c, g); e == nil {
			t.Fatal("accepted", g)
		}
	}
	// No hops at all: every gated metric is missing and undecided.
	if r, e := EvaluateGate(c, Gate{Metrics: []string{"tcp_connect"}}); e != nil || r.Outcome != "undecided" || r.Findings[0].Verdict != "missing" {
		t.Fatal(r, e)
	}
}
