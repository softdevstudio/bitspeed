package main

import (
	"bytes"
	"errors"
	"github.com/softdevstudio/bitspeed/transfer"
	"io"
	"strings"
	"testing"
)

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--help"}, {"help"}, {"serve", "--help"}, {"send", "--help"}, {"status", "--help"}, {"clean", "--help"}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil || out.Len() == 0 {
			t.Fatal(args, err)
		}
		if err := run(args, brokenWriter{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal("lost output error", err)
		}
	}
	var out bytes.Buffer
	if err := run([]string{"--version"}, &out); err != nil || out.String() != "bitspeed-transfer "+transfer.Version+"\n" {
		t.Fatal(out.String(), err)
	}
}

func TestOperationalCommandsFailClosed(t *testing.T) {
	for _, command := range []string{"serve", "send", "status", "clean"} {
		for _, tail := range [][]string{nil, {"--root=/private/canary", "--token=SECRET"}, {"https://private.invalid/SECRET"}, {"--help", "SECRET"}} {
			args := append([]string{command}, tail...)
			var out bytes.Buffer
			err := run(args, &out)
			if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "SECRET") {
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

func TestPreparationOptionsRejectBeforeIO(t *testing.T) {
	for _, tail := range [][]string{nil, {"--source=SECRET"}, {"--spool=SECRET"}, {"--spool=SECRET", "--source=SECRET", "--reuse=SECRET"}, {"--spool=SECRET", "--source=SECRET", "--token=SECRET"}, {"--spool=SECRET", "--source=SECRET", "--chunk-size=0"}, {"--spool=SECRET", "--spool=SECRET", "--source=SECRET"}} {
		var out bytes.Buffer
		err := run(append([]string{"send", "--prepare-only"}, tail...), &out)
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid flags leaked or succeeded", err)
		}
	}
}
