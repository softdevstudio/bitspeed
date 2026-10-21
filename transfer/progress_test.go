package transfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func preparedOnly(t *testing.T, data []byte) (*Receiver, string, string, Manifest) {
	t.Helper()
	r, server := testReceiver(t)
	src, spool := snapshotPaths(t, data)
	m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	return r, server.URL, spool, m
}

func TestSendProgressReportsOnlyAcknowledgedChunks(t *testing.T) {
	if !spoolSupported() {
		t.Skip("sender unsupported on this platform")
	}
	data := bytes.Repeat([]byte("p"), 3*int(MinChunkSize)+19)
	_, url, spool, m := preparedOnly(t, data)
	var events []SendProgress
	s, e := SendWithProgress(context.Background(), url, testToken, spool, m.TransferID, func(p SendProgress) error {
		events = append(events, p)
		return nil
	})
	if e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	n := len(m.ChunkSHA256)
	if len(events) != n+2 || events[0].Stage != "resume" || events[0].ToSend != n || events[0].VerifiedChunks != 0 || events[0].BytesToSend != m.FileSize {
		t.Fatal("wrong resume event", events)
	}
	for i := 1; i <= n; i++ {
		p := events[i]
		if p.Stage != "chunk" || p.Sent != i || p.ChunkIndex != i-1 || p.BytesSent > p.BytesToSend {
			t.Fatal("wrong chunk event", i, p)
		}
	}
	if last := events[n]; last.BytesSent != m.FileSize {
		t.Fatal("final byte count", last)
	}
	if f := events[n+1]; f.Stage != "finalize" || f.VerifiedChunks != n {
		t.Fatal("wrong finalize event", f)
	}
	// A repeat send of a completed transfer has nothing to report.
	events = nil
	if _, e = SendWithProgress(context.Background(), url, testToken, spool, m.TransferID, func(p SendProgress) error {
		events = append(events, p)
		return nil
	}); e != nil || len(events) != 0 {
		t.Fatal("completed transfer reported progress", events, e)
	}
}

func TestSendProgressAfterPartialUploadAndCallbackStop(t *testing.T) {
	if !spoolSupported() {
		t.Skip("sender unsupported on this platform")
	}
	data := bytes.Repeat([]byte("q"), 4*int(MinChunkSize))
	r, url, spool, m := preparedOnly(t, data)
	stop := errors.New("stop")
	_, e := UploadOnlyWithProgress(context.Background(), url, testToken, spool, m.TransferID, func(p SendProgress) error {
		if p.Stage == "chunk" && p.Sent == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(e, stop) {
		t.Fatal("callback error must stop the upload", e)
	}
	s, e := QueryStatus(context.Background(), url, testToken, m.TransferID)
	if e != nil || s.VerifiedChunks != 2 {
		t.Fatal("chunks acknowledged before the stop must stay verified", s, e)
	}
	// Remove one stored chunk so the receiver must report it missing again.
	if e = os.Remove(filepath.Join(r.root, "sessions", m.TransferID, "chunks", "0")); e != nil {
		t.Fatal(e)
	}
	var first SendProgress
	var sent []int
	s, e = SendWithProgress(context.Background(), url, testToken, spool, m.TransferID, func(p SendProgress) error {
		switch p.Stage {
		case "resume":
			first = p
		case "chunk":
			sent = append(sent, p.ChunkIndex)
		}
		return nil
	})
	if e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	if first.VerifiedChunks != 1 || first.ToSend != 3 || first.BytesToSend != 3*MinChunkSize {
		t.Fatal("resume must reflect what the receiver can prove", first)
	}
	if len(sent) != 3 || sent[0] != 0 || sent[1] != 2 || sent[2] != 3 {
		t.Fatal("only missing chunks may be sent", sent)
	}
}

func TestSendProgressStopAtFinalizeAndEmptyFile(t *testing.T) {
	if !spoolSupported() {
		t.Skip("sender unsupported on this platform")
	}
	data := bytes.Repeat([]byte("f"), int(MinChunkSize)+1)
	_, url, spool, m := preparedOnly(t, data)
	stop := errors.New("stop")
	_, e := SendWithProgress(context.Background(), url, testToken, spool, m.TransferID, func(p SendProgress) error {
		if p.Stage == "finalize" {
			return stop
		}
		return nil
	})
	if !errors.Is(e, stop) {
		t.Fatal(e)
	}
	s, e := QueryStatus(context.Background(), url, testToken, m.TransferID)
	if e != nil || s.State != Uploading || s.Receipt != nil || len(s.MissingChunks) != 0 {
		t.Fatal("a stop before finalize must not publish", s, e)
	}
	if _, e = os.Lstat(filepath.Join(spool, m.TransferID, "receipt.json")); !os.IsNotExist(e) {
		t.Fatal("no receipt may be cached", e)
	}
	if s, e = Send(context.Background(), url, testToken, spool, m.TransferID); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}

	_, url, spool, m = preparedOnly(t, nil)
	var events []SendProgress
	if s, e = SendWithProgress(context.Background(), url, testToken, spool, m.TransferID, func(p SendProgress) error {
		events = append(events, p)
		return nil
	}); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	if len(events) != 2 || events[0].Stage != "resume" || events[0].ToSend != 0 || events[1].Stage != "finalize" {
		t.Fatal("empty file progress", events)
	}
}
