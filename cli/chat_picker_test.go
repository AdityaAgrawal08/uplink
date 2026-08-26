package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// Pure listing / navigation helpers
// ---------------------------------------------------------------------------

func TestListDirDirsFirstWithDotfiles(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, isDir bool) {
		p := filepath.Join(dir, name)
		if isDir {
			os.MkdirAll(p, 0o755)
			return
		}
		os.WriteFile(p, []byte("x"), 0o644)
	}
	mk("zeta.txt", false)
	mk(".hidden", false)
	mk("beta", true)
	mk("alpha", true)

	got, err := listDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.name)
	}
	want := "alpha,beta,.hidden,zeta.txt" // dirs (alpha,beta) before files
	if strings.Join(names, ",") != want {
		t.Fatalf("listing = %v; want %v", names, want)
	}
	if !got[0].dir || !got[1].dir || got[2].dir || got[3].dir {
		t.Fatalf("dir flags wrong: %+v", got)
	}
}

func TestParentDirAndBreadcrumb(t *testing.T) {
	if p := parentDir("/a/b"); p != "/a" {
		t.Fatalf("parent = %q", p)
	}
	if p := parentDir("/"); p != "" {
		t.Fatalf("root parent = %q; want empty", p)
	}
	if b := breadcrumb("/home/me/x/y", "/home/me"); b != "~/x/y" {
		t.Fatalf("breadcrumb = %q", b)
	}
	if b := breadcrumb("/etc/passwd", "/home/me"); b != "/etc/passwd" {
		t.Fatalf("breadcrumb outside home = %q", b)
	}
}

func TestPickerRangeMath(t *testing.T) {
	if lo, hi := pickerRange(-1, 4); lo != 4 || hi != 4 {
		t.Fatalf("no-anchor range = [%d,%d]", lo, hi)
	}
	if lo, hi := pickerRange(2, 5); lo != 2 || hi != 5 {
		t.Fatalf("forward range = [%d,%d]", lo, hi)
	}
	if lo, hi := pickerRange(5, 2); lo != 2 || hi != 5 {
		t.Fatalf("reversed range = [%d,%d]", lo, hi)
	}
}

// ---------------------------------------------------------------------------
// Picker behaviour through Update
// ---------------------------------------------------------------------------

func newPickerScreenAt(t *testing.T, dir string) chatScreen {
	t.Helper()
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc.picker = pickerState{active: true, cwd: dir, home: dir, anchor: -1, inBuf: map[string]bool{}}
	sc.loadPickerDir()
	return sc
}

func TestPickerBrowseBufferAndQuickUpload(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644)

	c := newPickerScreenAt(t, root)

	// Rows: [..] [sub] [hello.txt] — dirs first.
	if c.picker.rowCount() != 3 {
		t.Fatalf("rows = %d; want 3", c.picker.rowCount())
	}

	// Enter on the dir row browses into it.
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown}) // -> sub
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.HasSuffix(c.picker.cwd, "sub") {
		t.Fatalf("enter on folder did not browse: cwd=%q", c.picker.cwd)
	}

	// Backspace returns to parent.
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyBackspace})
	if c.picker.cwd != root {
		t.Fatalf("backspace did not go up: cwd=%q", c.picker.cwd)
	}

	// Space buffers hello.txt into the tray without closing.
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown})
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown}) // -> hello.txt
	c, _ = step(c, tea.KeyMsg{Type: tea.KeySpace})
	if len(c.picker.buffered) != 1 {
		t.Fatalf("buffer after space = %v", c.picker.buffered)
	}

	// Esc cancels and discards.
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyEsc})
	if c.picker.isActive() || len(c.picker.buffered) != 0 {
		t.Fatal("esc must close the browser and drop the buffer")
	}
}

func TestPickerRangeSelectBuffersWholeRange(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d"} {
		os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644)
	}
	c := newPickerScreenAt(t, root)

	// v starts visual at cursor; skip ".." first so rows 1..3 are files a,b,c.
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown})
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown})
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown})
	c.pickerToggleBuffer()

	if len(c.picker.buffered) != 3 {
		t.Fatalf("range buffer = %v", c.picker.buffered)
	}

	// Shift+Down extends too (distinct key type on supporting terminals).
	// Cursor 0 is ".."; shift-down to row 1 buffers just [a].
	c2 := newPickerScreenAt(t, root)
	c2, _ = step(c2, tea.KeyMsg{Type: tea.KeyShiftDown})
	c2.pickerToggleBuffer()
	if len(c2.picker.buffered) != 1 ||
		filepath.Base(c2.picker.buffered[0]) != "a" {
		t.Fatalf("shift-range buffer = %v", c2.picker.buffered)
	}
}

// Enter on a file quick-uploads it through the whole wire pipeline.
func TestPickerEnterOnFileRunsUploadPipeline(t *testing.T) {
	var calls []string
	var announced struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
		Sha256   string `json:"sha256"`
	}
	fileBody := []byte("session file payload")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/announce"):
			calls = append(calls, "announce")
			json.NewDecoder(r.Body).Decode(&announced)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"fileId":"fid-1","shareId":"sid-1"}`)
		case r.URL.Path == "/api/v1/share/init":
			calls = append(calls, "init")
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if req["shareId"] != "sid-1" {
				t.Errorf("init shareId = %v; want sid-1", req["shareId"])
			}
			fmt.Fprintf(w, `{"shareId":"sid-1","uploadUrl":%q,"objectKey":"k","filename":"f"}`,
				srv.URL+"/put-here")
		case r.URL.Path == "/put-here":
			calls = append(calls, "PUT")
			body, _ := io.ReadAll(r.Body)
			if !bytes.Equal(body, fileBody) {
				t.Errorf("uploaded bytes mismatch")
			}
			w.WriteHeader(200)
		case strings.HasSuffix(r.URL.Path, "/confirm"):
			calls = append(calls, "confirm")
			fmt.Fprint(w, `{"message":"ok"}`)
		case strings.HasSuffix(r.URL.Path, "/upload-complete"):
			calls = append(calls, "complete")
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if req["fileId"] != "fid-1" || req["shareId"] != "sid-1" {
				t.Errorf("complete payload = %v", req)
			}
			fmt.Fprint(w, `{"success":true}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, 404)
		}
	}))
	defer srv.Close()

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "payload.bin"), fileBody, 0o644)

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc.client = newChatClient(srv.URL, "123456", "bob")
	sc.picker = pickerState{active: true, cwd: root, home: root, anchor: -1, inBuf: map[string]bool{}}
	sc.loadPickerDir()

	// Cursor onto the single file row and press Enter (quick upload).
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyDown})
	sc, cmd := step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	// Drain the batched cmds until the done message lands.
	msgs := drainCmds(cmd)
	var done *uploadDoneMsg
	for _, m := range msgs {
		if dm, ok := m.(uploadDoneMsg); ok {
			done = &dm
		}
		// Feed every message back through Update like the real event loop.
		sc, _ = step(sc, m)
	}
	if done == nil {
		t.Fatalf("pipeline produced no uploadDoneMsg; got %d msgs", len(msgs))
	}
	if done.err != nil {
		t.Fatalf("upload failed: %v", done.err)
	}
	want := []string{"announce", "init", "PUT", "confirm", "complete"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("wire sequence = %v; want %v", calls, want)
	}
	if announced.Size != int64(len(fileBody)) || announced.Filename != "payload.bin" ||
		len(announced.Sha256) != 64 {
		t.Fatalf("announce payload wrong: %+v", announced)
	}

	// Success annotates the transcript in the room bucket.
	found := false
	for _, ll := range sc.localLines {
		if strings.Contains(ll.text, "you shared payload.bin") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing success line: %v", sc.localLines)
	}
}

// drainCmds executes a tea.Cmd tree, following Batch wrappers recursively,
// returning every produced message in order.
func drainCmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch m := msg.(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range m {
			out = append(out, drainCmds(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// Oversized and empty files are rejected BEFORE any network call.
func TestUploadLocalGuards(t *testing.T) {
	hitServer := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitServer = true
		w.WriteHeader(500)
	}))
	defer srv.Close()

	big := filepath.Join(t.TempDir(), "big.bin")
	f, _ := os.Create(big)
	f.Truncate(uploadMaxBytes + 1)
	f.Close()

	job := uploadJob{Path: big}
	prog := make(chan uploadProgressMsg, 8) // left open; sends are select-defaulted
	client := newChatClient(srv.URL, "123456", "bob")
	if _, _, err := runSessionUpload(context.Background(), client, "bob", job, prog); err == nil {
		t.Fatal("oversize upload must fail locally")
	}
	if hitServer {
		t.Fatal("oversize rejection must not touch the network")
	}

	empty := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(empty, nil, 0o644)
	if _, _, err := runSessionUpload(context.Background(), client, "bob", uploadJob{Path: empty}, prog); err == nil {
		t.Fatal("empty upload must fail locally")
	}
}

// Receiver side: unseen UPLOADED room files paint announcements; own files,
// duplicates and non-UPLOADED statuses never do.
func TestApplyRoomFilesAnnounces(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.filesSeen = map[string]bool{}
	files := []sessionFile{
		{FileId: "f1", Username: "alice", Filename: "report.pdf", Size: 2400000, Status: "UPLOADED", UploadedAt: "2026-08-26T01:00:00Z"},
		{FileId: "f2", Username: "bob", Filename: "mine.zip", Size: 10, Status: "UPLOADED", UploadedAt: "2026-08-26T01:01:00Z"},
		{FileId: "f3", Username: "alice", Filename: "again.pdf", Size: 5, Status: "ANNOUNCED", UploadedAt: "2026-08-26T01:02:00Z"},
	}
	c.applyRoomFiles(files)
	var lines []string
	for _, ll := range c.localLines {
		lines = append(lines, ll.text)
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "alice shared report.pdf (2.3 MB)") {
		t.Fatalf("missing alice announcement: %v", lines)
	}
	if strings.Contains(joined, "mine.zip") || strings.Contains(joined, "again.pdf") {
		t.Fatalf("own or unannounced file leaked: %v", lines)
	}
	// Cursor advances only over PAINTED uploads: skipping ANNOUNCED rows
	// must not move it, or their later completion (which bumps uploadedAt)
	// would fall behind the watermark and never be announced.
	if c.lastFilesAt != "2026-08-26T01:01:00Z" {
		t.Fatalf("cursor = %q; want newest UPLOADED uploadedAt", c.lastFilesAt)
	}

	// Re-delivery of the same fileId must not duplicate the line.
	before := len(c.localLines)
	c.applyRoomFiles(files[:1])
	if len(c.localLines) != before {
		t.Fatal("duplicate fileId painted twice")
	}
}

func TestTarballDirPacksFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "one.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(dir, "two.txt"), []byte("22"), 0o644)

	out, err := tarballDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)

	f, _ := os.Open(out)
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	if strings.Join(names, ",") != "one.txt,two.txt" {
		t.Fatalf("tarball contents = %v", names)
	}
}
