package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
)

func decodeReceipt(data []byte) (Receipt, error) {
	var r Receipt
	fields, e := exactObject(data, "transfer_id", "manifest_sha256", "file_size", "file_sha256", "output_name")
	if e != nil {
		return r, e
	}
	if r.TransferID, e = wireString(fields["transfer_id"]); e != nil || !hex256(r.TransferID) {
		return r, ErrProtocol
	}
	if r.ManifestSHA256, e = wireString(fields["manifest_sha256"]); e != nil || !hex256(r.ManifestSHA256) {
		return r, ErrProtocol
	}
	if r.FileSHA256, e = wireString(fields["file_sha256"]); e != nil || !hex256(r.FileSHA256) {
		return r, ErrProtocol
	}
	if r.FileSize, e = wireInt(fields["file_size"]); e != nil || r.FileSize > MaxFileSize {
		return r, ErrProtocol
	}
	if r.OutputName, e = wireString(fields["output_name"]); e != nil || r.OutputName != r.TransferID+".data" {
		return r, ErrProtocol
	}
	return r, nil
}

// Send returns only after a verified completed receipt is durably cached locally.
// It retains the snapshot and never automatically deletes receiver evidence.
func Send(ctx context.Context, base, token, spool, id string) (StoredStatus, error) {
	return SendWithProgress(ctx, base, token, spool, id, nil)
}

// SendWithProgress is Send with an optional progress callback; see SendProgress.
func SendWithProgress(ctx context.Context, base, token, spool, id string, progress func(SendProgress) error) (StoredStatus, error) {
	s, e := UploadOnlyWithProgress(ctx, base, token, spool, id, progress)
	if e != nil {
		return s, e
	}
	m, e := Reuse(ctx, spool, id)
	if e != nil {
		return StoredStatus{}, e
	}
	if s.State != Completed {
		if progress != nil {
			if e = progress(SendProgress{Stage: "finalize", Chunks: len(m.ChunkSHA256), VerifiedChunks: len(m.ChunkSHA256)}); e != nil {
				return StoredStatus{}, e
			}
		}
		identity, _ := m.Identity()
		body, _ := json.Marshal(struct {
			Identity string `json:"manifest_sha256"`
		}{identity})
		normalized, e := endpoint(base)
		if e != nil {
			return StoredStatus{}, e
		}
		client := transferHTTPClient()
		defer client.CloseIdleConnections()
		data, code, e := request(ctx, client, "POST", normalized+"/v1/transfers/"+id+"/finalize", token, "application/json", bytes.NewReader(body), int64(len(body)))
		if e != nil {
			return StoredStatus{}, e
		}
		if code != 200 {
			return StoredStatus{}, ErrProtocol
		}
		s, e = decodeStatus(data, id)
		if e != nil {
			return StoredStatus{}, e
		}
	}
	if s.State != Completed || !statusMatches(s, m) || s.Receipt == nil {
		return StoredStatus{}, ErrProtocol
	}
	if e = cacheReceipt(ctx, spool, id, s.Receipt, realReceiverOps()); e != nil {
		return StoredStatus{}, e
	}
	return s, nil
}

func cacheReceipt(ctx context.Context, spool, id string, receipt *Receipt, ops receiverOps) error {
	spool = filepath.Clean(spool)
	unlock, e := openSpool(spool, ops.snapshotOps)
	if e != nil {
		return e
	}
	defer unlock()
	if e = ctx.Err(); e != nil {
		return e
	}
	r := Receiver{ops: ops}
	if e = r.metadata(filepath.Join(spool, id, "receipt.json"), receipt); e != nil {
		return e
	}
	if e = syncDir(spool, ops.snapshotOps); e != nil {
		return e
	}
	return ctx.Err()
}
