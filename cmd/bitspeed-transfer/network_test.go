package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/softdevstudio/bitspeed/transfer"
)

func TestNetworkConfigurationRejectedBeforeIO(t *testing.T) {
	cases := [][]string{{"serve", "--root=SECRET", "--token-file=SECRET", "--listen=0.0.0.0:80"}, {"serve", "--root=SECRET", "--token-file=SECRET", "--listen=hostname:80"}, {"serve", "--root=SECRET", "--token-file=SECRET", "--tls-cert=SECRET"}, {"status", "--url=http://127.0.0.1:1", "--id=SECRET"}, {"send", "--upload-only", "--reuse=SECRET", "--spool=SECRET", "--url=http://127.0.0.1:1"}, {"serve", "--root=SECRET", "--root=SECRET", "--token-file=SECRET"}}
	for _, args := range cases {
		var out bytes.Buffer
		err := runContext(context.Background(), args, &out, &out)
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid network options accepted or exposed", args, err)
		}
	}
}

func TestBoundedListenerExclusionAndClose(t *testing.T) {
	raw, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	l := &boundedListener{Listener: raw, slots: make(chan struct{}, 1), done: make(chan struct{})}
	defer l.Close()
	first, e := net.Dial("tcp", raw.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	accepted, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	blocked := make(chan error, 1)
	go func() {
		c, e := l.Accept()
		if c != nil {
			c.Close()
		}
		blocked <- e
	}()
	l.Close()
	select {
	case e := <-blocked:
		if e == nil {
			t.Fatal("accepted after shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not unblock capacity wait")
	}
	if e = accepted.Close(); e != nil {
		t.Fatal(e)
	}
	accepted.Close()
	if len(l.slots) != 0 {
		t.Fatal("connection slot leaked")
	}
}

func TestProgressPrinterIsBounded(t *testing.T) {
	var out bytes.Buffer
	print := progressPrinter(&out)
	const chunks = 1000
	if err := print(transfer.SendProgress{Stage: "resume", Chunks: chunks + 24, VerifiedChunks: 24, ToSend: chunks, BytesToSend: chunks * 10}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= chunks; i++ {
		if err := print(transfer.SendProgress{Stage: "chunk", Chunks: chunks + 24, ToSend: chunks, Sent: i, ChunkIndex: i + 23, BytesSent: int64(i) * 10, BytesToSend: chunks * 10}); err != nil {
			t.Fatal(err)
		}
	}
	if err := print(transfer.SendProgress{Stage: "finalize", Chunks: chunks + 24, VerifiedChunks: chunks + 24}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) > 104 || !strings.HasPrefix(lines[0], "receiver has 24 of 1024 chunks verified") || !strings.Contains(lines[1], "sent 1 of 1000") || !strings.Contains(lines[len(lines)-2], "sent 1000 of 1000") || !strings.HasPrefix(lines[len(lines)-1], "all 1024 chunks verified") {
		t.Fatal(len(lines), lines[0], lines[len(lines)-1])
	}
	if err := progressPrinter(brokenWriter{})(transfer.SendProgress{Stage: "resume"}); err == nil {
		t.Fatal("a broken stderr must be reported")
	}
}

func TestProgressPrinterAtProtocolLimit(t *testing.T) {
	var out bytes.Buffer
	print := progressPrinter(&out)
	chunks := int(transfer.MaxChunks)
	total := int64(chunks)*transfer.MaxChunkSize - 7
	var sent int64
	for i := 1; i <= chunks; i++ {
		size := transfer.MaxChunkSize
		if i == chunks {
			size -= 7
		}
		sent += size
		if err := print(transfer.SendProgress{Stage: "chunk", ToSend: chunks, Sent: i, BytesSent: sent, BytesToSend: total}); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) > 102 || !strings.Contains(lines[len(lines)-1], "(100%)") {
		t.Fatal(len(lines), lines[len(lines)-1])
	}
}
