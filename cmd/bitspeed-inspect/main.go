package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/softdevstudio/bitspeed/inspect"
	"github.com/softdevstudio/bitspeed/internal/fixture"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bitspeed-inspect --version | fixture | record | explain | compare")
	}
	switch args[0] {
	case "--version":
		if len(args) != 1 {
			return errors.New("--version takes no arguments")
		}
		_, err := fmt.Fprintln(stdout, "bitspeed-inspect "+inspect.Version)
		return err
	case "--help", "help":
		_, err := fmt.Fprintln(stdout, "BitSpeed Inspect development build\nAvailable: --version, fixture, record\nPlanned: explain, compare")
		return err
	case "record":
		return record(ctx, args[1:], stdout, stderr)
	case "explain", "compare":
		return fmt.Errorf("%s is not implemented in this development build", args[0])
	case "fixture":
		return serveFixture(ctx, args[1:], stdout, stderr)
	default:
		return errors.New("unknown command; use --help")
	}
}

func serveFixture(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	address := flags.String("listen", "127.0.0.1:0", "loopback address and port")
	headerDelay := flags.Duration("header-delay", 200*time.Millisecond, "delay before response headers")
	chunkDelay := flags.Duration("chunk-delay", 100*time.Millisecond, "delay between body chunks")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *headerDelay < 0 || *chunkDelay < 0 {
		return errors.New("unexpected arguments or negative delay")
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return errors.New("listen requires a loopback IP address and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("fixture only listens on a literal loopback IP")
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           fixture.New(fixture.Config{HeaderDelay: *headerDelay, ChunkDelay: *chunkDelay}),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if _, err := fmt.Fprintf(stdout, "http://%s\n", listener.Addr()); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			server.Close()
		case <-done:
		}
	}()
	defer close(done)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func record(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o := inspect.DefaultOptions()
	flags := flag.NewFlagSet("record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.IntVar(&o.Samples, "samples", o.Samples, "number of sequential samples (1-100)")
	flags.IntVar(&o.MaxRedirects, "max-redirects", o.MaxRedirects, "maximum redirects per sample (0-10)")
	flags.DurationVar(&o.Timeout, "timeout", o.Timeout, "total timeout per sample")
	flags.DurationVar(&o.ConnectTimeout, "connect-timeout", o.ConnectTimeout, "DNS, dial and TLS timeout")
	flags.DurationVar(&o.HeaderTimeout, "header-timeout", o.HeaderTimeout, "response header timeout after request writing")
	flags.DurationVar(&o.BodyTimeout, "body-timeout", o.BodyTimeout, "total body reading timeout per hop")
	flags.DurationVar(&o.Interval, "interval", o.Interval, "pause between samples (0-1m)")
	flags.StringVar(&o.LocationLabel, "location", "", "operator-supplied location label")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: bitspeed-inspect record [flags] URL; writes JSON to stdout")
	}
	recording, err := inspect.Record(ctx, flags.Arg(0), o)
	if err != nil {
		return err
	}
	if err := inspect.Encode(stdout, recording); err != nil {
		return err
	}
	if !inspect.RecordingSucceeded(recording) {
		return errors.New("recording saved with failed, cancelled or unattempted samples")
	}
	return nil
}
