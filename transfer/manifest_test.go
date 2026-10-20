package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func manifest(size int64) Manifest {
	m := Manifest{ProtocolVersion: 1, TransferID: strings.Repeat("a", 64), FileSize: size, ChunkSize: DefaultChunkSize, FileSHA256: strings.Repeat("b", 64), ChunkSHA256: []string{}}
	count := size / m.ChunkSize
	if size%m.ChunkSize != 0 {
		count++
	}
	for i := int64(0); i < count; i++ {
		m.ChunkSHA256 = append(m.ChunkSHA256, strings.Repeat("c", 64))
	}
	if size == 0 {
		sum := sha256.Sum256(nil)
		m.FileSHA256 = hex.EncodeToString(sum[:])
	}
	return m
}

func TestManifestCanonicalEmpty(t *testing.T) {
	m := manifest(0)
	data, err := m.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":1,"transfer_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","file_size":0,"chunk_size":4194304,"chunk_sha256":[],"file_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`
	if string(data) != want {
		t.Fatalf("canonical bytes: %s", data)
	}
	id, err := m.Identity()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(want))
	if id != hex.EncodeToString(sum[:]) {
		t.Fatal("identity mismatch")
	}
	if _, err := m.ChunkLength(0); err == nil {
		t.Fatal("empty file has no chunk")
	}
}

func TestManifestShapeFailures(t *testing.T) {
	tests := map[string]func(*Manifest){
		"version":         func(m *Manifest) { m.ProtocolVersion = 2 },
		"ID path":         func(m *Manifest) { m.TransferID = "../outside" },
		"ID uppercase":    func(m *Manifest) { m.TransferID = strings.Repeat("A", 64) },
		"negative size":   func(m *Manifest) { m.FileSize = -1 },
		"over size":       func(m *Manifest) { m.FileSize = MaxFileSize + 1 },
		"zero chunk":      func(m *Manifest) { m.ChunkSize = 0 },
		"small chunk":     func(m *Manifest) { m.ChunkSize = MinChunkSize - 1 },
		"large chunk":     func(m *Manifest) { m.ChunkSize = MaxChunkSize + 1 },
		"null hashes":     func(m *Manifest) { m.ChunkSHA256 = nil },
		"missing hash":    func(m *Manifest) { m.ChunkSHA256 = []string{} },
		"extra hash":      func(m *Manifest) { m.ChunkSHA256 = append(m.ChunkSHA256, strings.Repeat("c", 64)) },
		"bad chunk hash":  func(m *Manifest) { m.ChunkSHA256[0] = strings.Repeat("z", 64) },
		"bad file hash":   func(m *Manifest) { m.FileSHA256 = strings.Repeat("f", 63) },
		"too many chunks": func(m *Manifest) { m.ChunkSize = MinChunkSize; m.FileSize = MinChunkSize * (MaxChunks + 1) },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			m := manifest(1)
			change(&m)
			if m.Validate() == nil {
				t.Fatal("accepted invalid manifest")
			}
			if _, e := m.CanonicalJSON(); e == nil {
				t.Fatal("encoded invalid manifest")
			}
			if _, e := m.Identity(); e == nil {
				t.Fatal("hashed invalid manifest")
			}
			if _, e := m.ChunkLength(0); e == nil {
				t.Fatal("calculated invalid manifest")
			}
		})
	}
	empty := manifest(0)
	empty.FileSHA256 = strings.Repeat("b", 64)
	if empty.Validate() == nil {
		t.Fatal("accepted wrong empty digest")
	}
}

func TestChunkLengthsAndLimits(t *testing.T) {
	for _, size := range []int64{1, DefaultChunkSize - 1, DefaultChunkSize, DefaultChunkSize + 1, 2 * DefaultChunkSize, MaxFileSize} {
		m := manifest(size)
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		var total int64
		indices := []int64{0}
		if len(m.ChunkSHA256) > 1 {
			indices = append(indices, int64(len(m.ChunkSHA256)-1))
		}
		for _, i := range indices {
			n, err := m.ChunkLength(i)
			if err != nil || n <= 0 || n > m.ChunkSize {
				t.Fatal(n, err)
			}
			if i == int64(len(m.ChunkSHA256)-1) {
				total = i*m.ChunkSize + n
			}
		}
		if total != size {
			t.Fatal("chunk lengths do not cover file")
		}
		for _, index := range []int64{-1, int64(len(m.ChunkSHA256)), 1 << 62} {
			if _, e := m.ChunkLength(index); e == nil {
				t.Fatal("accepted invalid index")
			}
		}
		data, err := m.CanonicalJSON()
		if err != nil || len(data) > MaxManifestBytes || !json.Valid(data) {
			t.Fatal("canonical manifest bound", err)
		}
	}
}

func TestManifestIdentityBindsFields(t *testing.T) {
	base := manifest(1)
	want, _ := base.Identity()
	for _, change := range []func(*Manifest){func(m *Manifest) { m.TransferID = strings.Repeat("d", 64) }, func(m *Manifest) { m.FileSize = 2 }, func(m *Manifest) { m.ChunkSize++ }, func(m *Manifest) { m.ChunkSHA256[0] = strings.Repeat("d", 64) }, func(m *Manifest) { m.FileSHA256 = strings.Repeat("d", 64) }} {
		m := manifest(1)
		change(&m)
		got, err := m.Identity()
		if err != nil || got == want {
			t.Fatal("identity did not bind field", err)
		}
	}
	if base.ChunkSHA256[0] != strings.Repeat("c", 64) {
		t.Fatal("mutated input")
	}
}
