package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/softdevstudio/bitspeed/transfer"
)

func options(args []string, allowed ...string) (map[string]string, error) {
	known := map[string]bool{}
	for _, key := range allowed {
		known["--"+key] = true
	}
	out := map[string]string{}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || value == "" || !known[key] || out[key] != "" {
			return nil, transfer.ErrConfig
		}
		out[key] = value
	}
	return out, nil
}
func uploadOnly(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return sendFile(ctx, args, stdout, stderr, true)
}

// progressPrinter writes short progress lines to stderr: one when the receiver
// reports what it already has, then at most about a hundred chunk lines however
// large the file, and one before finalizing. It never prints paths or tokens.
func progressPrinter(stderr io.Writer) func(transfer.SendProgress) error {
	lastPercent := -1
	return func(p transfer.SendProgress) error {
		var err error
		switch p.Stage {
		case "resume":
			_, err = fmt.Fprintf(stderr, "receiver has %d of %d chunks verified; sending %d (%d bytes)\n", p.VerifiedChunks, p.Chunks, p.ToSend, p.BytesToSend)
		case "chunk":
			percent := 100
			if p.BytesToSend > 0 {
				percent = int(p.BytesSent * 100 / p.BytesToSend)
			}
			if p.Sent == 1 || p.Sent == p.ToSend || percent != lastPercent {
				lastPercent = percent
				_, err = fmt.Fprintf(stderr, "sent %d of %d chunks, %d/%d bytes (%d%%)\n", p.Sent, p.ToSend, p.BytesSent, p.BytesToSend, percent)
			}
		case "finalize":
			_, err = fmt.Fprintf(stderr, "all %d chunks verified; receiver is checking and publishing the file\n", p.Chunks)
		}
		return err
	}
}

func sendFile(ctx context.Context, args []string, stdout, stderr io.Writer, upload bool) error {
	v, e := options(args, "url", "token-file", "spool", "reuse")
	if e != nil || v["--url"] == "" || v["--token-file"] == "" || v["--spool"] == "" || v["--reuse"] == "" {
		return transfer.ErrConfig
	}
	token, e := transfer.LoadToken(v["--token-file"])
	if e != nil {
		return e
	}
	send := transfer.SendWithProgress
	if upload {
		send = transfer.UploadOnlyWithProgress
	}
	s, e := send(ctx, v["--url"], token, v["--spool"], v["--reuse"], progressPrinter(stderr))
	if e != nil {
		return e
	}
	return json.NewEncoder(stdout).Encode(struct {
		Uploaded    bool                  `json:"uploaded"`
		Transferred bool                  `json:"transferred"`
		Status      transfer.StoredStatus `json:"status"`
	}{true, !upload, s})
}
func queryReceiver(ctx context.Context, args []string, stdout io.Writer) error {
	v, e := options(args, "url", "token-file", "id")
	if e != nil || v["--url"] == "" || v["--token-file"] == "" || v["--id"] == "" {
		return transfer.ErrConfig
	}
	token, e := transfer.LoadToken(v["--token-file"])
	if e != nil {
		return e
	}
	s, e := transfer.QueryStatus(ctx, v["--url"], token, v["--id"])
	if e != nil {
		return e
	}
	return json.NewEncoder(stdout).Encode(s)
}
func serveReceiver(ctx context.Context, args []string, stdout io.Writer) error {
	v, e := options(args, "root", "token-file", "listen", "tls-cert", "tls-key", "max-sessions", "max-reserved-bytes")
	if e != nil || v["--root"] == "" || v["--token-file"] == "" {
		return transfer.ErrConfig
	}
	address := v["--listen"]
	if address == "" {
		address = "127.0.0.1:8080"
	}
	host, port, e := net.SplitHostPort(address)
	number, pe := strconv.Atoi(port)
	if e != nil || pe != nil || number < 0 || number > 65535 || net.ParseIP(host) == nil {
		return transfer.ErrConfig
	}
	hasTLS := v["--tls-cert"] != "" && v["--tls-key"] != ""
	if (v["--tls-cert"] != "") != (v["--tls-key"] != "") || !net.ParseIP(host).IsLoopback() && !hasTLS {
		return transfer.ErrConfig
	}
	var tlsConfig *tls.Config
	if hasTLS {
		pair, e := tls.LoadX509KeyPair(v["--tls-cert"], v["--tls-key"])
		if e != nil {
			return transfer.ErrConfig
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}
	}
	token, e := transfer.LoadToken(v["--token-file"])
	if e != nil {
		return e
	}
	c := transfer.ReceiverConfig{Root: v["--root"], Token: token}
	if v["--max-sessions"] != "" {
		n, e := strconv.Atoi(v["--max-sessions"])
		if e != nil || n <= 0 {
			return transfer.ErrConfig
		}
		c.MaxSessions = n
	}
	if v["--max-reserved-bytes"] != "" {
		n, e := strconv.ParseInt(v["--max-reserved-bytes"], 10, 64)
		if e != nil || n <= 0 {
			return transfer.ErrConfig
		}
		c.MaxReservedBytes = n
	}
	startup, cancelStartup := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelStartup()
	receiver, e := transfer.NewReceiverContext(startup, c)
	if e != nil {
		return e
	}
	defer receiver.Close()
	listener, e := net.Listen("tcp", address)
	if e != nil {
		return errors.New("receiver listener unavailable")
	}
	defer listener.Close()
	listener = &boundedListener{Listener: listener, slots: make(chan struct{}, 64), done: make(chan struct{})}
	scheme := "http"
	if hasTLS {
		listener = tls.NewListener(listener, tlsConfig)
		scheme = "https"
	}
	server := &http.Server{Handler: receiver, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	if _, e = fmt.Fprintln(stdout, scheme+"://"+listener.Addr().String()); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case e = <-done:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
		e = <-done
	}
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		return errors.New("receiver stopped unexpectedly")
	}
	return nil
}

// Bound accepted connections, including slow headers and TLS handshakes.
type boundedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, e := l.Listener.Accept()
	if e != nil {
		<-l.slots
		return nil, e
	}
	return &boundedConn{Conn: c, release: func() { <-l.slots }}, nil
}
func (l *boundedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type boundedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *boundedConn) Close() error { e := c.Conn.Close(); c.once.Do(c.release); return e }
