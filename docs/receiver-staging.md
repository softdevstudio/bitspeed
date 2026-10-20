# Development chunk staging

Transfer remains 0.0.0-dev and unreleased. This session adds authenticated manifest admission, sequential chunk upload and status. It does not assemble a destination, issue completion receipts or implement finalize. The only successful staging result is uploaded true and transferred false; receiver state remains uploading even when every chunk exists. Normal send still refuses to claim completed delivery. This is source-build development functionality validated on Linux/macOS; Windows CLI token-file/staging operations are not supported yet.

## Loopback exercise

Build with `go build -o /tmp/bitspeed-transfer ./cmd/bitspeed-transfer`. Use an existing private parent directory on a supported local filesystem. Generate a token file without exposing its value in command arguments:

```sh
python3 - <<'PY'
import os, secrets
fd = os.open('token', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
os.write(fd, (secrets.token_hex(32) + '\n').encode())
os.close(fd)
PY
/tmp/bitspeed-transfer serve --root=/private/parent/receiver --token-file=token --listen=127.0.0.1:8080
```

In a second terminal, prepare a stable source and read its transfer_id from the JSON. Substitute that ID in the two later commands. Tokens must be the same at both ends and distributed through a secure channel.

```sh
/tmp/bitspeed-transfer send --prepare-only --source=/path/to/stable-file --spool=/private/parent/sender
/tmp/bitspeed-transfer send --upload-only --spool=/private/parent/sender --reuse=TRANSFER_ID --url=http://127.0.0.1:8080 --token-file=token
/tmp/bitspeed-transfer status --url=http://127.0.0.1:8080 --id=TRANSFER_ID --token-file=token
```

Status rehashes stored chunks and reports verified counts, byte totals and missing indices. A null receipt and uploading state mean the transfer is incomplete. An empty file also stays incomplete; no destination is created. Upload-only revalidates the private sender snapshot, admits its immutable manifest, stages missing chunks one at a time, checks each acknowledgement and verifies final receiver observations against its manifest. No original-source access is needed. A repeated upload-only request can skip verified chunks, but full process-kill/network-loss resume acceptance remains the next session's gate.

## Authentication and transport

The token file must be a private regular file (no group/other access) with exactly 64 lowercase hex characters and at most one trailing LF. Symlinks and oversized/invalid token files fail without echoing their contents or paths. Authorization covers creation, writes and status; invalid authentication returns the same response before application body reads or session lookup. A single token authorizes the whole root. The implementation does not log headers or token values.

Plain HTTP binds only a literal loopback address and clients accept HTTP only for literal loopback IPs. DNS localhost aliases are not accepted as plaintext exceptions. Off-machine serve requires both --tls-cert=FILE and --tls-key=FILE and uses TLS 1.2 or later. Certificate/key loading errors are sanitized. The client uses standard certificate/hostname verification, has no insecure mode, ignores proxy environment variables and refuses redirects. A reverse proxy may reach the loopback listener, but its TLS, logs and authorization forwarding remain the operator's responsibility. Forwarded headers never grant trust.

The development server uses HTTP/1.1, a maximum of 64 accepted connections, a configured 16 KiB header limit, ten-second header timeout, five-minute read/write and handler-operation limits, and thirty-second idle timeout. The client uses a six-minute request limit and bounded response reads. These development timeouts are currently fixed; very slow storage or links can fail safely rather than produce a completion claim. One global nonblocking operation lock serializes receiver work; contention returns session_busy. Status also uses that lock and rehashes stored bytes, so it can be expensive on large sessions. Chunk PUT checks only its own data plus session persistence gates rather than rescanning every preceding chunk.

## Storage and admission

The receiver holds an OS-backed exclusive root lock for its lifetime. The root and parent must be trusted against same-account writers; files and directories are private. Linux ext-family, XFS, Btrfs and overlay mounts with trusted local backing, and macOS APFS/HFS, are the initial accepted filesystem families. Startup also probes file sync, directory sync and same-filesystem no-replace hard linking. Unknown/remote filesystems and Windows receiver operation are rejected. Capability probes and process tests do not prove every hardware or mount's power-loss behavior.

Defaults retain at most sixteen sessions and reserve at most 256 GiB. --max-sessions and --max-reserved-bytes may lower those values. Each admission reserves twice its file size plus 32 MiB. The physical free-space check conservatively includes the full previous reservations as well as the new one, even when some reserved bytes are already materialized. This can reject admission early; it avoids promising the same free space twice. Free-space checks remain advisory and later I/O failures are handled independently. No existing session is evicted and no automatic complete-session cleanup runs.

Admission writes a canonical manifest and uploading journal in an exclusive .admitting-ID directory, syncs and re-reads both files to verify their saved bytes, and syncs the chunks/session directories, publishes the ID directory and syncs its parent. Only then can an admitted status be returned. Identity changes at an existing ID are conflicts. Strict manifest decoding rejects unknown/duplicate/case-alias/null fields, exponent/fractional integers, invalid hashes and excess chunk counts before allocating an excessive hash array. All paths are receiver-chosen after validated IDs and indices.

Chunk writes stream with a 32 KiB buffer into incoming.tmp, enforce exact length and SHA-256, sync/close, re-read and verify saved bytes, then hard-link to the fixed index without replacing an existing entry. The chunks directory is synced before acknowledgement. A duplicate still validates the full incoming body and saved chunk. Bad input cannot overwrite verified bytes. Published chunks remain recoverable after a lost response, but recovery must reverify and sync them before reporting progress. Store and recovery failures use stable protocol codes, never raw filesystem paths or errors.

## Current recovery boundary

Startup loads bounded session metadata and rejects malformed, unknown or partial admission evidence rather than advertising unsubstantiated progress. Status removes a recognized regular incoming.tmp under the held lock, rehashes each published chunk, and syncs data, metadata and ancestor directories. Corrupt chunks or journals block with recovery_required. The session-nine work will add the full tested recovery/quarantine policy; this prototype does not silently repair corruption. Unexpected files in the reserved destination directory also block startup because final publication is not implemented yet. Preserve evidence and stop the receiver before offline intervention; do not hand-edit manifests or counters.

Tests cover authentication before body reads, strict JSON, conflicting manifests, physical and logical reservations, concurrent requests, malformed paths, invalid lengths/digests, duplicates, write/short-write/corrupt-write/sync/close/link failures, publication-before-directory-sync failure and failed recovery sync. Client tests reject redirects, untrusted TLS, invented status/acknowledgements and malformed responses. Executable smoke prepares a snapshot, stages it, queries status and verifies that no destination file exists. Streaming allocation results and environments are recorded under test/results. The full interruption/crash matrix and assembly/full-file publication remain later sessions.
