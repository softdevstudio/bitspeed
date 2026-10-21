package transfer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
)

// CleanResult reports offline chunk cleanup for one receiver session.
type CleanResult struct {
	TransferID     string `json:"transfer_id"`
	State          State  `json:"state"`
	Cleaned        bool   `json:"cleaned"`
	ChunksRemoved  int    `json:"chunks_removed"`
	BytesFreed     int64  `json:"bytes_freed"`
	EvidenceKept   int    `json:"evidence_kept"`
	SkippedBecause string `json:"skipped_because,omitempty"`
}

// CleanReceiver removes stored chunk files from completed sessions while the
// receiver is stopped. It holds the root lock and runs the full startup
// recovery first, so a completed session has just had its destination bytes,
// witness link and receipt verified. Completed status already tolerates
// missing chunks, so a crash part way through leaves a valid root.
// Everything else stays: manifest, journal, receipt, the ready witness,
// the destination, corrupt-chunk evidence and every session that is not
// completed. Reservation accounting is unchanged.
func CleanReceiver(ctx context.Context, root string, dryRun bool) ([]CleanResult, error) {
	return cleanReceiver(ctx, root, dryRun, realReceiverOps())
}

func cleanReceiver(ctx context.Context, root string, dryRun bool, ops receiverOps) ([]CleanResult, error) {
	if root == "" {
		return nil, ErrConfig
	}
	if info, e := os.Lstat(root); e != nil || !info.IsDir() {
		return nil, ErrConfig
	}
	r, e := openReceiver(ctx, ReceiverConfig{Root: root, MaxSessions: MaxReceiverSessions, MaxReservedBytes: MaxReservedBytes}, ops)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	entries, e := boundedEntries(filepath.Join(r.root, "sessions"), MaxReceiverSessions)
	if e != nil {
		return nil, ErrRecovery
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	results := make([]CleanResult, 0, len(names))
	for _, id := range names {
		if e = ctx.Err(); e != nil {
			return results, e
		}
		m, e := r.load(id)
		if e != nil {
			return results, e
		}
		j, e := r.readJournal(m)
		if e != nil {
			return results, e
		}
		result := CleanResult{TransferID: id, State: j.State}
		if j.State != Completed {
			result.SkippedBecause = "session is not completed"
			results = append(results, result)
			continue
		}
		if e = r.cleanChunks(m, dryRun, &result); e != nil {
			return results, e
		}
		results = append(results, result)
	}
	return results, nil
}

func (r *Receiver) cleanChunks(m Manifest, dryRun bool, result *CleanResult) error {
	chunks := filepath.Join(r.sessionDir(m), "chunks")
	entries, e := boundedEntries(chunks, 2*MaxChunks+1)
	if e != nil {
		return ErrRecovery
	}
	count := len(m.ChunkSHA256)
	for _, entry := range entries {
		_, ok := validIndex(entry.Name(), count)
		if !ok {
			result.EvidenceKept++
			continue
		}
		path := filepath.Join(chunks, entry.Name())
		info, e := os.Lstat(path)
		if e != nil {
			return ErrStorage
		}
		if !info.Mode().IsRegular() {
			return ErrRecovery
		}
		if !dryRun {
			if e = os.Remove(path); e != nil {
				return ErrStorage
			}
		}
		result.ChunksRemoved++
		result.BytesFreed += info.Size()
	}
	if !dryRun && result.ChunksRemoved > 0 {
		if e = syncDir(chunks, r.ops.snapshotOps); e != nil {
			return e
		}
	}
	result.Cleaned = !dryRun
	return nil
}
