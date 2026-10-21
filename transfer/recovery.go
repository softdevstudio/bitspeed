package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var errChunkCorrupt = errors.New("stored chunk bytes do not match manifest")

func privateChunk(path string, limit int64) error {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || !privateFileOK(path, info) || info.Size() > limit {
		return ErrRecovery
	}
	return nil
}

// Keep one bounded piece of corrupt evidence per chunk. Never replace evidence.
// Sync the extra link before unlinking the published name so every crash leaves
// at least one name. Repeating recovery recognizes its own hard link by inode.
func (r *Receiver) quarantine(path string, limit int64) error {
	if e := privateChunk(path, limit); e != nil {
		return e
	}
	bad := path + ".bad"
	if _, e := os.Lstat(bad); e == nil {
		if e = privateChunk(bad, limit); e != nil {
			return e
		}
		a, ae := os.Lstat(path)
		b, be := os.Lstat(bad)
		if ae != nil || be != nil || !os.SameFile(a, b) {
			return ErrRecovery
		}
	} else if !os.IsNotExist(e) {
		return ErrStorage
	} else if e = r.ops.link(path, bad); e != nil {
		return ErrStorage
	}
	if e := syncDir(filepath.Dir(path), r.ops.snapshotOps); e != nil {
		return e
	}
	if e := os.Remove(path); e != nil {
		return ErrStorage
	}
	return syncDir(filepath.Dir(path), r.ops.snapshotOps)
}

// Admission has not been acknowledged while this private staging name exists.
// Only its exact, bounded, data-free shape may be discarded. Published bindings
// are immutable and are never repaired by guessing another manifest's owner.
func (r *Receiver) cleanAdmissions(ctx context.Context) error {
	root := filepath.Join(r.root, "sessions")
	entries, e := boundedEntries(root, MaxReceiverSessions+1)
	if e != nil {
		return ErrRecovery
	}
	for _, entry := range entries {
		if e = ctx.Err(); e != nil {
			return e
		}
		name := entry.Name()
		if !strings.HasPrefix(name, ".admitting-") {
			continue
		}
		if !hex256(strings.TrimPrefix(name, ".admitting-")) {
			return ErrRecovery
		}
		dir := filepath.Join(root, name)
		if e = privateDir(dir); e != nil {
			return ErrRecovery
		}
		children, ce := boundedEntries(dir, 3)
		if ce != nil {
			return ErrRecovery
		}
		for _, child := range children {
			path := filepath.Join(dir, child.Name())
			switch child.Name() {
			case "manifest.json", "journal.json":
				if e = privateChunk(path, MaxManifestBytes); e != nil {
					return e
				}
			case "chunks":
				if e = privateDir(path); e != nil {
					return ErrRecovery
				}
				if _, e = boundedEntries(path, 0); e != nil {
					return ErrRecovery
				}
			default:
				return ErrRecovery
			}
		}
		// Validate everything before the first removal; Remove never follows links.
		for _, child := range children {
			if e = os.Remove(filepath.Join(dir, child.Name())); e != nil {
				return ErrStorage
			}
		}
		if e = os.Remove(dir); e != nil {
			return ErrStorage
		}
	}
	return syncDir(root, r.ops.snapshotOps)
}

func probeName(name string) bool {
	suffix, ok := strings.CutPrefix(name, ".probe-")
	if !ok || len(suffix) == 0 || len(suffix) > 20 {
		return false
	}
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (r *Receiver) cleanProbes(ctx context.Context) error {
	entries, e := boundedEntries(r.root, 3+MaxReceiverSessions)
	if e != nil {
		return ErrRecovery
	}
	for _, entry := range entries {
		if e = ctx.Err(); e != nil {
			return e
		}
		if !probeName(entry.Name()) {
			continue
		}
		dir := filepath.Join(r.root, entry.Name())
		source := filepath.Join(dir, "source")
		dest := filepath.Join(r.root, "files", entry.Name())
		if e = privateDir(dir); e != nil {
			return ErrRecovery
		}
		children, ce := boundedEntries(dir, 1)
		if ce != nil {
			return ErrRecovery
		}
		if len(children) == 1 {
			if children[0].Name() != "source" {
				return ErrRecovery
			}
			data, de := readRegular(source, 5)
			if de != nil || !strings.HasPrefix("probe", string(data)) {
				return ErrRecovery
			}
		}
		if d, de := os.Lstat(dest); de == nil {
			a, ae := os.Lstat(source)
			if ae != nil || !d.Mode().IsRegular() || !os.SameFile(a, d) {
				return ErrRecovery
			}
			if e = os.Remove(dest); e != nil {
				return ErrStorage
			}
			if e = syncDir(filepath.Dir(dest), r.ops.snapshotOps); e != nil {
				return e
			}
		} else if !os.IsNotExist(de) {
			return ErrStorage
		}
		if len(children) == 1 {
			if e = os.Remove(source); e != nil {
				return ErrStorage
			}
		}
		if e = os.Remove(dir); e != nil {
			return ErrStorage
		}
	}
	if e = syncDir(filepath.Join(r.root, "files"), r.ops.snapshotOps); e != nil {
		return e
	}
	return syncDir(r.root, r.ops.snapshotOps)
}
