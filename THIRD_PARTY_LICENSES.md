# Third Party Licenses

The source has no external Go module dependencies. Distributed executables link the Go runtime and standard library, including applicable bundled components. Each archive contains the exact building toolchain's LICENSE, PATENTS, VERSION and available license/notice files under third_party/go, preserving their original paths.

BUILD_INFO.json records the Go version and source revision used for that archive. Go itself is covered by its BSD-style license, with additional notices for bundled components. GitHub Actions are build infrastructure and are not included in the runtime binary.
