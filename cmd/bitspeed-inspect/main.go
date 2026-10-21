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
		var gate gateExit
		if errors.As(err, &gate) {
			os.Exit(gate.code)
		}
		os.Exit(1)
	}
}

// gateExit carries the compare gate's exit status: 3 for a regression, 4 when
// a gated interval can't be judged. Ordinary errors keep exit status 1.
type gateExit struct {
	code int
	msg  string
}

func (g gateExit) Error() string { return g.msg }

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
		_, err := fmt.Fprintln(stdout, "BitSpeed Inspect development build\nAvailable: --version, fixture, record, explain, compare")
		return err
	case "record":
		return record(ctx, args[1:], stdout, stderr)
	case "explain":
		return explain(args[1:], os.Stdin, stdout, stderr)
	case "compare":
		return compare(args[1:], os.Stdin, stdout, stderr)
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

func explain(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("explain", flag.ContinueOnError)
	flags.SetOutput(stderr)
	format := flags.String("format", "text", "report format: text or json")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 || (*format != "text" && *format != "json") {
		return errors.New("usage: bitspeed-inspect explain [--format=text|json] FILE (use - for stdin)")
	}
	reader := stdin
	if flags.Arg(0) != "-" {
		file, err := os.Open(flags.Arg(0))
		if err != nil {
			return errors.New("cannot open recording file")
		}
		defer file.Close()
		reader = file
	}
	recording, err := inspect.Decode(reader)
	if err != nil {
		return fmt.Errorf("invalid recording: %w", err)
	}
	report, err := inspect.Explain(recording)
	if err != nil {
		return err
	}
	if *format == "json" {
		return inspect.WriteReportJSON(stdout, report)
	}
	return inspect.WriteReportText(stdout, report)
}

func readRecording(path string, stdin io.Reader) (inspect.Recording, error) {
	reader := stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return inspect.Recording{}, errors.New("cannot open recording file")
		}
		defer file.Close()
		reader = file
	}
	return inspect.Decode(reader)
}

func compare(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	format := flags.String("format", "text", "comparison format: text or json")
	failIfSlower := flags.String("fail-if-slower", "", "comma-separated intervals that must not get slower; exit 3 on a regression, 4 if undecided")
	minChange := flags.Duration("min-change", 0, "with --fail-if-slower, ignore slowdowns smaller than this")
	minPercent := flags.Float64("min-percent", 0, "with --fail-if-slower, ignore slowdowns smaller than this percent")
	allowUndecided := flags.Bool("allow-undecided", false, "with --fail-if-slower, pass when a gated interval can't be judged")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 2 || (*format != "text" && *format != "json") || (flags.Arg(0) == "-" && flags.Arg(1) == "-") {
		return errors.New("usage: bitspeed-inspect compare [--format=text|json] [--fail-if-slower=INTERVALS [--min-change=DURATION] [--min-percent=N] [--allow-undecided]] BASELINE CANDIDATE (at most one - for stdin)")
	}
	var gate *inspect.Gate
	if *failIfSlower != "" {
		metrics, err := inspect.ParseGateMetrics(*failIfSlower)
		if err != nil {
			return err
		}
		if *minChange < 0 || *minPercent < 0 {
			return errors.New("--min-change and --min-percent must not be negative")
		}
		gate = &inspect.Gate{Metrics: metrics, MinChangeNS: float64(*minChange), MinPercent: *minPercent, AllowUndecided: *allowUndecided}
	} else if *minChange != 0 || *minPercent != 0 || *allowUndecided {
		return errors.New("--min-change, --min-percent and --allow-undecided need --fail-if-slower")
	}
	baseline, err := readRecording(flags.Arg(0), stdin)
	if err != nil {
		return fmt.Errorf("invalid baseline recording: %w", err)
	}
	candidate, err := readRecording(flags.Arg(1), stdin)
	if err != nil {
		return fmt.Errorf("invalid candidate recording: %w", err)
	}
	report, err := inspect.Compare(baseline, candidate)
	if err != nil {
		return err
	}
	if *format == "json" {
		err = inspect.WriteComparisonJSON(stdout, report)
	} else {
		err = inspect.WriteComparisonText(stdout, report)
	}
	if err != nil || gate == nil {
		return err
	}
	result, err := inspect.EvaluateGate(report, *gate)
	if err != nil {
		return err
	}
	for _, f := range result.Findings {
		line := fmt.Sprintf("gate: %s", f.Metric)
		if f.Hop >= 0 {
			line += fmt.Sprintf(" hop %d", f.Hop)
		}
		line += ": " + f.Outcome + " (" + f.Verdict
		if f.AbsoluteChange != nil {
			line += fmt.Sprintf(", %+.3f ms", *f.AbsoluteChange/1e6)
		}
		if f.PercentChange != nil {
			line += fmt.Sprintf(", %+.1f%%", *f.PercentChange)
		}
		line += ") " + f.Reason
		if _, err = fmt.Fprintln(stderr, line); err != nil {
			return err
		}
	}
	switch result.Outcome {
	case "regressed":
		return gateExit{3, "gate failed: a gated interval got slower in these samples"}
	case "undecided":
		return gateExit{4, "gate undecided: a gated interval could not be judged; see gate lines above"}
	}
	_, err = fmt.Fprintln(stderr, "gate passed")
	return err
}
