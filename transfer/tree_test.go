package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// sampleTree writes nested folders, an empty folder, an empty file and files
// that cross chunk boundaries.
func sampleTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	files := map[string][]byte{
		"README.txt":             []byte("hello tree\n"),
		"a/b/c/deep.bin":         bytes.Repeat([]byte{1, 2, 3}, int(MinChunkSize)),
		"a/b/second.txt":         []byte("second"),
		"a-b/sorts-between.txt":  []byte("dash sorts before slash"),
		"empty-file":             nil,
		"photos/2026/frame.raw":  bytes.Repeat([]byte{0xfe}, int(MinChunkSize)/3),
		"photos/café.jpg":        []byte("unicode name"),
		"spaces in name/x y.txt": []byte("spaces"),
	}
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.MkdirAll(filepath.Join(dir, "empty-folder", "inner"), 0700); e != nil {
		t.Fatal(e)
	}
	return dir
}

// treeListing maps each path below dir to its digest, or "dir".
func treeListing(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	e := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func sameListing(t *testing.T, a, b map[string]string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatal("listings differ in size", len(a), len(b), a, b)
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatal("listing differs at", k, v, b[k])
		}
	}
}

// sendTree prepares src, sends it with a parallel window and returns the
// receiver root and transfer ID.
func sendTree(t *testing.T, src string) (string, Manifest, TreeSummary) {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "spool")
	m, summary, e := PrepareTree(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	r, server := testReceiver(t)
	s, e := SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 4})
	if e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	return r.root, m, summary
}

func TestTreeSendAndUnpack(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	src := sampleTree(t)
	root, m, summary := sendTree(t, src)
	if summary.Files != 8 || summary.Folders != 9 {
		t.Fatal("summary", summary)
	}
	out := filepath.Join(t.TempDir(), "restored")
	result, e := Unpack(context.Background(), root, m.TransferID, out)
	if e != nil {
		t.Fatal(e)
	}
	if result.TreeSummary != summary || result.TransferID != m.TransferID {
		t.Fatal("result", result)
	}
	sameListing(t, treeListing(t, src), treeListing(t, out))
	// No temp folder is left next to the output.
	entries, _ := os.ReadDir(filepath.Dir(out))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".bitspeed-unpack-") {
			t.Fatal("temp folder left", entry.Name())
		}
	}
	// Unpacking again into the same place is refused and leaves it alone.
	if _, e = Unpack(context.Background(), root, m.TransferID, out); !errors.Is(e, ErrUnpack) {
		t.Fatal("existing output accepted", e)
	}
	sameListing(t, treeListing(t, src), treeListing(t, out))
	// An empty existing folder is refused too.
	empty := filepath.Join(t.TempDir(), "empty")
	os.Mkdir(empty, 0700)
	if _, e = Unpack(context.Background(), root, m.TransferID, empty); !errors.Is(e, ErrUnpack) {
		t.Fatal("existing empty output accepted", e)
	}
	// Output inside the receiver root is refused.
	if _, e = Unpack(context.Background(), root, m.TransferID, filepath.Join(root, "files", "x")); !errors.Is(e, ErrUnpack) {
		t.Fatal("output inside the root accepted", e)
	}
	// A missing parent is refused.
	if _, e = Unpack(context.Background(), root, m.TransferID, filepath.Join(t.TempDir(), "no", "such")); !errors.Is(e, ErrUnpack) {
		t.Fatal("missing parent accepted", e)
	}
}

func TestEmptyTree(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	src := filepath.Join(t.TempDir(), "nothing")
	os.Mkdir(src, 0700)
	root, m, summary := sendTree(t, src)
	if summary != (TreeSummary{}) {
		t.Fatal(summary)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, e := Unpack(context.Background(), root, m.TransferID, out); e != nil {
		t.Fatal(e)
	}
	if entries, e := os.ReadDir(out); e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
}

func TestUnpackRefusesFilesAndUnfinishedTransfers(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	// A plain file transfer is not a tree.
	data := []byte("just one file, not a tree")
	r, url, spool, m := preparedOnly(t, data)
	if _, e := Send(context.Background(), url, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, e := Unpack(context.Background(), r.root, m.TransferID, out); !errors.Is(e, ErrNotTree) {
		t.Fatal("file transfer unpacked", e)
	}
	// An uploaded but unfinished tree is not ready.
	src := sampleTree(t)
	treeSpool := filepath.Join(t.TempDir(), "spool")
	tm, _, e := PrepareTree(context.Background(), src, treeSpool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = UploadOnly(context.Background(), url, testToken, treeSpool, tm.TransferID); e != nil {
		t.Fatal(e)
	}
	if _, e = Unpack(context.Background(), r.root, tm.TransferID, out); !errors.Is(e, ErrNotReady) {
		t.Fatal("unfinished tree unpacked", e)
	}
	if _, e = Unpack(context.Background(), r.root, strings.Repeat("e", 64), out); !errors.Is(e, ErrNotReady) {
		t.Fatal("unknown transfer", e)
	}
	if _, e = os.Lstat(out); !os.IsNotExist(e) {
		t.Fatal("output created by a refused unpack")
	}
}

func TestUnpackDetectsDamagedPayload(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	src := sampleTree(t)
	root, m, _ := sendTree(t, src)
	data := filepath.Join(root, "files", m.TransferID+".data")
	original, e := os.ReadFile(data)
	if e != nil {
		t.Fatal(e)
	}
	damage := func(t *testing.T, at int) {
		changed := bytes.Clone(original)
		changed[at] ^= 0xff
		unseal(data)
		os.Chmod(data, 0600)
		if e := os.WriteFile(data, changed, 0600); e != nil {
			t.Fatal(e)
		}
	}
	defer os.WriteFile(data, original, 0600)
	for name, at := range map[string]int{"file byte": 10, "index byte": len(original) - treeFooterSize - 40, "footer magic": len(original) - 1} {
		t.Run(name, func(t *testing.T) {
			damage(t, at)
			out := filepath.Join(t.TempDir(), "out")
			if _, e := Unpack(context.Background(), root, m.TransferID, out); e == nil {
				t.Fatal("damaged payload unpacked")
			}
			if _, e := os.Lstat(out); !os.IsNotExist(e) {
				t.Fatal("output created from a damaged payload")
			}
			entries, _ := os.ReadDir(filepath.Dir(out))
			if len(entries) != 0 {
				t.Fatal("temp folder left", entries)
			}
		})
	}
}

func TestTreeIndexRules(t *testing.T) {
	emptyDigest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	digest := strings.Repeat("a", 64)
	good := TreeIndex{TreeFormat, TreeVersion, []TreeEntry{{"dir", "a", 0, ""}, {"file", "a/b", 3, digest}, {"file", "c", 0, emptyDigest}}}
	data, e := good.CanonicalJSON()
	if e != nil {
		t.Fatal(e)
	}
	if back, e := decodeTreeIndex(data); e != nil || len(back.Entries) != 3 {
		t.Fatal(back, e)
	}
	bad := map[string]TreeIndex{
		"format":        {"other", TreeVersion, good.Entries},
		"version":       {TreeFormat, 2, good.Entries},
		"nil entries":   {TreeFormat, TreeVersion, nil},
		"unsorted":      {TreeFormat, TreeVersion, []TreeEntry{{"file", "c", 0, emptyDigest}, {"dir", "a", 0, ""}}},
		"duplicate":     {TreeFormat, TreeVersion, []TreeEntry{{"dir", "a", 0, ""}, {"dir", "a", 0, ""}}},
		"no parent":     {TreeFormat, TreeVersion, []TreeEntry{{"file", "a/b", 3, digest}}},
		"file parent":   {TreeFormat, TreeVersion, []TreeEntry{{"file", "a", 3, digest}, {"file", "a/b", 3, digest}}},
		"dot dot":       {TreeFormat, TreeVersion, []TreeEntry{{"file", "..", 3, digest}}},
		"absolute":      {TreeFormat, TreeVersion, []TreeEntry{{"file", "/etc", 3, digest}}},
		"backslash":     {TreeFormat, TreeVersion, []TreeEntry{{"file", `a\b`, 3, digest}}},
		"control":       {TreeFormat, TreeVersion, []TreeEntry{{"file", "a\nb", 3, digest}}},
		"empty segment": {TreeFormat, TreeVersion, []TreeEntry{{"dir", "a", 0, ""}, {"file", "a//b", 3, digest}}},
		"bad utf8":      {TreeFormat, TreeVersion, []TreeEntry{{"file", "a\xff", 3, digest}}},
		"long name":     {TreeFormat, TreeVersion, []TreeEntry{{"file", strings.Repeat("n", MaxTreeNameBytes+1), 3, digest}}},
		"dir digest":    {TreeFormat, TreeVersion, []TreeEntry{{"dir", "a", 0, digest}}},
		"bad digest":    {TreeFormat, TreeVersion, []TreeEntry{{"file", "a", 3, "XYZ"}}},
		"empty digest":  {TreeFormat, TreeVersion, []TreeEntry{{"file", "a", 0, digest}}},
		"negative":      {TreeFormat, TreeVersion, []TreeEntry{{"file", "a", -1, digest}}},
		"type":          {TreeFormat, TreeVersion, []TreeEntry{{"link", "a", 0, ""}}},
	}
	for name, index := range bad {
		if index.Validate() == nil {
			t.Error(name, "accepted")
		}
	}
	// Only the exact canonical bytes decode.
	for name, raw := range map[string]string{
		"whitespace": strings.Replace(string(data), ",", ", ", 1),
		"extra key":  strings.Replace(string(data), `"version":1`, `"version":1,"x":1`, 1),
		"dup key":    strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1),
		"case key":   strings.Replace(string(data), `"version"`, `"Version"`, 1),
		"trailing":   string(data) + " ",
	} {
		if _, e := decodeTreeIndex([]byte(raw)); e == nil {
			t.Error(name, "decoded")
		}
	}
	if !windowsName("photos/frame.raw") || windowsName("a:b") || windowsName("con.txt") || windowsName("COM1") || windowsName("x/lpt9.log") || windowsName("trailing.") || windowsName("space ") || windowsName("q?") {
		t.Error("windows name rules")
	}
	if !windowsName("console") || !windowsName("COM10") {
		t.Error("windows name rules too strict")
	}
}

func TestReadTreeIndexBounds(t *testing.T) {
	short := bytes.NewReader([]byte("tiny"))
	if _, _, e := readTreeIndex(short, 4); !errors.Is(e, ErrNotTree) {
		t.Fatal(e)
	}
	// Length larger than the payload.
	payload := append(bytes.Repeat([]byte{0}, 8), treeFooter(1<<20)...)
	if _, _, e := readTreeIndex(bytes.NewReader(payload), int64(len(payload))); !errors.Is(e, ErrTree) {
		t.Fatal(e)
	}
	// Index claims fewer content bytes than precede it.
	index := TreeIndex{TreeFormat, TreeVersion, []TreeEntry{}}
	data, _ := index.CanonicalJSON()
	payload = append(append([]byte("stray"), data...), treeFooter(len(data))...)
	if _, _, e := readTreeIndex(bytes.NewReader(payload), int64(len(payload))); !errors.Is(e, ErrTree) {
		t.Fatal(e)
	}
}

func TestTreeRefusesSymlinksAndOverlap(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	src := sampleTree(t)
	spool := filepath.Join(t.TempDir(), "spool")
	if e := os.Symlink("README.txt", filepath.Join(src, "link")); e != nil {
		t.Skip("symlinks unavailable", e)
	}
	if _, _, e := PrepareTree(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize}); !errors.Is(e, ErrSource) || !strings.Contains(e.Error(), "symbolic link") {
		t.Fatal("symlink accepted", e)
	}
	os.Remove(filepath.Join(src, "link"))
	// The spool inside the source, or the source inside the spool, is refused.
	if _, _, e := PrepareTree(context.Background(), src, filepath.Join(src, "spool"), PrepareOptions{}); !errors.Is(e, ErrSource) {
		t.Fatal("spool inside source accepted", e)
	}
	if _, _, e := PrepareTree(context.Background(), filepath.Join(src, "a"), src, PrepareOptions{}); !errors.Is(e, ErrSource) {
		t.Fatal("source inside spool accepted", e)
	}
	// A symlinked root is not followed.
	link := filepath.Join(t.TempDir(), "rootlink")
	os.Symlink(src, link)
	if _, _, e := PrepareTree(context.Background(), link, spool, PrepareOptions{}); !errors.Is(e, ErrSource) {
		t.Fatal("symlinked root accepted", e)
	}
	if entries, _ := os.ReadDir(spool); len(entries) > 1 {
		t.Fatal("refused preparation left spool entries", entries)
	}
}

func TestTreeDetectsChangesDuringCopy(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	for name, change := range map[string]func(string) error{
		"file grows": func(src string) error {
			f, e := os.OpenFile(filepath.Join(src, "README.txt"), os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			defer f.Close()
			_, e = f.WriteString("more")
			return e
		},
		"file added":   func(src string) error { return os.WriteFile(filepath.Join(src, "late.txt"), []byte("late"), 0600) },
		"folder added": func(src string) error { return os.Mkdir(filepath.Join(src, "late"), 0700) },
		"file removed": func(src string) error { return os.Remove(filepath.Join(src, "spaces in name", "x y.txt")) },
	} {
		t.Run(name, func(t *testing.T) {
			if name == "file grows" && runtime.GOOS == "windows" {
				t.Skip("Windows holds the file being copied with read-only sharing, so no writer can open it; TestWindowsSourceWritersAreRefusedAndBlocked covers that")
			}
			src := sampleTree(t)
			spool := filepath.Join(t.TempDir(), "spool")
			changed := false
			_, _, e := PrepareTree(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize, Progress: func(p Progress) error {
				if !changed && p.Stage == "copying" {
					changed = true
					return change(src)
				}
				return nil
			}})
			if !errors.Is(e, ErrSource) {
				t.Fatal("change not detected", e)
			}
			entries, _ := os.ReadDir(spool)
			for _, entry := range entries {
				if entry.Name() != ".lock" {
					t.Fatal("refused preparation left", entry.Name())
				}
			}
		})
	}
}

func TestTreeRefusesBadNamesAndNonFolders(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	spool := filepath.Join(t.TempDir(), "spool")
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, []byte("x"), 0600)
	if _, _, e := PrepareTree(context.Background(), file, spool, PrepareOptions{}); !errors.Is(e, ErrSource) {
		t.Fatal("file accepted as a tree", e)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, name := range []string{"back\\slash", "new\nline"} {
		src := filepath.Join(t.TempDir(), "src")
		os.Mkdir(src, 0700)
		if e := os.WriteFile(filepath.Join(src, name), []byte("x"), 0600); e != nil {
			t.Fatal(e)
		}
		if _, _, e := PrepareTree(context.Background(), src, spool, PrepareOptions{}); !errors.Is(e, ErrSource) || !strings.Contains(e.Error(), "name") {
			t.Fatal(name, "accepted", e)
		}
	}
}

// treePayload builds tree bytes by hand, so tests can carry names the local
// filesystem could not hold.
func treePayload(t *testing.T, files []string, contents [][]byte) []byte {
	t.Helper()
	index := TreeIndex{TreeFormat, TreeVersion, []TreeEntry{}}
	var body []byte
	for i, name := range files {
		sum := sha256.Sum256(contents[i])
		index.Entries = append(index.Entries, TreeEntry{"file", name, int64(len(contents[i])), hex.EncodeToString(sum[:])})
		body = append(body, contents[i]...)
	}
	data, e := index.CanonicalJSON()
	if e != nil {
		t.Fatal(e)
	}
	return append(append(body, data...), treeFooter(len(data))...)
}

// On a case-insensitive filesystem two names that differ only in case would
// collide; unpack must fail rather than merge them. On a case-sensitive one
// both come back.
func TestUnpackCaseClash(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	probe := t.TempDir()
	os.WriteFile(filepath.Join(probe, "Probe"), nil, 0600)
	_, e := os.Lstat(filepath.Join(probe, "probe"))
	insensitive := e == nil
	payload := treePayload(t, []string{"Name.txt", "name.txt"}, [][]byte{[]byte("upper"), []byte("lower")})
	r, url, spool, m := preparedOnly(t, payload)
	if _, e := Send(context.Background(), url, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(t.TempDir(), "out")
	_, e = Unpack(context.Background(), r.root, m.TransferID, out)
	if insensitive {
		if !errors.Is(e, ErrUnpack) {
			t.Fatal("case clash merged", e)
		}
		if _, e := os.Lstat(out); !os.IsNotExist(e) {
			t.Fatal("output created despite the clash")
		}
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	a, _ := os.ReadFile(filepath.Join(out, "Name.txt"))
	b, _ := os.ReadFile(filepath.Join(out, "name.txt"))
	if string(a) != "upper" || string(b) != "lower" {
		t.Fatal(string(a), string(b))
	}
}

func TestUnpackRefusesWindowsNamesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	payload := treePayload(t, []string{"a:stream"}, [][]byte{[]byte("x")})
	r, url, spool, m := preparedOnly(t, payload)
	if _, e := Send(context.Background(), url, testToken, spool, m.TransferID); e != nil {
		t.Fatal(e)
	}
	if _, e := Unpack(context.Background(), r.root, m.TransferID, filepath.Join(t.TempDir(), "out")); !errors.Is(e, ErrTree) {
		t.Fatal("Windows-invalid name unpacked", e)
	}
}

func TestTreeResumesAfterFailedUpload(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	src := sampleTree(t)
	spool := filepath.Join(t.TempDir(), "spool")
	m, _, e := PrepareTree(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	r, _ := testReceiver(t)
	var puts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "PUT" && puts.Add(1) == 3 {
			// The connection drops mid-upload.
			io.CopyN(io.Discard, q.Body, 100)
			panic(http.ErrAbortHandler)
		}
		r.ServeHTTP(w, q)
	}))
	defer server.Close()
	if _, e = SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 2}); e == nil {
		t.Fatal("dropped upload reported success")
	}
	var sent []int
	if _, e = SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 3, Progress: func(p SendProgress) error {
		if p.Stage == "chunk" {
			sent = append(sent, p.ChunkIndex)
		}
		return nil
	}}); e != nil {
		t.Fatal(e)
	}
	if len(sent) == 0 || len(sent) >= len(m.ChunkSHA256) {
		t.Fatal("resume resent everything or nothing", sent)
	}
	sort.Ints(sent)
	out := filepath.Join(t.TempDir(), "out")
	if _, e = Unpack(context.Background(), r.root, m.TransferID, out); e != nil {
		t.Fatal(e)
	}
	sameListing(t, treeListing(t, src), treeListing(t, out))
}

// A tree is an ordinary payload, but check it end to end anyway: kill both
// processes part way through a parallel tree upload, restart, resume and
// unpack.
func TestKilledTreeUploadResume(t *testing.T) {
	if !spoolSupported() {
		t.Skip("unsupported platform")
	}
	src := sampleTree(t)
	spool := filepath.Join(t.TempDir(), "spool")
	m, _, e := PrepareTree(context.Background(), src, spool, PrepareOptions{ChunkSize: MinChunkSize})
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(t.TempDir(), "receiver")
	receiver := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=receiver", "BITSPEED_RECOVERY_ROOT="+root, "BITSPEED_RECOVERY_POINT=chunk write")
	url := childLine(t, receiver, "url")
	sender := startRecoveryChild(t, "BITSPEED_RECOVERY_HELPER=sender", "BITSPEED_RECOVERY_URL="+url, "BITSPEED_RECOVERY_SPOOL="+spool, "BITSPEED_RECOVERY_ID="+m.TransferID, "BITSPEED_RECOVERY_PARALLEL=4")
	childLine(t, receiver, "ready")
	sender.cmd.Process.Kill()
	sender.cmd.Wait()
	receiver.cmd.Process.Kill()
	receiver.cmd.Wait()
	restarted, e := NewReceiver(ReceiverConfig{Root: root, Token: testToken})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(restarted)
	defer func() { server.Close(); restarted.Close() }()
	if s, e := SendWithOptions(context.Background(), server.URL, testToken, spool, m.TransferID, SendOptions{Parallel: 4}); e != nil || s.State != Completed {
		t.Fatal(s, e)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, e := Unpack(context.Background(), root, m.TransferID, out); e != nil {
		t.Fatal(e)
	}
	sameListing(t, treeListing(t, src), treeListing(t, out))
}
