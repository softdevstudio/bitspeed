package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--help"}, {"help"}, {"serve", "--help"}, {"send", "--help"}, {"status", "--help"}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil || out.Len() == 0 {
			t.Fatal(args, err)
		}
		if err := run(args, brokenWriter{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("lost output error", err)
		}
	}
	var out bytes.Buffer
	if err := run([]string{"--version"}, &out); err != nil || out.String() != "bitspeed-transfer 0.0.0-dev\n" {
		t.Fatal(out.String(), err)
	}
}

func TestOperationalCommandsFailClosed(t *testing.T) {
	for _, command := range []string{"serve", "send", "status"} {
		for _, tail := range [][]string{nil, {"--root=/private/canary", "--token=SECRET"}, {"https://private.invalid/SECRET"}, {"--help", "SECRET"}} {
			args := append([]string{command}, tail...)
			var out bytes.Buffer
			err := run(args, &out)
			if !errors.Is(err, errNotImplemented) || out.Len() != 0 || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("operation must fail without output or reflected inputs", args, err)
			}
		}
	}
}

func TestInvalidCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"--version", "extra"}, {"--help", "extra"}, {"help", "extra"}} {
		var out bytes.Buffer
		if err := run(args, &out); err == nil || out.Len() != 0 {
			t.Fatal(args, err)
		}
	}
}
