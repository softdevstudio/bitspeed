package transfer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func stagedFile(t *testing.T, data []byte) (*Receiver, *httptest.Server, string, Manifest) {
	t.Helper()
	r, server := testReceiver(t)
	src, spool := snapshotPaths(t, data)
	m, e := Prepare(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(src)
	if _, e = UploadOnly(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	return r, server, spool, m
}
func finalRequest(r *Receiver, m Manifest) *httptest.ResponseRecorder {
	identity, _ := m.Identity()
	body := `{"manifest_sha256":"` + identity + `"}`
	return perform(r, "POST", "/v1/transfers/"+m.TransferID+"/finalize", "application/json", strings.NewReader(body), int64(len(body)))
}
func TestCompletedFileAndReceipt(t *testing.T) {
	for _, size := range []int{0, 1, int(MinChunkSize) + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte{8}, size)
			r, server, spool, m := stagedFile(t, data)
			s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID)
			if e != nil || s.State != Completed || s.Receipt == nil || *s.Receipt != expectedReceipt(m) {
				t.Fatal(s, e)
			}
			receipt := *s.Receipt
			if saved, e := os.ReadFile(r.output(m)); e != nil || !bytes.Equal(saved, data) {
				t.Fatal("wrong destination", e)
			}
			if saved, e := os.ReadFile(filepath.Join(spool, m.TransferID, "receipt.json")); e != nil {
				t.Fatal(e)
			} else if cached, e := decodeReceipt(saved); e != nil || cached != receipt {
				t.Fatal("wrong cached receipt", e)
			}
			server.Close()
			r.Close()
			again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken})
			if e != nil {
				t.Fatal(e)
			}
			defer again.Close()
			resumed := httptest.NewServer(again)
			defer resumed.Close()
			s, e = Send(context.Background(), resumed.URL, testToken, spool, m.TransferID)
			if e != nil || s.Receipt == nil || *s.Receipt != receipt {
				t.Fatal("lost stable receipt", s, e)
			}
			outputInfo, _ := os.Stat(again.output(m))
			readyInfo, _ := os.Stat(filepath.Join(again.sessionDir(m), "ready"))
			if !os.SameFile(outputInfo, readyInfo) {
				t.Fatal("no retained witness")
			}
			w := perform(again, "PUT", "/v1/transfers/"+m.TransferID+"/chunks/0", "application/octet-stream", strings.NewReader("x"), 1)
			if w.Code != 409 {
				t.Fatal("accepted completed mutation", w.Code)
			}
		})
	}
}
func TestFinalizationFailuresAndConflicts(t *testing.T) {
	for _, kind := range []string{"missing", "damaged chunk", "full digest", "foreign destination", "equal foreign destination", "damaged output", "missing receipt", "corrupt receipt", "invalid ready"} {
		t.Run(kind, func(t *testing.T) {
			r, _, _, m := stagedFile(t, []byte("data"))
			dir := r.sessionDir(m)
			switch kind {
			case "missing":
				os.Remove(filepath.Join(dir, "chunks", "0"))
			case "damaged chunk":
				os.WriteFile(filepath.Join(dir, "chunks", "0"), []byte("bad!"), 0600)
			case "full digest":
				// The chunk is valid but the manifest's whole-file claim disagrees.
				m.FileSHA256 = strings.Repeat("0", 64)
				data, _ := m.CanonicalJSON()
				os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600)
				identity, _ := m.Identity()
				j, _ := json.Marshal(journal{1, identity, Uploading, nil})
				os.WriteFile(filepath.Join(dir, "journal.json"), j, 0600)
			case "foreign destination", "equal foreign destination":
				os.WriteFile(r.output(m), []byte("data"), 0600)
			case "damaged output", "missing receipt", "corrupt receipt":
				if w := finalRequest(r, m); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if kind == "damaged output" {
					os.WriteFile(r.output(m), []byte("bad!"), 0600)
				}
				if kind == "missing receipt" {
					os.Remove(filepath.Join(dir, "completion.json"))
				}
				if kind == "corrupt receipt" {
					os.WriteFile(filepath.Join(dir, "completion.json"), []byte("{}"), 0600)
				}
			case "invalid ready":
				r.setState(m, Finalizing, nil)
				os.WriteFile(filepath.Join(dir, "ready"), []byte("bad!"), 0600)
			}
			w := finalRequest(r, m)
			if w.Code == 200 {
				t.Fatal("claimed invalid destination", w.Body.String())
			}
			if kind == "missing" || kind == "damaged chunk" {
				if w.Code != 409 || !strings.Contains(w.Body.String(), "chunks_missing") {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if kind == "full digest" {
				if w.Code != 422 {
					t.Fatal(w.Code, w.Body.String())
				}
				if found, _ := exists(r.output(m)); found {
					t.Fatal("published wrong full digest")
				}
			}
			if kind == "foreign destination" || kind == "equal foreign destination" {
				if b, _ := os.ReadFile(r.output(m)); string(b) != "data" {
					t.Fatal("overwrote foreign output")
				}
				j, e := r.readJournal(m)
				if e != nil || j.State != Failed {
					t.Fatal(j, e)
				}
			}
			if kind == "damaged output" {
				s, e := r.status(context.Background(), m)
				if e != nil || s.State != Failed || s.Receipt != nil || s.ErrorCode == nil || *s.ErrorCode != "storage_corrupt" {
					t.Fatal(s, e)
				}
			}
		})
	}
}
func TestFinalizeWireBounds(t *testing.T) {
	r, _, _, m := stagedFile(t, []byte("data"))
	identity, _ := m.Identity()
	good := `{"manifest_sha256":"` + identity + `"}`
	for _, body := range []string{`{}`, `{"manifest_sha256":null}`, `{"manifest_sha256":"` + identity + `","manifest_sha256":"` + identity + `"}`, good + " {}", strings.Repeat(" ", 257), `{"manifest_sha256":"` + strings.Repeat("0", 64) + `"}`} {
		w := perform(r, "POST", "/v1/transfers/"+m.TransferID+"/finalize", "application/json", strings.NewReader(body), int64(len(body)))
		if w.Code == 200 {
			t.Fatal("accepted bad finalize", body)
		}
	}
	if found, _ := exists(r.output(m)); found {
		t.Fatal("bad request published")
	}
}
func TestFinalizeDiskFailureGates(t *testing.T) {
	for _, stage := range []string{"intent write", "intent close", "intent sync", "intent rename", "assembly write", "assembly short", "assembly corrupt", "assembly close", "assembly sync", "ready rename", "ready dir sync", "output link", "output dir sync", "receipt write", "receipt close", "receipt sync", "receipt rename", "completed rename", "completion read sync"} {
		t.Run(stage, func(t *testing.T) {
			r, _, _, m := stagedFile(t, []byte("data"))
			real := r.ops
			dir := r.sessionDir(m)
			injected := errors.New("injected")
			target := ""
			switch stage {
			case "intent write", "intent close", "intent sync", "intent rename":
				target = "journal.json.tmp"
			case "assembly write", "assembly short", "assembly corrupt", "assembly close", "assembly sync":
				target = "assembly.tmp"
			case "receipt write", "receipt close", "receipt sync", "receipt rename":
				target = "completion.json.tmp"
			}
			r.ops.write = func(f *os.File, b []byte) (int, error) {
				if filepath.Base(f.Name()) == target {
					switch stage {
					case "intent write", "assembly write", "receipt write":
						return 0, injected
					case "assembly short":
						return f.Write(b[:len(b)-1])
					case "assembly corrupt":
						return f.Write(bytes.Repeat([]byte{0}, len(b)))
					}
				}
				return f.Write(b)
			}
			r.ops.close = func(f *os.File) error {
				e := f.Close()
				if filepath.Base(f.Name()) == target && strings.HasSuffix(stage, "close") {
					return injected
				}
				return e
			}
			r.ops.sync = func(f *os.File) error {
				if filepath.Base(f.Name()) == target && strings.HasSuffix(stage, "sync") {
					return injected
				}
				if stage == "ready dir sync" && f.Name() == dir {
					if ok, _ := exists(filepath.Join(dir, "ready")); ok {
						return injected
					}
				}
				if stage == "output dir sync" && f.Name() == filepath.Join(r.root, "files") {
					return injected
				}
				if stage == "completion read sync" && filepath.Base(f.Name()) == "completion.json" {
					return injected
				}
				return f.Sync()
			}
			r.ops.rename = func(a, b string) error {
				if stage == "intent rename" && filepath.Base(a) == target {
					return injected
				}
				if stage == "ready rename" && filepath.Base(b) == "ready" {
					return injected
				}
				if stage == "receipt rename" && filepath.Base(b) == "completion.json" {
					return injected
				}
				if stage == "completed rename" && filepath.Base(b) == "journal.json" {
					data, _ := os.ReadFile(a)
					if bytes.Contains(data, []byte(`"state":"completed"`)) {
						return injected
					}
				}
				return os.Rename(a, b)
			}
			r.ops.link = func(a, b string) error {
				if stage == "output link" && b == r.output(m) {
					return injected
				}
				return os.Link(a, b)
			}
			if w := finalRequest(r, m); w.Code == 200 {
				t.Fatal("storage failure claimed completion", w.Body.String())
			}
			r.ops = real
			if w := finalRequest(r, m); w.Code != 200 {
				t.Fatal("same-ID retry could not recover", w.Code, w.Body.String())
			}
		})
	}
}
func TestLostFinalizeResponse(t *testing.T) {
	r, _, spool, m := stagedFile(t, []byte("data"))
	var dropped atomic.Bool
	var finalizes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, "/finalize") {
			finalizes.Add(1)
			if !dropped.Swap(true) {
				response := httptest.NewRecorder()
				r.ServeHTTP(response, q)
				if response.Code != 200 {
					t.Error(response.Code, response.Body.String())
				}
				conn, _, e := w.(http.Hijacker).Hijack()
				if e == nil {
					conn.Close()
				}
				return
			}
		}
		r.ServeHTTP(w, q)
	}))
	defer server.Close()
	if _, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID); e == nil {
		t.Fatal("accepted lost response")
	}
	s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID)
	if e != nil || s.State != Completed || finalizes.Load() != 1 {
		t.Fatal("repeated publication", s, e, finalizes.Load())
	}
}

func TestFinalizeProcessHelper(t *testing.T) {
	if os.Getenv("BITSPEED_FINAL_HELPER") == "" {
		return
	}
	point := os.Getenv("BITSPEED_FINAL_POINT")
	root := os.Getenv("BITSPEED_FINAL_ROOT")
	id := os.Getenv("BITSPEED_FINAL_ID")
	stop := func() {
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	ops := realReceiverOps()
	ops.write = func(f *os.File, b []byte) (int, error) {
		n, e := f.Write(b)
		if point == "assembly write" && filepath.Base(f.Name()) == "assembly.tmp" {
			stop()
		}
		if point == "receipt write" && filepath.Base(f.Name()) == "completion.json.tmp" {
			stop()
		}
		return n, e
	}
	ops.rename = func(a, b string) error {
		e := os.Rename(a, b)
		if e == nil {
			if point == "ready rename" && filepath.Base(b) == "ready" {
				stop()
			}
			if point == "receipt rename" && filepath.Base(b) == "completion.json" {
				stop()
			}
			if filepath.Base(b) == "journal.json" {
				data, _ := os.ReadFile(b)
				if point == "intent rename" && bytes.Contains(data, []byte(`"state":"finalizing"`)) {
					stop()
				}
				if point == "completed rename" && bytes.Contains(data, []byte(`"state":"completed"`)) {
					stop()
				}
			}
		}
		return e
	}
	ops.link = func(a, b string) error {
		e := os.Link(a, b)
		if e == nil && point == "output link" && strings.HasSuffix(b, ".data") {
			stop()
		}
		return e
	}
	r, e := newReceiver(ReceiverConfig{Root: root, Token: testToken}, ops)
	if e != nil {
		os.Exit(4)
	}
	m, e := r.load(id)
	if e != nil {
		os.Exit(5)
	}
	_, _ = r.finalize(context.Background(), m)
	os.Exit(6)
}
func TestKilledFinalizationRecovery(t *testing.T) {
	for _, point := range []string{"intent rename", "assembly write", "ready rename", "output link", "receipt write", "receipt rename", "completed rename"} {
		t.Run(point, func(t *testing.T) {
			r, server, spool, m := stagedFile(t, bytes.Repeat([]byte{5}, int(MinChunkSize)+17))
			server.Close()
			r.Close()
			child := startFinalChild(t, r.root, m.TransferID, point)
			childLine(t, child, "ready")
			child.cmd.Process.Kill()
			child.cmd.Wait()
			// Failed recovery persistence cannot report completed after publication kill.
			if point == "output link" {
				ops := realReceiverOps()
				recoveredData := false
				ops.sync = func(f *os.File) error {
					if f.Name() == filepath.Join(r.sessionDir(m), "ready") {
						recoveredData = true
					}
					if recoveredData && f.Name() == filepath.Join(r.root, "files") {
						return errors.New("injected recovery sync")
					}
					return f.Sync()
				}
				if failed, e := newReceiver(ReceiverConfig{Root: r.root, Token: testToken}, ops); e == nil {
					failed.Close()
					t.Fatal("ignored recovery sync failure")
				}
			}
			again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken})
			if e != nil {
				t.Fatal(e)
			}
			defer again.Close()
			resumed := httptest.NewServer(again)
			defer resumed.Close()
			s, e := Send(context.Background(), resumed.URL, testToken, spool, m.TransferID)
			if e != nil || s.State != Completed || s.Receipt == nil {
				t.Fatal(s, e)
			}
			files, e := os.ReadDir(filepath.Join(r.root, "files"))
			if e != nil || len(files) != 1 || files[0].Name() != m.TransferID+".data" {
				t.Fatal("duplicate output", files, e)
			}
			if data, e := os.ReadFile(again.output(m)); e != nil || !bytes.Equal(data, bytes.Repeat([]byte{5}, int(MinChunkSize)+17)) {
				t.Fatal("wrong published data", e)
			}
		})
	}
}

func startFinalChild(t *testing.T, root, id, point string) *recoveryChild {
	t.Helper()
	c := &recoveryChild{cmd: exec.Command(os.Args[0], "-test.run=^TestFinalizeProcessHelper$"), lines: make(chan string, 8)}
	c.cmd.Env = append(os.Environ(), "BITSPEED_FINAL_HELPER=1", "BITSPEED_FINAL_ROOT="+root, "BITSPEED_FINAL_ID="+id, "BITSPEED_FINAL_POINT="+point)
	c.cmd.Stderr = &c.stderr
	out, e := c.cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = c.cmd.Start(); e != nil {
		t.Fatal(e)
	}
	go func() {
		scan := bufio.NewScanner(out)
		for scan.Scan() {
			c.lines <- scan.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() { c.cmd.Process.Kill(); c.cmd.Wait() })
	return c
}

func TestCompletedReceiptSurvivesMissingChunks(t *testing.T) {
	r, server, spool, m := stagedFile(t, []byte("data"))
	if _, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	os.Remove(filepath.Join(r.sessionDir(m), "chunks", "0"))
	s, e := Send(context.Background(), server.URL, testToken, spool, m.TransferID)
	if e != nil || s.State != Completed || s.VerifiedChunks != 0 || s.Receipt == nil {
		t.Fatal("retried completed chunks instead of receipt", s, e)
	}
}
func TestFinalizingMissingChunkReturnsToUploading(t *testing.T) {
	r, _, _, m := stagedFile(t, []byte("data"))
	r.setState(m, Finalizing, nil)
	os.WriteFile(filepath.Join(r.sessionDir(m), "assembly.tmp"), []byte("pa"), 0600)
	os.Remove(filepath.Join(r.sessionDir(m), "chunks", "0"))
	s, e := r.status(context.Background(), m)
	if e != nil || s.State != Uploading || len(s.MissingChunks) != 1 {
		t.Fatal(s, e)
	}
	j, e := r.readJournal(m)
	if e != nil || j.State != Uploading {
		t.Fatal(j, e)
	}
	if ok, _ := exists(filepath.Join(r.sessionDir(m), "assembly.tmp")); ok {
		t.Fatal("retained incomplete assembly on reset")
	}
}
func TestSenderReceiptFailureGates(t *testing.T) {
	for _, op := range []string{"write", "close", "sync", "rename", "directory"} {
		t.Run(op, func(t *testing.T) {
			_, _, spool, m := stagedFile(t, []byte("data"))
			receipt := expectedReceipt(m)
			ops := realReceiverOps()
			injected := errors.New("injected")
			if op == "write" {
				ops.write = func(f *os.File, b []byte) (int, error) {
					if filepath.Base(f.Name()) == "receipt.json.tmp" {
						return 0, injected
					}
					return f.Write(b)
				}
			}
			if op == "close" {
				ops.close = func(f *os.File) error {
					e := f.Close()
					if filepath.Base(f.Name()) == "receipt.json.tmp" {
						return injected
					}
					return e
				}
			}
			if op == "sync" || op == "directory" {
				ops.sync = func(f *os.File) error {
					if op == "sync" && filepath.Base(f.Name()) == "receipt.json.tmp" {
						return injected
					}
					if op == "directory" && f.Name() == filepath.Join(spool, m.TransferID) {
						return injected
					}
					return f.Sync()
				}
			}
			if op == "rename" {
				ops.rename = func(string, string) error { return injected }
			}
			if e := cacheReceipt(context.Background(), spool, m.TransferID, &receipt, ops); e == nil {
				t.Fatal("acknowledged failed receipt persistence")
			}
			if _, e := Reuse(context.Background(), spool, m.TransferID); e != nil {
				t.Fatal("failed cache prevented snapshot reuse", e)
			}
			if e := cacheReceipt(context.Background(), spool, m.TransferID, &receipt, realReceiverOps()); e != nil {
				t.Fatal("receipt retry failed", e)
			}
		})
	}
}
func TestReservationUpgradeFailsClosed(t *testing.T) {
	r, _, _, m := stagedFile(t, []byte("data"))
	r.Close()
	// An older two-copy policy cannot silently fund three retained copies.
	if again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken, MaxReservedBytes: 2*m.FileSize + spoolHeadroom}); e == nil {
		again.Close()
		t.Fatal("under-reserved existing session")
	}
	again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken, MaxReservedBytes: reservation(m)})
	if e != nil {
		t.Fatal(e)
	}
	again.Close()
}

func TestFinalizingReceiptDamageBecomesFailed(t *testing.T) {
	for _, damage := range []string{"missing output", "missing witness", "corrupt data", "foreign output"} {
		t.Run(damage, func(t *testing.T) {
			r, server, _, m := stagedFile(t, []byte("data"))
			if w := finalRequest(r, m); w.Code != 200 {
				t.Fatal(w.Code)
			}
			r.setState(m, Finalizing, nil)
			switch damage {
			case "missing output":
				os.Remove(r.output(m))
			case "missing witness":
				os.Remove(filepath.Join(r.sessionDir(m), "ready"))
			case "corrupt data":
				os.WriteFile(r.output(m), []byte("bad!"), 0600)
			case "foreign output":
				os.Remove(r.output(m))
				os.WriteFile(r.output(m), []byte("data"), 0600)
			}
			server.Close()
			r.Close()
			again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken})
			if e != nil {
				t.Fatal("could not serve durable failure", e)
			}
			defer again.Close()
			s, e := again.status(context.Background(), m)
			if e != nil || s.State != Failed || s.ErrorCode == nil || *s.ErrorCode != "storage_corrupt" {
				t.Fatal(s, e)
			}
		})
	}
}
func TestMalformedReceiptBlocksFailedBinding(t *testing.T) {
	r, server, _, m := stagedFile(t, []byte("data"))
	r.setState(m, Failed, strptr("final_digest_mismatch"))
	os.WriteFile(filepath.Join(r.sessionDir(m), "completion.json"), []byte("{}"), 0600)
	w := perform(r, "GET", "/v1/transfers/"+m.TransferID, "", nil, 0)
	if w.Code != 503 {
		t.Fatal("accepted malformed failure receipt", w.Code)
	}
	server.Close()
	r.Close()
	if again, e := NewReceiver(ReceiverConfig{Root: r.root, Token: testToken}); e == nil {
		again.Close()
		t.Fatal("startup accepted malformed receipt")
	}
}

func TestFailedAdmissionRetryRejectsMutation(t *testing.T) {
	r, _, _, m := stagedFile(t, []byte("data"))
	r.setState(m, Failed, strptr("final_digest_mismatch"))
	body, _ := m.CanonicalJSON()
	w := perform(r, "POST", "/v1/transfers", "application/json", bytes.NewReader(body), int64(len(body)))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "transfer_failed") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestFinalizationCapacityAndCancellation(t *testing.T) {
	for _, stage := range []string{"capacity", "cancel before", "cancel after link"} {
		t.Run(stage, func(t *testing.T) {
			r, _, _, m := stagedFile(t, []byte("data"))
			real := r.ops
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "capacity":
				r.ops.space = func(string) (int64, error) { return 0, nil }
			case "cancel before":
				cancel()
			case "cancel after link":
				r.ops.link = func(a, b string) error {
					e := os.Link(a, b)
					if e == nil && b == r.output(m) {
						cancel()
					}
					return e
				}
			}
			if _, e := r.finalize(ctx, m); e == nil {
				t.Fatal("claimed completion across failed capacity/cancel gate")
			}
			r.ops = real
			if w := finalRequest(r, m); w.Code != 200 {
				t.Fatal("retry failed", w.Code, w.Body.String())
			}
		})
	}
}
func TestCompletedWireReceiptBinding(t *testing.T) {
	m := dataManifest([]byte("data"))
	identity, _ := m.Identity()
	receipt := expectedReceipt(m)
	s := StoredStatus{1, m.TransferID, identity, Completed, 1, 4, []int{}, &receipt, nil}
	good, _ := json.Marshal(s)
	for _, data := range [][]byte{
		bytes.Replace(good, []byte(`"output_name":"`+m.TransferID+`.data"`), []byte(`"output_name":"../escape"`), 1),
		bytes.Replace(good, []byte(`"file_size":4`), []byte(`"file_size":4,"file_size":4`), 1),
		bytes.Replace(good, []byte(`"file_size":4`), []byte(`"file_size":4.0`), 1),
		bytes.Replace(good, []byte(`"file_sha256":"`+m.FileSHA256+`"`), []byte(`"file_sha256":"`+strings.Repeat("0", 64)+`"`), 1),
	} {
		decoded, e := decodeStatus(data, m.TransferID)
		if e == nil && statusMatches(decoded, m) {
			t.Fatal("accepted forged completion receipt")
		}
	}
}
