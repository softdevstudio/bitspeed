package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"
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
