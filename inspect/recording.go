// Package inspect defines the evidence format for BitSpeed Inspect.
package inspect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

const SchemaVersion = 1
const MaxRecordingBytes = 16 << 20
const MaxSamples = 100
const MaxHops = 11
const MaxEvents = 256

// Recording stores observations only. Derived reports must never alter this evidence.
type Recording struct {
	SchemaVersion int       `json:"schema_version"`
	ToolVersion   string    `json:"tool_version"`
	StartedAt     time.Time `json:"started_at"`
	Request       Request   `json:"request"`
	Samples       []Sample  `json:"samples"`
}

type Request struct {
	SamplesRequested int    `json:"samples_requested"`
	MaxRedirects     int    `json:"max_redirects"`
	TimeoutNS        int64  `json:"timeout_ns"`
	ConnectTimeoutNS int64  `json:"connect_timeout_ns"`
	HeaderTimeoutNS  int64  `json:"header_timeout_ns"`
	BodyTimeoutNS    int64  `json:"body_timeout_ns"`
	IntervalNS       int64  `json:"interval_ns"`
	Method           string `json:"method"`
	URL              string `json:"url"`
	ConnectionPolicy string `json:"connection_policy"`
	Decompression    bool   `json:"decompression"`
	LocationLabel    string `json:"location_label,omitempty"`
}

type Sample struct {
	Index     int    `json:"index"`
	Outcome   string `json:"outcome"`
	ElapsedNS int64  `json:"elapsed_ns"`
	Hops      []Hop  `json:"hops"`
}

type Hop struct {
	ContentEncoding       *string `json:"content_encoding"`
	WinningConnectAttempt *string `json:"winning_connect_attempt"`
	URL                   string  `json:"url"`
	Outcome               string  `json:"outcome"`
	ElapsedNS             int64   `json:"elapsed_ns"`
	StatusCode            *int    `json:"status_code"`
	Protocol              *string `json:"protocol"`
	RemoteAddress         *string `json:"remote_address"`
	ConnectionReused      *bool   `json:"connection_reused"`
	BodyBytes             int64   `json:"body_bytes"`
	BodyComplete          bool    `json:"body_complete"`
	Events                []Event `json:"events"`
	ErrorCode             string  `json:"error_code,omitempty"`
}

// Event time is relative to its hop's monotonic start. Attempt links overlapping operations.
type Event struct {
	Kind     string `json:"kind"`
	OffsetNS int64  `json:"offset_ns"`
	Attempt  string `json:"attempt,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Address  string `json:"address,omitempty"`
}

// SafeURL rejects embedded credentials and redacts every query value and fragment.
// Query-bearing recordings must be compared cautiously: redaction hides request differences.
func SafeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("expected an absolute HTTP or HTTPS URL")
	}
	if u.User != nil {
		return "", errors.New("URL credentials are not allowed")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("invalid URL query")
	}
	for key, values := range q {
		for i := range values {
			values[i] = "[redacted]"
		}
		q[key] = values
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

func validateURL(raw string) error {
	safe, err := SafeURL(raw)
	if err != nil {
		return err
	}
	if safe != raw {
		return errors.New("recorded URL must already be normalized and redacted")
	}
	return nil
}

func validOutcome(s string) bool {
	return s == "complete" || s == "failed" || s == "cancelled"
}

func (r Recording) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", r.SchemaVersion)
	}
	if r.ToolVersion == "" || r.StartedAt.IsZero() {
		return errors.New("tool version and start time are required")
	}
	if r.Request.Method != "GET" || r.Request.ConnectionPolicy != "fresh" {
		return errors.New("schema v1 requires GET and fresh connections")
	}
	if err := validateURL(r.Request.URL); err != nil {
		return err
	}
	config := Options{Samples: r.Request.SamplesRequested, MaxRedirects: r.Request.MaxRedirects, Timeout: time.Duration(r.Request.TimeoutNS), ConnectTimeout: time.Duration(r.Request.ConnectTimeoutNS), HeaderTimeout: time.Duration(r.Request.HeaderTimeoutNS), BodyTimeout: time.Duration(r.Request.BodyTimeoutNS), Interval: time.Duration(r.Request.IntervalNS), LocationLabel: r.Request.LocationLabel}
	if err := config.validate(); err != nil {
		return err
	}
	if r.Request.Decompression {
		return errors.New("decompression is unsupported in schema v1")
	}

	if len(r.Samples) == 0 || len(r.Samples) > r.Request.SamplesRequested {
		return errors.New("recording must contain at least one sample")
	}
	for i, s := range r.Samples {
		if s.Index != i || !validOutcome(s.Outcome) || s.ElapsedNS < 0 || len(s.Hops) == 0 || len(s.Hops) > r.Request.MaxRedirects+1 {
			return fmt.Errorf("invalid sample %d", i)
		}
		remaining := s.ElapsedNS
		for j, h := range s.Hops {
			if h.ElapsedNS > remaining {
				return errors.New("sequential hop durations exceed sample duration")
			}
			remaining -= h.ElapsedNS
			if h.ConnectionReused != nil && *h.ConnectionReused {
				return errors.New("fresh connection policy cannot report reuse")
			}
			if err := validateURL(h.URL); err != nil {
				return fmt.Errorf("sample %d hop %d: %w", i, j, err)
			}
			if !validOutcome(h.Outcome) || h.ElapsedNS < 0 || h.ElapsedNS > s.ElapsedNS || h.BodyBytes < 0 {
				return fmt.Errorf("invalid sample %d hop %d", i, j)
			}
			if h.ContentEncoding != nil && !validEncoding(*h.ContentEncoding) {
				return errors.New("unknown content encoding classification")
			}
			if h.StatusCode != nil && (*h.StatusCode < 100 || *h.StatusCode > 599) {
				return errors.New("invalid HTTP status")
			}
			if h.Outcome == "complete" && (!h.BodyComplete || h.StatusCode == nil || h.ErrorCode != "") {
				return errors.New("complete hop requires complete body and status without error")
			}
			if h.Outcome != "complete" && h.ErrorCode == "" {
				return errors.New("incomplete hop requires an error code")
			}
			if (h.Outcome == "cancelled") != (h.ErrorCode == "cancelled") {
				return errors.New("cancelled outcome requires cancelled error code")
			}
			if h.ErrorCode != "" && !validErrorCode(h.ErrorCode) {
				return errors.New("unknown error code")
			}
			if len(h.Events) == 0 || len(h.Events) > MaxEvents {
				return errors.New("invalid event count")
			}
			if err := validateEvidence(h); err != nil {
				return err
			}
			var previous int64
			for _, e := range h.Events {
				if !validEvent(e.Kind) || e.OffsetNS < previous || e.OffsetNS > h.ElapsedNS {
					return errors.New("invalid or unordered event")
				}
				if e.Outcome != "" && e.Outcome != "success" && e.Outcome != "failed" {
					return errors.New("invalid operation outcome")
				}
				previous = e.OffsetNS
			}
			if s.Outcome == "complete" && h.Outcome != "complete" {
				return errors.New("complete sample contains incomplete hop")
			}
		}
		if s.Outcome != s.Hops[len(s.Hops)-1].Outcome {
			return errors.New("sample outcome differs from final hop")
		}
		for _, h := range s.Hops[:len(s.Hops)-1] {
			if h.Outcome != "complete" {
				return errors.New("sample continued after a failed hop")
			}
		}
	}
	return nil
}

func validEvent(kind string) bool {
	switch kind {
	case "dns_start", "dns_done", "connect_start", "connect_done", "tls_start", "tls_done",
		"connection_acquired", "request_written", "first_byte", "body_done", "hop_done":
		return true
	}
	return false
}

func validErrorCode(code string) bool {
	switch code {
	case "dns_failed", "connect_failed", "tls_failed", "request_write_failed",
		"response_failed", "body_failed", "timeout", "cancelled", "redirect_limit", "invalid_redirect", "event_limit", "unsupported_upgrade":
		return true
	}
	return false
}

// Decode rejects unsupported fields, oversized input and trailing JSON values.
func Decode(reader io.Reader) (Recording, error) {
	b, err := io.ReadAll(io.LimitReader(reader, MaxRecordingBytes+1))
	if err != nil {
		return Recording{}, err
	}
	if len(b) > MaxRecordingBytes {
		return Recording{}, errors.New("recording exceeds size limit")
	}
	var r Recording
	if err := decodeStrict(b, &r); err != nil {
		return Recording{}, err
	}
	return r, r.Validate()
}

func Encode(w io.Writer, r Recording) error {
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > MaxRecordingBytes {
		return errors.New("recording exceeds size limit")
	}
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func validateEvidence(h Hop) error {
	last := h.Events[len(h.Events)-1]
	if last.Kind != "hop_done" || last.OffsetNS != h.ElapsedNS {
		return errors.New("hop must end with its termination event")
	}
	starts := map[string]Event{}
	done := map[string]Event{}
	bodyDone := false
	for i, e := range h.Events {
		if e.Kind == "hop_done" && i != len(h.Events)-1 {
			return errors.New("premature hop termination event")
		}
		if e.Kind == "body_done" {
			bodyDone = true
		}
		operation := strings.HasPrefix(e.Kind, "dns_") || strings.HasPrefix(e.Kind, "connect_") || strings.HasPrefix(e.Kind, "tls_")
		if operation {
			if e.Attempt == "" {
				return errors.New("operation has no attempt identity")
			}
			if strings.HasSuffix(e.Kind, "_start") {
				if _, exists := starts[e.Attempt]; exists {
					return errors.New("duplicate operation identity")
				}
				starts[e.Attempt] = e
			} else {
				start, exists := starts[e.Attempt]
				if !exists || strings.TrimSuffix(start.Kind, "_start") != strings.TrimSuffix(e.Kind, "_done") || start.Address != e.Address {
					return errors.New("unpaired operation completion")
				}
				if _, exists := done[e.Attempt]; exists {
					return errors.New("duplicate operation completion")
				}
				done[e.Attempt] = e
			}
		}
		needsOutcome := e.Kind == "dns_done" || e.Kind == "connect_done" || e.Kind == "tls_done" || e.Kind == "request_written" || e.Kind == "connection_acquired"
		if needsOutcome != (e.Outcome != "") {
			return errors.New("operation outcome missing or misplaced")
		}
	}
	if bodyDone != h.BodyComplete || (h.BodyComplete && h.StatusCode == nil) {
		return errors.New("body completion evidence inconsistent")
	}
	if h.WinningConnectAttempt != nil {
		winner, exists := done[*h.WinningConnectAttempt]
		if !exists || winner.Kind != "connect_done" || winner.Outcome != "success" || h.RemoteAddress == nil || winner.Address != *h.RemoteAddress {
			return errors.New("winning connection lacks supporting evidence")
		}
		acquired := 0
		for _, e := range h.Events {
			if e.Kind == "connection_acquired" {
				if e.Outcome != "success" || e.Address != winner.Address || e.OffsetNS < winner.OffsetNS {
					return errors.New("winning connection acquisition is inconsistent")
				}
				acquired++
			}
		}
		if acquired != 1 {
			return errors.New("winning connection requires unique acquisition evidence")
		}

	}
	return nil
}

func validEncoding(value string) bool {
	switch value {
	case "identity", "gzip", "br", "deflate", "zstd", "compress", "other":
		return true
	}
	return false
}
