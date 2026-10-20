package inspect

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func cohort(waits ...int64) Recording {
	r := timedRecording()
	r.Samples = nil
	r.Request.SamplesRequested = len(waits)
	for i, waiting := range waits {
		h := timedRecording().Samples[0].Hops[0]
		protocol, encoding := "HTTP/1.1", "identity"
		h.Protocol = &protocol
		h.ContentEncoding = &encoding
		h.Events[4].OffsetNS = 200 + waiting
		h.Events[5].OffsetNS = 500 + waiting
		h.Events[6].OffsetNS = 500 + waiting
		h.ElapsedNS = 500 + waiting
		r.Samples = append(r.Samples, Sample{Index: i, Outcome: "complete", ElapsedNS: h.ElapsedNS, Hops: []Hop{h}})
	}
	return r
}
func compared(t *testing.T, b, a Recording) Comparison {
	t.Helper()
	c, err := Compare(b, a)
	if err != nil {
		t.Fatal(err)
	}
	verifyComparisonEvidence(t, b, a, c)
	return c
}
func comparedMetric(t *testing.T, c Comparison, name string) MetricComparison {
	t.Helper()
	for _, m := range c.Hops[0].Calculations {
		if m.Name == name {
			return m
		}
	}
	t.Fatal("missing metric", name)
	return MetricComparison{}
}

func TestCompareExactStatistics(t *testing.T) {
	b, a := cohort(900, 1000, 1100, 1200), cohort(400, 500, 600, 700)
	beforeB, _ := json.Marshal(b)
	beforeA, _ := json.Marshal(a)
	c := compared(t, b, a)
	m := comparedMetric(t, c, "first_byte_wait")
	if m.Baseline.Count != 4 || *m.Baseline.Median != 1050 || *m.Baseline.Minimum != 900 || *m.Baseline.Maximum != 1200 || *m.Candidate.Median != 550 || *m.AbsoluteChange != -500 {
		t.Fatalf("incorrect stats: %+v", m)
	}
	if m.PercentChange == nil || math.Abs(*m.PercentChange-(-500.0/1050*100)) > 1e-12 || m.Interpretation.Status != "lower_in_samples" {
		t.Fatalf("incorrect change: %+v", m)
	}
	if comparedMetric(t, c, "connection_setup").Interpretation.Status != "equal_medians" {
		t.Fatal("equal observed medians misclassified")
	}
	again := compared(t, b, a)
	if !reflect.DeepEqual(c, again) {
		t.Fatal("comparison not deterministic")
	}
	afterB, _ := json.Marshal(b)
	afterA, _ := json.Marshal(a)
	if !bytes.Equal(beforeB, afterB) || !bytes.Equal(beforeA, afterA) {
		t.Fatal("source recordings mutated")
	}
}

func TestCompareScopesAndBlocking(t *testing.T) {
	for _, mode := range []string{"payload", "encoding", "unknown_encoding", "other_encoding", "query", "status", "protocol", "target", "chain", "settings", "tool", "location", "mixed_payload"} {
		t.Run(mode, func(t *testing.T) {
			b, a := cohort(900, 1000, 1100), cohort(400, 500, 600)
			connectionBlocked := false
			responseBlocked := true
			switch mode {
			case "payload":
				for i := range a.Samples {
					a.Samples[i].Hops[0].BodyBytes++
				}
			case "mixed_payload":
				a.Samples[0].Hops[0].BodyBytes++
			case "encoding":
				for i := range a.Samples {
					encoding := "gzip"
					a.Samples[i].Hops[0].ContentEncoding = &encoding
				}
			case "unknown_encoding":
				for i := range a.Samples {
					a.Samples[i].Hops[0].ContentEncoding = nil
				}
			case "other_encoding":
				for i := range a.Samples {
					encoding := "other"
					a.Samples[i].Hops[0].ContentEncoding = &encoding
				}
			case "query":
				for _, r := range []*Recording{&b, &a} {
					r.Request.URL += "?x=%5Bredacted%5D"
					for i := range r.Samples {
						r.Samples[i].Hops[0].URL = r.Request.URL
					}
				}
			case "status":
				for i := range a.Samples {
					code := 500
					a.Samples[i].Hops[0].StatusCode = &code
				}
			case "protocol":
				connectionBlocked = true
				for i := range a.Samples {
					protocol := "HTTP/2.0"
					a.Samples[i].Hops[0].Protocol = &protocol
				}
			case "target":
				connectionBlocked = true
				a.Request.URL = "https://different.example/"
			case "chain":
				connectionBlocked = true
				a.Samples[0].Hops[0].URL = "https://different.example/"
			case "settings":
				connectionBlocked = true
				a.Request.TimeoutNS = int64(2 * time.Second)
			case "tool":
				connectionBlocked = true
				a.ToolVersion = "different"
			case "location":
				responseBlocked = false
				a.Request.LocationLabel = "region-b"
			}
			c := compared(t, b, a)
			response := comparedMetric(t, c, "first_byte_wait")
			connection := comparedMetric(t, c, "connection_setup")
			if response.Baseline.Median == nil || response.Candidate.Median == nil {
				t.Fatal("blocking erased descriptive values")
			}
			blocked := func(status string) bool { return status == "not_comparable" || status == "inconclusive" }
			if blocked(response.Interpretation.Status) != responseBlocked {
				t.Fatalf("wrong response scope: %+v", response.Interpretation)
			}
			if blocked(connection.Interpretation.Status) != connectionBlocked {
				t.Fatalf("wrong connection scope: %+v", connection.Interpretation)
			}
		})
	}
}

func TestCompareFailuresCoverageZeroAndOverlap(t *testing.T) {
	for _, mode := range []string{"failure", "unattempted", "missing_phase", "small", "zero", "overlap", "higher"} {
		t.Run(mode, func(t *testing.T) {
			b, a := cohort(900, 1000, 1100), cohort(400, 500, 600)
			want := "inconclusive"
			switch mode {
			case "failure":
				h := &a.Samples[0].Hops[0]
				h.Outcome = "failed"
				h.ErrorCode = "body_failed"
				h.BodyComplete = false
				h.Events = append(h.Events[:5], h.Events[6:]...)
				a.Samples[0].Outcome = "failed"
			case "unattempted":
				a.Request.SamplesRequested++
			case "missing_phase":
				h := &a.Samples[0].Hops[0]
				h.Events = append(h.Events[:3], h.Events[4:]...)
			case "small":
				b = cohort(900)
				a = cohort(400)
			case "zero":
				b = cohort(0, 0, 0)
				a = cohort(0, 0, 0)
				want = "equal_medians"
			case "overlap":
				b = cohort(300, 1000, 1100)
				a = cohort(400, 500, 1200)
			case "higher":
				b, a = a, b
				want = "higher_in_samples"
			}
			c := compared(t, b, a)
			m := comparedMetric(t, c, "first_byte_wait")
			if mode == "failure" && m.Interpretation.Status == "not_comparable" {
				want = "not_comparable"
			}
			if m.Interpretation.Status != want {
				t.Fatalf("got %s want %s", m.Interpretation.Status, want)
			}
			if mode == "zero" && (m.PercentChange != nil || m.AbsoluteChange == nil || *m.AbsoluteChange != 0) {
				t.Fatal("zero baseline divided")
			}
			if mode == "failure" {
				if c.Candidate.Failed != 1 || comparedMetric(t, c, "response_read").Candidate.Count != 2 {
					t.Fatal("partial response included as complete interval")
				}
			}
		})
	}
}

func TestComparisonOutputAndInvalidInput(t *testing.T) {
	b, a := cohort(900, 1000, 1100), cohort(400, 500, 600)
	a.Request.LocationLabel = "\x1b[2J\u202e"
	c := compared(t, b, a)
	var text, jsonOut bytes.Buffer
	if err := WriteComparisonText(&text, c); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(text.String(), "\x1b\u202e") {
		t.Fatal("terminal controls emitted")
	}
	if err := WriteComparisonJSON(&jsonOut, c); err != nil || !json.Valid(jsonOut.Bytes()) {
		t.Fatal("invalid comparison JSON")
	}
	if WriteComparisonText(brokenWriter{}, c) == nil || WriteComparisonJSON(brokenWriter{}, c) == nil {
		t.Fatal("output error ignored")
	}
	if WriteComparisonText(shortWriter{}, c) != io.ErrShortWrite || WriteComparisonJSON(shortWriter{}, c) != io.ErrShortWrite {
		t.Fatal("short write ignored")
	}
	b.SchemaVersion++
	if _, err := Compare(b, a); err == nil {
		t.Fatal("invalid input accepted")
	}
}

func TestNormalizeComparisonTarget(t *testing.T) {
	if normalizedTarget("https://EXAMPLE.org:443") != normalizedTarget("https://example.org/") {
		t.Fatal("equivalent target normalization failed")
	}
	if normalizedTarget("https://example.org/%61") == normalizedTarget("https://example.org/a") {
		t.Fatal("server-sensitive path escapes normalized")
	}
}

func verifyComparisonEvidence(t *testing.T, b, a Recording, c Comparison) {
	t.Helper()
	be := explained(t, b)
	ae := explained(t, a)
	docs := map[string]any{}
	for name, value := range map[string]any{"baseline_recording": b, "candidate_recording": a, "baseline_explanation": be, "candidate_explanation": ae, "comparison": c} {
		data, _ := json.Marshal(value)
		var node any
		json.Unmarshal(data, &node)
		docs[name] = node
	}
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case []any:
			for _, v := range n {
				walk(v)
			}
		case map[string]any:
			if doc, ok := n["document"].(string); ok {
				current, ok := docs[doc]
				if !ok {
					t.Fatal("unknown evidence document", doc)
				}
				pointer := n["pointer"].(string)
				for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
					switch v := current.(type) {
					case map[string]any:
						var exists bool
						current, exists = v[part]
						if !exists {
							t.Fatal("missing evidence", doc, pointer)
						}
					case []any:
						i, err := strconv.Atoi(part)
						if err != nil || i < 0 || i >= len(v) {
							t.Fatal("invalid evidence index")
						}
						current = v[i]
					default:
						t.Fatal("scalar evidence traversal")
					}
				}
			}
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(docs["comparison"])
}

func TestPartialResponseStatisticsRemainDescriptive(t *testing.T) {
	b, a := cohort(900, 1000, 1100), cohort(400, 500, 600)
	for _, r := range []*Recording{&b, &a} {
		for i := range r.Samples {
			h := &r.Samples[i].Hops[0]
			h.BodyComplete = false
			h.Outcome = "failed"
			h.ErrorCode = "body_failed"
			h.Events = append(h.Events[:5], h.Events[6:]...)
			r.Samples[i].Outcome = "failed"
		}
	}
	c := compared(t, b, a)
	partial := comparedMetric(t, c, "partial_response_read")
	if partial.Baseline.Count != 3 || partial.Candidate.Count != 3 || *partial.Baseline.Median != 300 || partial.Interpretation.Status != "descriptive_only" || partial.AbsoluteChange != nil || partial.PercentChange != nil {
		t.Fatalf("partial windows lost or compared unsafely: %+v", partial)
	}
	complete := comparedMetric(t, c, "response_read")
	if complete.Baseline.Count != 0 || complete.Baseline.Excluded != 3 || complete.Baseline.Median != nil {
		t.Fatal("partial and complete cohorts mixed")
	}
}

func TestScopedIPv6ZoneCasePreserved(t *testing.T) {
	if normalizedTarget("http://[FE80::1%25ethA]/") == normalizedTarget("http://[fe80::1%25etha]/") {
		t.Fatal("case-sensitive zones conflated")
	}
	if normalizedTarget("http://[FE80::1%25ethA]/") != normalizedTarget("http://[fe80::1%25ethA]:80/") {
		t.Fatal("IPv6 address normalization failed")
	}
}

func TestComparisonMultipleHops(t *testing.T) {
	b, a := cohort(900, 1000, 1100), cohort(400, 500, 600)
	for _, r := range []*Recording{&b, &a} {
		for i := range r.Samples {
			first := &r.Samples[i].Hops[0]
			redirect := 302
			first.StatusCode = &redirect
			second := timedRecording().Samples[0].Hops[0]
			protocol, encoding := "HTTP/1.1", "identity"
			second.Protocol = &protocol
			second.ContentEncoding = &encoding
			second.URL = "https://example.org/next"
			r.Samples[i].Hops = append(r.Samples[i].Hops, second)
			r.Samples[i].ElapsedNS += second.ElapsedNS
		}
	}
	c := compared(t, b, a)
	if len(c.Hops) != 2 || c.Hops[1].Index != 1 {
		t.Fatal("hop indices lost")
	}
	a.Samples[0].Hops = a.Samples[0].Hops[:1]
	c = compared(t, b, a)
	if c.Hops[1].Calculations[4].Candidate.Unavailable != 1 {
		t.Fatal("missing hop excluded from coverage")
	}
}
