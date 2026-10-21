package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ErrRequest = errors.New("receiver request failed")

func LoadToken(path string) (string, error) {
	if !spoolSupported() {
		return "", ErrUnsupported
	}
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65 {
		return "", ErrConfig
	}
	f, e := openSource(path)
	if e != nil {
		return "", ErrConfig
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !sameSource(info, opened) {
		return "", ErrConfig
	}
	data, e := io.ReadAll(io.LimitReader(f, 66))
	ce := f.Close()
	if e != nil || ce != nil {
		return "", ErrConfig
	}
	token := strings.TrimSuffix(string(data), "\n")
	if !hex256(token) {
		return "", ErrConfig
	}
	return token, nil
}
func endpoint(base string) (string, error) {
	u, e := url.Parse(base)
	if e != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.Host == "" || u.RawPath != "" || u.Path != "" && u.Path != "/" {
		return "", ErrConfig
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !literalLoopback(u.Hostname())) {
		return "", ErrConfig
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}
func transferHTTPClient() *http.Client {
	return &http.Client{Timeout: 6 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRequest }, Transport: &http.Transport{Proxy: nil, DisableCompression: true, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 << 10}}
}
func request(ctx context.Context, client *http.Client, method, target, token, kind string, body io.Reader, length int64) ([]byte, int, error) {
	req, e := http.NewRequestWithContext(ctx, method, target, body)
	if e != nil {
		return nil, 0, ErrConfig
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	req.ContentLength = length
	resp, e := client.Do(req)
	if e != nil {
		return nil, 0, ErrRequest
	}
	defer resp.Body.Close()
	limit := int64(MaxManifestBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limit = 4096
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	ce := resp.Body.Close()
	media, params, me := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e != nil || ce != nil || int64(len(data)) > limit || me != nil || media != "application/json" || len(params) > 1 || len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8") || len(resp.Header.Values("Content-Encoding")) != 0 {
		return nil, 0, ErrProtocol
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fields, e := exactObject(data, "protocol_version", "error_code", "retryable")
		if e != nil {
			return nil, resp.StatusCode, ErrProtocol
		}
		v, e := wireInt(fields["protocol_version"])
		if e != nil || v != 1 {
			return nil, resp.StatusCode, ErrProtocol
		}
		code, e := wireString(fields["error_code"])
		if e != nil {
			return nil, resp.StatusCode, ErrProtocol
		}
		retry := string(fields["retryable"])
		if retry != "true" && retry != "false" {
			return nil, resp.StatusCode, ErrProtocol
		}
		known := map[string]bool{"invalid_request": true, "invalid_manifest": true, "invalid_index": true, "unauthorized": true, "unknown_transfer": true, "method_not_allowed": true, "manifest_conflict": true, "session_busy": true, "invalid_state": true, "length_required": true, "limit_exceeded": true, "unsupported_media_type": true, "unsupported_encoding": true, "chunk_length_mismatch": true, "chunk_digest_mismatch": true, "storage_unavailable": true, "capacity_exhausted": true, "recovery_required": true, "chunks_missing": true, "destination_conflict": true, "transfer_failed": true, "final_digest_mismatch": true}
		if !known[code] {
			return nil, resp.StatusCode, ErrProtocol
		}
		return nil, resp.StatusCode, errors.New("receiver: " + code)
	}
	return data, resp.StatusCode, nil
}
func decodeStatus(data []byte, id string) (StoredStatus, error) {
	var s StoredStatus
	fields, e := exactObject(data, "protocol_version", "transfer_id", "manifest_sha256", "state", "verified_chunks", "verified_bytes", "missing_chunks", "receipt", "error_code")
	if e != nil {
		return s, e
	}
	v, e := wireInt(fields["protocol_version"])
	if e != nil || v != 1 {
		return s, ErrProtocol
	}
	if s.TransferID, e = wireString(fields["transfer_id"]); e != nil || s.TransferID != id {
		return s, ErrProtocol
	}
	if s.ManifestSHA256, e = wireString(fields["manifest_sha256"]); e != nil || !hex256(s.ManifestSHA256) {
		return s, ErrProtocol
	}
	state, e := wireString(fields["state"])
	if e != nil || state != string(Uploading) && state != string(Completed) && state != string(Failed) {
		return s, ErrProtocol
	}
	count, e := wireInt(fields["verified_chunks"])
	if e != nil || count > MaxChunks {
		return s, ErrProtocol
	}
	s.VerifiedChunks = int(count)
	if s.VerifiedBytes, e = wireInt(fields["verified_bytes"]); e != nil || s.VerifiedBytes > MaxFileSize {
		return s, ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(fields["missing_chunks"]))
	token, e := d.Token()
	if e != nil || token != json.Delim('[') {
		return s, ErrProtocol
	}
	s.MissingChunks = []int{}
	for d.More() {
		if len(s.MissingChunks) >= MaxChunks {
			return s, ErrProtocol
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return s, ErrProtocol
		}
		i, e := wireInt(raw)
		if e != nil || i >= MaxChunks || len(s.MissingChunks) > 0 && int(i) <= s.MissingChunks[len(s.MissingChunks)-1] {
			return s, ErrProtocol
		}
		s.MissingChunks = append(s.MissingChunks, int(i))
	}
	if _, e = d.Token(); e != nil || s.VerifiedChunks+len(s.MissingChunks) > MaxChunks {
		return s, ErrProtocol
	}
	s.ProtocolVersion = 1
	s.State = State(state)
	if s.State == Completed {
		receipt, e := decodeReceipt(fields["receipt"])
		if e != nil || receipt.TransferID != id || receipt.ManifestSHA256 != s.ManifestSHA256 || string(fields["error_code"]) != "null" {
			return s, ErrProtocol
		}
		s.Receipt = &receipt
	} else if s.State == Failed {
		code, e := wireString(fields["error_code"])
		if e != nil || !terminalCode(code) || string(fields["receipt"]) != "null" {
			return s, ErrProtocol
		}
		s.ErrorCode = &code
	} else if string(fields["receipt"]) != "null" || string(fields["error_code"]) != "null" {
		return s, ErrProtocol
	}
	return s, nil
}
func statusMatches(s StoredStatus, m Manifest) bool {
	identity, _ := m.Identity()
	if s.State == Completed && (s.Receipt == nil || *s.Receipt != expectedReceipt(m)) {
		return false
	}
	if s.ManifestSHA256 != identity || s.TransferID != m.TransferID || s.VerifiedChunks+len(s.MissingChunks) != len(m.ChunkSHA256) {
		return false
	}
	missingBytes := int64(0)
	for _, i := range s.MissingChunks {
		if i >= len(m.ChunkSHA256) {
			return false
		}
		missingBytes += chunkSizeAt(m, i)
	}
	return s.VerifiedBytes == m.FileSize-missingBytes
}

// QueryStatus returns authenticated receiver observations and validates receipt shape.
func QueryStatus(ctx context.Context, base, token, id string) (StoredStatus, error) {
	var empty StoredStatus
	base, e := endpoint(base)
	if e != nil || !hex256(token) || !hex256(id) {
		return empty, ErrConfig
	}
	client := transferHTTPClient()
	defer client.CloseIdleConnections()
	data, code, e := request(ctx, client, "GET", base+"/v1/transfers/"+id, token, "", nil, 0)
	if e != nil {
		return empty, e
	}
	if code != 200 {
		return empty, ErrProtocol
	}
	return decodeStatus(data, id)
}

// SendProgress describes sender progress. Stage is "resume" once the receiver
// has reported which chunks it can prove, "chunk" after each chunk the receiver
// acknowledges as stored, and "finalize" before asking it to publish the file.
// Counts and byte totals come from the manifest and receiver acknowledgements;
// they never include a chunk the receiver has not confirmed.
type SendProgress struct {
	Stage          string `json:"stage"`
	Chunks         int    `json:"chunks"`
	VerifiedChunks int    `json:"verified_chunks"`
	ToSend         int    `json:"to_send"`
	Sent           int    `json:"sent"`
	ChunkIndex     int    `json:"chunk_index"`
	BytesSent      int64  `json:"bytes_sent"`
	BytesToSend    int64  `json:"bytes_to_send"`
}

// UploadOnly stages verified chunks. It never finalizes or claims delivery.
func UploadOnly(ctx context.Context, base, token, spool, id string) (StoredStatus, error) {
	return UploadOnlyWithProgress(ctx, base, token, spool, id, nil)
}

// UploadOnlyWithProgress is UploadOnly with an optional progress callback.
// A callback error stops the upload; already stored chunks stay verified.
func UploadOnlyWithProgress(ctx context.Context, base, token, spool, id string, progress func(SendProgress) error) (StoredStatus, error) {
	var empty StoredStatus
	report := func(p SendProgress) error {
		if progress == nil {
			return nil
		}
		return progress(p)
	}
	base, e := endpoint(base)
	if e != nil || !hex256(token) || !hex256(id) {
		return empty, ErrConfig
	}
	m, e := Reuse(ctx, spool, id)
	if e != nil {
		return empty, e
	}
	spool = filepath.Clean(spool)
	unlock, e := openSpool(spool, realSnapshotOps())
	if e != nil {
		return empty, e
	}
	defer unlock()
	path := filepath.Join(spool, id, "snapshot")
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() != m.FileSize {
		return empty, ErrSnapshot
	}
	f, e := os.Open(path)
	if e != nil {
		return empty, ErrSnapshot
	}
	defer f.Close()
	client := transferHTTPClient()
	defer client.CloseIdleConnections()
	manifest, _ := m.CanonicalJSON()
	data, code, e := request(ctx, client, "POST", base+"/v1/transfers", token, "application/json", bytes.NewReader(manifest), int64(len(manifest)))
	if e != nil {
		return empty, e
	}
	if code != 200 && code != 201 {
		return empty, ErrProtocol
	}
	s, e := decodeStatus(data, id)
	if e != nil || !statusMatches(s, m) || s.State == Failed {
		return empty, ErrProtocol
	}
	if s.State == Completed {
		if e = f.Close(); e != nil {
			return empty, ErrStorage
		}
		return s, nil
	}
	p := SendProgress{Stage: "resume", Chunks: len(m.ChunkSHA256), VerifiedChunks: s.VerifiedChunks, ToSend: len(s.MissingChunks)}
	for _, index := range s.MissingChunks {
		p.BytesToSend += chunkSizeAt(m, index)
	}
	if e = report(p); e != nil {
		return empty, e
	}
	for _, index := range s.MissingChunks {
		length := chunkSizeAt(m, index)
		body := io.NewSectionReader(f, int64(index)*m.ChunkSize, length)
		data, code, e = request(ctx, client, "PUT", base+"/v1/transfers/"+id+"/chunks/"+strconv.Itoa(index), token, "application/octet-stream", body, length)
		if e != nil {
			return empty, e
		}
		if code != 200 {
			return empty, ErrProtocol
		}
		fields, e := exactObject(data, "protocol_version", "transfer_id", "manifest_sha256", "chunk_index", "chunk_sha256", "stored")
		if e != nil {
			return empty, e
		}
		v, ve := wireInt(fields["protocol_version"])
		i, ie := wireInt(fields["chunk_index"])
		tid, te := wireString(fields["transfer_id"])
		mid, me := wireString(fields["manifest_sha256"])
		digest, de := wireString(fields["chunk_sha256"])
		identity, _ := m.Identity()
		if ve != nil || v != 1 || ie != nil || int64(index) != i || te != nil || tid != id || me != nil || mid != identity || de != nil || digest != m.ChunkSHA256[index] || string(fields["stored"]) != "true" {
			return empty, ErrProtocol
		}
		p.Stage, p.Sent, p.ChunkIndex = "chunk", p.Sent+1, index
		p.BytesSent += length
		if e = report(p); e != nil {
			return empty, e
		}
	}
	data, code, e = request(ctx, client, "GET", base+"/v1/transfers/"+id, token, "", nil, 0)
	if e != nil {
		return empty, e
	}
	if code != 200 {
		return empty, ErrProtocol
	}
	s, e = decodeStatus(data, id)
	if e != nil || !statusMatches(s, m) || s.State == Failed || s.State != Completed && len(s.MissingChunks) != 0 {
		return empty, ErrProtocol
	}
	if e = f.Close(); e != nil {
		return empty, ErrStorage
	}
	return s, nil
}
