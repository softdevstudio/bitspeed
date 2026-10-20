# BitSpeed

BitSpeed houses two independent Go tools. Inspect will explain HTTP transfer timing and compare saved runs. Transfer will move a file in verified chunks and resume interrupted work. The project home is https://bitspeed.org.

This is an unreleased development skeleton. The runnable part is Inspect's local HTTP fixture. The evidence format and its validator are implemented. The record, explain and compare commands return explicit unimplemented errors. Transfer has no implementation yet.

## Build and run

Use Go 1.23 or later. No external Go modules are required.

```sh
go build -o /tmp/bitspeed-inspect ./cmd/bitspeed-inspect
/tmp/bitspeed-inspect --version
/tmp/bitspeed-inspect fixture
```

The fixture prints its loopback URL and waits until interrupted. Append /headers for delayed response headers, /body for a flushed header followed by delayed chunks, /redirect for a redirect to /empty, /empty for a 204 response, /disconnect for a truncated HTTP/1 body, or /timeout for a request that waits until cancellation. Delays can be set with --header-delay and --chunk-delay. The listener accepts literal loopback IPs only. This is a development fixture, not a public endpoint.

## Checks

```sh
bash scripts/record-results.sh
```

The script runs formatting checks, go vet, race-enabled tests and a build/version smoke check. It records the environment, source-tree fingerprint and outcomes under test/results. CI runs on Linux and macOS and uploads its own results. A successful skeleton check is not an Inspect release: recording, explanations and comparisons still need implementation and review.

The recording contract is in docs/recording-v1.md. AGENTS.md holds the agreed development sequence and session handoff.

## License

Code is licensed under Apache 2.0. See LICENSE, NOTICE and THIRD_PARTY_LICENSES.md.
