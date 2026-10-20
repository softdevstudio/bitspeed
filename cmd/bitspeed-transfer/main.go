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

var errNotImplemented = errors.New("Transfer is not implemented; no files were read or written and no network connection was opened")

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
		return errors.New("usage: bitspeed-transfer --version | serve | send | status; use --help")
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
		_, err := fmt.Fprintln(stdout, "BitSpeed Transfer development skeleton\nPlanned commands: serve, send, status\nLocal send --prepare-only is available on Linux/macOS. Network transfer is not implemented. Transfer is not released.\nSee docs/transfer-v1.md for the proposed protocol.")
		return err
	case "serve", "send", "status":
		if len(args) == 2 && args[1] == "--help" {
			help := map[string]string{
				"serve":  "serve: planned authenticated receiver using a private storage root",
				"send":   "send --prepare-only --source=FILE --spool=DIR [--chunk-size=BYTES]\nsend --prepare-only --spool=DIR --reuse=ID\nLocal preparation/revalidation only on Linux/macOS. Not implemented: network upload and resume.",
				"status": "status: planned authenticated receiver progress and completion query",
			}
			text := help[args[0]]
			if args[0] != "send" {
				text += "\nNot implemented. Operational flags are not accepted yet."
			}
			_, err := fmt.Fprintln(stdout, text)
			return err
		}
		if args[0] == "send" && len(args) > 1 && args[1] == "--prepare-only" {
			return prepareOnly(ctx, args[2:], stdout, stderr)
		}
		return errNotImplemented
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
