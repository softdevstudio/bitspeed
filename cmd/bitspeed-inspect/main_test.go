package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/softdevstudio/bitspeed/inspect"
	"github.com/softdevstudio/bitspeed/internal/fixture"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	for _, command := range []string{"record", "fixture", "explain", "compare"} {
		var out, errOut bytes.Buffer
		if err := run(context.Background(), []string{command, "--help"}, &out, &errOut); err != nil {
			t.Fatal("help failed", err)
		}
	}
}

func TestExplainCommand(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{}))
	defer s.Close()
	options := inspect.DefaultOptions()
	options.Samples = 1
	r, err := inspect.Record(context.Background(), s.URL+"/disconnect", options)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := inspect.Encode(&input, r); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		var out, errOut bytes.Buffer
		if err := explain([]string{"--format=" + format, "-"}, bytes.NewReader(input.Bytes()), &out, &errOut); err != nil {
			t.Fatal(err)
		}
		if format == "json" && !json.Valid(out.Bytes()) {
			t.Fatal("invalid JSON")
		}
		if format == "text" && !strings.Contains(out.String(), "Interpretation") {
			t.Fatal("missing explanation")
		}
	}
	path := filepath.Join(t.TempDir(), "recording.json")
	if err := os.WriteFile(path, input.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"explain", path}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--format=bad", "-"}, {"missing-file"}, {"-"}} {
		var out, errOut bytes.Buffer
		if err := explain(args, strings.NewReader("{}"), &out, &errOut); err == nil {
			t.Fatal("invalid explanation input accepted")
		}
		if out.Len() != 0 {
			t.Fatal("invalid input produced partial report")
		}
	}
}

func TestCompareCommand(t *testing.T) {
	s := httptest.NewServer(fixture.New(fixture.Config{}))
	defer s.Close()
	options := inspect.DefaultOptions()
	options.Samples = 1
	r, err := inspect.Record(context.Background(), s.URL+"/empty", options)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	inspect.Encode(&input, r)
	path := filepath.Join(t.TempDir(), "baseline.json")
	os.WriteFile(path, input.Bytes(), 0600)
	for _, format := range []string{"text", "json"} {
		var out, errOut bytes.Buffer
		if err := compare([]string{"--format=" + format, path, "-"}, bytes.NewReader(input.Bytes()), &out, &errOut); err != nil {
			t.Fatal(err)
		}
		if format == "json" && !json.Valid(out.Bytes()) {
			t.Fatal("invalid comparison JSON")
		}
		if format == "text" && !strings.Contains(out.String(), "Interpretation") {
			t.Fatal("missing verdict labels")
		}
	}
	for _, args := range [][]string{nil, {"-", "-"}, {"--format=bad", path, path}, {"missing", path}, {path, "missing"}, {path, "-"}} {
		var out, errOut bytes.Buffer
		if err := compare(args, strings.NewReader("{}"), &out, &errOut); err == nil {
			t.Fatal("invalid comparison accepted")
		}
		if out.Len() != 0 {
			t.Fatal("invalid comparison produced output")
		}
	}
}
