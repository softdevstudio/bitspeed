# Inspect Comparisons

Compare reads two saved recordings and applies the same interval definitions as Explain. It never accesses the network or changes source observations.

```sh
bitspeed-inspect compare before.json after.json
bitspeed-inspect compare --format=json before.json after.json
bitspeed-inspect compare --format=json before.json - < after.json
```

At most one input can be stdin. A valid comparison exits successfully even when its verdicts are inconclusive or blocked. Invalid recordings, arguments and output errors exit nonzero. This command does not yet provide a CI regression threshold or fail-on-slower option.

## Statistics and cohort

Each group reports requested, recorded, completed, failed, cancelled and unattempted sample counts, a canonical source SHA256 and its operator-supplied location label. Location labels are metadata, not verified geography.

Per-hop calculations cover DNS resolution, winning TCP dial, TLS handshake, connection setup, first-byte waiting, response reading and observed payload rate. Phase definitions come from inspect-explain-v1. Each statistic reports available, unavailable and excluded counts among recorded samples. A sample without that hop counts as unavailable. Response-reading and payload-rate statistics exclude incomplete bodies; available connection and first-byte intervals from failed samples remain descriptive values. Count plus unavailable plus excluded equals the number of recorded samples. Any failed, cancelled or unattempted sample withholds change verdicts across the comparison, avoiding a success-only performance claim.

Median is the middle sorted value, or the arithmetic mean of the two middle values for an even count. Spread is the minimum-to-maximum observed range. These are descriptive sample statistics, not confidence intervals. Candidate-minus-baseline is the signed absolute change; percentage change uses the baseline median as denominator. A zero baseline yields a null percentage, never infinity or a fabricated zero percentage. Numeric differences remain visible when interpretation is blocked.

When incomplete hops exist, partial_response_read shows their response windows in a separate descriptive cohort. Completed bodies are excluded from that cohort, and incomplete hops without usable first-byte timing count as unavailable. Its medians and ranges never mix with completed-response statistics. It has no delta, percentage or change verdict; status is descriptive_only. This preserves useful failure timing without presenting an interrupted response as a completed transfer.

## Comparability checks

Checks have a severity of blocking, caution or information, and scope all or response. Response scope covers first-byte waiting, response reading and observed payload rate. Connection phases can remain interpretable when response metadata differs. Every check cites the fields it used.

Different normalized request targets or redirect chains block all change verdicts. Chain checks include variability within each recording, not just differences between files. Target normalization lowercases scheme and hostname, removes a default HTTP/HTTPS port and maps an empty path to /. It preserves path escaping and query structure, since servers can distinguish those forms. IPv6 address text is normalized while the case-sensitive zone/interface suffix is preserved. Request method, connection/decompression policy, redirect limits, timeouts and sampling interval must match. Different recorder versions create a caution because equivalent instrumentation behavior is not established.

Protocol differences or unknown protocol block all change verdicts. Changed, variable or missing status, payload size or content encoding blocks response verdicts. Non-2xx status creates a response caution even when identical in both recordings. Thus a quickly returned error is not presented as an improved successful response. Redacted query values also block response verdicts: matching saved URLs cannot prove matching original query values. Remote-address and location-label differences are informational; they do not establish what caused a difference.

The recorder now retains a sanitized Content-Encoding classification: identity, gzip, br, deflate, zstd, compress or other. An absent header is identity; unfamiliar or compound values become other, and their raw text is discarded. Null and other are insufficient to establish comparable response encoding. Older development recordings lacking the required content_encoding field need regeneration. No other response headers are retained.

Metadata equality and equal payload sizes cannot prove identical response content, workload or outside conditions. That limitation always appears in the report.

## Verdict rules

A missing median produces unavailable. A blocking check in scope produces not_comparable. Cautionary checks, fewer than three values per group, or any unavailable or excluded phase values produce inconclusive. Three is a practical reporting floor, not a claim of statistical confidence. Unequal group sizes are allowed when each meets the floor and has full coverage; both sizes remain visible.

Otherwise equal medians produce equal_medians, explicitly without establishing unchanged performance. Different medians whose observed ranges overlap remain inconclusive. Only nonoverlapping ranges produce lower_in_samples or higher_in_samples. These statuses describe that phase in the collected samples and do not assert causation or a population-level improvement. Lower latency and higher payload rate have different practical meanings, so the report uses neutral lower/higher wording and makes no overall faster-transfer verdict.

## Report contract and evidence

Comparison schema_version is 1, rule_set is inspect-compare-v1 and explanation_rule_set identifies the shared explanation rules. The report has baseline and candidate summaries, global checks, per-hop checks and calculations, and common limitations. Each phase calculation contains a rule_id, evidence, baseline/candidate statistics, nullable absolute_change and percent_change, and a separately labelled interpretation with status, rule_id, text, evidence and limitations.

Evidence document names are baseline_recording, candidate_recording, baseline_explanation, candidate_explanation and comparison. Pointers are JSON pointers into those documents. Explanation documents are deterministic results of Explain on the respective source recordings; their canonical recording SHA256 matches the group summary. Statistics cite source sample arrays and the individual available derived values. Interpretation references point to statistics and comparability checks. Output is bounded by the common 32 MiB report limit, and text quotes untrusted values for terminal safety.

## CI gate (Inspect 0.2.0)

`compare --fail-if-slower=INTERVALS` evaluates a gate over the finished comparison without changing it; stdout text or JSON is identical with and without the gate. For each gated interval in each hop, the gate reads only the verdict status: higher_in_samples is regressed when the absolute change is at least --min-change (nanoseconds internally) and, if --min-percent is above zero, the percent change is at least that value; otherwise it passes. not_comparable and unavailable are undecided. inconclusive, equal_medians and lower_in_samples pass. A gated interval that appears in no hop is undecided with verdict missing. The overall outcome is regressed if any finding regressed, else undecided if any finding is undecided and --allow-undecided is not set, else pass. Exit statuses are 0 pass, 3 regressed, 4 undecided and 1 for errors including unknown interval names. observed_payload_rate and partial_response_read can't be gated: a lower rate is the slower direction, and partial reads carry no change verdict. The library form is inspect.ParseGateMetrics and inspect.EvaluateGate.
