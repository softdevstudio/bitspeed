# Inspect Recording Format Version 1

This is the initial development contract. The recorder is not implemented. Changes to this contract before release must update the validator and tests together. After release, incompatible changes require a new schema version.

## Envelope

schema_version is 1. tool_version identifies the recording tool. started_at is an RFC 3339 timestamp for identification. request stores GET, a safe URL, connection_policy set to fresh, decompression policy and an optional operator-supplied location_label. No geographic verification is implied.

samples is a nonempty ordered array with zero-based contiguous index, outcome, elapsed_ns and a nonempty hops array. A sample covers one attempted request including redirects and failures. A hop is one HTTP request attempt. elapsed_ns is elapsed monotonic time, serialized as an integer number of nanoseconds. Wall-clock values must never be subtracted to calculate durations.

Each hop has a safe URL, outcome, elapsed_ns, status_code, protocol, remote_address, connection_reused, body_bytes, body_complete, events and an optional error_code. Status, protocol, address and reuse are nullable. Null means unobserved; zero and false are values. body_bytes counts application bytes actually read, including bytes read before a failure. It is not wire traffic and excludes response headers. Decompression policy must be retained when interpreting body size.

Outcomes are complete, failed or cancelled. Complete describes transport completion through body EOF, independently of HTTP status. A complete hop requires a status and complete body with no error code. An incomplete hop requires a nonempty stable error code and an incomplete body, even if failure occurred before response headers. Accepted codes are dns_failed, connect_failed, tls_failed, request_write_failed, response_failed, body_failed, timeout, cancelled and redirect_limit. Other strings are rejected. Do not store raw network errors or credentials in error_code. A successful sample cannot contain a failed hop.

## Event boundaries

Events contain kind, offset_ns and optional attempt. Offset is measured from the hop start, is nonnegative and cannot exceed hop elapsed time. Events must be sorted by observed offset before encoding; callbacks may run concurrently. Duplicate kinds are allowed. attempt identifies a DNS, connection or TLS operation where pairing is meaningful. Do not pair starts and completions by adjacency or assume the latest connect callback won.

dns_start and dns_done bracket a resolver operation, including errors. connect_start and connect_done bracket a connection attempt, including failures. tls_start and tls_done bracket a TLS attempt. connection_acquired marks when the HTTP transport supplies a usable connection. request_written marks completion of request writing; the recorder must distinguish a write error before treating this as a successful write. first_byte marks receipt of the first response byte. body_done marks successful EOF. hop_done marks termination on success or failure.

DNS and connection operations can overlap, repeat or be absent due to reuse, IP literals or transport behavior. Preserve available events and report unpairable phases as unavailable. The initial event structure does not yet carry per-operation error outcomes or winning-connection identities. Session two must add that evidence, with matching contract and tests, before producing phase calculations. Do not infer successful operations merely from completion callbacks.

First-byte waiting is the interval between a successfully written request and the first response byte when those boundaries form a nonnegative interval. It includes effects beyond origin execution. Response body reading is measured from first_byte to successful EOF, or to hop termination with an explicitly incomplete label. These intervals describe client observations. They do not establish database, CPU, congestion or geographic causes.

## Privacy and input limits

SafeURL rejects non-HTTP schemes, relative targets and embedded user information. It removes fragments and replaces every query value with [redacted], preserving key names and multiplicity. It does not sanitize sensitive information in URL paths, hostnames or query key names. Treat recordings as potentially sensitive files. Query-bearing targets cannot be assumed identical across recordings because the original values are absent. The future comparison engine must warn about that limitation.

The current encoder and decoder limit a recording to 16 MiB including whitespace. Encoding checks the serialized size before writing. The decoder rejects unknown fields and trailing JSON, checks the schema and validates known structural invariants. Go's JSON decoder still accepts duplicate keys and case-insensitive field matching. This development decoder is not a hardened import boundary for hostile files; tighten its grammar and required-field handling before releasing recording imports. The future recorder must cap sample, redirect and event counts to stay within the file limit.

## Reports

Raw recordings contain observations. A derived report will separately represent calculations and labelled interpretations, each linked to evidence paths and rule identifiers. Its schema is deferred until the explanation phase. Missing timing evidence must never become a zero-duration phase.
