package inspect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options controls real network requests. Timeouts apply to each sample, not the whole batch.
type Options struct {
	Samples        int
	MaxRedirects   int
	Timeout        time.Duration
	ConnectTimeout time.Duration
	HeaderTimeout  time.Duration
	BodyTimeout    time.Duration
	Interval       time.Duration
	LocationLabel  string
	// Method is GET (the default when empty) or POST. Body and ContentType
	// apply to POST only. Warm reuses connections across samples.
	Method      string
	Body        []byte
	ContentType string
	Warm        bool
}

func (o Options) method() string {
	if o.Method == "" {
		return "GET"
	}
	return o.Method
}

func DefaultOptions() Options {
	return Options{Samples: 10, MaxRedirects: 10, Timeout: 30 * time.Second, ConnectTimeout: 5 * time.Second, HeaderTimeout: 10 * time.Second, BodyTimeout: 15 * time.Second}
}

func (o Options) validate() error {
	if o.Samples < 1 || o.Samples > MaxSamples || o.MaxRedirects < 0 || o.MaxRedirects >= MaxHops || o.Interval < 0 || o.Interval > time.Minute || len(o.LocationLabel) > 256 {
		return errors.New("invalid recording limits")
	}
	if m := o.method(); m != "GET" && m != "POST" {
		return errors.New("method must be GET or POST")
	}
	if o.method() == "GET" && (o.Body != nil || o.ContentType != "") {
		return errors.New("a request body and content type need POST")
	}
	if len(o.Body) > MaxRequestBodyBytes || !validContentType(o.ContentType) {
		return errors.New("request body or content type out of bounds")
	}
	for _, d := range []time.Duration{o.Timeout, o.ConnectTimeout, o.HeaderTimeout, o.BodyTimeout} {
		if d <= 0 || d > time.Hour {
			return errors.New("timeouts must be positive and at most one hour")
		}
	}
	return nil
}

// Record performs GET or POST requests and streams response bodies without storing them.
// Transport failures are observations, not function errors. Invalid input returns an error.
func Record(ctx context.Context, target string, o Options) (Recording, error) {
	if err := o.validate(); err != nil {
		return Recording{}, err
	}
	safe, err := SafeURL(target)
	if err != nil || len(target) > 8192 {
		return Recording{}, errors.New("invalid HTTP target")
	}
	targetURL, _ := url.Parse(target)
	targetURL.Fragment = ""
	policy := "fresh"
	if o.Warm {
		policy = "warm"
	}
	request := Request{
		Method: o.method(), URL: safe, ConnectionPolicy: policy, SamplesRequested: o.Samples, MaxRedirects: o.MaxRedirects,
		TimeoutNS: int64(o.Timeout), ConnectTimeoutNS: int64(o.ConnectTimeout), HeaderTimeoutNS: int64(o.HeaderTimeout), BodyTimeoutNS: int64(o.BodyTimeout), IntervalNS: int64(o.Interval), LocationLabel: o.LocationLabel,
	}
	if request.Method == "POST" {
		sum := sha256.Sum256(o.Body)
		request.RequestBodyBytes, request.RequestBodySHA256, request.ContentType = int64(len(o.Body)), hex.EncodeToString(sum[:]), o.ContentType
	}
	r := Recording{SchemaVersion: wantSchema(request), ToolVersion: Version, StartedAt: time.Now().UTC(), Request: request, Samples: make([]Sample, 0, o.Samples)}
	// Warm mode shares one keep-alive transport for the whole recording, so
	// later samples can reuse connections. Each hop still records whether it did.
	var shared *http.Transport
	if o.Warm {
		shared = newTransport(o, true)
		defer shared.CloseIdleConnections()
	}
	for i := 0; i < o.Samples; i++ {
		start := time.Now()
		sampleCtx, cancel := context.WithTimeout(ctx, o.Timeout)
		sample := Sample{Index: i, Outcome: "complete", Hops: make([]Hop, 0, o.MaxRedirects+1)}
		current := targetURL
		method, body := request.Method, o.Body
		for j := 0; ; j++ {
			h, next := recordHop(sampleCtx, current, o, j == o.MaxRedirects, shared, method, body)
			if r.SchemaVersion == 2 {
				h.Method = method
			}
			sample.Hops = append(sample.Hops, h)
			if h.Outcome != "complete" {
				sample.Outcome = h.Outcome
				break
			}
			if next == nil {
				break
			}
			if s := *h.StatusCode; s == 301 || s == 302 || s == 303 {
				method, body = "GET", nil
			}
			current = next
		}
		sample.ElapsedNS = time.Since(start).Nanoseconds()
		cancel()
		r.Samples = append(r.Samples, sample)
		if ctx.Err() != nil {
			break
		}
		if i+1 < o.Samples && o.Interval > 0 {
			timer := time.NewTimer(o.Interval)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return r, r.Validate()
			}
		}
	}
	return r, r.Validate()
}

var errBodyTimeout = errors.New("body timeout")
var errEventLimit = errors.New("event limit")

type traceLog struct {
	limited      bool
	mu           sync.Mutex
	start        time.Time
	events       []Event
	frozen       bool
	cancel       context.CancelCauseFunc
	connect      map[string][]string
	successful   map[string][]string
	dns          string
	tls          string
	count        int
	winner       *string
	acquisitions int
	remote       *string
	reused       *bool
}

func (l *traceLog) addLocked(kind, attempt, outcome, address string) {
	if l.frozen {
		return
	}
	// Reserve one slot for hop_done, and keep callback memory bounded.
	if len(l.events) >= MaxEvents-1 {
		l.limited = true
		l.cancel(errEventLimit)
		return
	}
	l.events = append(l.events, Event{Kind: kind, OffsetNS: time.Since(l.start).Nanoseconds(), Attempt: attempt, Outcome: outcome, Address: address})
}
func (l *traceLog) add(kind, attempt, outcome, address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addLocked(kind, attempt, outcome, address)
}
func operationOutcome(err error) string {
	if err != nil {
		return "failed"
	}
	return "success"
}
func (l *traceLog) id(prefix string) string { l.count++; return prefix + strconv.Itoa(l.count) }
func (l *traceLog) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.frozen {
				return
			}
			if len(l.events) >= MaxEvents-1 {
				l.limited = true
				l.cancel(errEventLimit)
				return
			}
			l.dns = l.id("dns-")
			l.addLocked("dns_start", l.dns, "", "")
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.addLocked("dns_done", l.dns, operationOutcome(info.Err), "")
		},
		ConnectStart: func(network, address string) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.frozen {
				return
			}
			if len(l.events) >= MaxEvents-1 {
				l.limited = true
				l.cancel(errEventLimit)
				return
			}
			id := l.id("connect-")
			key := network + "|" + address
			if len(l.connect) < MaxEvents {
				l.connect[key] = append(l.connect[key], id)
			}
			l.addLocked("connect_start", id, "", address)
		},
		ConnectDone: func(network, address string, err error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.frozen {
				return
			}
			if len(l.events) >= MaxEvents-1 {
				l.limited = true
				l.cancel(errEventLimit)
				return
			}
			key := network + "|" + address
			id := ""
			if ids := l.connect[key]; len(ids) > 0 {
				id = ids[0]
				l.connect[key] = ids[1:]
			}
			if err == nil && len(l.successful) < MaxEvents {
				l.successful[address] = append(l.successful[address], id)
			}
			l.addLocked("connect_done", id, operationOutcome(err), address)
		},
		TLSHandshakeStart: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.frozen {
				return
			}
			if len(l.events) >= MaxEvents-1 {
				l.limited = true
				l.cancel(errEventLimit)
				return
			}
			l.tls = l.id("tls-")
			l.addLocked("tls_start", l.tls, "", "")
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.addLocked("tls_done", l.tls, operationOutcome(err), "")
		},
		GotConn: func(info httptrace.GotConnInfo) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.frozen {
				return
			}
			if len(l.events) >= MaxEvents-1 {
				l.limited = true
				l.cancel(errEventLimit)
				return
			}
			address := info.Conn.RemoteAddr().String()
			reused := info.Reused
			l.remote = &address
			l.reused = &reused
			l.acquisitions++
			if ids := l.successful[address]; l.acquisitions == 1 && len(ids) == 1 && ids[0] != "" {
				winner := ids[0]
				l.winner = &winner
			} else {
				// A second acquisition means the transport retried the request,
				// typically after a reused keep-alive connection turned out to be
				// closed. No single connection explains the hop, so keep every
				// event, report the connection that served the response and
				// claim no winning connect attempt.
				l.winner = nil
			}
			l.addLocked("connection_acquired", "", "success", address)
		},
		WroteRequest:         func(info httptrace.WroteRequestInfo) { l.add("request_written", "", operationOutcome(info.Err), "") },
		GotFirstResponseByte: func() { l.add("first_byte", "", "", "") },
	}
}

func (l *traceLog) failureCode() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A failed losing dial must not be labelled as the cause of a later response error.
	for _, kind := range []string{"request_written", "tls_done"} {
		for _, e := range l.events {
			if e.Kind == kind && e.Outcome == "failed" {
				if kind == "request_written" {
					return "request_write_failed"
				}
				return "tls_failed"
			}
		}
	}
	if l.remote == nil {
		for _, kind := range []string{"dns_done", "connect_done"} {
			for _, e := range l.events {
				if e.Kind == kind && e.Outcome == "failed" {
					if kind == "dns_done" {
						return "dns_failed"
					}
					return "connect_failed"
				}
			}
		}
	}
	return "response_failed"
}

func (l *traceLog) finish(h *Hop) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limited {
		h.ErrorCode = "event_limit"
		h.Outcome = "failed"
		h.BodyComplete = false
		for _, e := range l.events {
			if e.Kind == "body_done" {
				h.BodyComplete = true
			}
		}
	}
	elapsed := time.Since(l.start).Nanoseconds()
	l.events = append(l.events, Event{Kind: "hop_done", OffsetNS: elapsed})
	l.frozen = true
	h.ElapsedNS = elapsed
	h.Events = append([]Event(nil), l.events...)
	h.WinningConnectAttempt = l.winner
	h.RemoteAddress = l.remote
	h.ConnectionReused = l.reused
}

func newTransport(o Options, keepAlive bool) *http.Transport {
	return &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: o.ConnectTimeout}).DialContext,
		TLSHandshakeTimeout: o.ConnectTimeout, ResponseHeaderTimeout: o.HeaderTimeout,
		DisableKeepAlives: !keepAlive, DisableCompression: true, ForceAttemptHTTP2: true, MaxResponseHeaderBytes: 1 << 20,
	}
}

func recordHop(parent context.Context, target *url.URL, o Options, atLimit bool, shared *http.Transport, method string, body []byte) (h Hop, next *url.URL) {
	safe, _ := SafeURL(target.String())
	h = Hop{URL: safe, Outcome: "failed", Events: []Event{}}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	log := &traceLog{start: time.Now(), events: make([]Event, 0, 24), cancel: cancel, connect: map[string][]string{}, successful: map[string][]string{}}
	defer log.finish(&h)
	transport := shared
	if transport == nil {
		transport = newTransport(o, false)
		defer transport.CloseIdleConnections()
	}
	var reader io.Reader
	if method == "POST" {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, log.trace()), method, target.String(), reader)
	if err != nil {
		h.ErrorCode = "response_failed"
		return h, nil
	}
	req.Header.Set("User-Agent", "BitSpeed-Inspect/"+Version)
	if method == "POST" && o.ContentType != "" {
		req.Header.Set("Content-Type", o.ContentType)
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		h.ErrorCode = classifyError(ctx, err, false)
		if h.ErrorCode == "response_failed" {
			h.ErrorCode = log.failureCode()
		}
	} else {
		status, protocol := response.StatusCode, response.Proto
		h.StatusCode = &status
		h.Protocol = &protocol
		encoding := strings.ToLower(strings.TrimSpace(strings.Join(response.Header.Values("Content-Encoding"), ",")))
		if encoding == "" {
			encoding = "identity"
		} else if !validEncoding(encoding) {
			encoding = "other"
		}
		h.ContentEncoding = &encoding
		if status == http.StatusSwitchingProtocols {
			response.Body.Close()
			h.ErrorCode = "unsupported_upgrade"
			return h, nil
		}
		// Upgraded or unusual bodies may not observe request-context cancellation themselves.
		stopClose := context.AfterFunc(ctx, func() { response.Body.Close() })
		defer stopClose()
		timer := time.AfterFunc(o.BodyTimeout, func() { cancel(errBodyTimeout) })
		// A private fixed buffer avoids io.Discard's pooled-buffer behavior obscuring the bound.
		h.BodyBytes, err = io.CopyBuffer(discardWriter{}, response.Body, make([]byte, 32<<10))
		timer.Stop()
		response.Body.Close()
		if err != nil {
			h.ErrorCode = classifyError(ctx, err, true)
		} else if ctx.Err() != nil {
			h.ErrorCode = classifyError(ctx, ctx.Err(), true)
		} else {
			h.BodyComplete = true
			h.Outcome = "complete"
			log.add("body_done", "", "", "")
			if isRedirect(status) && response.Header.Get("Location") != "" {
				candidate, parseErr := target.Parse(response.Header.Get("Location"))
				if parseErr != nil {
					h.ErrorCode = "invalid_redirect"
				} else if _, safeErr := SafeURL(candidate.String()); safeErr != nil || len(candidate.String()) > 8192 {
					h.ErrorCode = "invalid_redirect"
				} else if atLimit {
					h.ErrorCode = "redirect_limit"
				} else if method == "POST" && (status == 307 || status == 308) && !sameOrigin(target, candidate) {
					// 307 and 308 would resend the body. Never send it to another origin.
					h.ErrorCode = "cross_origin_body_redirect"
				} else {
					candidate.Fragment = ""
					next = candidate
				}
				// Redirect policy errors occur after a complete response body. Preserve that evidence.
			}
		}
	}
	if h.ErrorCode != "" {
		h.Outcome = "failed"
		if h.ErrorCode == "cancelled" {
			h.Outcome = "cancelled"
		}
	}
	return h, next
}

func isRedirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

type discardWriter struct{}

func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }

func classifyError(ctx context.Context, err error, body bool) string {
	if errors.Is(context.Cause(ctx), errEventLimit) {
		return "event_limit"
	}
	if errors.Is(context.Cause(ctx), errBodyTimeout) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_failed"
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "dial" {
		return "connect_failed"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls_failed"
	}
	if body {
		return "body_failed"
	}
	return "response_failed"
}

// RecordingSucceeded reports transport completion, independently of HTTP status codes.
func RecordingSucceeded(r Recording) bool {
	if len(r.Samples) != r.Request.SamplesRequested {
		return false
	}
	for _, s := range r.Samples {
		if s.Outcome != "complete" {
			return false
		}
	}
	return true
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
