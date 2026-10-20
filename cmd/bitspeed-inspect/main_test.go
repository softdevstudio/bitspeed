package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"help"}, {"--help"}} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), args, &out, &errOut); err != nil || out.Len() == 0 {
			t.Fatalf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		nil, {"unknown"}, {"--version", "extra"}, {"record"}, {"explain"}, {"compare"},
		{"fixture", "--listen", "0.0.0.0:0"}, {"fixture", "--listen", "localhost:0"},
		{"fixture", "--header-delay=-1s"}, {"fixture", "extra"}, {"fixture", "--bad"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			if run(context.Background(), args, &out, &errOut) == nil {
				t.Fatal("expected explicit failure")
			}
		})
	}
}

func TestFixtureShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if err := run(ctx, []string{"fixture"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "http://127.0.0.1:") {
		t.Fatal("missing listener URL")
	}
}
