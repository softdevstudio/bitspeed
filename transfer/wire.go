package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

var ErrProtocol = errors.New("invalid transfer protocol data")

// exactObject preserves duplicate detection and exact field spelling. Bodies
// are bounded before this function; nested values still need typed validation.
func exactObject(data []byte, names ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return nil, ErrProtocol
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		key, e := d.Token()
		name, ok := key.(string)
		if e != nil || !ok || !allowed[name] || out[name] != nil {
			return nil, ErrProtocol
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return nil, ErrProtocol
		}
		out[name] = raw
	}
	if token, e = d.Token(); e != nil || token != json.Delim('}') || len(out) != len(names) {
		return nil, ErrProtocol
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, ErrProtocol
	}
	return out, nil
}
func wireInt(raw []byte) (int64, error) {
	if len(raw) == 0 {
		return 0, ErrProtocol
	}
	for i, b := range raw {
		if b < '0' || b > '9' || i == 0 && b == '0' && len(raw) > 1 {
			return 0, ErrProtocol
		}
	}
	n, e := strconv.ParseInt(string(raw), 10, 64)
	if e != nil {
		return 0, ErrProtocol
	}
	return n, nil
}
func wireString(raw []byte) (string, error) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", ErrProtocol
	}
	return s, nil
}

// DecodeManifest is the bounded HTTP decoder. Local spool manifests remain
// stricter: reuse also requires exact canonical bytes.
func DecodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if len(data) > MaxManifestBytes {
		return m, ErrProtocol
	}
	fields, e := exactObject(data, "protocol_version", "transfer_id", "file_size", "chunk_size", "chunk_sha256", "file_sha256")
	if e != nil {
		return m, e
	}
	n, e := wireInt(fields["protocol_version"])
	if e != nil || n != 1 {
		return m, ErrProtocol
	}
	m.ProtocolVersion = 1
	if m.TransferID, e = wireString(fields["transfer_id"]); e != nil {
		return m, e
	}
	if m.FileSize, e = wireInt(fields["file_size"]); e != nil {
		return m, e
	}
	if m.ChunkSize, e = wireInt(fields["chunk_size"]); e != nil {
		return m, e
	}
	if m.FileSHA256, e = wireString(fields["file_sha256"]); e != nil {
		return m, e
	}
	d := json.NewDecoder(bytes.NewReader(fields["chunk_sha256"]))
	token, e := d.Token()
	if e != nil || token != json.Delim('[') {
		return m, ErrProtocol
	}
	m.ChunkSHA256 = []string{}
	for d.More() {
		if len(m.ChunkSHA256) >= MaxChunks {
			return m, ErrProtocol
		}
		token, e = d.Token()
		s, ok := token.(string)
		if e != nil || !ok || !hex256(s) {
			return m, ErrProtocol
		}
		m.ChunkSHA256 = append(m.ChunkSHA256, s)
	}
	if token, e = d.Token(); e != nil || token != json.Delim(']') {
		return m, ErrProtocol
	}
	if m.Validate() != nil {
		return m, ErrProtocol
	}
	return m, nil
}

type StoredStatus struct {
	ProtocolVersion int     `json:"protocol_version"`
	TransferID      string  `json:"transfer_id"`
	ManifestSHA256  string  `json:"manifest_sha256"`
	State           State   `json:"state"`
	VerifiedChunks  int     `json:"verified_chunks"`
	VerifiedBytes   int64   `json:"verified_bytes"`
	MissingChunks   []int   `json:"missing_chunks"`
	Receipt         any     `json:"receipt"`
	ErrorCode       *string `json:"error_code"`
}
type chunkAck struct {
	ProtocolVersion int    `json:"protocol_version"`
	TransferID      string `json:"transfer_id"`
	ManifestSHA256  string `json:"manifest_sha256"`
	ChunkIndex      int    `json:"chunk_index"`
	ChunkSHA256     string `json:"chunk_sha256"`
	Stored          bool   `json:"stored"`
}
type journal struct {
	ProtocolVersion int     `json:"protocol_version"`
	ManifestSHA256  string  `json:"manifest_sha256"`
	State           State   `json:"state"`
	ErrorCode       *string `json:"error_code"`
}

type protocolError struct {
	status int
	code   string
	retry  bool
}

func (e *protocolError) Error() string                  { return e.code }
func failure(status int, code string, retry bool) error { return &protocolError{status, code, retry} }
