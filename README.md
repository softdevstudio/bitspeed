# BitSpeed

BitSpeed houses two independent Go tools. Inspect will explain HTTP transfer timing and compare saved runs. Transfer will move a file in verified chunks and resume interrupted work. The project home is https://bitspeed.org.

This is an unreleased development build. Inspect records HTTP and HTTPS GET transfers as validated JSON, retaining redirects, trace events and partial failures. Its local HTTP fixture supplies controlled test cases. Explain renders saved recordings as labelled text or JSON. Compare remains unimplemented. Transfer has no implementation yet.

## Build and run

Use Go 1.23 or later. No external Go modules are required.

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

## Checks

```sh
bash scripts/record-results.sh
```

The script runs formatting checks, go vet, race-enabled tests and a build/version smoke check and an executable recorder check including SIGTERM cancellation. It records the environment, source-tree fingerprint and outcomes under test/results. CI runs on Linux and macOS and uploads its own results. The streamed-body test records its allocation check alongside the correctness results. Passing these checks is not an Inspect release: comparisons and release preparation remain ahead.

The recording contract is in docs/recording-v1.md and the report contract is in docs/explanations.md. AGENTS.md holds the agreed development sequence and session handoff.

## License

Code is licensed under Apache 2.0. See LICENSE, NOTICE and THIRD_PARTY_LICENSES.md.
