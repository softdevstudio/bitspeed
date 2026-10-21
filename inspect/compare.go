package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const ComparisonSchemaVersion = 1

type GroupSummary struct {
	SourceSHA256  string     `json:"source_sha256"`
	LocationLabel string     `json:"location_label"`
	Requested     int        `json:"requested"`
	Recorded      int        `json:"recorded"`
	Completed     int        `json:"completed"`
	Failed        int        `json:"failed"`
	Cancelled     int        `json:"cancelled"`
	Unattempted   int        `json:"unattempted"`
	RuleID        string     `json:"rule_id"`
	Evidence      []Evidence `json:"evidence"`
}

type ComparabilityCheck struct {
	RuleID   string     `json:"rule_id"`
	Severity string     `json:"severity"`
	Scope    string     `json:"scope"`
	Text     string     `json:"text"`
	Evidence []Evidence `json:"evidence"`
}

type Statistics struct {
	Excluded    int        `json:"excluded"`
	Count       int        `json:"count"`
	Unavailable int        `json:"unavailable"`
	Median      *float64   `json:"median"`
	Minimum     *float64   `json:"minimum"`
	Maximum     *float64   `json:"maximum"`
	RuleID      string     `json:"rule_id"`
	Evidence    []Evidence `json:"evidence"`
}

type ComparisonVerdict struct {
	Status      string     `json:"status"`
	RuleID      string     `json:"rule_id"`
	Text        string     `json:"text"`
	Evidence    []Evidence `json:"evidence"`
	Limitations []string   `json:"limitations"`
}

type MetricComparison struct {
	RuleID         string            `json:"rule_id"`
	Evidence       []Evidence        `json:"evidence"`
	Name           string            `json:"name"`
	Unit           string            `json:"unit"`
	Baseline       Statistics        `json:"baseline"`
	Candidate      Statistics        `json:"candidate"`
	AbsoluteChange *float64          `json:"absolute_change"`
	PercentChange  *float64          `json:"percent_change"`
	Interpretation ComparisonVerdict `json:"interpretation"`
}

type HopComparison struct {
	Index        int                  `json:"index"`
	Checks       []ComparabilityCheck `json:"checks"`
	Calculations []MetricComparison   `json:"calculations"`
}

type Comparison struct {
	SchemaVersion      int                  `json:"schema_version"`
	RuleSet            string               `json:"rule_set"`
	ExplanationRuleSet string               `json:"explanation_rule_set"`
	Baseline           GroupSummary         `json:"baseline"`
	Candidate          GroupSummary         `json:"candidate"`
	Checks             []ComparabilityCheck `json:"checks"`
	Hops               []HopComparison      `json:"hops"`
	Limitations        []string             `json:"limitations"`
}

func comparisonRef(document, pointer string) Evidence {
	return Evidence{Document: document, Pointer: pointer}
}
func groupSummary(r Recording, report Report, side string) GroupSummary {
	g := GroupSummary{SourceSHA256: report.SourceSHA256, LocationLabel: r.Request.LocationLabel, Requested: r.Request.SamplesRequested, Recorded: len(r.Samples), Unattempted: r.Request.SamplesRequested - len(r.Samples), RuleID: "sample_counts_v1", Evidence: []Evidence{comparisonRef(side+"_recording", "/request/samples_requested"), comparisonRef(side+"_recording", "/request/location_label"), comparisonRef(side+"_recording", "/samples")}}
	// Omitted optional labels cannot be cited as existing fields.
	if r.Request.LocationLabel == "" {
		g.Evidence = append(g.Evidence[:1], g.Evidence[2:]...)
	}
	for _, s := range r.Samples {
		switch s.Outcome {
		case "complete":
			g.Completed++
		case "failed":
			g.Failed++
		case "cancelled":
			g.Cancelled++
		}
	}
	return g
}

func check(id, severity, scope, text string, refs ...Evidence) ComparabilityCheck {
	return ComparabilityCheck{RuleID: id, Severity: severity, Scope: scope, Text: text, Evidence: refs}
}
func requestRefs(field string) []Evidence {
	return []Evidence{comparisonRef("baseline_recording", "/request/"+field), comparisonRef("candidate_recording", "/request/"+field)}
}

func normalizedTarget(raw string) string {
	u, _ := url.Parse(raw)
	u.Scheme = strings.ToLower(u.Scheme)
	host := u.Hostname()
	if zone := strings.LastIndex(host, "%"); zone >= 0 {
		host = strings.ToLower(host[:zone]) + host[zone:]
	} else {
		host = strings.ToLower(host)
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	u.Host = host
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// Compare describes the supplied samples; it does not establish a causal or population-level improvement.
func Compare(baseline, candidate Recording) (Comparison, error) {
	before, err := Explain(baseline)
	if err != nil {
		return Comparison{}, err
	}
	after, err := Explain(candidate)
	if err != nil {
		return Comparison{}, err
	}
	c := Comparison{SchemaVersion: ComparisonSchemaVersion, RuleSet: "inspect-compare-v1", ExplanationRuleSet: before.RuleSet, Baseline: groupSummary(baseline, before, "baseline"), Candidate: groupSummary(candidate, after, "candidate"), Checks: []ComparabilityCheck{}, Hops: []HopComparison{}, Limitations: []string{
		"Medians and minimum-to-maximum ranges describe the recorded samples, not statistical confidence or a causal effect.",
		"Available phase values are retained even when a comparison is blocked; no overall faster-transfer claim is made.",
		"Matching metadata and payload size do not prove identical response contents or identical external conditions.",
	}}
	if normalizedTarget(baseline.Request.URL) != normalizedTarget(candidate.Request.URL) {
		c.Checks = append(c.Checks, check("request_target_v1", "blocking", "all", "Recorded request targets differ.", requestRefs("url")...))
	}
	settings := []struct {
		name string
		a, b any
	}{
		{"method", baseline.Request.Method, candidate.Request.Method}, {"connection_policy", baseline.Request.ConnectionPolicy, candidate.Request.ConnectionPolicy},
		{"decompression", baseline.Request.Decompression, candidate.Request.Decompression}, {"timeout_ns", baseline.Request.TimeoutNS, candidate.Request.TimeoutNS},
		{"connect_timeout_ns", baseline.Request.ConnectTimeoutNS, candidate.Request.ConnectTimeoutNS}, {"header_timeout_ns", baseline.Request.HeaderTimeoutNS, candidate.Request.HeaderTimeoutNS},
		{"body_timeout_ns", baseline.Request.BodyTimeoutNS, candidate.Request.BodyTimeoutNS}, {"max_redirects", baseline.Request.MaxRedirects, candidate.Request.MaxRedirects},
		{"interval_ns", baseline.Request.IntervalNS, candidate.Request.IntervalNS},
	}
	for _, setting := range settings {
		if setting.a != setting.b {
			c.Checks = append(c.Checks, check("request_setting_"+setting.name+"_v1", "blocking", "all", "Request settings differ: "+setting.name+".", requestRefs(setting.name)...))
		}
	}
	// Schema 2 request bodies. These fields are absent from schema 1 and from
	// GET recordings, so the evidence points at the whole request object.
	bodies := []struct {
		name string
		a, b any
	}{
		{"request_body_bytes", baseline.Request.RequestBodyBytes, candidate.Request.RequestBodyBytes},
		{"request_body_sha256", baseline.Request.RequestBodySHA256, candidate.Request.RequestBodySHA256},
		{"content_type", baseline.Request.ContentType, candidate.Request.ContentType},
	}
	for _, setting := range bodies {
		if setting.a != setting.b {
			c.Checks = append(c.Checks, check("request_setting_"+setting.name+"_v1", "blocking", "all", "Request settings differ: "+setting.name+".", comparisonRef("baseline_recording", "/request"), comparisonRef("candidate_recording", "/request")))
		}
	}
	if baseline.ToolVersion != candidate.ToolVersion {
		c.Checks = append(c.Checks, check("tool_version_v1", "caution", "all", "Recorder versions differ; equivalent measurement behavior is not established.", comparisonRef("baseline_recording", "/tool_version"), comparisonRef("candidate_recording", "/tool_version")))
	}
	if baseline.Request.LocationLabel != candidate.Request.LocationLabel {
		c.Checks = append(c.Checks, check("location_labels_v1", "information", "all", "Operator-supplied location labels differ and are not independently verified.", comparisonRef("baseline_recording", "/request"), comparisonRef("candidate_recording", "/request")))
	}
	if c.Baseline.Completed != c.Baseline.Requested || c.Candidate.Completed != c.Candidate.Requested {
		c.Checks = append(c.Checks, check("failed_or_missing_samples_v1", "caution", "all", "Failed, cancelled or unattempted samples prevent a performance-change verdict.", comparisonRef("comparison", "/baseline"), comparisonRef("comparison", "/candidate")))
	}
	// Warm runs can reuse connections differently, for example when one server
	// closes keep-alive connections. Equal settings then don't mean equal work.
	reuse := func(r Recording) string {
		counts := map[int][2]int{}
		for _, s := range r.Samples {
			for j, h := range s.Hops {
				c := counts[j]
				c[0]++
				if h.ConnectionReused != nil && *h.ConnectionReused {
					c[1]++
				}
				counts[j] = c
			}
		}
		return fmt.Sprint(counts)
	}
	if (baseline.Request.ConnectionPolicy == "warm" || candidate.Request.ConnectionPolicy == "warm") && reuse(baseline) != reuse(candidate) {
		c.Checks = append(c.Checks, check("connection_reuse_v1", "caution", "all", "Connection reuse differs between recordings; reused and new connections do different work.", comparisonRef("baseline_recording", "/samples"), comparisonRef("candidate_recording", "/samples")))
	}
	// A chain signature includes hop targets, not just length. Mixed chains block index-based interpretation.
	chains := func(r Recording) []string {
		values := []string{}
		for _, s := range r.Samples {
			parts := []string{}
			for _, h := range s.Hops {
				// Schema 2 hops also carry their method; schema 1 hops are GET.
				method := h.Method
				if method == "" {
					method = "GET"
				}
				parts = append(parts, method+" "+normalizedTarget(h.URL))
			}
			values = append(values, strings.Join(parts, "\n"))
		}
		return distinct(values)
	}
	bchains, achains := chains(baseline), chains(candidate)
	if len(bchains) != 1 || len(achains) != 1 || bchains[0] != achains[0] {
		c.Checks = append(c.Checks, check("redirect_chain_v1", "blocking", "all", "Hop targets or redirect chains differ within or between recordings; hop indices do not establish equivalent requests.", comparisonRef("baseline_recording", "/samples"), comparisonRef("candidate_recording", "/samples")))
	}
	maxHops := 0
	for _, r := range []Recording{baseline, candidate} {
		for _, s := range r.Samples {
			if len(s.Hops) > maxHops {
				maxHops = len(s.Hops)
			}
		}
	}
	for index := 0; index < maxHops; index++ {
		h := HopComparison{Index: index, Checks: hopChecks(baseline, candidate, index), Calculations: []MetricComparison{}}
		names := []string{"dns_resolution", "tcp_connect", "tls_handshake", "connection_setup", "first_byte_wait", "response_read", "observed_payload_rate"}
		hasPartial := false
		for _, r := range []Recording{baseline, candidate} {
			for _, s := range r.Samples {
				if index < len(s.Hops) && !s.Hops[index].BodyComplete {
					hasPartial = true
				}
			}
		}
		if hasPartial {
			names = append(names, "partial_response_read")
		}
		for _, name := range names {
			m := MetricComparison{Name: name, Unit: "ns", Baseline: phaseStatistics(baseline, before, index, name, "baseline"), Candidate: phaseStatistics(candidate, after, index, name, "candidate")}
			if name == "observed_payload_rate" {
				m.Unit = "bytes/s"
			}
			calcPath := fmt.Sprintf("/hops/%d/calculations/%d", index, len(h.Calculations))
			m.RuleID = "median_difference_v1"
			m.Evidence = []Evidence{comparisonRef("comparison", calcPath+"/baseline"), comparisonRef("comparison", calcPath+"/candidate")}
			if name == "partial_response_read" {
				m.RuleID = "descriptive_partial_windows_v1"
			}
			if name != "partial_response_read" && m.Baseline.Median != nil && m.Candidate.Median != nil {
				delta := *m.Candidate.Median - *m.Baseline.Median
				m.AbsoluteChange = &delta
				if *m.Baseline.Median != 0 {
					percent := delta / *m.Baseline.Median * 100
					m.PercentChange = &percent
				}
			}
			m.Interpretation = compareVerdict(m, c.Checks, h.Checks, index, len(h.Calculations))
			h.Calculations = append(h.Calculations, m)
		}
		c.Hops = append(c.Hops, h)
	}
	return c, nil
}

func distinct(values []string) []string {
	sort.Strings(values)
	out := []string{}
	for _, v := range values {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}
func responseMetric(name string) bool {
	return name == "first_byte_wait" || name == "response_read" || name == "observed_payload_rate"
}

func hopChecks(baseline, candidate Recording, index int) []ComparabilityCheck {
	checks := []ComparabilityCheck{}
	for _, field := range []string{"status_code", "protocol", "content_encoding", "body_bytes", "remote_address", "query"} {
		groups := [][]string{}
		refs := []Evidence{}
		unknown := false
		badStatus := false
		for side, r := range []Recording{baseline, candidate} {
			doc := []string{"baseline_recording", "candidate_recording"}[side]
			values := []string{}
			for i, s := range r.Samples {
				if index >= len(s.Hops) {
					unknown = true
					continue
				}
				h := s.Hops[index]
				path := fmt.Sprintf("/samples/%d/hops/%d", i, index)
				switch field {
				case "status_code":
					refs = append(refs, comparisonRef(doc, path+"/status_code"))
					if h.StatusCode == nil {
						unknown = true
					} else {
						values = append(values, strconv.Itoa(*h.StatusCode))
						if *h.StatusCode < 200 || *h.StatusCode >= 300 {
							badStatus = true
						}
					}
				case "protocol":
					refs = append(refs, comparisonRef(doc, path+"/protocol"))
					if h.Protocol == nil {
						unknown = true
					} else {
						values = append(values, *h.Protocol)
					}
				case "content_encoding":
					refs = append(refs, comparisonRef(doc, path+"/content_encoding"))
					if h.ContentEncoding == nil || *h.ContentEncoding == "other" {
						unknown = true
					} else {
						values = append(values, *h.ContentEncoding)
					}
				case "body_bytes":
					refs = append(refs, comparisonRef(doc, path+"/body_bytes"), comparisonRef(doc, path+"/body_complete"))
					if !h.BodyComplete {
						unknown = true
					} else {
						values = append(values, strconv.FormatInt(h.BodyBytes, 10))
					}
				case "remote_address":
					refs = append(refs, comparisonRef(doc, path+"/remote_address"))
					if h.RemoteAddress == nil {
						unknown = true
					} else {
						values = append(values, *h.RemoteAddress)
					}
				case "query":
					refs = append(refs, comparisonRef(doc, path+"/url"))
					u, _ := url.Parse(h.URL)
					if u.RawQuery != "" {
						unknown = true
					}
				}
			}
			groups = append(groups, distinct(values))
		}
		stable := len(groups[0]) == 1 && len(groups[1]) == 1 && groups[0][0] == groups[1][0]
		switch field {
		case "query":
			if unknown {
				checks = append(checks, check("query_identity_v1", "blocking", "response", "Redacted query values prevent establishing equivalent response requests.", refs...))
			}
		case "remote_address":
			if !stable || unknown {
				checks = append(checks, check("remote_address_v1", "information", "all", "Observed remote addresses differ, vary or are unavailable.", refs...))
			}
		default:
			scope := "response"
			if field == "protocol" {
				scope = "all"
			}
			if !stable || unknown {
				checks = append(checks, check("hop_"+field+"_v1", "blocking", scope, "Hop metadata differs, varies or is unavailable: "+field+".", refs...))
			}
			if field == "status_code" && badStatus {
				checks = append(checks, check("non_success_status_v1", "caution", "response", "A non-2xx HTTP response is present; response performance-change verdicts are withheld.", refs...))
			}
		}
	}
	return checks
}

func phaseStatistics(r Recording, report Report, hop int, name, side string) Statistics {
	stats := Statistics{Unavailable: len(r.Samples), RuleID: "available_phase_statistics_v1", Evidence: []Evidence{comparisonRef(side+"_recording", "/samples"), comparisonRef(side+"_explanation", "/samples")}}
	values := []float64{}
	for i, s := range r.Samples {
		if hop >= len(s.Hops) {
			continue
		}
		if ((name == "response_read" || name == "observed_payload_rate") && !s.Hops[hop].BodyComplete) || (name == "partial_response_read" && s.Hops[hop].BodyComplete) {
			stats.Excluded++
			stats.Unavailable--
			continue
		}
		sourceName := name
		if name == "partial_response_read" {
			sourceName = "response_read"
		}
		for j, m := range report.Samples[i].Hops[hop].Calculations {
			if m.Name == sourceName && m.Value != nil {
				values = append(values, *m.Value)
				stats.Evidence = append(stats.Evidence, comparisonRef(side+"_explanation", fmt.Sprintf("/samples/%d/hops/%d/calculations/%d/value", i, hop, j)))
			}
		}
	}
	stats.Count = len(values)
	stats.Unavailable -= stats.Count
	if len(values) > 0 {
		sort.Float64s(values)
		minimum, maximum := values[0], values[len(values)-1]
		median := values[len(values)/2]
		if len(values)%2 == 0 {
			median = values[len(values)/2-1]/2 + median/2
		}
		stats.Minimum = &minimum
		stats.Maximum = &maximum
		stats.Median = &median
	}
	return stats
}

func compareVerdict(m MetricComparison, global, local []ComparabilityCheck, hop, index int) ComparisonVerdict {
	path := fmt.Sprintf("/hops/%d/calculations/%d", hop, index)
	verdict := ComparisonVerdict{Status: "unavailable", RuleID: "median_change_v1", Text: "A median is unavailable in one or both recordings.", Evidence: []Evidence{comparisonRef("comparison", path+"/baseline"), comparisonRef("comparison", path+"/candidate"), comparisonRef("comparison", "/checks"), comparisonRef("comparison", fmt.Sprintf("/hops/%d/checks", hop))}, Limitations: []string{"This describes collected samples and does not establish a causal or general performance change."}}
	if m.Name == "partial_response_read" {
		verdict.Status = "descriptive_only"
		verdict.Text = "Incomplete-response windows are shown separately and do not support a performance-change verdict."
		return verdict
	}
	if m.Baseline.Median == nil || m.Candidate.Median == nil {
		return verdict
	}
	caution := false
	for _, checks := range [][]ComparabilityCheck{global, local} {
		for _, c := range checks {
			if c.Scope == "all" || responseMetric(m.Name) {
				if c.Severity == "blocking" {
					verdict.Status = "not_comparable"
					verdict.Text = "Comparability checks block a change verdict for this phase."
					return verdict
				}
				if c.Severity == "caution" {
					caution = true
				}
			}
		}
	}
	verdict.Status = "inconclusive"
	if caution {
		verdict.Text = "Cautionary comparability checks prevent a performance-change verdict."
		return verdict
	}
	if m.Baseline.Count < 3 || m.Candidate.Count < 3 || m.Baseline.Unavailable > 0 || m.Candidate.Unavailable > 0 || m.Baseline.Excluded > 0 || m.Candidate.Excluded > 0 {
		verdict.Text = "At least three available values per recording and full phase coverage are required for a change verdict."
		return verdict
	}
	if *m.AbsoluteChange == 0 {
		verdict.Status = "equal_medians"
		verdict.Text = "The recorded medians are equal; unchanged performance is not established."
		return verdict
	}
	overlap := *m.Baseline.Minimum <= *m.Candidate.Maximum && *m.Candidate.Minimum <= *m.Baseline.Maximum
	if overlap {
		verdict.Text = "Observed minimum-to-maximum ranges overlap; the change is inconclusive beyond these samples."
		return verdict
	}
	if *m.AbsoluteChange < 0 {
		verdict.Status = "lower_in_samples"
		verdict.Text = "The candidate median is lower and observed ranges do not overlap in these samples."
	} else {
		verdict.Status = "higher_in_samples"
		verdict.Text = "The candidate median is higher and observed ranges do not overlap in these samples."
	}
	return verdict
}

func WriteComparisonJSON(w io.Writer, c Comparison) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeReportBytes(w, append(b, '\n'))
}
func WriteComparisonText(w io.Writer, c Comparison) error {
	var b strings.Builder
	fmt.Fprintf(&b, "BitSpeed Inspect comparison\nCalculated baseline counts: requested=%d recorded=%d complete=%d failed=%d cancelled=%d unattempted=%d\nCalculated candidate counts: requested=%d recorded=%d complete=%d failed=%d cancelled=%d unattempted=%d\n", c.Baseline.Requested, c.Baseline.Recorded, c.Baseline.Completed, c.Baseline.Failed, c.Baseline.Cancelled, c.Baseline.Unattempted, c.Candidate.Requested, c.Candidate.Recorded, c.Candidate.Completed, c.Candidate.Failed, c.Candidate.Cancelled, c.Candidate.Unattempted)
	fmt.Fprintf(&b, "Recorded source hashes: baseline=%s candidate=%s\n", strconv.Quote(c.Baseline.SourceSHA256), strconv.Quote(c.Candidate.SourceSHA256))
	fmt.Fprintf(&b, "Recorded location labels: baseline=%s candidate=%s\n", strconv.Quote(c.Baseline.LocationLabel), strconv.Quote(c.Candidate.LocationLabel))
	renderChecks := func(checks []ComparabilityCheck) {
		for _, check := range checks {
			fmt.Fprintf(&b, "Check [%s, %s, scope=%s]: %s\n", strconv.Quote(check.RuleID), strconv.Quote(check.Severity), strconv.Quote(check.Scope), strconv.Quote(check.Text))
		}
	}
	renderChecks(c.Checks)
	format := func(v *float64, unit string) string {
		if v == nil {
			return "unavailable"
		}
		if unit == "ns" {
			return strconv.FormatFloat(*v/1e6, 'f', 6, 64) + " ms"
		}
		return strconv.FormatFloat(*v, 'f', 3, 64) + " " + strconv.Quote(unit)
	}
	for _, h := range c.Hops {
		fmt.Fprintf(&b, "Hop %d\n", h.Index)
		renderChecks(h.Checks)
		for _, m := range h.Calculations {
			fmt.Fprintf(&b, " Calculated %s: baseline median=%s range=[%s, %s] n=%d unavailable=%d excluded=%d; candidate median=%s range=[%s, %s] n=%d unavailable=%d excluded=%d\n", strconv.Quote(m.Name), format(m.Baseline.Median, m.Unit), format(m.Baseline.Minimum, m.Unit), format(m.Baseline.Maximum, m.Unit), m.Baseline.Count, m.Baseline.Unavailable, m.Baseline.Excluded, format(m.Candidate.Median, m.Unit), format(m.Candidate.Minimum, m.Unit), format(m.Candidate.Maximum, m.Unit), m.Candidate.Count, m.Candidate.Unavailable, m.Candidate.Excluded)
			percent := "unavailable (zero baseline or missing median)"
			if m.PercentChange != nil {
				percent = strconv.FormatFloat(*m.PercentChange, 'f', 3, 64) + "%"
			}
			if m.Name != "partial_response_read" {
				fmt.Fprintf(&b, " Calculated candidate minus baseline: %s; percent change: %s\n", format(m.AbsoluteChange, m.Unit), percent)
			}
			fmt.Fprintf(&b, " Interpretation [%s]: %s\n", strconv.Quote(m.Interpretation.Status), strconv.Quote(m.Interpretation.Text))
		}
	}
	for _, limit := range c.Limitations {
		fmt.Fprintf(&b, "Limit: %s\n", strconv.Quote(limit))
	}
	return writeReportBytes(w, []byte(b.String()))
}
