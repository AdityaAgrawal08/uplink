package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Enter on a file buffers it; Ctrl+Enter runs the P2P upload pipeline: with
// no live peer the engine seals frames into the inbox fallback, and the
// transcript still gets its success card.
func TestPickerEnterOnFileRunsUploadPipeline(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	fileBody := []byte("session file payload")

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "payload.bin"), fileBody, 0o644)

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob", "alice")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc.picker = pickerState{active: true, cwd: root, home: root, anchor: -1, inBuf: map[string]bool{}}
	sc.loadPickerDir()

	// Cursor onto the single file row and press Enter (buffers it).
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyDown})
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	if len(sc.picker.buffered) != 1 {
		t.Fatalf("Enter should buffer the file; got %d buffered", len(sc.picker.buffered))
	}
	// Ctrl+Enter triggers the upload pipeline.
	sc, cmd := step(sc, tea.KeyMsg{Type: tea.KeyCtrlJ})
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
	// No live data channel in this test, so the small file went as ONE
	// self-contained box (streaming meta/chunk/complete frames share a
	// msgId and would overwrite each other in the msgId-keyed inbox).
	if n := inboxDeposits(fs, "alice"); n != 1 {
		t.Fatalf("inbox deposits = %d; want 1 (single self-contained box)", n)
	}

	// Success annotates the transcript in the room bucket.
	found := false
	for _, ll := range sc.localLines {
		if ll.kind == lineFileCard && ll.fileData != nil && ll.fileData.filename == "payload.bin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing success card: %v", sc.localLines)
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

// pump feeds every message back through Update AND executes any follow-up
// command the model schedules (queue promotions), until quiescent.
func pump(sc chatScreen, cmds ...tea.Cmd) chatScreen {
	queue := append([]tea.Cmd(nil), cmds...)
	for len(queue) > 0 {
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		for _, msg := range drainCmds(cmd) {
			var next tea.Cmd
			sc, next = step(sc, msg)
			queue = append(queue, next)
		}
	}
	return sc
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
	eng := newEngine("bob", mustTestIdentity(t), &signalClient{serverURL: srv.URL, key: "123456", me: "bob"}, engineCallbacks{})
	if _, _, err := runSessionUpload(context.Background(), eng, job, prog); err == nil {
		t.Fatal("oversize upload must fail locally")
	}
	if hitServer {
		t.Fatal("oversize rejection must not touch the network")
	}

	empty := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(empty, nil, 0o644)
	if _, _, err := runSessionUpload(context.Background(), eng, uploadJob{Path: empty}, prog); err == nil {
		t.Fatal("empty upload must fail locally")
	}
}

// mustTestIdentity mints an engine identity (test-only; never touches disk).
func mustTestIdentity(t *testing.T) *identityKey {
	t.Helper()
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Receiver side: an arrived file paints one attachment card and is recorded
// for the /download drawer.
func TestNetFileMsgPaintsCard(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	nm, _ := sc.Update(netFileMsg{file: engineFile{
		MsgId: "f1", From: "alice", To: "", Filename: "report.pdf", Size: 2400000, Path: "/tmp/report.pdf",
	}})
	got := nm.(chatScreen)
	found := false
	for _, ll := range got.localLines {
		if ll.kind == lineFileCard && ll.fileData != nil && ll.fileData.filename == "report.pdf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no file card painted: %+v", got.localLines)
	}
	if len(got.received) != 1 || got.received[0].path != "/tmp/report.pdf" {
		t.Fatalf("received not recorded: %+v", got.received)
	}
}

// ---------------------------------------------------------------------------
// /download drawer + keyboard peer access
// ---------------------------------------------------------------------------

// Tab cycles the roster highlight and empty-Enter opens that thread — the
// keyboard path to private chat (mouse-less terminals had none).
func TestKeyboardPeerCycleAndOpen(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.vp = *viewportPtr(40, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)

	tab := func(m tea.Model) chatScreen { m, _ = step(m, tea.KeyMsg{Type: tea.KeyTab}); return m.(chatScreen) }

	if sc.hoverPeer != "" {
		t.Fatalf("initial focus = %q; want none", sc.hoverPeer)
	}
	sc = tab(sc)
	if sc.hoverPeer == "" || sc.hoverPeer == "bob" {
		t.Fatalf("tab did not focus a peer: %q", sc.hoverPeer)
	}
	first := sc.hoverPeer
	sc = tab(sc)
	sc = tab(sc)
	if sc.hoverPeer != first {
		t.Fatal("cycling must wrap and skip self (bob)")
	}

	// Empty Enter opens the highlighted thread.
	sc2 := newFilterScreen("bob", "", "bob", "alice")
	m2, _ := sc2.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	s2 := m2.(chatScreen)
	s2.hoverPeer = "alice"
	s2, cmd := step(s2, tea.KeyMsg{Type: tea.KeyEnter})
	if s2.targetUser != "alice" {
		t.Fatalf("empty enter did not open thread; target=%q", s2.targetUser)
	}
	_ = cmd // backlog fetch may be non-nil; harmless here
}

func newFilesDrawer(t *testing.T, files []receivedFile) chatScreen {
	t.Helper()
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.received = files
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc.openFilesDrawer()
	return sc
}

// The /download listing shows received files, most recent first.
func TestFilesDrawerSortsRecentFirst(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/download")
	got, _ := step(c, tea.KeyMsg{Type: tea.KeyEnter})
	if !got.picker.isActive() || got.picker.mode != modeFiles {
		t.Fatal("/download must open the files drawer")
	}

	old := time.Now().Add(-time.Hour)
	newer := time.Now()
	got.received = []receivedFile{
		{filename: "old.txt", from: "alice", size: 5, path: "/tmp/old.txt", at: old},
		{filename: "new.txt", from: "bob", size: 6, path: "/tmp/new.txt", at: newer},
	}
	got.openFilesDrawer()
	var order []string
	for _, f := range got.picker.files {
		order = append(order, f.filename)
	}
	if strings.Join(order, ",") != "new.txt,old.txt" {
		t.Fatalf("listing = %v; want newest first", order)
	}
	if !strings.Contains(got.View(), "shared files — 2") {
		t.Fatal("breadcrumb must show the count")
	}
}

// Enter on a row opens the detail window (files arrive complete; the
// detail shows metadata plus the save location).
func TestFilesDrawerEnterOpensDetail(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(40, 10)
	c.targetUser = "alice" // private conversation changes nothing now
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc.targetUser = "alice"
	sc = newFilesDrawerAt(sc, []receivedFile{
		{filename: "grab.bin", from: "alice", size: 25, path: "/tmp/grab.bin", at: time.Now()},
	})

	// Cursor sits on row 0; Enter opens detail (previously: downloaded).
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	if !sc.picker.isActive() || sc.picker.mode != modeDetail {
		t.Fatal("Enter must open the detail window")
	}
	if sc.picker.detailFile == nil || sc.picker.detailFile.filename != "grab.bin" {
		t.Fatalf("wrong detail file: %+v", sc.picker.detailFile)
	}
	// Enter again returns to the list.
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	if sc.picker.mode != modeFiles {
		t.Fatal("second Enter must return to the list")
	}
}

// newFilesDrawerAt reuses an already-wired screen for pipeline tests.
func newFilesDrawerAt(sc chatScreen, files []receivedFile) chatScreen {
	sc.picker = pickerState{
		active: true, mode: modeFiles, home: ".", anchor: -1,
		files: files,
		inBuf: map[string]bool{},
	}
	sc.picker.clampCursor()
	return sc
}

// Files mode is read-only: Space buffers nothing, Ctrl+Enter just closes.
func TestFilesModeReadOnly(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc = newFilesDrawerAt(sc, []receivedFile{
		{filename: "one.txt", from: "alice", size: 12, path: "/tmp/one.txt", at: time.Now()},
		{filename: "two.txt", from: "alice", size: 12, path: "/tmp/two.txt", at: time.Now()},
	})

	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeySpace})
	if len(sc.picker.buffered) != 0 {
		t.Fatal("Space must not buffer in files mode")
	}
	if sc.picker.notice == "" {
		t.Fatal("expected an explanatory notice")
	}
	sc, cmd := step(sc, tea.KeyMsg{Type: tea.KeyCtrlJ})
	_ = cmd
	if sc.picker.isActive() {
		t.Fatal("Ctrl+Enter must close the read-only drawer")
	}
	// Upload buffer untouched by the files drawer.
	if len(sc.uploadBuf) != 0 {
		t.Fatal("files drawer must not pollute the upload buffer")
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
