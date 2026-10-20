package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/softdevstudio/bitspeed/transfer"
)

var errNotImplemented = errors.New("Transfer is not implemented; no files were read or written and no network connection was opened")

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
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
		_, err := fmt.Fprintln(stdout, "BitSpeed Transfer development skeleton\nPlanned commands: serve, send, status\nOnly help and version are implemented. Transfer is not released.\nSee docs/transfer-v1.md for the proposed protocol.")
		return err
	case "serve", "send", "status":
		if len(args) == 2 && args[1] == "--help" {
			help := map[string]string{
				"serve":  "serve: planned authenticated receiver using a private storage root",
				"send":   "send: planned immutable source snapshot, upload and resume",
				"status": "status: planned authenticated receiver progress and completion query",
			}
			_, err := fmt.Fprintln(stdout, help[args[0]]+"\nNot implemented. Operational flags are not accepted yet.")
			return err
		}
		return errNotImplemented
	default:
		return errors.New("unknown command; use --help")
	}
}
