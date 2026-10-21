# Inspect Recording Format Versions 1 and 2

This is a development contract for the unreleased recorder. Earlier skeleton recordings need regeneration: version 1 now requires request limits and richer trace evidence. After the first release, incompatible changes require a new schema version.

## Requests and samples

schema_version is 1. tool_version identifies the recording tool. started_at is an RFC 3339 timestamp for identification. request contains method GET, a redacted URL, connection_policy fresh, decompression false, samples_requested, max_redirects, timeout_ns, connect_timeout_ns, header_timeout_ns, body_timeout_ns, interval_ns and an optional operator-supplied location_label. The location is not independently verified.

Each sample contains a zero-based contiguous index, outcome, elapsed_ns and a nonempty hops array. A sample covers one requested URL and any followed redirects. Each hop is a distinct HTTP request with its own transport and monotonic start. A failed hop ends its sample. The sample outcome matches its final hop. Cancelled batches may contain fewer samples than requested; failed attempts are retained. A timeout ends the affected sample, while cancellation of the calling context stops the batch. HTTP error status codes can still represent completed transport operations.

Each hop contains URL, outcome, elapsed_ns, nullable status_code, protocol, content_encoding, remote_address, connection_reused and winning_connect_attempt, plus body_bytes, body_complete, events and optional error_code. Null means unavailable. Zero bytes and false reuse are observed values. Body bytes count application bytes read, including a partial body; headers and transport overhead are excluded. Automatic decompression is disabled, so Content-Encoding is preserved and compressed payload bytes are counted without inflation.

Outcomes are complete, failed or cancelled. Complete means the response body reached EOF. Failed and cancelled hops carry a stable code: dns_failed, connect_failed, tls_failed, request_write_failed, response_failed, body_failed, timeout, cancelled, redirect_limit, invalid_redirect, event_limit or unsupported_upgrade. Raw network errors are never serialized. A redirect policy failure can retain body_complete true because the HTTP response finished before the next target was rejected. The final URL and raw Location header are never copied into error text. HTTP 101 upgrades are rejected immediately; upgraded protocol streams are outside the recorder scope. Body readers are explicitly closed on cancellation so a blocked read cannot outlive its timeout.

## Trace events

Each event contains kind and a nonnegative offset_ns from the hop's monotonic start, with optional attempt, outcome and address. Wall-clock timestamps are never subtracted for durations. A mutex serializes observation and append operations, so offsets are ordered. DNS, connection and TLS start/completion pairs share an attempt identity. Completion events carry success or failed; request_written also carries a write outcome. connection_acquired carries success. Event outcomes are absent on event types that do not define them.

dns_start and dns_done bracket a resolver operation, including errors. connect_start and connect_done bracket an individual dial attempt and retain the dial address. tls_start and tls_done bracket negotiation. connection_acquired marks transport delivery of a usable connection. request_written marks completion of request writing and records whether writing succeeded. first_byte marks receipt of the first response byte, which can precede completion of header parsing. body_done marks successful EOF. The final hop_done event marks termination and has offset equal to hop elapsed_ns.

Concurrent connection attempts are paired by network and address. winning_connect_attempt is present only when a unique successful connect attempt matches the acquired connection's remote address; otherwise it is null. The winning identity is validated against successful completion evidence. Starts without completions remain valid after cancellation. Late callbacks after hop termination cannot alter saved evidence. The current direct, fresh-connection transport performs one origin TLS negotiation per hop. Proxy, reused-connection and custom-dialer modes are not supported.

The recorder does not flush OS resolver caches. DNS events may be absent, especially with literal IP targets. Timing phases may overlap, repeat or be unavailable; do not sum them into an additive waterfall. Later explanations must calculate first-byte waiting only from a successfully written request to first_byte when that interval is nonnegative. Client waiting cannot establish origin processing, database, CPU or congestion causes.

## Timeouts and bounds

Default recording uses ten sequential samples, at most ten redirects per sample, a thirty-second total timeout per sample, a five-second dial and TLS timeout, a ten-second response-header timeout after request writing and a fifteen-second total body-reading timeout per hop. --interval defaults to zero. Every timeout must be positive and at most one hour. Sample count is bounded at 100, redirects at ten and interval at one minute. The overall timeout includes all redirects and body reads in that sample. Dial timeout includes DNS resolution; TLS has a separate limit with the same configured value. There is no body idle-timeout mode.

Payloads stream through a fixed 32 KiB copy buffer to a discard writer. Response headers are bounded at 1 MiB. Each hop keeps at most 256 trace events, reserving its final slot for termination. Exceeding that bound cancels the hop with event_limit. The recorder retains bounded request metadata and observations, never whole response bodies. Recording metadata still grows with the configured sample and hop limits. JSON serialization is separately limited to 16 MiB and can reject a batch that exceeds that file limit.

## Privacy and JSON validation

SafeURL rejects relative targets, non-HTTP schemes, empty hostnames and embedded user information. It removes fragments and replaces every query value with [redacted], preserving query key names and multiplicity. Actual requests use their original query values. Redirects receive the same target validation. No cookies, authorization headers, custom request headers or raw errors are retained. Response metadata includes only the sanitized Content-Encoding classification; all other response headers and unknown encoding text are discarded. The recorder connects directly and ignores proxy environment variables. Connections still reveal the target to the destination in the normal way.

Redaction does not remove secrets from paths, hostnames, query key names or user-provided location labels. Treat recordings as potentially sensitive. Query values are absent, so later comparisons must warn that identical recorded URLs do not establish identical original queries. Request targets are limited to 8 KiB and location labels to 256 bytes at recording time.

Encode validates observations and checks the serialized size before writing. Decode bounds input to 16 MiB, rejects trailing JSON, duplicate keys, unknown or incorrectly cased keys, missing required fields and null values outside nullable pointer fields. Nesting is bounded at 64. Optional fields are explicitly marked in Go json tags. Semantic validation checks operation identities, completion evidence, sample outcomes and known field bounds. This validates recorded evidence; it cannot prove a file was produced honestly by a remote machine.

## Reports

Recordings contain observations. Calculations and labelled interpretations use the derived-report schema in docs/explanations.md, with links to evidence paths and rule identifiers. explain and compare are implemented. See docs/comparisons.md for comparison statistics, scopes and verdict limits.

## Schema 2 (Inspect 0.3.0)

Schema 2 extends schema 1 for POST requests and warm connections. Inspect writes schema 1 whenever the request is GET with connection_policy fresh, so older releases keep reading those recordings, and schema 2 otherwise. Decode accepts both; a schema 2 recording of a plain fresh GET, or a schema 1 recording with any schema 2 field, is rejected.

In schema 2, request.method is GET or POST and request.connection_policy is fresh or warm. A POST request adds request_body_sha256 (64 lowercase hex characters, always present), request_body_bytes (0 to 1 MiB, omitted when 0) and an optional content_type of at most 256 characters without line breaks. The body is never stored. A GET request carries none of these fields. Every schema 2 hop carries method: the first hop uses the request method; after a redirect, 307 and 308 keep the previous hop's method and body, and 301, 302 and 303 switch to GET without a body.

Under the warm policy one keep-alive transport serves every sample of the recording, and connection_reused may be true; under fresh it must not be. Reused hops normally have no DNS, connect or TLS events, so those calculations are unavailable and comparison verdicts on them are withheld. Explanations of schema 2 recordings add connection_policy, and for POST request_body_bytes, request_body_sha256 and content_type when set, plus a method measurement per hop. Schema 1 explanations are unchanged.

Comparisons block a verdict when method, connection_policy, request_body_bytes, request_body_sha256 or content_type differ, and the redirect-chain check includes each hop's method (schema 1 hops count as GET).
