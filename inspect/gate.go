package inspect

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// GateMetrics are the comparison intervals a CI gate may check. Payload rate is
// left out because "slower" would mean a lower value, and partial reads never
// carry a change verdict.
var GateMetrics = []string{"dns_resolution", "tcp_connect", "tls_handshake", "connection_setup", "first_byte_wait", "response_read"}

// Gate describes which intervals must not get slower and how large a change
// must be before it fails. MinChangeNS and MinPercent both apply when set.
type Gate struct {
	Metrics        []string
	MinChangeNS    float64
	MinPercent     float64
	AllowUndecided bool
}

// GateFinding is the gate's reading of one metric in one hop.
type GateFinding struct {
	Hop            int      `json:"hop"`
	Metric         string   `json:"metric"`
	Verdict        string   `json:"verdict"`
	Outcome        string   `json:"outcome"`
	AbsoluteChange *float64 `json:"absolute_change"`
	PercentChange  *float64 `json:"percent_change"`
	Reason         string   `json:"reason"`
}

// GateResult is "pass", "regressed" or "undecided".
type GateResult struct {
	Outcome  string        `json:"outcome"`
	Findings []GateFinding `json:"findings"`
}

var ErrGate = errors.New("invalid gate")

// ParseGateMetrics reads a comma-separated metric list.
func ParseGateMetrics(list string) ([]string, error) {
	allowed := map[string]bool{}
	for _, m := range GateMetrics {
		allowed[m] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(list, ",") {
		name := strings.TrimSpace(part)
		if !allowed[name] {
			return nil, fmt.Errorf("%w: unknown metric %q; use one of %s", ErrGate, name, strings.Join(GateMetrics, ", "))
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

// EvaluateGate applies a gate to a finished comparison. It only reads the
// comparison's own verdicts: a regression needs the "higher_in_samples"
// verdict (candidate median higher, observed ranges not overlapping) and a
// change at least as large as the thresholds. A blocked or unavailable verdict
// on a gated metric is "undecided" because the gate cannot show the interval
// did not get slower. Inconclusive, equal and lower verdicts pass.
func EvaluateGate(c Comparison, g Gate) (GateResult, error) {
	if len(g.Metrics) == 0 || !finite(g.MinChangeNS) || !finite(g.MinPercent) || g.MinChangeNS < 0 || g.MinPercent < 0 {
		return GateResult{}, ErrGate
	}
	result := GateResult{Outcome: "pass", Findings: []GateFinding{}}
	found := map[string]bool{}
	for _, hop := range c.Hops {
		for _, m := range hop.Calculations {
			if !contains(g.Metrics, m.Name) {
				continue
			}
			found[m.Name] = true
			f := GateFinding{Hop: hop.Index, Metric: m.Name, Verdict: m.Interpretation.Status, AbsoluteChange: m.AbsoluteChange, PercentChange: m.PercentChange}
			switch m.Interpretation.Status {
			case "higher_in_samples":
				switch {
				case m.AbsoluteChange == nil:
					f.Outcome, f.Reason = "undecided", "higher verdict without a recorded change"
				case *m.AbsoluteChange < g.MinChangeNS:
					f.Outcome, f.Reason = "pass", "slower in these samples, but below the minimum change"
				case g.MinPercent > 0 && m.PercentChange == nil && !zeroBaseline(m):
					f.Outcome, f.Reason = "undecided", "no percent change was recorded to test against the minimum percent"
				case g.MinPercent > 0 && m.PercentChange != nil && *m.PercentChange < g.MinPercent:
					f.Outcome, f.Reason = "pass", "slower in these samples, but below the minimum percent"
				default:
					f.Outcome, f.Reason = "regressed", "candidate median is higher and observed ranges do not overlap"
				}
			case "inconclusive":
				// compare uses inconclusive both for overlapping ranges and for
				// verdicts it withheld. Only the first is evidence of no slowdown.
				if reason := withheld(m, c.Checks, hop.Checks); reason != "" {
					f.Outcome, f.Reason = "undecided", reason
				} else {
					f.Outcome, f.Reason = "pass", "observed ranges overlap, so these samples show no slowdown"
				}
			case "not_comparable":
				f.Outcome, f.Reason = "undecided", "comparability checks block a verdict for this interval"
			case "unavailable":
				if neverObserved(m) {
					f.Verdict = "not_applicable"
					f.Outcome, f.Reason = "pass", "this interval did not occur in any sample of either recording"
				} else {
					f.Outcome, f.Reason = "undecided", "a median is unavailable in one recording"
				}
			case "equal_medians", "lower_in_samples":
				f.Outcome, f.Reason = "pass", "no evidence in these samples that this interval got slower"
			default:
				f.Outcome, f.Reason = "undecided", "the comparison gave no change verdict for this interval"
			}
			result.Findings = append(result.Findings, f)
		}
	}
	missing := []string{}
	for _, name := range g.Metrics {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		result.Findings = append(result.Findings, GateFinding{Hop: -1, Metric: name, Verdict: "missing", Outcome: "undecided", Reason: "the comparison has no hop with this interval"})
	}
	for _, f := range result.Findings {
		if f.Outcome == "regressed" {
			result.Outcome = "regressed"
			return result, nil
		}
	}
	for _, f := range result.Findings {
		if f.Outcome == "undecided" && !g.AllowUndecided {
			result.Outcome = "undecided"
		}
	}
	return result, nil
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func zeroBaseline(m MetricComparison) bool {
	return m.Baseline.Median != nil && *m.Baseline.Median == 0
}

// neverObserved is true when every sample on both sides lacked this interval,
// for example TLS on a plain HTTP hop. Nothing can have got slower.
func neverObserved(m MetricComparison) bool {
	return m.Baseline.Count == 0 && m.Candidate.Count == 0 && m.Baseline.Unavailable > 0 && m.Candidate.Unavailable > 0 && m.Baseline.Excluded == 0 && m.Candidate.Excluded == 0
}

// withheld mirrors the conditions under which compareVerdict reports
// inconclusive without looking at the ranges: a cautionary check that applies
// to this interval, or too little coverage. It returns "" when the verdict
// rests on overlapping ranges.
func withheld(m MetricComparison, global, local []ComparabilityCheck) string {
	for _, checks := range [][]ComparabilityCheck{global, local} {
		for _, c := range checks {
			if (c.Scope == "all" || responseMetric(m.Name)) && c.Severity == "caution" {
				return "a cautionary comparability check withheld the verdict"
			}
		}
	}
	if m.Baseline.Count < 3 || m.Candidate.Count < 3 || m.Baseline.Unavailable > 0 || m.Candidate.Unavailable > 0 || m.Baseline.Excluded > 0 || m.Candidate.Excluded > 0 {
		return "too few available values for a verdict; record at least three complete samples on each side"
	}
	return ""
}
