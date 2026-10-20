# Inspect Releases

Inspect is versioned independently of the future Transfer tool. Its constant is inspect/version.go, its tags are inspect/vX.Y.Z, and stable archive names start with bitspeed-inspect. The first release is prepared as 0.1.1 after independent review and the gates below. Transfer has no release or binary yet.

## Automated gates

A successful CI push on main triggers Release Inspect. The plan step verifies the CI run's event, repository, exact source SHA and current main head through the API. Pull-request runs cannot enter this privileged release path. All jobs check out the same verified source. A development version runs packaging and native checks without publishing. A stable version with an already published release is skipped.

Five CGO-disabled archives are built with the exact Go version pinned in go.mod: Linux amd64/arm64, macOS amd64/arm64 and Windows amd64. Native archive verification and CLI smoke run on Linux amd64, macOS arm64 and Windows amd64. Linux arm64 and macOS amd64 are cross-built; no native runtime coverage is claimed for those two targets. Unix smoke includes SIGTERM cancellation; Windows smoke uses request timeouts rather than assuming Unix signal delivery.

Only a publisher that sees all five packaging jobs and all three native jobs succeed can publish. It verifies archive provenance and clean source, records verification metadata and SHA256SUMS, creates or resumes a draft at the exact source tag, uploads assets, and publishes latest after upload verification. Published releases are immutable to this automation. Retry a failed release run instead of manually publishing or moving a tag.

Every archive includes the executable, README, Apache license and notice, reference documentation, BUILD_INFO.json and Go toolchain licensing material under third_party/go. Build metadata records tool version, source SHA, target, toolchain and whether native smoke is required. Licensing files are copied from the exact building toolchain, including bundled vendor notices. SHA256SUMS covers all published archives and verification metadata.

## Stable downloads and coexistence

Website download links use releases/latest/download/bitspeed-inspect-{os}-{arch}.tar.gz, or .zip for Windows. Archive filenames carry no version; BUILD_INFO.json carries provenance. The latest release must retain every released product's five archives.

When Inspect publishes after Transfer exists, the publisher carries forward all five existing Transfer archives only after checking them against the previous latest release's SHA256SUMS. An incomplete or corrupted carry-forward set blocks publication. The future Transfer publisher must use the same release concurrency group and mirror this process for Inspect, preserving stable download URLs. Do not create placeholder archives for an unreleased product. Each carried archive retains its own internal BUILD_INFO.json; the current Inspect verification file describes only Inspect's new build.

The final verification checks all five Inspect asset names, SHA256SUMS and a downloaded native executable, using the release URL and stable latest download path. Site verification waits until the website exists.
