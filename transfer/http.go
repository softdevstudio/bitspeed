package transfer

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

func literalLoopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	auth := req.Header.Values("Authorization")
	candidate := ""
	if len(auth) == 1 && strings.HasPrefix(auth[0], "Bearer ") {
		candidate = strings.TrimPrefix(auth[0], "Bearer ")
	}
	if len(candidate) != 64 || subtle.ConstantTimeCompare([]byte(candidate), []byte(r.token)) != 1 {
		replyError(w, failure(401, "unauthorized", false))
		return
	}
	host, _, e := net.SplitHostPort(req.RemoteAddr)
	if req.TLS == nil && (e != nil || !literalLoopback(host)) {
		replyError(w, failure(400, "invalid_request", false))
		return
	}
	if req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.RawPath != "" || strings.Contains(req.RequestURI, "?") {
		replyError(w, failure(400, "invalid_request", false))
		return
	}
	if !r.mu.TryLock() {
		replyError(w, failure(409, "session_busy", true))
		return
	}
	defer r.mu.Unlock()
	if r.closed {
		replyError(w, ErrStorage)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Minute)
	defer cancel()
	if len(req.Header.Values("Content-Encoding")) != 0 {
		replyError(w, failure(415, "unsupported_encoding", false))
		return
	}
	path := req.URL.Path
	if path == "/v1/transfers" {
		if req.Method != http.MethodPost {
			replyError(w, failure(405, "method_not_allowed", false))
			return
		}
		if !contentType(req, "application/json") {
			replyError(w, failure(415, "unsupported_media_type", false))
			return
		}
		if req.ContentLength > MaxManifestBytes {
			replyError(w, failure(413, "limit_exceeded", false))
			return
		}
		data, e := io.ReadAll(io.LimitReader(req.Body, MaxManifestBytes+1))
		if e != nil {
			replyError(w, failure(400, "invalid_request", false))
			return
		}
		if len(data) > MaxManifestBytes {
			replyError(w, failure(413, "limit_exceeded", false))
			return
		}
		m, e := DecodeManifest(data)
		if e != nil {
			replyError(w, failure(400, "invalid_manifest", false))
			return
		}
		s, created, e := r.admit(ctx, m)
		if e != nil {
			replyError(w, e)
			return
		}
		status := 200
		if created {
			status = 201
		}
		replyJSON(w, status, s)
		return
	}
	id, tail := safeIDPath(path)
	if id == "" || strings.HasSuffix(path, "/") {
		replyError(w, failure(400, "invalid_request", false))
		return
	}
	isStatus := tail == ""
	isChunk := strings.HasPrefix(tail, "chunks/") && !strings.Contains(strings.TrimPrefix(tail, "chunks/"), "/")
	if !isStatus && !isChunk {
		replyError(w, failure(400, "invalid_request", false))
		return
	}
	if isStatus && req.Method != http.MethodGet || isChunk && req.Method != http.MethodPut {
		replyError(w, failure(405, "method_not_allowed", false))
		return
	}
	if isStatus && (req.ContentLength != 0 || len(req.TransferEncoding) != 0) {
		replyError(w, failure(400, "invalid_request", false))
		return
	}

	if _, e := r.sessionExists(id); e != nil {
		replyError(w, e)
		return
	}
	m, e := r.load(id)
	if e != nil {
		replyError(w, e)
		return
	}
	if isStatus {
		s, e := r.status(ctx, m)
		if e != nil {
			replyError(w, e)
			return
		}
		replyJSON(w, 200, s)
		return
	}
	if !contentType(req, "application/octet-stream") {
		replyError(w, failure(415, "unsupported_media_type", false))
		return
	}
	if req.ContentLength < 0 || len(req.TransferEncoding) != 0 {
		replyError(w, failure(411, "length_required", false))
		return
	}
	if req.ContentLength > MaxChunkSize {
		replyError(w, failure(413, "limit_exceeded", false))
		return
	}
	index, ok := validIndex(strings.TrimPrefix(tail, "chunks/"), len(m.ChunkSHA256))
	if !ok {
		replyError(w, failure(400, "invalid_index", false))
		return
	}
	if e = r.syncSession(ctx, m); e != nil {
		replyError(w, e)
		return
	}
	ack, e := r.store(ctx, m, index, req.Body, req.ContentLength)
	if e != nil {
		replyError(w, e)
		return
	}
	replyJSON(w, 200, ack)
}
func contentType(r *http.Request, want string) bool {
	values := r.Header.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	media, params, e := mime.ParseMediaType(values[0])
	return e == nil && media == want && (len(params) == 0 || len(params) == 1 && strings.EqualFold(params["charset"], "utf-8") && want == "application/json")
}
func replyJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func replyError(w http.ResponseWriter, err error) {
	p := receiverError(err)
	replyJSON(w, p.status, struct {
		Version int    `json:"protocol_version"`
		Code    string `json:"error_code"`
		Retry   bool   `json:"retryable"`
	}{1, p.code, p.retry})
}
