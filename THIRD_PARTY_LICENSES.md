# Third Party Licenses

The current source uses only the Go standard library and has no external Go module dependencies. Go runtime and standard library code linked into future binaries is covered by the Go distribution's BSD-style license and associated notices. Release packaging must include the license and applicable notices from the exact Go toolchain used to build those binaries.

GitHub Actions used by CI are build infrastructure, not vendored source or runtime dependencies.
