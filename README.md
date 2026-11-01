# BitSpeed

BitSpeed is two small, independent command-line tools written in Go. Neither needs the other, an account or a hosted service.

- **BitSpeed Inspect** (current release 0.3.0) records HTTP and HTTPS requests, explains where the time went and compares runs, with a mode that fails CI when a chosen interval gets slower.
- **BitSpeed Transfer** (current release 0.5.0) sends a file or a folder in verified chunks, several at a time, and picks up where it stopped after a crash, a kill or a lost connection.

Project site: https://bitspeed.org. Each tool has its own version and release tags (inspect/vX.Y.Z and transfer/vX.Y.Z); the latest release always carries both tools' archives.

## Download

Every release has five archives per tool and one SHA256SUMS file covering all of them:

| Platform | Inspect | Transfer |
| --- | --- | --- |
| Linux amd64 | bitspeed-inspect-linux-amd64.tar.gz | bitspeed-transfer-linux-amd64.tar.gz |
| Linux arm64 | bitspeed-inspect-linux-arm64.tar.gz | bitspeed-transfer-linux-arm64.tar.gz |
| macOS Apple silicon | bitspeed-inspect-darwin-arm64.tar.gz | bitspeed-transfer-darwin-arm64.tar.gz |
| macOS Intel | bitspeed-inspect-darwin-amd64.tar.gz | bitspeed-transfer-darwin-amd64.tar.gz |
| Windows amd64 | bitspeed-inspect-windows-amd64.zip | bitspeed-transfer-windows-amd64.zip |

Download from `https://github.com/softdevstudio/bitspeed/releases/latest/download/NAME` and check the archive against SHA256SUMS before you unpack it. On Linux amd64:

```sh
base=https://github.com/softdevstudio/bitspeed/releases/latest/download
curl -fLO $base/bitspeed-transfer-linux-amd64.tar.gz
curl -fLO $base/SHA256SUMS
grep '  bitspeed-transfer-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
tar -xzf bitspeed-transfer-linux-amd64.tar.gz
./bitspeed-transfer --version
```

On macOS use `shasum -a 256 -c -` in place of `sha256sum -c -`. Each archive holds the executable, this README, the tool's reference docs, the licenses and BUILD_INFO.json, which records the exact source commit and Go version. Linux amd64, macOS arm64 and Windows amd64 builds are run on real machines before every release. Linux arm64 and macOS amd64 are cross-built and not run before release; docs/releases.md and docs/transfer-releases.md have the details.

To build from source instead, use the Go version in go.mod. There are no external modules:

```sh
go build -o bitspeed-inspect ./cmd/bitspeed-inspect
go build -o bitspeed-transfer ./cmd/bitspeed-transfer
```

## BitSpeed Inspect

Inspect answers "this request got slower; which part?" It records each sample's DNS lookup, TCP connect, TLS handshake, wait for the first byte and response read, keeps redirects and failures, and never turns a missing measurement into zero. Recordings are plain JSON files you can save, diff and compare later.

### Record a transfer

```sh
bitspeed-inspect record --samples=3 --timeout=10s https://example.org/ > run.json
```

JSON goes to stdout; diagnostics go to stderr. Exit status is nonzero if any sample fails, the batch is cancelled or output cannot be saved. A failed transfer still produces a recording with partial observations. Invalid configuration produces no recording. Follow redirects up to --max-redirects, use fresh direct connections, and ignore proxy environment variables. Normal TLS certificate verification is always enabled. Use record --help for the timeout, interval and location options.

To try controlled failures, start fixture in another terminal and use its printed address with /disconnect or /timeout. Query values and fragments are redacted in saved URLs; original query values are used for the request. Paths, hostnames and location labels can still contain sensitive information. Never publish recordings without reviewing them. The recorder performs real GET requests, including redirects; use targets you intend to access.

#### POST requests and warm connections

From Inspect 0.3.0, record can send a POST body and can reuse connections across samples:

```sh
bitspeed-inspect record --method=POST --body-file=query.json --content-type=application/json https://api.example.com/search > post.json
bitspeed-inspect record --connection=warm https://example.org/ > warm.json
```

The body file is read once, up to 1 MiB, and sent with every sample. The recording never contains the body. It keeps its size and SHA-256 so compare can tell whether two runs sent the same thing, and a short guessable body can be recovered from that hash, so treat recordings as sensitive. On a redirect, 307 and 308 resend the POST with its body to the same origin, while 301, 302 and 303 switch to GET without it, the way browsers do; each hop records the method it sent. A 307 or 308 to a different scheme, host or port is not followed, and the hop records cross_origin_body_redirect, so the body never goes somewhere you didn't name. The content type must be a plain header value without control characters. POST requests really are sent, so only point them at endpoints where repeating the request is safe.

--connection=warm keeps one keep-alive connection pool for the whole recording. The first sample normally opens the connection and later samples reuse it, which each hop records in connection_reused. Connection phases are then missing for reused samples, so their verdicts are withheld; first-byte wait and response read are the useful intervals in warm mode, and sample 0 is usually the cold one. When a server has already closed a kept-alive connection, Go retries a GET on a new connection; the hop keeps both connection events, reports the new connection as not reused and claims no single winning connection, so its intervals are unavailable rather than guessed. A POST is never retried that way, so a stale connection fails that sample. Warm and fresh recordings measure different things and compare blocks a verdict between them. Two warm recordings whose reuse patterns differ, for example because one server closes connections, get a cautionary check, which withholds verdicts and makes the CI gate undecided.

Plain GET recordings with fresh connections are still written as schema 1, so Inspect 0.1.1 and 0.2.0 can read them; those releases reject schema 2 files with an unknown-field error rather than a version message. POST or warm recordings use schema 2, described in docs/recording-v1.md. Compare blocks a verdict when the method, body size, body SHA-256, content type or connection policy differ.

### Explain a recording

```sh
bitspeed-inspect explain run.json
bitspeed-inspect explain --format=json run.json
```

The report separates measured fields, calculated intervals and labelled interpretations. Every calculation links to its evidence. Missing or ambiguous timing is unavailable; it is never turned into zero. First-byte waiting is not equated with origin processing time. Response reading includes header handling and scheduling. An incomplete response has no completed-payload rate. Reading a recording makes no network requests.

Use - as the filename to read stdin. Explaining a valid failed recording exits successfully. See docs/explanations.md for the report schema, interval definitions and rule limits.

### Compare recordings

```sh
bitspeed-inspect compare before.json after.json
bitspeed-inspect compare --format=json before.json after.json
```

The comparison retains descriptive statistics when requests or responses differ, and withholds unsupported change verdicts. It reports failures and missing values, handles a zero baseline without dividing by zero, and treats overlapping ranges as inconclusive. Three available values per group and full phase coverage are the minimum for a change verdict. These rules do not establish statistical confidence or causation. Response mismatches can block response verdicts while leaving connection-phase comparisons available. See docs/comparisons.md for exact rules and evidence references.

#### Use compare as a CI check

From Inspect 0.2.0, compare can fail a pipeline when a chosen interval gets slower:

```sh
bitspeed-inspect compare --fail-if-slower=first_byte_wait,connection_setup --min-change=20ms before.json after.json
```

The comparison prints as usual, unchanged. Gate lines go to stderr, one per gated interval and hop (hops count from 0, as in compare's own output). The exit status is 0 when nothing gated got slower, 3 when an interval regressed, 4 when an interval couldn't be judged, and 1 for errors. An interval regresses only when compare itself says higher_in_samples, meaning the candidate median is higher and the observed ranges don't overlap, and the change is at least --min-change and --min-percent when you set them. An increase from a zero baseline exceeds any percent. Faster, equal and overlapping intervals pass, and so does an interval that never happened in any sample on either side, such as TLS on a plain HTTP hop, as long as no comparability check blocks or cautions the comparison. The gate is undecided when compare blocked the comparison, when it withheld a verdict because of a cautionary check or fewer than three complete values on a side, or when an interval is missing on one side only. Add --allow-undecided to let undecided results pass; that includes blocked comparisons, so use it with care. Gate names are dns_resolution, tcp_connect, tls_handshake, connection_setup, first_byte_wait and response_read. A gate result describes the recorded samples. Record enough samples on a stable runner, the default is ten, and expect noisy shared runners to give inconclusive results rather than failures.

### A local fixture for experiments

`bitspeed-inspect fixture` starts a loopback test server and prints its URL. Append /headers for delayed response headers, /body for a flushed header followed by delayed chunks, /redirect for a redirect to /empty, /empty for a 204 response, /disconnect for a truncated HTTP/1 body, or /timeout for a request that waits until cancellation. --header-delay and --chunk-delay set the delays. It listens on literal loopback addresses only. examples/README.md walks through a before and after demo with saved recordings.

## BitSpeed Transfer

Transfer moves a file or a folder from one machine to another over HTTPS. It cuts the payload into chunks of a fixed size (4 MiB by default), and the receiver checks the SHA-256 of every chunk, writes it to disk, syncs it and only then says it has it. If anything stops, the next send asks the receiver which chunks it can still prove and sends only the rest. At the end the receiver checks the whole payload's digest, publishes it without overwriting anything and gives back a receipt, which the sender keeps.

It works on Linux and macOS local filesystems and on local NTFS volumes on Windows. There is no compression, no delta sync, no discovery and no daemon to manage: a receiver is `serve` on one machine, a sender is `send` on another.

### Send a file

Both sides need the same token, 64 lowercase hex characters in a file only you can read:

```sh
openssl rand -hex 32 > ~/bitspeed-token && chmod 600 ~/bitspeed-token
```

On the receiving machine, start a receiver with a private folder for its data. Across a network it needs a TLS certificate; plain HTTP is allowed on loopback only:

```sh
bitspeed-transfer serve --root=$HOME/bitspeed-receiver --token-file=$HOME/bitspeed-token \
  --listen=0.0.0.0:8443 --tls-cert=cert.pem --tls-key=key.pem
```

On the sending machine, first take a snapshot. This copies the file into a private spool folder and works out every chunk's digest, so later changes to the original don't matter; you can even delete it:

```sh
bitspeed-transfer send --prepare-only --source=big-file.iso --spool=$HOME/bitspeed-spool
```

The JSON it prints includes `manifest.transfer_id`. Send with that ID:

```sh
bitspeed-transfer send --spool=$HOME/bitspeed-spool --reuse=TRANSFER_ID \
  --url=https://receiver.example:8443 --token-file=$HOME/bitspeed-token
```

Progress goes to stderr and the JSON result to stdout. The file lands on the receiver as `files/TRANSFER_ID.data` under its root, and the receipt is saved in the spool. If the send stops for any reason, run the same command again. `bitspeed-transfer status --url=URL --token-file=FILE --id=ID` shows what the receiver can prove at any time.

### Several chunks at once

Send keeps up to four chunk uploads in flight by default. `--parallel=N` sets it from 1 to 8. This helps most on links with a long round trip, where one chunk at a time leaves the line idle while waiting for each acknowledgement. Every chunk still goes through the full check-and-sync sequence before it counts, and progress counts only chunks the receiver has confirmed. A receiver older than 0.5.0 handles one upload at a time; send notices and still finishes, just without the speed-up.

### Send a folder

Point `--source` at a folder and Transfer packs it as one payload, called a tree:

```sh
bitspeed-transfer send --prepare-only --source=photos-2026 --spool=$HOME/bitspeed-spool
bitspeed-transfer send --spool=$HOME/bitspeed-spool --reuse=TRANSFER_ID --url=URL --token-file=FILE
```

Prepare reports how many files and folders it packed. The tree sends, resumes and finishes like a single file and gets one receipt covering every file in it. On the receiver, write it out as a folder:

```sh
bitspeed-transfer unpack --root=$HOME/bitspeed-receiver --id=TRANSFER_ID --out=$HOME/photos-2026
```

Unpack checks each file and the whole payload against the receipt before the output folder appears, refuses to write into an existing folder or inside the receiver root, and can run while serve is running. Trees carry folders (empty ones too) and regular files. Symbolic links, devices and other special files are refused at prepare time, and permissions, owners and timestamps are not kept. A file or folder that changes while it is being copied fails preparation instead of producing a mixed copy. docs/transfer-tree.md has the format and the limits: 100,000 entries, 4096-byte paths and 64 GiB per transfer.

### Free space after delivery

The receiver keeps the chunks after publishing a transfer. To remove them, stop the receiver and run:

```sh
bitspeed-transfer clean --root=$HOME/bitspeed-receiver --dry-run
bitspeed-transfer clean --root=$HOME/bitspeed-receiver
```

Clean first runs the same recovery checks as starting the receiver, which re-verify every completed destination, then removes the chunks of completed transfers only. Published files, receipts, unfinished transfers and evidence of corrupt chunks stay. Status and a repeated send still return the receipt afterwards. docs/finalization.md covers removing whole sessions.

### Disk space and limits

The sender spool holds a full copy of each snapshot. The receiver reserves three times the payload size per transfer (chunks, assembly and the published file) and holds at most 16 transfers per root; unpack writes one more copy wherever you point it. A transfer is at most 64 GiB and 16,384 chunks. The spool and the receiver root must be private folders whose parent only you control; Transfer creates them private and refuses ones other accounts can read. Snapshots, receipts and receiver evidence are kept until you remove them. Requests have fixed timeouts (five minutes per request), so very slow storage or links fail safely rather than claim completion. docs/receiver-staging.md and docs/sender-spool.md list the exact rules, and docs/transfer-v1.md is the protocol.

### Windows

Every Transfer command works on Windows 10 version 1903 or later, Windows 11 and Windows Server 2022 or later, on local NTFS volumes on fixed drives. Create the token file inside your profile so it inherits a private access list, with plain ASCII and no byte order mark:

```powershell
$bytes = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
-join ($bytes | ForEach-Object { $_.ToString("x2") }) | Set-Content -NoNewline -Encoding ascii $HOME\bitspeed-token
bitspeed-transfer serve --root=$HOME\bitspeed-receiver --token-file=$HOME\bitspeed-token
```

Privacy on Windows is checked through access lists instead of mode bits. A root or spool that Transfer creates gets a protected list for you, SYSTEM and Administrators. One you create yourself must allow no other account, which a folder under your profile normally satisfies and a folder directly under C:\ does not. While preparing a snapshot Transfer holds the source with read-only sharing, so preparation is refused while another program has the file open for writing. docs/windows-persistence.md maps each persistence step to its Windows call and lists the limits.

## Checks

```sh
bash scripts/record-results.sh
```

This runs formatting checks, go vet, race-enabled tests for both tools, both builds and the executable smoke scripts (Inspect recording with cancellation; Transfer delivery, interruption and restart, TLS, and a folder send and unpack). It records the environment and results under test/results/latest. CI runs it on Linux and macOS, the Transfer Windows workflow runs the Transfer suite on Windows, and the release workflows run the smoke scripts again on the packaged archives before publishing.

## Documentation

- Inspect: docs/recording-v1.md (recording format), docs/explanations.md (report and intervals), docs/comparisons.md (comparison and gate rules), docs/releases.md.
- Transfer: docs/transfer-v1.md (protocol), docs/transfer-tree.md (folders), docs/sender-spool.md, docs/receiver-staging.md, docs/finalization.md, docs/windows-persistence.md, docs/transfer-releases.md.

## License

Apache 2.0. See LICENSE, NOTICE and THIRD_PARTY_LICENSES.md.
