//go:build windows

package transfer

import (
	"os/exec"
	"testing"
)

func icacls(t *testing.T, args ...string) {
	t.Helper()
	if out, e := exec.Command("icacls", args...).CombinedOutput(); e != nil {
		t.Fatalf("icacls %v: %v\n%s", args, e, out)
	}
}

// Everyone (S-1-1-0) stands in for any other account.
func exposeFile(t *testing.T, path string)   { icacls(t, path, "/grant", "*S-1-1-0:(R)") }
func unexposeFile(t *testing.T, path string) { icacls(t, path, "/remove:g", "*S-1-1-0") }

// Sealed files carry no attribute on Windows; see docs/windows-persistence.md.
func unseal(string) {}
func reseal(string) {}

// On Windows a sealed file is refused when it stops being private.
func breakSeal(t *testing.T, path string) {
	t.Helper()
	exposeFile(t, path)
}
