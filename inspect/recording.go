// Package inspect defines the evidence format for BitSpeed Inspect.
package inspect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

const SchemaVersion = 1
const MaxRecordingBytes = 16 << 20

// Recording stores observations only. Derived reports must never alter this evidence.
type Recording struct {
	SchemaVersion int       `json:"schema_version"`
	ToolVersion   string    `json:"tool_version"`
	StartedAt     time.Time `json:"started_at"`
	Request       Request   `json:"request"`
	Samples       []Sample  `json:"samples"`
}

type Request struct {
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
	URL              string  `json:"url"`
	Outcome          string  `json:"outcome"`
	ElapsedNS        int64   `json:"elapsed_ns"`
	StatusCode       *int    `json:"status_code"`
	Protocol         *string `json:"protocol"`
	RemoteAddress    *string `json:"remote_address"`
	ConnectionReused *bool   `json:"connection_reused"`
	BodyBytes        int64   `json:"body_bytes"`
	BodyComplete     bool    `json:"body_complete"`
	Events           []Event `json:"events"`
	ErrorCode        string  `json:"error_code,omitempty"`
}

// Event time is relative to its hop's monotonic start. Attempt links overlapping operations.
type Event struct {
	Kind     string `json:"kind"`
	OffsetNS int64  `json:"offset_ns"`
	Attempt  string `json:"attempt,omitempty"`
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
	if len(r.Samples) == 0 {
		return errors.New("recording must contain at least one sample")
	}
	for i, s := range r.Samples {
		if s.Index != i || !validOutcome(s.Outcome) || s.ElapsedNS < 0 || len(s.Hops) == 0 {
			return fmt.Errorf("invalid sample %d", i)
		}
		for j, h := range s.Hops {
			if err := validateURL(h.URL); err != nil {
				return fmt.Errorf("sample %d hop %d: %w", i, j, err)
			}
			if !validOutcome(h.Outcome) || h.ElapsedNS < 0 || h.ElapsedNS > s.ElapsedNS || h.BodyBytes < 0 {
				return fmt.Errorf("invalid sample %d hop %d", i, j)
			}
			if h.StatusCode != nil && (*h.StatusCode < 100 || *h.StatusCode > 599) {
				return errors.New("invalid HTTP status")
			}
			if h.Outcome == "complete" && (!h.BodyComplete || h.StatusCode == nil || h.ErrorCode != "") {
				return errors.New("complete hop requires complete body and status without error")
			}
			if h.Outcome != "complete" && (h.BodyComplete || h.ErrorCode == "") {
				return errors.New("incomplete hop requires an error code and incomplete body")
			}
			if h.ErrorCode != "" && !validErrorCode(h.ErrorCode) {
				return errors.New("unknown error code")
			}
			var previous int64
			for _, e := range h.Events {
				if !validEvent(e.Kind) || e.OffsetNS < previous || e.OffsetNS > h.ElapsedNS {
					return errors.New("invalid or unordered event")
				}
				previous = e.OffsetNS
			}
			if s.Outcome == "complete" && h.Outcome != "complete" {
				return errors.New("complete sample contains incomplete hop")
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
		"response_failed", "body_failed", "timeout", "cancelled", "redirect_limit":
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
