//go:build !linux && !darwin && !windows

package transfer

import "testing"

// Transfer is unsupported here; these keep the package's tests compiling.
func exposeFile(t *testing.T, _ string)   { t.Skip("unsupported platform") }
func unexposeFile(t *testing.T, _ string) { t.Skip("unsupported platform") }
func unseal(string)                       {}
func reseal(string)                       {}
func breakSeal(t *testing.T, _ string)    { t.Skip("unsupported platform") }
