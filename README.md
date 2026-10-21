# BitSpeed

BitSpeed houses two independent Go tools. Inspect explains HTTP transfer timing and compares saved runs. Transfer will move a file in verified chunks and resume interrupted work. The project home is https://bitspeed.org.

Inspect 0.1.1 is the first independent release. Inspect records HTTP and HTTPS GET transfers as validated JSON, retaining redirects, trace events and partial failures. Its local HTTP fixture supplies controlled test cases. Explain renders saved recordings as labelled text or JSON. Compare reports phase medians, observed ranges and scoped comparability checks. Transfer 0.1.0 provides snapshot preparation, restart resume, whole-file verification and safe destination publication on supported Linux/macOS local filesystems. A retained witness and stable receipt support completion recovery. See docs/transfer-releases.md for exact native/cross-build limits.

## Download

Download a platform archive and SHA256SUMS from [Inspect releases](https://github.com/softdevstudio/bitspeed/releases/tag/inspect/v0.1.1). Verify the archive against the manifest before extracting. Archives contain the executable, documentation, licenses and BUILD_INFO.json. Linux arm64 and macOS amd64 are cross-built only; Linux amd64, macOS arm64 and Windows amd64 receive native CLI checks. See docs/releases.md for details.

For example, on Linux amd64:

```sh
curl -fLO https://github.com/softdevstudio/bitspeed/releases/latest/download/bitspeed-inspect-linux-amd64.tar.gz
curl -fLO https://github.com/softdevstudio/bitspeed/releases/latest/download/SHA256SUMS
grep '  bitspeed-inspect-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
tar -xzf bitspeed-inspect-linux-amd64.tar.gz
./bitspeed-inspect --version
```

## Build and run

Use the Go version pinned in go.mod (1.27.3). No external Go modules are required.

```sh
go build -o /tmp/bitspeed-inspect ./cmd/bitspeed-inspect
/tmp/bitspeed-inspect --version
/tmp/bitspeed-inspect fixture
```

The fixture prints its loopback URL and waits until interrupted. Append /headers for delayed response headers, /body for a flushed header followed by delayed chunks, /redirect for a redirect to /empty, /empty for a 204 response, /disconnect for a truncated HTTP/1 body, or /timeout for a request that waits until cancellation. Delays can be set with --header-delay and --chunk-delay. The listener accepts literal loopback IPs only. This is a development fixture, not a public endpoint.

## Record a transfer

```sh
/tmp/bitspeed-inspect record --samples=3 --timeout=10s https://example.org/ > run.json
```

JSON goes to stdout; diagnostics go to stderr. Exit status is nonzero if any sample fails, the batch is cancelled or output cannot be saved. A failed transfer still produces a recording with partial observations. Invalid configuration produces no recording. Follow redirects up to --max-redirects, use fresh direct connections, and ignore proxy environment variables. Normal TLS certificate verification is always enabled. Use record --help for the timeout, interval and location options.

To try controlled failures, start fixture in another terminal and use its printed address with /disconnect or /timeout. Query values and fragments are redacted in saved URLs; original query values are used for the request. Paths, hostnames and location labels can still contain sensitive information. Never publish recordings without reviewing them. The recorder performs real GET requests, including redirects; use targets you intend to access.

## Explain a recording

```sh
/tmp/bitspeed-inspect explain run.json
/tmp/bitspeed-inspect explain --format=json run.json
```

The report separates measured fields, calculated intervals and labelled interpretations. Every calculation links to its evidence. Missing or ambiguous timing is unavailable; it is never turned into zero. First-byte waiting is not equated with origin processing time. Response reading includes header handling and scheduling. An incomplete response has no completed-payload rate. Reading a recording makes no network requests.

Use - as the filename to read stdin. Explaining a valid failed recording exits successfully. See docs/explanations.md for the report schema, interval definitions and rule limits.

## Compare recordings

```sh
/tmp/bitspeed-inspect compare before.json after.json
/tmp/bitspeed-inspect compare --format=json before.json after.json
```

The comparison retains descriptive statistics when requests or responses differ, and withholds unsupported change verdicts. It reports failures and missing values, handles a zero baseline without dividing by zero, and treats overlapping ranges as inconclusive. Three available values per group and full phase coverage are the minimum for a change verdict. These rules do not establish statistical confidence or causation. Response mismatches can block response verdicts while leaving connection-phase comparisons available. See docs/comparisons.md for exact rules and evidence references.

### Use compare as a CI check

From Inspect 0.2.0, compare can fail a pipeline when a chosen interval gets slower:

```sh
bitspeed-inspect compare --fail-if-slower=first_byte_wait,connection_setup --min-change=20ms before.json after.json
```

The comparison prints as usual. Gate lines go to stderr, one per gated interval and hop. The exit status is 0 when nothing gated got slower, 3 when an interval regressed, 4 when an interval couldn't be judged, and 1 for errors. An interval regresses only when compare itself says higher_in_samples, meaning the candidate median is higher and the observed ranges don't overlap, and the change is at least --min-change and --min-percent when you set them. Inconclusive, equal and faster intervals pass. A comparison blocked by comparability checks, a missing median or an interval absent from both recordings is undecided, because the gate can't show it didn't get slower; add --allow-undecided to let those pass. Gate names are dns_resolution, tcp_connect, tls_handshake, connection_setup, first_byte_wait and response_read. A gate result describes the recorded samples. Record enough samples on a stable runner, the default is ten, and expect noisy shared runners to give inconclusive results rather than failures.

## Controlled demo

See [examples/README.md](examples/README.md) for a reproducible loopback before/after demo and saved recordings generated by the downloaded Inspect binary. These are controlled examples, not network benchmarks.

## Checks

```sh
bash scripts/record-results.sh
```

The script runs formatting checks, go vet, race-enabled tests and a build/version smoke check and an executable recorder check including SIGTERM cancellation. It records the environment, source-tree fingerprint and outcomes under test/results. CI runs on Linux and macOS and uploads its own results. The streamed-body test records its allocation check alongside the correctness results. Release automation also verifies packaged executables on Linux amd64, macOS arm64 and Windows amd64.

The recording contract is in docs/recording-v1.md and the report contract is in docs/explanations.md. CLAUDE.md holds the agreed development sequence and session handoff.

## License

Code is licensed under Apache 2.0. See LICENSE, NOTICE and THIRD_PARTY_LICENSES.md.

## Release packaging

CI-gated release automation builds five archives with stable filenames, checks native Linux, macOS and Windows executables, and publishes only after the gates pass. See docs/releases.md for platform coverage, provenance, licensing and retry behavior. Transfer is separately versioned; Windows operations are unsupported. No hosted service or account is required.

## Transfer development

Transfer 0.1.0 is a separate command for supported Linux/macOS local filesystems. Build it with `go build -o /tmp/bitspeed-transfer ./cmd/bitspeed-transfer`. Local preparation works on Linux and macOS filesystems supporting file and directory synchronization. Its parent directory must be trusted and the spool private. These preparation commands perform no network requests:

```sh
/tmp/bitspeed-transfer send --prepare-only --source=/path/to/stable-file --spool=/private/parent/sender-spool
/tmp/bitspeed-transfer send --prepare-only --spool=/private/parent/sender-spool --reuse=TRANSFER_ID
```

Preparation returns JSON with prepared true, transferred false and the manifest. Progress uses stderr without source paths. Reuse checks the saved bytes using their existing identity and never needs the original file. A cancelled/failed preparation exits nonzero. Serve, status and send --upload-only support authenticated chunk staging and restart resume with no completion receipt or destination file. Restart the receiver with its existing root, then repeat upload-only with the same spool and ID; only missing or damaged chunks are uploaded. See [receiver recovery](docs/receiver-staging.md#restart-recovery) for retained evidence and operator limits. Normal send now completes delivery from the same saved snapshot and ID, and durably caches its completion receipt. See [verified delivery](docs/finalization.md). Archives are published only by the CI-gated workflow; use a verified release asset for your supported platform. See [docs/sender-spool.md](docs/sender-spool.md) for capacity, recovery, platform limits and failure behavior, and [docs/transfer-v1.md](docs/transfer-v1.md) for the network protocol.

For a loopback staging exercise, see [docs/receiver-staging.md](docs/receiver-staging.md). Use a private token file, start serve with a dedicated private root, prepare the sender snapshot, then invoke send --upload-only using that snapshot ID. Its JSON says uploaded true and transferred false. This is an explicit development stage, not completed file delivery.

For development delivery after preparation, use `bitspeed-transfer send --spool=DIR --reuse=ID --url=URL --token-file=FILE`. It stages missing chunks, finalizes one fixed destination, verifies the completed receipt against the snapshot and saves receipt.json before returning transferred true. Repeat the same command after a lost response.

From Transfer 0.3.0, send and send --upload-only print progress to stderr while the JSON result still goes to stdout alone. The first line says how many chunks the receiver can already prove and how many will be sent, chunk lines follow at most once per percent of the bytes to send, and a last line appears before the receiver checks and publishes the file. A chunk counts only after the receiver acknowledges it as stored. Progress lines contain counts and byte totals only, never paths or tokens. A transfer the receiver already reports as completed prints no progress. If stderr can't be written, for example a closed pipe, the command stops, and the operating system may end it at once; stored chunks stay verified and running the same command again continues the transfer. Snapshots, chunks, corrupt evidence, witnesses and receipts are retained by default.

From Transfer 0.2.0, to free the space held by chunks of finished transfers, stop the receiver and run:

```sh
bitspeed-transfer clean --root=/private/parent/receiver-root --dry-run
bitspeed-transfer clean --root=/private/parent/receiver-root
```

Clean takes the receiver's root lock, so it refuses to run while serve is using that root. It first runs the same recovery checks as starting the receiver, which re-verifies every completed destination against its manifest. Then it removes the stored chunk files of completed sessions only. Destinations, receipts, witnesses, journals, corrupt-chunk evidence and every unfinished session stay as they are. The JSON output lists each session with the chunks removed and the bytes they held, an estimate of the space returned. --dry-run runs the same recovery as starting the receiver, which can repair or mark sessions just as serve would, and then reports what it would remove without removing chunks. Clean skips the receiver's write test, so it works on a full disk unless recovery itself has something to write. A completed transfer keeps working afterwards: status and a repeated send still return its receipt. Clean does not change the receiver's reservation limit, and removing whole sessions still follows the offline procedure in docs/finalization.md. The Windows archive supports version/help and explicit unsupported-operation checks only; Linux arm64/macOS amd64 are cross-built without native runtime coverage.
