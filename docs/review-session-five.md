# Inspect release review

An independent reviewer checked the recorder, import/report privacy boundaries, packaging, exact-commit CI gates and publishing privileges using Go 1.27.3. Unit tests, vet, race tests, Python guard tests and fuzz seeds passed. No confirmed release-blocking defect was found.

Review follow-ups requested tests for missing native gates, draft retry, uploaded corruption and future Transfer asset carry-forward. These tests were added. Publishing re-downloads the complete draft asset set and compares every hash before making it public. The final gate still requires actual public latest/tag downloads and a downloaded native executable smoke check; passing local mocks does not establish that result.

Release status and the next action are maintained in AGENTS.md.

The first live publisher run exposed a draft lookup 404 after all package/native gates passed. No release became public. Authenticated release-list lookup now finds drafts and paginates; its regression test passes with the existing publisher guards. The unpublished 0.1.0 tag is preserved, and the first public candidate is 0.1.1.

The targeted draft-discovery fix passed a fresh independent review with all nine Python guards. The controlled demo was independently executed and reviewed without a blocker. Public release 0.1.1 then passed fresh Linux/macOS CI, all five package builds and all three native archive gates. Tag/latest download checks, clean archive provenance, checksum validation and a downloaded Linux executable smoke check passed. Results are committed under test/results/session-five-release; samples were generated with that downloaded executable.
