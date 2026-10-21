package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/softdevstudio/bitspeed/transfer"
)

var errSendOptions = errors.New("invalid send options; use send --help")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runContext(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	return runContext(context.Background(), args, stdout, io.Discard)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bitspeed-transfer --version | serve | send | status | clean; use --help")
	}
	switch args[0] {
	case "--version":
		if len(args) != 1 {
			return errors.New("--version takes no arguments")
		}
		_, err := fmt.Fprintln(stdout, "bitspeed-transfer "+transfer.Version)
		return err
	case "--help", "help":
		if len(args) != 1 {
			return errors.New("help takes no arguments")
		}
		_, err := fmt.Fprintln(stdout, "BitSpeed Transfer "+transfer.Version+"\nAvailable: serve, status, send, send --prepare-only, send --upload-only, clean\nSend verifies and publishes one file; upload-only stages chunks. See docs/transfer-releases.md for platform limits.\nSee docs/transfer-v1.md for the protocol.")
		return err
	case "clean":
		if len(args) == 2 && args[1] == "--help" {
			_, err := fmt.Fprintln(stdout, "clean --root=DIR [--dry-run]\nWith the receiver stopped, removes stored chunks from completed sessions after startup recovery verifies each destination. Keeps destinations, receipts, witnesses, corrupt-chunk evidence and unfinished sessions.")
			return err
		}
		return cleanReceiver(ctx, args[1:], stdout)
	case "serve", "send", "status":
		if len(args) == 2 && args[1] == "--help" {
			help := map[string]string{
				"serve":  "serve --root=DIR --token-file=FILE [--listen=127.0.0.1:8080] [--tls-cert=FILE --tls-key=FILE]\nReceiver on supported Linux/macOS local filesystems.",
				"send":   "send --prepare-only --source=FILE --spool=DIR [--chunk-size=BYTES]\nsend --prepare-only --spool=DIR --reuse=ID\nLocal preparation/revalidation only on Linux/macOS. send --upload-only --spool=DIR --reuse=ID --url=URL --token-file=FILE\nsend --spool=DIR --reuse=ID --url=URL --token-file=FILE\nPublishes a verified destination and retains a local receipt.",
				"status": "status --url=URL --token-file=FILE --id=ID\nAuthenticated verified observations and completion receipt.",
			}
			text := help[args[0]]

			_, err := fmt.Fprintln(stdout, text)
			return err
		}
		if args[0] == "send" && len(args) > 1 && args[1] == "--prepare-only" {
			return prepareOnly(ctx, args[2:], stdout, stderr)
		}
		if len(args) > 1 || args[0] == "serve" || args[0] == "status" {
			switch args[0] {
			case "serve":
				return serveReceiver(ctx, args[1:], stdout)
			case "status":
				return queryReceiver(ctx, args[1:], stdout)
			case "send":
				if args[1] == "--upload-only" {
					return uploadOnly(ctx, args[2:], stdout)
				}
				return sendFile(ctx, args[1:], stdout, false)
			}
		}
		return errSendOptions
	default:
		return errors.New("unknown command; use --help")
	}
}

func prepareOnly(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	invalid := errors.New("invalid preparation options; use send --help (options require =VALUE)")
	values := map[string]string{}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || value == "" {
			return invalid
		}
		switch key {
		case "--source", "--spool", "--reuse", "--chunk-size":
		default:
			return invalid
		}
		if _, exists := values[key]; exists {
			return invalid
		}
		values[key] = value
	}
	if values["--spool"] == "" {
		return invalid
	}
	source, reuse := values["--source"], values["--reuse"]
	if (source == "") == (reuse == "") || reuse != "" && values["--chunk-size"] != "" {
		return invalid
	}
	var m transfer.Manifest
	var err error
	if reuse != "" {
		m, err = transfer.Reuse(ctx, values["--spool"], reuse)
	} else {
		chunk := int64(0)
		if value := values["--chunk-size"]; value != "" {
			chunk, err = strconv.ParseInt(value, 10, 64)
			if err != nil || chunk < transfer.MinChunkSize || chunk > transfer.MaxChunkSize {
				return invalid
			}
		}
		var last int64
		lastStage := ""
		m, err = transfer.Prepare(ctx, source, values["--spool"], transfer.PrepareOptions{ChunkSize: chunk, Progress: func(p transfer.Progress) error {
			if p.Stage != lastStage || p.Bytes-last >= 1<<20 || p.Bytes == p.Total {
				last = p.Bytes
				lastStage = p.Stage
				_, err := fmt.Fprintf(stderr, "%s %d/%d bytes\n", p.Stage, p.Bytes, p.Total)
				return err
			}
			return nil
		}})
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Prepared    bool              `json:"prepared"`
		Transferred bool              `json:"transferred"`
		Manifest    transfer.Manifest `json:"manifest"`
	}{true, false, m})
}

func cleanReceiver(ctx context.Context, args []string, stdout io.Writer) error {
	invalid := errors.New("invalid clean options; use clean --help")
	root, dryRun := "", false
	for _, arg := range args {
		switch {
		case arg == "--dry-run" && !dryRun:
			dryRun = true
		case strings.HasPrefix(arg, "--root=") && root == "" && len(arg) > len("--root="):
			root = strings.TrimPrefix(arg, "--root=")
		default:
			return invalid
		}
	}
	if root == "" {
		return invalid
	}
	results, err := transfer.CleanReceiver(ctx, root, dryRun)
	if err != nil {
		return err
	}
	var removed int
	var freed int64
	for _, r := range results {
		removed += r.ChunksRemoved
		freed += r.BytesFreed
	}
	return json.NewEncoder(stdout).Encode(struct {
		DryRun        bool                   `json:"dry_run"`
		ChunksRemoved int                    `json:"chunks_removed"`
		BytesFreed    int64                  `json:"bytes_freed"`
		Sessions      []transfer.CleanResult `json:"sessions"`
	}{dryRun, removed, freed, results})
}
