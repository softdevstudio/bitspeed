package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

const ReportSchemaVersion = 1
const MaxReportBytes = 32 << 20

// Evidence is a JSON pointer into either the recording or this derived report.
type Evidence struct {
	Document string `json:"document"`
	Pointer  string `json:"pointer"`
}

type Measurement struct {
	Name     string   `json:"name"`
	Value    any      `json:"value"`
	Evidence Evidence `json:"evidence"`
}

type Calculation struct {
	Name              string     `json:"name"`
	Unit              string     `json:"unit"`
	Value             *float64   `json:"value"`
	RuleID            string     `json:"rule_id"`
	Evidence          []Evidence `json:"evidence"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
	Limitations       []string   `json:"limitations"`
}

type Interpretation struct {
	RuleID      string     `json:"rule_id"`
	Text        string     `json:"text"`
	Evidence    []Evidence `json:"evidence"`
	Limitations []string   `json:"limitations"`
}

type HopReport struct {
	Measurements    []Measurement    `json:"measurements"`
	Calculations    []Calculation    `json:"calculations"`
	Interpretations []Interpretation `json:"interpretations"`
}

type SampleReport struct {
	Index        int           `json:"index"`
	Measurements []Measurement `json:"measurements"`
	Hops         []HopReport   `json:"hops"`
}

type Report struct {
	SchemaVersion   int              `json:"schema_version"`
	RuleSet         string           `json:"rule_set"`
	SourceSHA256    string           `json:"source_sha256"`
	Measurements    []Measurement    `json:"measurements"`
	Samples         []SampleReport   `json:"samples"`
	Interpretations []Interpretation `json:"interpretations"`
}

func source(pointer string) Evidence  { return Evidence{Document: "recording", Pointer: pointer} }
func derived(pointer string) Evidence { return Evidence{Document: "report", Pointer: pointer} }
func measurement(name string, value any, pointer string) Measurement {
	return Measurement{Name: name, Value: value, Evidence: source(pointer)}
}
func observed[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

// Explain uses explicit rules, without changing the recording or accessing the network.
func Explain(r Recording) (Report, error) {
	if err := r.Validate(); err != nil {
		return Report{}, err
	}
	canonical, err := json.Marshal(r)
	if err != nil {
		return Report{}, err
	}
	if len(canonical) > MaxRecordingBytes {
		return Report{}, errors.New("recording exceeds size limit")
	}
	sum := sha256.Sum256(canonical)
	report := Report{SchemaVersion: ReportSchemaVersion, RuleSet: "inspect-explain-v1", SourceSHA256: hex.EncodeToString(sum[:]),
		Measurements: []Measurement{measurement("url", r.Request.URL, "/request/url"), measurement("method", r.Request.Method, "/request/method"), measurement("samples_requested", r.Request.SamplesRequested, "/request/samples_requested")},
		Samples:      make([]SampleReport, 0, len(r.Samples)), Interpretations: []Interpretation{}}
	if r.Request.LocationLabel != "" {
		report.Measurements = append(report.Measurements, measurement("location_label", r.Request.LocationLabel, "/request/location_label"))
	}
	for i, s := range r.Samples {
		path := fmt.Sprintf("/samples/%d", i)
		sr := SampleReport{Index: s.Index, Measurements: []Measurement{measurement("outcome", s.Outcome, path+"/outcome"), measurement("elapsed_ns", s.ElapsedNS, path+"/elapsed_ns")}, Hops: make([]HopReport, 0, len(s.Hops))}
		for j, h := range s.Hops {
			sr.Hops = append(sr.Hops, explainHop(h, fmt.Sprintf("%s/hops/%d", path, j)))
		}
		report.Samples = append(report.Samples, sr)
	}
	if len(r.Samples) < r.Request.SamplesRequested {
		report.Interpretations = append(report.Interpretations, Interpretation{RuleID: "incomplete_batch_v1", Text: "Fewer samples were recorded than requested.", Evidence: []Evidence{source("/samples"), source("/request/samples_requested")}, Limitations: []string{"The recording does not establish results for unattempted samples."}})
	}
	queryEvidence := []Evidence{}
	for i, s := range r.Samples {
		for j, h := range s.Hops {
			u, _ := url.Parse(h.URL)
			if u.RawQuery != "" {
				queryEvidence = append(queryEvidence, source(fmt.Sprintf("/samples/%d/hops/%d/url", i, j)))
			}
		}
	}
	u, _ := url.Parse(r.Request.URL)
	if u.RawQuery != "" {
		queryEvidence = append(queryEvidence, source("/request/url"))
	}
	if len(queryEvidence) > 0 {
		report.Interpretations = append(report.Interpretations, Interpretation{RuleID: "query_redaction_v1", Text: "Query values were redacted from saved URLs.", Evidence: queryEvidence, Limitations: []string{"Matching saved URLs do not establish that original query values matched."}})
	}
	return report, nil
}

// eventAt carries its original array index for auditable evidence paths.
type eventAt struct {
	event Event
	index int
}

func eventsOf(h Hop, kind string) []eventAt {
	out := []eventAt{}
	for i, e := range h.Events {
		if e.Kind == kind {
			out = append(out, eventAt{event: e, index: i})
		}
	}
	return out
}
func eventRef(path string, e eventAt) Evidence {
	return source(fmt.Sprintf("%s/events/%d", path, e.index))
}
func unavailable(name, unit, reason string, evidence ...Evidence) Calculation {
	return Calculation{Name: name, Unit: unit, RuleID: name + "_v1", Evidence: evidence, UnavailableReason: reason, Limitations: []string{}}
}
func interval(name string, start, end eventAt, path string) Calculation {
	m := unavailable(name, "ns", "End event preceded start event.", eventRef(path, start), eventRef(path, end))
	if end.event.OffsetNS >= start.event.OffsetNS {
		v := float64(end.event.OffsetNS - start.event.OffsetNS)
		m.Value = &v
		m.UnavailableReason = ""
	}
	return m
}

func singleOperation(h Hop, kind, path string) Calculation {
	name := kind
	if kind == "dns" {
		name = "dns_resolution"
	}
	if kind == "tls" {
		name = "tls_handshake"
	}
	starts, ends := eventsOf(h, kind+"_start"), eventsOf(h, kind+"_done")
	m := unavailable(name, "ns", "A single completed successful operation was not observed.", source(path+"/events"))
	if len(starts) == 1 && len(ends) == 1 && starts[0].event.Attempt == ends[0].event.Attempt && ends[0].event.Outcome == "success" {
		m = interval(name, starts[0], ends[0], path)
	}
	if kind == "dns" {
		m.Limitations = append(m.Limitations, "Resolver timing does not establish whether a network DNS lookup occurred.")
	}
	return m
}

func winningTCP(h Hop, path string) Calculation {
	m := unavailable("tcp_connect", "ns", "A unique winning dial attempt was not established.", source(path+"/winning_connect_attempt"), source(path+"/events"))
	if h.WinningConnectAttempt == nil {
		return m
	}
	var start, end *eventAt
	for i, e := range h.Events {
		if e.Attempt == *h.WinningConnectAttempt {
			a := eventAt{event: e, index: i}
			if e.Kind == "connect_start" {
				start = &a
			}
			if e.Kind == "connect_done" {
				end = &a
			}
		}
	}
	if start != nil && end != nil && end.event.Outcome == "success" {
		m = interval("tcp_connect", *start, *end, path)
		m.Evidence = append(m.Evidence, source(path+"/winning_connect_attempt"))
	}
	return m
}

func explainHop(h Hop, path string) HopReport {
	hr := HopReport{Measurements: []Measurement{
		measurement("url", h.URL, path+"/url"), measurement("outcome", h.Outcome, path+"/outcome"), measurement("elapsed_ns", h.ElapsedNS, path+"/elapsed_ns"),
		measurement("status_code", observed(h.StatusCode), path+"/status_code"), measurement("protocol", observed(h.Protocol), path+"/protocol"),
		measurement("remote_address", observed(h.RemoteAddress), path+"/remote_address"), measurement("connection_reused", observed(h.ConnectionReused), path+"/connection_reused"),
		measurement("body_bytes", h.BodyBytes, path+"/body_bytes"), measurement("body_complete", h.BodyComplete, path+"/body_complete"),
	}, Interpretations: []Interpretation{}}
	if h.ErrorCode != "" {
		hr.Measurements = append(hr.Measurements, measurement("error_code", h.ErrorCode, path+"/error_code"))
	}
	tcp := winningTCP(h, path)
	acquired := eventsOf(h, "connection_acquired")
	setup := unavailable("connection_setup", "ns", "Winning connection and acquisition evidence are incomplete or ambiguous.", source(path+"/events"), source(path+"/winning_connect_attempt"))
	if tcp.Value != nil && len(acquired) == 1 && acquired[0].event.Outcome == "success" && h.RemoteAddress != nil && acquired[0].event.Address == *h.RemoteAddress {
		// A winning connect completion must precede acquisition before setup can be explained.
		for _, e := range eventsOf(h, "connect_done") {
			if e.event.Attempt == *h.WinningConnectAttempt && e.event.OffsetNS <= acquired[0].event.OffsetNS {
				v := float64(acquired[0].event.OffsetNS)
				setup.Value = &v
				setup.UnavailableReason = ""
				setup.Evidence = append(tcp.Evidence, eventRef(path, acquired[0]))
			}
		}
	}
	setup.Evidence = append(setup.Evidence, source(path+"/remote_address"))
	setup.Limitations = append(setup.Limitations, "Elapsed time from hop start to connection acquisition includes resolver, dial, TLS and scheduling effects where present; these intervals are not additive.")
	tlsMetric := singleOperation(h, "tls", path)
	if tlsMetric.Value != nil {
		tlsStarts, tlsEnds := eventsOf(h, "tls_start"), eventsOf(h, "tls_done")
		valid := false
		if tcp.Value != nil && len(acquired) == 1 {
			for _, e := range eventsOf(h, "connect_done") {
				if e.event.Attempt == *h.WinningConnectAttempt && e.event.OffsetNS <= tlsStarts[0].event.OffsetNS && tlsEnds[0].event.OffsetNS <= acquired[0].event.OffsetNS {
					valid = true
				}
			}
		}
		if !valid {
			tlsMetric.Value = nil
			tlsMetric.UnavailableReason = "TLS could not be associated with the winning connection lifecycle."
		}
	}
	tlsMetric.Evidence = append(tlsMetric.Evidence, tcp.Evidence...)
	tlsMetric.Evidence = append(tlsMetric.Evidence, source(path+"/winning_connect_attempt"), source(path+"/remote_address"))
	for _, e := range acquired {
		tlsMetric.Evidence = append(tlsMetric.Evidence, eventRef(path, e))
	}
	if len(eventsOf(h, "tls_start")) > 0 {
		setup.Evidence = append(setup.Evidence, tlsMetric.Evidence...)
	}
	if len(eventsOf(h, "tls_start")) > 0 && tlsMetric.Value == nil {
		setup.Value = nil
		setup.UnavailableReason = "TLS evidence does not establish completed connection setup."
	}
	writes, first, bodyDone := eventsOf(h, "request_written"), eventsOf(h, "first_byte"), eventsOf(h, "body_done")
	wait := unavailable("first_byte_wait", "ns", "A unique successful request write and first-response-byte event were not observed.", source(path+"/events"))
	if len(writes) == 1 && writes[0].event.Outcome == "success" && len(first) == 1 {
		wait = interval("first_byte_wait", writes[0], first[0], path)
	}
	wait.Limitations = append(wait.Limitations, "Client first-byte waiting includes effects beyond origin processing and does not identify a server-side cause.")
	response := unavailable("response_read", "ns", "A unique first-response-byte event was not observed.", source(path+"/events"))
	if len(first) == 1 {
		if h.BodyComplete && len(bodyDone) == 1 {
			response = interval("response_read", first[0], bodyDone[0], path)
		} else if h.BodyComplete {
			response.UnavailableReason = "A unique body-completion event was not observed."
		} else if !h.BodyComplete {
			last := eventAt{event: h.Events[len(h.Events)-1], index: len(h.Events) - 1}
			response = interval("response_read", first[0], last, path)
			response.Limitations = append(response.Limitations, "Incomplete response; this interval ends at hop termination and includes failure handling.")
		}
	}
	response.Evidence = append(response.Evidence, source(path+"/body_complete"))
	response.Limitations = append(response.Limitations, "Starts at the first response byte, so it includes response-header reading and client scheduling as well as body reading.")
	rate := unavailable("observed_payload_rate", "bytes/s", "A complete body and a positive response-reading interval are required.", source(path+"/body_bytes"), source(path+"/body_complete"))
	if h.BodyComplete && response.Value != nil && *response.Value > 0 {
		v := float64(h.BodyBytes) * 1e9 / *response.Value
		rate.Value = &v
		rate.UnavailableReason = ""
	}
	rate.Evidence = append(rate.Evidence, derived(path+"/calculations/5"))
	rate.Limitations = append(rate.Limitations, "Payload bytes divided by the observed response interval is not link capacity or a pure body-transfer rate.")
	hr.Calculations = []Calculation{singleOperation(h, "dns", path), tcp, tlsMetric, setup, wait, response, rate}
	// Whole-event references support uniqueness and absence checks as well as cited event pairs.
	for i := range hr.Calculations {
		hr.Calculations[i].Evidence = append(hr.Calculations[i].Evidence, source(path+"/events"))
		hr.Calculations[i].Evidence = uniqueEvidence(hr.Calculations[i].Evidence)
	}

	// Interpret only comparable nonoverlapping intervals, with no causal claim.
	candidates := []int{3, 4, 5}
	if setup.Value != nil && wait.Value != nil && response.Value != nil && h.BodyComplete && len(writes) == 1 && len(acquired) == 1 && acquired[0].event.OffsetNS <= writes[0].event.OffsetNS {
		winner := candidates[0]
		unique := true
		for _, i := range candidates[1:] {
			if *hr.Calculations[i].Value > *hr.Calculations[winner].Value {
				winner = i
				unique = true
			} else if *hr.Calculations[i].Value == *hr.Calculations[winner].Value {
				unique = false
			}
		}
		if unique {
			names := map[int]string{3: "Connection setup", 4: "First-byte waiting", 5: "Response reading after the first byte"}
			refs := []Evidence{}
			for _, i := range candidates {
				refs = append(refs, derived(fmt.Sprintf("%s/calculations/%d", path, i)))
			}
			hr.Interpretations = append(hr.Interpretations, Interpretation{RuleID: "largest_interval_v1", Text: names[winner] + " was the largest of the compared intervals in this hop.", Evidence: refs, Limitations: []string{"These intervals do not cover every part of the hop and do not establish the cause of the delay."}})
		}
	}
	if h.ErrorCode != "" {
		hr.Interpretations = append(hr.Interpretations, Interpretation{RuleID: "reported_failure_v1", Text: failureExplanation(h.ErrorCode), Evidence: []Evidence{source(path + "/error_code"), source(path + "/outcome")}, Limitations: []string{"The error category does not establish the underlying system or network cause."}})
	}
	if !h.BodyComplete {
		hr.Interpretations = append(hr.Interpretations, Interpretation{RuleID: "incomplete_response_v1", Text: "The response body was not recorded as complete; no completed-payload rate is reported.", Evidence: []Evidence{source(path + "/body_complete"), source(path + "/body_bytes")}, Limitations: []string{}})
	}
	return hr
}

func failureExplanation(code string) string {
	switch code {
	case "dns_failed":
		return "The recorder reported a resolver failure."
	case "connect_failed":
		return "The recorder reported a connection failure."
	case "tls_failed":
		return "The recorder reported a TLS negotiation failure."
	case "request_write_failed":
		return "The recorder reported a request-write failure."
	case "body_failed":
		return "The recorder reported a body-reading failure."
	case "timeout":
		return "The recorder reported a timeout."
	case "cancelled":
		return "The recorder reported cancellation."
	case "redirect_limit":
		return "The recorder stopped at its configured redirect limit."
	case "invalid_redirect":
		return "The recorder rejected a redirect target."
	case "event_limit":
		return "The recorder stopped when its trace-event limit was reached."
	case "unsupported_upgrade":
		return "The recorder rejected an HTTP protocol upgrade."
	default:
		return "The recorder reported a response failure."
	}
}

func WriteReportJSON(w io.Writer, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeReportBytes(w, append(b, '\n'))
}
func writeReportBytes(w io.Writer, b []byte) error {
	if len(b) > MaxReportBytes {
		return errors.New("report exceeds size limit")
	}
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func WriteReportText(w io.Writer, r Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "BitSpeed Inspect explanation\nSource SHA256: %s\n", strconv.Quote(r.SourceSHA256))
	writeMeasurements := func(ms []Measurement) {
		for _, m := range ms {
			value := "unavailable"
			if m.Value != nil {
				if s, ok := m.Value.(string); ok {
					value = strconv.Quote(s)
				} else {
					value = fmt.Sprint(m.Value)
				}
			}
			fmt.Fprintf(&b, "  Measured %s: %s [recording:%s]\n", strconv.Quote(m.Name), value, strconv.Quote(m.Evidence.Pointer))
		}
	}
	writeInterpretations := func(is []Interpretation) {
		for _, i := range is {
			fmt.Fprintf(&b, "  Interpretation [%s]: %s\n", strconv.Quote(i.RuleID), strconv.Quote(i.Text))
			for _, l := range i.Limitations {
				fmt.Fprintf(&b, "    Limit: %s\n", strconv.Quote(l))
			}
			for _, e := range i.Evidence {
				fmt.Fprintf(&b, "    Evidence %s:%s\n", strconv.Quote(e.Document), strconv.Quote(e.Pointer))
			}
		}
	}
	writeMeasurements(r.Measurements)
	for _, s := range r.Samples {
		fmt.Fprintf(&b, "Sample %d\n", s.Index)
		writeMeasurements(s.Measurements)
		for j, h := range s.Hops {
			fmt.Fprintf(&b, " Hop %d\n", j)
			writeMeasurements(h.Measurements)
			for _, c := range h.Calculations {
				value := "unavailable: " + strconv.Quote(c.UnavailableReason)
				if c.Value != nil {
					if c.Unit == "ns" {
						value = strconv.FormatFloat(*c.Value/1e6, 'f', 6, 64) + " ms"
					} else {
						value = strconv.FormatFloat(*c.Value, 'f', 3, 64) + " " + strconv.Quote(c.Unit)
					}
				}
				fmt.Fprintf(&b, "  Calculated %s: %s [%s]\n", strconv.Quote(c.Name), value, strconv.Quote(c.RuleID))
				for _, l := range c.Limitations {
					fmt.Fprintf(&b, "    Limit: %s\n", strconv.Quote(l))
				}
				for _, e := range c.Evidence {
					fmt.Fprintf(&b, "    Evidence %s:%s\n", strconv.Quote(e.Document), strconv.Quote(e.Pointer))
				}
			}
			writeInterpretations(h.Interpretations)
		}
	}
	writeInterpretations(r.Interpretations)
	return writeReportBytes(w, []byte(b.String()))
}

func uniqueEvidence(refs []Evidence) []Evidence {
	seen := map[Evidence]bool{}
	out := make([]Evidence, 0, len(refs))
	for _, ref := range refs {
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}
