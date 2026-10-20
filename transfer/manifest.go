// Package transfer defines the independent single-file transfer contract.
// Network operations and persistent storage are not implemented yet.
package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const (
	ProtocolVersion        = 1
	MinChunkSize     int64 = 64 << 10
	DefaultChunkSize int64 = 4 << 20
	MaxChunkSize     int64 = 16 << 20
	MaxFileSize      int64 = 64 << 30
	MaxChunks              = 16384
	MaxManifestBytes       = 2 << 20
)

// Manifest has no sender-controlled filesystem name. Field order is part of
// the canonical encoding contract; all fields, including an empty array, exist.
type Manifest struct {
	ProtocolVersion int      `json:"protocol_version"`
	TransferID      string   `json:"transfer_id"`
	FileSize        int64    `json:"file_size"`
	ChunkSize       int64    `json:"chunk_size"`
	ChunkSHA256     []string `json:"chunk_sha256"`
	FileSHA256      string   `json:"file_sha256"`
}

// Validate checks shape and arithmetic, not that any source bytes match hashes.
func (m Manifest) Validate() error {
	if m.ProtocolVersion != ProtocolVersion {
		return errors.New("unsupported transfer protocol")
	}
	if !hex256(m.TransferID) {
		return errors.New("invalid transfer ID")
	}
	if m.FileSize < 0 || m.FileSize > MaxFileSize {
		return errors.New("file size outside protocol limits")
	}
	if m.ChunkSize < MinChunkSize || m.ChunkSize > MaxChunkSize {
		return errors.New("chunk size outside protocol limits")
	}
	count := m.FileSize / m.ChunkSize
	if m.FileSize%m.ChunkSize != 0 {
		count++
	}
	if count > MaxChunks || m.ChunkSHA256 == nil || int64(len(m.ChunkSHA256)) != count {
		return errors.New("invalid chunk hash count")
	}
	if !hex256(m.FileSHA256) {
		return errors.New("invalid file digest")
	}
	for _, digest := range m.ChunkSHA256 {
		if !hex256(digest) {
			return errors.New("invalid chunk digest")
		}
	}
	if m.FileSize == 0 {
		empty := sha256.Sum256(nil)
		if m.FileSHA256 != hex.EncodeToString(empty[:]) {
			return errors.New("invalid empty file digest")
		}
	}
	return nil
}

func hex256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// CanonicalJSON validates and encodes fixed-order fields without whitespace or
// a trailing newline. It does not parse untrusted JSON or read any file.
func (m Manifest) CanonicalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// Identity binds a transfer ID to its complete immutable manifest.
func (m Manifest) Identity() (string, error) {
	data, err := m.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ChunkLength returns the exact expected length, including the final partial chunk.
func (m Manifest) ChunkLength(index int64) (int64, error) {
	if err := m.Validate(); err != nil {
		return 0, err
	}
	if index < 0 || index >= int64(len(m.ChunkSHA256)) {
		return 0, errors.New("chunk index outside manifest")
	}
	remaining := m.FileSize - index*m.ChunkSize
	if remaining < m.ChunkSize {
		return remaining, nil
	}
	return m.ChunkSize, nil
}

type State string

const (
	Preparing  State = "preparing"
	Uploading  State = "uploading"
	Finalizing State = "finalizing"
	Completed  State = "completed"
	Failed     State = "failed"
)
