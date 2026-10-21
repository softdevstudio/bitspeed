//go:build linux || darwin

package transfer

import (
	"os"
	"testing"
)

// exposeFile gives another account access; unexposeFile restores privacy.
func exposeFile(t *testing.T, path string) {
	t.Helper()
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
}
func unexposeFile(t *testing.T, path string) {
	t.Helper()
	if e := os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
}

// unseal and reseal let a test rewrite a sealed spool file.
func unseal(path string) { os.Chmod(path, 0600) }
func reseal(path string) { os.Chmod(path, 0400) }

// breakSeal leaves a sealed file in a state reuse must refuse.
func breakSeal(t *testing.T, path string) {
	t.Helper()
	unseal(path)
}
