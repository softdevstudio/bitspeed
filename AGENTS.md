# BitSpeed

BitSpeed houses two independent tools: Inspect explains HTTP transfer timing and compares runs; Transfer moves a file in verified chunks and resumes after interruption.

Domain: bitspeed.org
Code repository: softdevstudio/bitspeed
Site repository: photozeitgeist/bitspeed.org
Language: Go
Code license: Apache 2.0
Contact: info@bitspeed.org

## Low maintenance workflow

YK supplies the go-ahead, language choice, repositories and access. The agent handles code, tests, documentation, CI, automated releases, site work and upkeep. Answer questions without treating them as instructions to implement. The development plan below was approved for recording on October 20, 2026. Update Next when YK approves a new next step; record actual progress without inventing approval.

Work on main only. This AGENTS.md is the sole plan and session handoff file. Do not create ROADMAP.md, RESUME.md or a competing status file. At the end of each roughly two-to-three-hour session, update Where we are with the work completed, exact checks and results, blockers and the next starting point. Pause and report at the session boundary.

Both tools live in this supplied code repository. Keep separate commands at cmd/bitspeed-inspect and cmd/bitspeed-transfer, separate implementation packages and independent tool versions. Neither executable may require the other. Do not create additional repositories. Avoid a shared runtime abstraction until a concrete need appears.

README.md is the manual and plain-language explainer. Reference documentation belongs in docs/ as Markdown. No Word or PDF. Keep LICENSE, NOTICE and THIRD_PARTY_LICENSES.md current. Do not mix in materials from earlier projects. Do not discuss domain sale or domain value anywhere in project materials.

Write in plain words with varied sentence length. Avoid em dashes, formulaic contrasts, decorative vocabulary and forced lists of three.

## Screening record

Real problem: developers can observe a slow HTTP request without knowing which measured interval changed, and interrupted file transfers can waste work by restarting. The tools address those problems separately.

Plain-language technology: Inspect records request events and compares saved samples using explicit rules. Transfer saves verified pieces and asks the receiver which pieces remain after restarting. Neither tool needs an LLM or hosted service.

Differentiation: Inspect separates observed evidence, calculations and labelled interpretations, with comparability checks across runs. Transfer centers on verifiable recovery and precisely documented completion guarantees. These are product aims to prove through tests; no uniqueness claim is established.

Money: no revenue model is approved. Candidate paths are paid support and integration work around a useful open-source tool. Demand and defensibility remain unvalidated. Initial delivery is a standalone open-source toolset, with no required paid infrastructure.

## Code and completion standard

Write tests for every operation, including failures that challenge each tool's main promise. Inspect tests must challenge timing attribution and comparison conclusions. Transfer tests must challenge integrity under SIGKILL, repeated requests, disk failures and restarts. Record reproducible results in test/results through scripts/record-results.sh. Site performance numbers must come from those recorded results and include relevant conditions.

Run CI on every push to main on Linux and macOS. Include go vet and race-enabled tests where supported. Before calling an implementation complete, obtain an independent review in a separate task that has not seen the implementation conversation, fix findings, rerun the relevant checks and record the results again. This independent review requirement authorizes a separate review task. Do not mark a review complete if it was only self-review.

Release automatically through .github/workflows/release.yml after CI succeeds for the exact source commit. Maintain independent version constants, proposed as inspect/version.go and transfer/version.go, and independent tags inspect/vX.Y.Z and transfer/vX.Y.Z. Adapt the workflow to publish only the tool whose version changed. Do not let an unfinished Transfer block Inspect. No placeholder release or version bump should publish unimplemented code.

Build Linux amd64 and arm64, macOS amd64 and arm64, and Windows amd64 archives with version-free names including the tool name, plus SHA256SUMS. Each binary supports --version. Smoke-test the native Linux, macOS and Windows archives and accurately label cross-built variants without runtime coverage. The workflow packages appropriate license files, tags only after all required checks succeed and uses the repository token to publish. Never ask YK to publish a release. Because this is one repository with two release families, verify how releases/latest selects the latest product release and ensure every referenced product archive remains available at the stable download URLs; publish both released tools in the latest release if that is the chosen solution. Do not expose links to an unreleased tool.

## Site standard

Use the unchanged Hugo Launch template from an identified authorized template source. The user supplied the existing Launch template and authorized content replacement on October 20, 2026; preserve its theme/archetype files byte for byte. Change content and configuration values only. No new layouts, CSS, _index.md files or public/ folder. Provide about, contact, quick start, download and reference pages, with distinct product instructions. Contact is info@bitspeed.org.

Prepare launch, category explainer, testing and name posts. Every post uses title in double quotes with plain title text, date only, draft: false and tags. Omit description and categories. Do not repeat the title in the body. Use normal title casing and lead with the subject. When a post includes an image, use an HTTPS URL in featured_image and inline after the second or third paragraph. Store project illustrations in static/images and use their final HTTPS addresses. Deliver requested post drafts as downloadable Markdown or commit them to content/posts when authorized. Download links use releases/latest/download/{archive} and must be checked against actual assets.

## Development plan

The sequence below is the approved project plan adapted to one code repository and the low maintenance workflow. Session estimates are planning ranges and correctness gates take precedence.

Inspect scope and design

The first version supports HTTP and HTTPS GET requests, follows a bounded number of redirects and records each redirect hop separately. It streams response bodies to a discard sink with bounded memory. It records observed body bytes and explains whether decompression was enabled; these are not claimed to be total network bytes. Requests have explicit connection, header, body and overall timeout policies. Errors and partial transfers remain valid observations with an incomplete status. Unsupported protocols and invalid inputs fail clearly.

The initial commands are bitspeed-inspect record, bitspeed-inspect explain and bitspeed-inspect compare. A record command writes a versioned JSON recording; explain reads it and renders a terminal report; compare reads two recordings and renders a comparison. Human output goes to stdout, diagnostics to stderr, and JSON output remains machine-readable. There is no LLM, account, cloud service, background agent or hosted probe network.

The record format separates observations, derived calculations and interpretations. Observations include timing events, status, remote address, negotiated HTTP protocol, connection reuse, redirect hops and observed payload size. Calculations include elapsed intervals, medians, spread and percentage changes. Interpretations cite the observation fields or calculations supporting them, name the rule that produced them and state limitations. Missing events are represented as unavailable, never silently converted to zero. Preserve raw event relationships because concurrent connection attempts and protocol behavior may prevent an honest additive waterfall.

Measure from a monotonic clock within each request and retain wall-clock timestamps only for identifying recordings. Define all interval boundaries before implementing report language. In particular, label waiting for the first response byte precisely and never equate it with origin processing time. Record connection reuse explicitly. Fresh-connection runs are the initial comparison default; warm-connection runs, if included in the first release, form a separately labelled mode. Do not claim to flush operating-system DNS caches or eliminate external caching.

Default comparisons collect ten sequential observations per group, with sample count and interval configurable. Report successful and failed attempts, median and a clearly defined spread; preserve every attempt. Compare like-for-like phases only when available. A verdict describes the collected samples and does not imply a statistically established general improvement. Zero baselines and insufficient samples have explicit handling. Different locations are operator-supplied labels on independently collected recordings, not verified geographic measurements.

Comparison checks cover method, normalized target identity, redirect chain, relevant request configuration, body size, compression, protocol and connection policy. Changes in remote address and status are visible context. Classify differences as blocking, cautionary or informational with documented rules. An overall comparability warning must not suppress useful phase measurements, but conclusions must narrow accordingly. Sensitive headers are never persisted by default, URL credentials are rejected, and sensitive query values are redacted unless explicitly opted in. Redirects must not forward credentials indiscriminately.

Inspect implementation sessions

Session one establishes the CLI skeleton, recording schema, timing definitions and controlled local fixtures. Fixtures include delayed response headers, a slowly streamed body, redirects, empty bodies, early disconnects and timeouts. The deliverable is a written contract and a runnable fixture, with tests for parsing and schema handling where those affect compatibility.

Session two implements the streaming recorder and HTTP tracing. Handle multiple connection attempts, TLS failures, redirect hops, cancellation and incomplete bodies without fabricating missing phase durations. The exit gate is bounded-memory recording with correct event ordering and meaningful partial output across controlled failures.

Session three implements deterministic single-record explanations. Validate delayed headers and slow bodies separately, and ensure every interpretation points to evidence. A client-only measurement must never conclude that a database, server CPU or particular network device caused a delay. The exit gate is a report that is useful when evidence is incomplete.

Session four implements repeated recordings and before-and-after comparison. Add comparability rules, medians, spread, absolute and relative changes, and inconclusive verdicts. Tests must cover changed payloads, mixed failures, connection reuse, zero baselines and a variable fixture where a strong improvement claim would be unjustified. Do not assert fragile exact wall-clock durations in tests; use controlled differences and tolerances.

Session five hardens the tool and prepares its independent release candidate. Run relevant unit, integration and race checks, verify cancellation and streaming with a generated large body, test packaged binaries on supported environments, and document limitations. Provide sample recordings and a short reproducible demo. Initial proposed platforms are Linux amd64 and arm64 and macOS arm64 and amd64; mark any platform without runtime verification accordingly. The release gate is a reproducible record, explain and compare workflow plus correct failure reporting. Inspect is useful and releasable even if Transfer is never built.

Transfer scope and persistence contract

Transfer starts with one source file and one destination, using one sequential chunk upload at a time. Its commands are provisionally bitspeed-transfer serve, send and status. The sender and receiver communicate over HTTP with authenticated access; off-machine use requires TLS with certificate verification, either directly or through a documented deployment configuration. Plain HTTP development mode is restricted to loopback by default. Exclude directory synchronization, delta patches, compression, parallel chunks and peer discovery from the first version.

Use a fixed chunk size within a transfer, SHA-256 chunk hashes and a full-file SHA-256 digest. Generate an immutable source snapshot in a local spool before creating the transfer manifest. This deliberately consumes extra disk space and startup time but makes resume semantics concrete even if the original file changes later. Check capacity, expose preparation progress and fail cleanly if the snapshot cannot be completed. Copying a file that is actively being modified is not promised to capture an application-consistent point in time; require a stable source during snapshot preparation and detect observable changes. Subsequent sends and resumes use the completed snapshot.

The manifest identifies the protocol version, transfer ID, total size, chunk size, ordered chunk hashes and full digest. The receiver creates its own session directory beneath a configured storage root; the sender cannot provide arbitrary filesystem paths. Bound manifest size, chunk size, file size, concurrent sessions and aggregate storage. Authenticate status and mutation endpoints, avoid logging secrets and use constant-time token comparison where appropriate.

For the first implementation, store each accepted chunk as its own file. Write a temporary chunk, verify its length and digest, synchronize its contents, atomically publish it and synchronize the containing directory on supported filesystems before acknowledging it. A durable acknowledgement means the implementation completed its documented persistence sequence; filesystem and hardware limits must be stated. On restart, discover and revalidate stored chunks instead of trusting a progress counter. A valid published chunk can be recovered even if the previous response never reached the sender. Duplicate uploads are idempotent, and incomplete temporary files are discarded safely.

Finalization assembles chunks into a temporary file on the destination filesystem, checks the full digest, synchronizes the completed data and publishes without silently replacing an existing destination. Define a collision-safe, platform-appropriate publication operation and recovery behavior before coding it. Persist finalization state so a lost success response does not produce a second file. Keep chunks until completion is durably recorded, then clean them up according to an explicit retention policy. Budget receiver disk space for both stored chunks and the assembled file; the sender snapshot adds its own storage cost.

Transfer implementation sessions

Session six writes the protocol, state transitions, persistence ordering and error semantics, then creates sender and receiver skeletons. Define preparing, uploading, finalizing, completed and failed states, plus restart behavior at every transition. Specify empty files, partial last chunks, duplicate requests, corrupt state and destination conflicts. The exit gate is a protocol precise enough to test without guessing.

Session seven implements source snapshot preparation, streaming hashes and manifest creation. Memory stays bounded independently of file size. Verify insufficient space, cancelled preparation, source changes observed during snapshotting and snapshot reuse after sender restart. No network transfer begins with a half-created manifest.

Session eight implements authenticated session creation, sequential chunk upload and receiver storage. Exercise incorrect lengths and hashes, malformed manifests, authorization failures and storage limits. The exit gate is that corrupted or incomplete chunks are never reported as verified progress.

Session nine implements resume and crash recovery. Kill the sender and receiver during chunk writes, after durable publication and before acknowledgement. Restart both, query receiver state and transmit only chunks still missing or invalid. Inject disk write and synchronization failures, not merely network disconnects. Recovery must not acknowledge progress it cannot substantiate.

Session ten implements assembly, full-file verification, safe publication and cleanup. Test crashes during assembly and around final publication, existing destination conflicts, empty files and damaged stored chunks. A successful exit must refer to one complete verified destination file. Lost final responses must be recoverable through status without repeating publication.

Session eleven prepares the Transfer release candidate. Run end-to-end interruption tests, race checks, malformed protocol tests and a large-file memory check. Document disk usage, persistence guarantees, authenticated deployment and cleanup. Demonstrate transfer, interruption, process restarts, resume and a matching final digest. Release remains blocked by any unresolved integrity or publication issue, regardless of schedule.

Website and delivery

Session twelve prepares BitSpeed.org content in photozeitgeist/bitspeed.org after the unchanged Hugo Launch template source has been identified. The homepage introduces Inspect and Transfer as separate tools, each with installation instructions, examples, limitations and release links. Inspect gets a short controlled comparison demo; Transfer gets an interruption-and-resume demo. A hosted interactive service is unnecessary. Follow the supplied site's existing template and structure, use plain title text and provide a contact page with info@bitspeed.org. Use Apache 2.0 for the code, with current LICENSE, NOTICE and THIRD_PARTY_LICENSES.md. Website copy must not discuss licensing of the private website repository.

The initial estimate is twelve focused sessions, approximately twenty-four to thirty-six working hours for narrow prototypes and release-candidate preparation, rather than a promise of production readiness. Transfer durability and platform behavior are the main reasons this could take longer. Inspect receives its own completion checkpoint after session five; Transfer receives one after session eleven. Optional HTML reports, CI regression thresholds, parallel transfers and shared recording imports come only after the independent tools prove useful. The first implementation step is Inspect session one. Releases must follow the automated workflow and independent review gate below.

## Where we are

October 20, 2026: Session eleven hardening is implemented and independently reviewed; clean candidate CI/package/native gates remain in progress. Transfer remains 0.0.0-dev and cannot publish. New actual-executable interruption tests kill sender and receiver at partial chunk, accepted chunk response and final response boundaries, restart from the same snapshot/ID with the original source removed, verify one complete output/stable receipt and retain verified chunk identity. Built TLS listener checks verify a locally trusted certificate and authenticated full delivery. The public Go client test verifies trusted HTTPS and rejects unrelated certificates and hostname mismatch. A sparse 64 MiB plus 17-byte assembly/full-verification allocation test measured roughly 2.6 MB locally against a 16 MiB allocation gate; this is a recorded environment result, not a maximum-file throughput promise.

Release Transfer is a separate workflow using the shared product-selected builder, independent tags and checksum-verified other-product carry-forward under the same concurrency group as Inspect. Both products' release guards run, twenty tests total. Linux amd64/macOS arm64 native archives must pass delivery, executable interruption and TLS checks. Linux arm64/macOS amd64 remain cross-built only. Windows amd64 has native version/help and unsupported-operation checks only; operational token/spool/send/status/receiver functionality remains unsupported. No Windows delivery coverage is implied. See docs/transfer-releases.md and docs/review-session-eleven.md. Independent review ran repository race tests/vet, all twenty release guards and actual executable smokes with no blocking findings. Actual clean CI/package/native results are still required before a stable version is selected. Source fingerprints exclude ignored generated caches.

October 20, 2026: Transfer session ten is complete. Normal send from a prepared snapshot stages missing chunks, assembles and verifies the complete file, publishes one fixed no-replace destination and durably caches a matching completion receipt locally before transferred true. Upload-only remains an explicit incomplete staging mode. Finalizing/completed/failed journals and receipt recovery are implemented; PUT and admission mutations reject terminal failures. Completed retries use the verified receipt even when retained chunks are missing. Transfer remains 0.0.0-dev and unreleased.

Reservations now use three times file size plus 32 MiB for valid chunks, bounded corrupt evidence and the assembly. ready and the output are hard links. All retained sessions, including earlier two-copy sessions, are reconstructed under this rule; insufficient configured policy fails startup without rewriting bindings or deleting evidence. Finalization checks physical free space again. No automatic cleanup runs. Exact persistence, migration, restart and offline retention behavior is in docs/finalization.md and docs/transfer-v1.md.

Process tests kill at finalizing journal rename, assembly write, ready rename, output link, receipt write/rename and completed journal rename. Recovery revalidates the witness and rejects an injected output-directory sync failure before successful restart. Tests cover lost final responses, empty and partial files, damaged/missing chunks, full-file digest disagreement despite valid chunk hashes, foreign equal-byte destinations, malformed receipts, missing witnesses, damaged output, strict completion response binding, capacity/cancellation and receiver/sender write/short/corrupt-write/close/sync/rename/link failures. No completion is returned across failed persistence gates. Native CLI smoke verifies actual destination digest and local receipt, restarts both receiver and sender processes, checks one stable receipt/output, and rejects later destination damage.

Local Go 1.27.3 formatting, nine Python release guards, vet, repository-wide race tests, both builds and both executable smoke workflows passed through scripts/record-results.sh. Independent review found and confirmed fixes for completed retries attempting chunk uploads, first-restart errors after durable reset/failure, malformed receipts in failed sessions and damaged receipt-backed finalization recovery. Final re-review found no remaining blockers in session-ten scope. See docs/review-session-ten.md and recorded source/environment fingerprints in test/results. Check this commit's Linux/macOS CI after push. Session-nine commit 264fe89f6d0719c84d1c1ce14e9d1a193ae952ad passed Linux/macOS CI 39387349411; release run 39387560214 skipped the immutable already-published Inspect version.

The user's additional instruction brought the website pass forward. Site commit ce8877afb119d34d2c518910bb57a6b68285a29b in photozeitgeist/bitspeed.org replaces all legacy project copy/images with BitSpeed material. All nineteen theme/archetype blob hashes are unchanged and major folders retained. The live https://bitspeed.org homepage, seven guide/product/contact pages and new illustration passed HTTPS checks; the old project image returns 404. Hugo build, internal links, substantive-page single H1, frontmatter, release coverage and independent content review passed. Download links expose only released Inspect assets. Contact is info@bitspeed.org in header/footer navigation. Evidence and scope are in docs/site-content-review.md and test/results/site-session-eight; optional live smoke runs with BITSPEED_CHECK_SITE=1 through the normal result recorder. Ordinary CI does not depend on the public site.

Inspect 0.1.1 remains released from 6df6f71227b3728905ec7a5cf15e7f2d82ae6f49 with five archives and three native checks. Its public asset verification is in test/results/session-five-release. Session-seven commit 447a3fa36cbcf3d8a4194be37a9ee3b56e809a13 passed Linux/macOS CI 39380946871; release run 39381089917 skipped the immutable published version. The unpublished Inspect 0.1.0 tag/draft remains reserved. No Transfer assets are published.

## Next

Finish the already-authorized session eleven. First commit the 0.0.0-dev hardening/workflow changes and verify exact-source Linux/macOS CI, five clean Transfer archives and three accurately scoped native jobs, with publish skipped. Only after those and independent review pass, select a fresh stable Transfer version and let automatic exact-commit CI/native gates publish it. Verify public assets, combined checksums, downloaded native binary/provenance and preserved Inspect latest URLs. Then update website content/download links against actual assets while preserving template hashes and major structure, record proof and update this handoff. Never manually publish or move an existing version tag. If a release gate fails, fix it and verify; no placeholder release or unsupported Windows delivery claim.

Keep Inspect independently releasable and keep Transfer unpublished until the independent release gates above pass. Preserve the live website template and current release status; only update content when verified behavior changes. Do not rewrite Inspect 0.1.1 or create placeholder Transfer archives.
