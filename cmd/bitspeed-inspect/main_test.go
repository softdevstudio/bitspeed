package main

import (
	"bytes"
	"context"
	"github.com/softdevstudio/bitspeed/inspect"
	"github.com/softdevstudio/bitspeed/internal/fixture"
	"net/http/httptest"
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

func TestRecordCommand(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{}))
	defer s.Close()
	for _, path := range []string{"/empty", "/disconnect"} {
		var out, errOut bytes.Buffer
		err := run(context.Background(), []string{"record", "--samples=1", s.URL + path}, &out, &errOut)
		if (err != nil) != (path == "/disconnect") {
			t.Fatalf("wrong exit result: %v", err)
		}
		r, decodeErr := inspect.Decode(&out)
		if decodeErr != nil || len(r.Samples) != 1 {
			t.Fatalf("invalid JSON output: %v", decodeErr)
		}
	}
	for _, args := range [][]string{{"record", "--samples=0", s.URL}, {"record", "https://user:secret@example.org/"}, {"record"}} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), args, &out, &errOut); err == nil {
			t.Fatal("invalid recording accepted")
		}
		if out.Len() != 0 {
			t.Fatal("invalid input wrote partial JSON")
		}
	}
	for _, command := range []string{"record", "fixture"} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), []string{command, "--help"}, &out, &errOut); err != nil {
			t.Fatal("help failed", err)
		}
	}
}
