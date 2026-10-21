package inspect

import (
	"math"
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
	// DNS never happens for a loopback IP on either side: nothing to judge, so it passes.
	if r = gateOn(t, before, after, Gate{Metrics: []string{"dns_resolution"}}); r.Outcome != "pass" || r.Findings[0].Verdict != "not_applicable" {
		t.Fatal("an interval absent on both sides must pass as not applicable", r)
	}
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
	if r = gateOn(t, base, cand, Gate{Metrics: []string{"first_byte_wait"}, AllowUndecided: true}); r.Outcome != "pass" {
		t.Fatal("allow-undecided must pass", r)
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

func f64(v float64) *float64 { return &v }

func gateMetric(name, status string, base, cand Statistics, abs, pct *float64) MetricComparison {
	return MetricComparison{Name: name, Unit: "ns", Baseline: base, Candidate: cand, AbsoluteChange: abs, PercentChange: pct, Interpretation: ComparisonVerdict{Status: status}}
}

func stats(count, unavailable int, median float64) Statistics {
	s := Statistics{Count: count, Unavailable: unavailable}
	if count > 0 {
		s.Median = f64(median)
	}
	return s
}

func oneMetric(checks []ComparabilityCheck, m MetricComparison) Comparison {
	return Comparison{Checks: checks, Hops: []HopComparison{{Index: 0, Checks: []ComparabilityCheck{}, Calculations: []MetricComparison{m}}}}
}

func TestGateWithheldInconclusiveIsUndecided(t *testing.T) {
	g := Gate{Metrics: []string{"first_byte_wait"}}
	full := stats(10, 0, 20e6)
	cases := []struct {
		name    string
		c       Comparison
		outcome string
	}{
		{"overlap", oneMetric(nil, gateMetric("first_byte_wait", "inconclusive", full, stats(10, 0, 30e6), f64(10e6), f64(50))), "pass"},
		{"too few", oneMetric(nil, gateMetric("first_byte_wait", "inconclusive", full, stats(2, 0, 200e6), f64(180e6), f64(900))), "undecided"},
		{"some unavailable", oneMetric(nil, gateMetric("first_byte_wait", "inconclusive", full, stats(8, 2, 200e6), f64(180e6), f64(900))), "undecided"},
		{"caution all", oneMetric([]ComparabilityCheck{{Severity: "caution", Scope: "all"}}, gateMetric("first_byte_wait", "inconclusive", full, stats(10, 0, 200e6), f64(180e6), f64(900))), "undecided"},
		{"caution response only", oneMetric([]ComparabilityCheck{{Severity: "caution", Scope: "response"}}, gateMetric("tcp_connect", "inconclusive", full, stats(10, 0, 30e6), f64(10e6), f64(50))), "pass"},
	}
	for _, tc := range cases {
		gate := g
		if tc.name == "caution response only" {
			gate.Metrics = []string{"tcp_connect"}
		}
		r, e := EvaluateGate(tc.c, gate)
		if e != nil || r.Outcome != tc.outcome {
			t.Fatal(tc.name, r, e)
		}
	}
}

func TestGateZeroBaselineNeverObservedAndBadThresholds(t *testing.T) {
	zero := gateMetric("first_byte_wait", "higher_in_samples", stats(10, 0, 0), stats(10, 0, 5e6), f64(5e6), nil)
	if r, _ := EvaluateGate(oneMetric(nil, zero), Gate{Metrics: []string{"first_byte_wait"}, MinPercent: 10}); r.Outcome != "regressed" {
		t.Fatal("an increase from zero exceeds any percent threshold", r)
	}
	noPct := gateMetric("first_byte_wait", "higher_in_samples", stats(10, 0, 3e6), stats(10, 0, 5e6), f64(2e6), nil)
	if r, _ := EvaluateGate(oneMetric(nil, noPct), Gate{Metrics: []string{"first_byte_wait"}, MinPercent: 10}); r.Outcome != "undecided" {
		t.Fatal(r)
	}
	never := gateMetric("tls_handshake", "unavailable", stats(0, 10, 0), stats(0, 10, 0), nil, nil)
	if r, _ := EvaluateGate(oneMetric(nil, never), Gate{Metrics: []string{"tls_handshake"}}); r.Outcome != "pass" || r.Findings[0].Verdict != "not_applicable" {
		t.Fatal("an interval absent from every sample can't have got slower", r)
	}
	for _, severity := range []string{"blocking", "caution"} {
		blocked := oneMetric([]ComparabilityCheck{{Severity: severity, Scope: "all"}}, never)
		if r, _ := EvaluateGate(blocked, Gate{Metrics: []string{"tls_handshake"}}); r.Outcome != "undecided" {
			t.Fatal("an absent interval in a "+severity+" comparison must stay undecided", r)
		}
	}
	oneSided := gateMetric("tls_handshake", "unavailable", stats(0, 10, 0), stats(10, 0, 5e6), nil, nil)
	if r, _ := EvaluateGate(oneMetric(nil, oneSided), Gate{Metrics: []string{"tls_handshake"}}); r.Outcome != "undecided" {
		t.Fatal("one-sided unavailability must stay undecided", r)
	}
	for _, g := range []Gate{{Metrics: []string{"tcp_connect"}, MinPercent: math.NaN()}, {Metrics: []string{"tcp_connect"}, MinPercent: math.Inf(1)}, {Metrics: []string{"tcp_connect"}, MinChangeNS: math.Inf(1)}} {
		if _, e := EvaluateGate(Comparison{}, g); e == nil {
			t.Fatal("non-finite threshold accepted")
		}
	}
}

func TestGateRedirectHops(t *testing.T) {
	// Hop 0 is plain HTTP, so TLS never happens there; hop 1 is HTTPS.
	rec := "examples/public/results/github-ubuntu/github-redirect.json"
	r := gateOn(t, rec, rec, Gate{Metrics: []string{"tls_handshake"}})
	if r.Outcome != "pass" || len(r.Findings) != 2 || r.Findings[0].Hop != 0 || r.Findings[0].Verdict != "not_applicable" || r.Findings[1].Hop != 1 {
		t.Fatal(r)
	}
}
