# Inspect Explanations

Explain reads one saved recording and applies deterministic rules. It makes no network requests and never changes source observations. Compare is still unimplemented.

```sh
bitspeed-inspect explain run.json
bitspeed-inspect explain --format=json run.json
bitspeed-inspect explain --format=json - < run.json
```

An explanation of a valid failed recording exits successfully. Invalid input, unsupported formats and output failures exit nonzero without a fabricated report. Text output quotes untrusted strings so terminal escape sequences cannot execute. JSON output keeps the same structured evidence distinctions.

## Report contract

schema_version is 1 and rule_set is inspect-explain-v1. source_sha256 identifies the compact Go JSON serialization of the validated Recording value, not the original file bytes. Whitespace changes do not change this identifier. This is a content identity for auditing and cannot authenticate a remote producer.

The report has measurements, samples and interpretations. Each sample has its original index, measurements and hops. Each hop has measurements, calculations and interpretations. Measurements copy values from the recording and carry one evidence reference. Nullable observations remain null. Calculations contain name, unit, nullable numeric value, rule_id, evidence, limitations and an unavailable_reason when value is null. A zero value is available evidence, distinct from null. Derived values use floating-point numbers; nanosecond calculations are displayed as milliseconds in text output. Original integer observations remain in the recording.

Interpretations contain rule_id, text, evidence and limitations. Every evidence reference has document set to recording or report and pointer set to a JSON pointer. References to calculations can be followed to their underlying event evidence. No generated interpretation is an unlabelled measurement.

## Interval rules

The dns_resolution calculation requires exactly one successful DNS start/completion pair. Its duration describes the resolver operation and does not establish that a network query occurred. Missing, failed, multiple or incomplete resolver operations produce an unavailable duration.

The tcp_connect calculation uses the start/completion pair named by winning_connect_attempt. Failed or losing dial attempts cannot supply its duration. A winning identity requires a unique successful acquisition of the matching remote address, after the successful connection completion.

The tls_handshake calculation requires one successful TLS pair inside the winning connection lifecycle, after the winning dial completes and before connection acquisition. Incomplete or ambiguous TLS evidence remains unavailable. No TLS events on an HTTP hop is an unavailable TLS measurement, rather than a fabricated zero.

The connection_setup interval runs from hop start to unique successful acquisition of the winning connection. It includes resolver, dial, TLS and scheduling effects where present. If observed TLS negotiation cannot be associated with completed setup, setup remains unavailable. DNS, TCP and TLS intervals are explanatory details within setup; they must not be added to it.

The first_byte_wait interval starts at a unique successful request_written event and ends at a unique first_byte event. If the first byte precedes completed request writing, the interval remains unavailable. Client waiting does not identify origin processing time, database work, congestion or any other unseen cause.

The response_read interval starts at first_byte and ends at successful EOF. It includes response-header reading and client scheduling alongside body reading. For an incomplete response, it ends at hop termination and is explicitly labelled incomplete, including failure handling. It is unavailable when first-byte evidence is absent or ambiguous.

The observed_payload_rate calculation divides complete body bytes by the positive response_read interval. Incomplete bodies and zero-length time intervals yield no rate. This is an observed payload rate over the response interval, not link capacity or a pure body-transfer benchmark.

## Interpretation rules

largest_interval_v1 selects a unique largest interval among connection_setup, first_byte_wait and response_read only when all are available, the body is complete and the intervals can be ordered without overlap. Ties produce no winner. The wording is restricted to the compared intervals in that hop. It makes no claim that those intervals cover all elapsed time or establish a root cause.

reported_failure_v1 explains the recorder's stable failure category. incomplete_response_v1 explicitly withholds a completed-payload rate. incomplete_batch_v1 identifies fewer observed samples than requested. query_redaction_v1 explains that matching saved URLs cannot establish identical original query values. All rules cite supporting fields or calculations.

Both output formats have a 32 MiB serialized/rendered size limit. Recordings retain their separate 16 MiB input limit. Explanations are per-hop observations and calculations; they do not yet provide medians, cross-run verdicts or statistical confidence claims.
