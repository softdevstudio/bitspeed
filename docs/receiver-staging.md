# Development chunk staging

Transfer remains 0.0.0-dev and unreleased. Development operations include authenticated manifest admission, sequential chunk upload, verified status and restart recovery. The explicit upload-only operation does not assemble a destination or issue a completion receipt. Its staging result is uploaded true and transferred false; receiver state remains uploading even when every chunk exists. Normal send now finalizes delivery and durably caches a verified completion receipt; see finalization.md. This is source-build development functionality validated on Linux/macOS; Windows CLI token-file/staging operations are not supported yet.

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

Status rehashes stored chunks and reports verified counts, byte totals and missing indices. A null receipt and uploading state mean the transfer is incomplete. An empty file also stays incomplete; no destination is created. Upload-only revalidates the private sender snapshot, admits its immutable manifest, stages missing chunks one at a time, checks each acknowledgement and verifies final receiver observations against its manifest. No original-source access is needed. After interruption, restart serve with the same root and token, then repeat upload-only with the same spool and transfer ID. The client asks for revalidated progress and sends only missing chunks. A new transfer ID loses this resume relationship. There is no automatic retry loop; a failed request exits nonzero and the operator repeats the command.

## Authentication and transport

The token file must be a private regular file (no group/other access) with exactly 64 lowercase hex characters and at most one trailing LF. Symlinks and oversized/invalid token files fail without echoing their contents or paths. Authorization covers creation, writes and status; invalid authentication returns the same response before application body reads or session lookup. A single token authorizes the whole root. The implementation does not log headers or token values.

Plain HTTP binds only a literal loopback address and clients accept HTTP only for literal loopback IPs. DNS localhost aliases are not accepted as plaintext exceptions. Off-machine serve requires both --tls-cert=FILE and --tls-key=FILE and uses TLS 1.2 or later. Certificate/key loading errors are sanitized. The client uses standard certificate/hostname verification, has no insecure mode, ignores proxy environment variables and refuses redirects. A reverse proxy may reach the loopback listener, but its TLS, logs and authorization forwarding remain the operator's responsibility. Forwarded headers never grant trust.

The development server uses HTTP/1.1, a maximum of 64 accepted connections, a configured 16 KiB header limit, ten-second header timeout, five-minute read/write and handler-operation limits, and thirty-second idle timeout. The client uses a six-minute request limit and bounded response reads. These development timeouts are currently fixed; very slow storage or links can fail safely rather than produce a completion claim. One global nonblocking operation lock serializes receiver work; contention returns session_busy. Status also uses that lock and rehashes stored bytes, so it can be expensive on large sessions. Chunk PUT checks only its own data plus session persistence gates rather than rescanning every preceding chunk.

## Storage and admission

The receiver holds an OS-backed exclusive root lock for its lifetime. The root and parent must be trusted against same-account writers; files and directories are private. Linux ext-family, XFS, Btrfs and overlay mounts with trusted local backing, and macOS APFS/HFS, are the initial accepted filesystem families. Startup also probes file sync, directory sync and same-filesystem no-replace hard linking. Unknown/remote filesystems and Windows receiver operation are rejected. Capability probes and process tests do not prove every hardware or mount's power-loss behavior.

Defaults retain at most sixteen sessions and reserve at most 256 GiB. --max-sessions and --max-reserved-bytes may lower those values. Each admission now reserves three times its file size plus 32 MiB. Existing sessions are reconstructed under that rule on restart; an older lower configured quota may require increasing the limit before startup can proceed. The physical free-space check conservatively includes the full previous reservations as well as the new one, even when some reserved bytes are already materialized. This can reject admission early; it avoids promising the same free space twice. Free-space checks remain advisory and later I/O failures are handled independently. No existing session is evicted and no automatic complete-session cleanup runs.

Admission writes a canonical manifest and uploading journal in an exclusive .admitting-ID directory, syncs and re-reads both files to verify their saved bytes, and syncs the chunks/session directories, publishes the ID directory and syncs its parent. Only then can an admitted status be returned. Identity changes at an existing ID are conflicts. Strict manifest decoding rejects unknown/duplicate/case-alias/null fields, exponent/fractional integers, invalid hashes and excess chunk counts before allocating an excessive hash array. All paths are receiver-chosen after validated IDs and indices.

Chunk writes stream with a 32 KiB buffer into incoming.tmp, enforce exact length and SHA-256, sync/close, re-read and verify saved bytes, then hard-link to the fixed index without replacing an existing entry. The chunks directory is synced before acknowledgement. A duplicate still validates the full incoming body and saved chunk. Bad input cannot overwrite verified bytes. Published chunks remain recoverable after a lost response, but recovery must reverify and sync them before reporting progress. Store and recovery failures use stable protocol codes, never raw filesystem paths or errors.

## Restart recovery

Startup holds the exclusive root lock, reconstructs reservations from canonical manifests and journals, rehashes every published chunk and completes the file/directory synchronization gates before listening. Startup recovery has a five-minute bound and honors CLI cancellation. Every scan is bounded by the fixed session/chunk limits. A failed read, sync or close cannot become verified progress. A lost acknowledgement is resolved from saved bytes rather than a progress counter.

An interrupted .admitting-ID is never an acknowledged session. Startup discards only a private directory containing any subset of the bounded regular manifest.json/journal.json files and an empty private chunks directory. Unknown names, links or chunk data block startup and remain untouched. Published manifests and journals must match their canonical binding exactly; recovery never substitutes a guessed identity. Interrupted startup probes are removed only when their exact reserved shape is recognized; any remaining destination link must share the source inode.

A recognized private incoming.tmp of at most the maximum chunk size is discarded. A published regular chunk with a wrong digest or a shorter length is hard-linked to INDEX.bad, the directory is synchronized, the published index is removed and the directory is synchronized again. Status marks that chunk missing. Replacement upload uses the original manifest digest and leaves the corrupt evidence intact. Recovery recognizes an interrupted quarantine's two names by inode. It retains at most one bad file per index, each no larger than the expected chunk. An oversized chunk, a symlink, an unknown name or a second independently damaged replacement blocks recovery rather than discarding or replacing evidence. Storage errors do not trigger quarantine.

The reservation of three times file size plus 32 MiB covers staged chunks, bounded corrupt evidence and the complete assembly. ready and the output share the assembly inode. Evidence is retained rather than silently deleted to fund assembly.

Unexpected names or destinations without a trustworthy session binding block startup. A known session encountering an unrelated destination records a terminal conflict. Stop the receiver and preserve evidence before offline intervention. Do not hand-edit manifests, fabricate counters or delete a published binding to reuse its ID. A failed admission can require a receiver restart to clean known unacknowledged metadata. Unknown/corrupt binding metadata requires operator investigation; recovery deliberately fails closed for the entire root.

Tests kill separate sender and receiver processes during admission writes, after admission rename, during chunk writes, after chunk hard-link publication and after durable acknowledgement preparation but before response delivery. Fresh processes reuse the same snapshot and ID; valid published chunks are skipped, and no destination file appears. A dropped HTTP response also resumes without resending its substantiated chunk. Tests cover truncated/corrupt chunks, preserved quarantine evidence, repeated damage, interrupted probes, malformed bindings, unexpected files/links, reconstructed reservations, cancellation and injected write/close/sync/link failures. These checks establish process-crash behavior on the recorded environments, not a guarantee against hardware or filesystem power-loss faults. Results are recorded under test/results. Final assembly, full-file verification and publication are documented and tested in finalization.md; Transfer remains unreleased.
