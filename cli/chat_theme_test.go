package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Regression: the camera strip must paint engine frames VERBATIM (truecolor
// braille blur). Stripping SGR or re-wrapping turns the image into
// monochrome dots.
func TestCamTilesKeepFrameColor(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	frame := "\x1b[38;2;10;20;30m\x1b[48;2;1;2;3m⣿⣿⣿\x1b[0m"
	c.selfLines = []string{frame, frame, frame}
	out := c.camerasStripView(100)
	if !strings.Contains(out, "38;2;10;20;30") {
		t.Fatalf("tile stripped frame foreground color:\n%s", stripANSI(out))
	}
	if !strings.Contains(out, "48;2;1;2;3") {
		t.Fatalf("tile stripped frame background color:\n%s", stripANSI(out))
	}
	// Braille cells survive (visible image, not placeholders).
	if !strings.Contains(stripANSI(out), "⣿") {
		t.Fatalf("tile lost braille cells:\n%s", stripANSI(out))
	}
}

func TestVisibleVideoPreservesRenderSize(t *testing.T) {
	// Screenshot-exact: video surfaces claim UI space on roomy terminals
	// (panel when wide, strip when narrow) and collapse only when short —
	// while never disturbing the negotiated render size.
	for _, size := range [][2]int{{100, 30}, {154, 44}} {
		c := newFilterScreen("bob", "", "bob", "alice", "carol")
		c.vp = *viewportPtr(60, 20)
		c.call = &mediaManager{videoOn: true, renderCols: 60, renderRows: 20}
		m, _ := c.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		sc := m.(chatScreen)
		l := sc.layoutFor()
		if l.vidPanelW == 0 && l.camRows == 0 {
			t.Fatalf("video UI claimed no space at %v", size)
		}
		if !sc.call.VideoOn() {
			t.Fatalf("video UI stopped publishing at %v", size)
		}
	}
	// Short terminal: the strip collapses cleanly, state still preserved.
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.vp = *viewportPtr(60, 20)
	c.call = &mediaManager{videoOn: true, renderCols: 60, renderRows: 20}
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 18})
	sc := m.(chatScreen)
	l := sc.layoutFor()
	if l.camRows != 0 || l.vidPanelW != 0 {
		t.Fatalf("video UI must collapse on short terminals, got panel=%d strip=%d", l.vidPanelW, l.camRows)
	}
	if sc.call.renderCols != 60 || sc.call.renderRows != 20 || !sc.call.VideoOn() {
		t.Fatal("collapsed video UI changed media state")
	}
}

// ---- adaptive density: every resize step must visibly rebalance --------

func TestTopBarCollapsesByWidth(t *testing.T) {
	// Screenshot-exact: wide terminals show the promises tagline.
	for _, w := range []int{90, 110, 160, 220} {
		got := stripANSI(topBarView(w))
		for _, want := range []string{"secure", "ephemeral", "p2p"} {
			if !strings.Contains(got, want) {
				t.Fatalf("topBarView(%d) must show %q: %q", w, want, got)
			}
		}
	}
	// Cramped terminals drop the tagline but keep the logo.
	for _, w := range []int{40, 60} {
		got := stripANSI(topBarView(w))
		for _, banned := range []string{"secure", "ephemeral", "p2p"} {
			if strings.Contains(got, banned) {
				t.Fatalf("topBarView(%d) must not show %q: %q", w, banned, got)
			}
		}
	}
	full := topBarView(160)
	if !strings.Contains(full, "202") {
		t.Fatalf("wide top bar must show date: %q", full)
	}
	mid := topBarView(90)
	if strings.Contains(mid, "202") {
		t.Fatalf("mid top bar must drop date: %q", mid)
	}
	mini := topBarView(40)
	if !strings.Contains(mini, "UPLINK") {
		t.Fatalf("mini top bar must keep the logo: %q", mini)
	}
	for _, w := range []int{30, 40, 55, 60, 75, 90, 110, 160, 220} {
		if got := lipgloss.Width(topBarView(w)); got > w {
			t.Fatalf("topBarView(%d) width %d overflows", w, got)
		}
	}
}

func TestBubbleRatioFluid(t *testing.T) {
	if got := bubbleRatioFor(40); got != 0.9 {
		t.Errorf("narrow ratio = %v; want 0.9", got)
	}
	if got := bubbleRatioFor(80); got != 0.7 {
		t.Errorf("mid ratio = %v; want 0.7", got)
	}
	if got := bubbleRatioFor(120); got != 0.62 {
		t.Errorf("wide ratio = %v; want 0.62", got)
	}
	if got := bubbleRatioFor(200); got != 0.5 {
		t.Errorf("xwide ratio = %v; want 0.5", got)
	}
}

func TestTranscriptSenderHasNoAvatar(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m := chatMessage{Seq: 1, Username: "alice", Kind: "chat", Text: "hi",
		ConvID: generalConv, CreatedAt: "2026-09-15T19:21:00Z"}
	for _, w := range []int{70, 140} {
		c.width = w
		c.renderCache = map[int]string{}
		got := stripANSI(c.renderLine(m))
		if !strings.Contains(got, "alice") {
			t.Fatalf("w=%d: sender must keep the colored name:\n%s", w, got)
		}
	}
}

func TestRenderCacheInvalidatesOnBubbleFlip(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.width = 70 // wide bubbles
	c.cacheForWidth(50)
	c.renderCache[7] = "stale"
	c.width = 200 // same viewport width, bubble ratio flipped
	c.cacheForWidth(50)
	// Width key dominates here (ratio derives from viewport width); the
	// density generation additionally guards terminal-only flips.
	if _, ok := c.renderCache[7]; ok {
		t.Fatal("density flip must invalidate render caches even at stable width")
	}
}

func TestRoomHeaderCompactRows(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	if got := strings.Count(c.roomHeaderCompact(80), "\n") + 1; got != 1 {
		t.Fatalf("compact header must be exactly 1 row, got %d", got)
	}
	if got := strings.Count(c.roomHeaderView(80), "\n") + 1; got != 2 {
		t.Fatalf("full header must be exactly 2 rows, got %d", got)
	}
	if h := c.roomHeaderCompact(80); !strings.Contains(h, "General") {
		t.Fatalf("compact header must keep the room name: %q", h)
	}
}

func TestSidebarItemHeightAdapts(t *testing.T) {
	c := newFilterScreen("me", "", "me", "a", "b", "c")
	c.width, c.height = 120, 40
	if got := c.chatItemHeight(); got != 2 {
		t.Fatalf("roomy terminal must use 2-row items, got %d", got)
	}
	c.height = 20
	if got := c.chatItemHeight(); got != 1 {
		t.Fatalf("short terminal must collapse to 1-row items, got %d", got)
	}
	// Row budget halves: twice the chats visible without scrolling.
	c.width, c.height = 120, 40
	full := c.chatItemRows(c.chatItems(), 24, "")
	c.height = 20
	compact := c.chatItemRows(c.chatItems(), 24, "")
	if len(compact)*2 != len(full) {
		t.Fatalf("compact rows %d must be half of comfortable %d", len(compact), len(full))
	}
}

func TestTilePeerTargetGrowsWithWidth(t *testing.T) {
	mk := func(w int) *chatScreen {
		c := newFilterScreen("me", "", "me", "a", "b", "c", "d", "e", "f")
		c.width, c.height = w, 40
		return c
	}
	if got := len(mk(60).camFeeds()); got != 2 {
		t.Fatalf("narrow strip must show 2 tiles, got %d", got)
	}
	if got := len(mk(100).camFeeds()); got != 3 {
		t.Fatalf("mid strip must show 3 tiles, got %d", got)
	}
	if got := len(mk(140).camFeeds()); got != 4 {
		t.Fatalf("wide strip must show 4 tiles, got %d", got)
	}
	if got := len(mk(200).camFeeds()); got != 6 {
		t.Fatalf("xwide strip must show 6 tiles, got %d", got)
	}
}

func TestWheelStepProportional(t *testing.T) {
	if got := wheelStepFor(5); got != 1 {
		t.Errorf("tiny pane step = %d; want 1", got)
	}
	if got := wheelStepFor(15); got != 3 {
		t.Errorf("mid pane step = %d; want 3", got)
	}
	if got := wheelStepFor(100); got != 6 {
		t.Errorf("huge pane step = %d; want clamp 6", got)
	}
	prev := 0
	for h := 0; h <= 80; h++ {
		if got := wheelStepFor(h); got < prev {
			t.Fatalf("wheelStepFor(%d) = %d < %d: not monotonic", h, got, prev)
		} else {
			prev = got
		}
	}
}

func TestComposerSendButton(t *testing.T) {
	// Screenshot-exact: the composer row carries a blue Send button beside
	// the message box (Enter and click both send).
	const W, H = 160, 44
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	got := stripANSI(sc.View())
	if !strings.Contains(got, "Send") {
		t.Fatalf("Send button must be visible:\n%s", got)
	}
	l := sc.layoutFor()
	sendX0, sendX1, y0, y1, _ := composerGeoms(l, W, H)
	if sendX0 < 0 || sendX1 <= sendX0 || y1 <= y0 {
		t.Fatalf("Send button must hit-test a real rect, got x=[%d,%d) y=[%d,%d)", sendX0, sendX1, y0, y1)
	}
	found := false
	for _, r := range strings.Split(got, "\n") {
		if strings.Contains(r, "Type a message") {
			found = true
		}
	}
	if !found {
		t.Fatal("composer row missing")
	}
}

func TestClipOpensPicker(t *testing.T) {
	const W, H = 120, 40
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	l := sc.layoutFor()
	_, _, y0, y1, clipX := composerGeoms(l, W, H)
	if clipX < 0 || y1-y0 <= 0 {
		t.Fatalf("clip must hit-test inside the composer box, got x=%d y=[%d,%d)", clipX, y0, y1)
	}
	// Click beside the clip (same row, one cell left) must not open it.
	sc.handleMouse(mouseAt(clipX-1, y0))
	if sc.picker.isActive() {
		t.Fatal("near-miss must not open the upload browser")
	}
	sc.handleMouse(mouseAt(clipX, y0))
	if !sc.picker.isActive() {
		t.Fatal("clip click must open the upload browser")
	}
}

// Resize sweep through the live Update path: healthy terminals paint
// EXACTLY their size (rows == H, widths <= W); degraded ones never exceed.
func TestResizeSweepExactFrame(t *testing.T) {
	healthy := [][2]int{{100, 30}, {120, 40}, {160, 45}, {200, 50}, {80, 24}}
	for _, wh := range healthy {
		w, h := wh[0], wh[1]
		c := newFilterScreen("bob", "", "bob", "alice", "carol")
		c.vp = *viewportPtr(80, 20)
		c.addMessage(chatMessage{Seq: 1, Username: "alice", Kind: "chat",
			Text: "resize me", ConvID: generalConv, CreatedAt: "2026-09-15T19:21:00Z"})
		m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
		got := m.(chatScreen).View()
		if rows := strings.Count(got, "\n") + 1; rows != h {
			t.Errorf("w=%d h=%d: painted %d rows; want exactly %d", w, h, rows, h)
		}
		for _, ln := range strings.Split(got, "\n") {
			if lw := lipgloss.Width(ln); lw > w {
				t.Fatalf("w=%d h=%d: row width %d exceeds terminal", w, h, lw)
			}
		}
		// Grow then shrink back: scroll position and content survive.
		m2, _ := m.(chatScreen).Update(tea.WindowSizeMsg{Width: w + 40, Height: h + 10})
		m3, _ := m2.(chatScreen).Update(tea.WindowSizeMsg{Width: w, Height: h})
		got3 := m3.(chatScreen).View()
		if rows := strings.Count(got3, "\n") + 1; rows != h {
			t.Errorf("w=%d h=%d after resize cycle: painted %d rows", w, h, rows)
		}
		if !strings.Contains(stripANSI(got3), "resize me") {
			t.Errorf("w=%d h=%d: message lost across resize cycle", w, h)
		}
	}
}

// ---- reference layout: right video panel, tabs, call card, hints ----

func liveCallScreen(t *testing.T, w, h int) chatScreen {
	t.Helper()
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.vp = *viewportPtr(80, 20)
	c.call = &mediaManager{videoOn: true}
	m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m.(chatScreen)
}

func TestVideoPanelShown(t *testing.T) {
	const W, H = 154, 44
	sc := liveCallScreen(t, W, H)
	l := sc.layoutFor()
	if l.vidPanelW <= 0 {
		t.Fatal("wide live call must allocate the right video panel")
	}
	sc.selfLines = []string{"VIDEO_FRAME_SENTINEL"}
	sc.videoLines = []string{"VIDEO_FRAME_SENTINEL"}
	got := sc.View()
	for _, want := range []string{"Video Call", "[M]", "[V]", "[S]", "[P]", "[X]", "intentionally blurred", "⛶"} {
		if !strings.Contains(stripANSI(got), want) {
			t.Errorf("video element %q missing", want)
		}
	}
	for _, want := range []string{" Chat ", " Files ", "⋮"} {
		if !strings.Contains(stripANSI(got), want) {
			t.Errorf("room tab %q missing", want)
		}
	}
	for _, want := range []string{"Video call started", "View Video",
		"End-to-End Encrypted", "Ctrl+k", "Ctrl+l", "Enter send"} {
		if !strings.Contains(stripANSI(got), want) {
			t.Errorf("reference element %q missing from view", want)
		}
	}
	if rows := strings.Count(got, "\n") + 1; rows != H {
		t.Fatalf("panel frame painted %d rows; want exactly %d", rows, H)
	}
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > W {
			t.Fatalf("panel row width %d exceeds terminal %d", w, W)
		}
	}
}

func TestSidebarRunsFullHeight(t *testing.T) {
	const W, H = 154, 44
	sc := liveCallScreen(t, W, H)
	rows := strings.Split(stripANSI(sc.View()), "\n")
	if len(rows) != H {
		t.Fatalf("painted %d rows; want exactly %d", len(rows), H)
	}
	// Composer input rides beside the sidebar (frame + sidebar borders),
	// not under a blank indent.
	comp := -1
	for i, r := range rows {
		if strings.Contains(r, "Type a message") {
			comp = i
		}
	}
	if comp < 0 {
		t.Fatal("composer row missing")
	}
	if !strings.HasPrefix(rows[comp], "││") {
		t.Fatalf("composer must sit beside the sidebar, row %d starts %q",
			comp, string([]rune(rows[comp])[:4]))
	}
	// Sidebar bottom border closes on the composer's bottom row instead of
	// floating above it: sidebar ╯ then transcript ╰ on the last body row.
	lastBody := rows[H-2]
	if !strings.HasPrefix(lastBody, "│╰") || !strings.Contains(lastBody, "╯ ╰") {
		t.Fatalf("sidebar must close on the composer bottom row, got %q",
			string([]rune(lastBody)[:16]))
	}
}

func TestVisiblePanelHasHitTargets(t *testing.T) {
	sc := liveCallScreen(t, 154, 44)
	l := sc.layoutFor()
	pg := videoPanelGeom(&sc, l, sc.camFeeds())
	if !pg.on || len(pg.btns) == 0 {
		t.Fatal("visible panel must expose mouse targets")
	}
	if cols, rows := sc.videoTileGeom(l, sc.camFeeds()); cols <= 0 || rows <= 0 {
		t.Fatalf("visible video tile geometry = %dx%d", cols, rows)
	}
}

func TestCallButtonsAct(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	c.call = &mediaManager{videoOn: true, audioOn: true}
	// M/V toggle live publishing off (same path as /audio + /video).
	c.pressCallButton("M")
	if c.call.AudioOn() {
		t.Fatal("M must switch the mic off")
	}
	c.pressCallButton("V")
	if c.call.VideoOn() {
		t.Fatal("V must switch the camera off")
	}
	// X hangs up whatever is still publishing.
	c.call.audioOn, c.call.videoOn = true, true
	c.pressCallButton("X")
	if c.call.AudioOn() || c.call.VideoOn() {
		t.Fatal("X must stop all publishing")
	}
	// S/P name unsupported features honestly instead of acting.
	c.pressCallButton("S")
	if !strings.Contains(c.status, "not supported") {
		t.Fatalf("S must explain itself, got %q", c.status)
	}
	c.pressCallButton("P")
	if !strings.Contains(c.status, "people panel") {
		t.Fatalf("P must explain itself, got %q", c.status)
	}
}

func TestRoomTabsPresent(t *testing.T) {
	const W, H = 120, 40
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	got := stripANSI(sc.View())
	for _, want := range []string{" Chat ", " Files ", "⋮"} {
		if !strings.Contains(got, want) {
			t.Fatalf("room tab %q missing", want)
		}
	}
	// Files drawer stays reachable through /download (backend untouched).
	sc.openFilesDrawer()
	if !sc.picker.isActive() || sc.picker.mode != modeFiles {
		t.Fatal("/download must still open the files drawer")
	}
}

func TestCallCardShowsLive(t *testing.T) {
	sc := liveCallScreen(t, 154, 44)
	if l := sc.layoutFor(); l.callRows != callCardRows {
		t.Fatalf("live call must budget %d card rows, got %d", callCardRows, l.callRows)
	}
	got := stripANSI(sc.View())
	for _, want := range []string{"Video call started", "View Video", "participant"} {
		if !strings.Contains(got, want) {
			t.Errorf("call card element %q missing", want)
		}
	}
	// Idle sessions budget no card and paint none.
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(80, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 154, Height: 44})
	idle := m.(chatScreen)
	if l := idle.layoutFor(); l.callRows != 0 {
		t.Fatalf("idle session must budget no card rows, got %d", l.callRows)
	}
	if strings.Contains(stripANSI(idle.View()), "Video call started") {
		t.Fatal("idle session must not paint a call card")
	}
}

func TestTopBarEncrypted(t *testing.T) {
	if got := topBarView(160); !strings.Contains(got, "End-to-End Encrypted") {
		t.Fatalf("wide top bar must name encryption: %q", got)
	}
}

func TestSidebarSelectedBorderAndCallIcon(t *testing.T) {
	sc := liveCallScreen(t, 120, 40)
	out := sc.rosterBody(40)
	plain := stripANSI(out)
	if !strings.Contains(plain, "┃") {
		t.Fatalf("selected chat must carry a side bar:\n%s", plain)
	}
	if !strings.Contains(plain, "◉") {
		t.Fatalf("in-call chat must show the live icon:\n%s", plain)
	}
	if !strings.Contains(plain, "in call") {
		t.Fatalf("in-call preview must name the call:\n%s", plain)
	}
}

func TestKeyHintsNeverClipMidWord(t *testing.T) {
	for _, w := range []int{20, 40, 57, 58, 80, 120} {
		got := stripANSI(keyHintsView(w))
		if lw := lipgloss.Width(got); lw > w {
			t.Fatalf("hints width %d exceeds %d: %q", lw, w, got)
		}
		if w >= 58 && !strings.Contains(got, "Ctrl+k commands") {
			t.Fatalf("wide hints must spell out bindings: %q", got)
		}
	}
}

func TestCtrlKOpensCommands(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	c.width, c.height = 120, 40
	m, _ := c.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if got := m.(chatScreen).input.Value(); got != "/" {
		t.Fatalf("Ctrl+K must focus /, got %q", got)
	}
}

func TestCtrlLClearsInput(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	c.width, c.height = 120, 40
	c.input.SetValue("half-typed")
	m, _ := c.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	got := m.(chatScreen)
	if got.input.Value() != "" {
		t.Fatalf("Ctrl+L must clear the composer, got %q", got.input.Value())
	}
}

func TestSystemCardGreenBar(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m := chatMessage{Seq: 9, Username: "", Kind: "system", Text: "alice joined",
		ConvID: generalConv, CreatedAt: "2026-09-15T19:21:00Z"}
	out := stripANSI(c.renderLine(m))
	if !strings.Contains(out, "System") || !strings.Contains(out, "▌") {
		t.Fatalf("system line must carry green sender + bar:\n%s", out)
	}
	if !strings.Contains(out, "alice joined") {
		t.Fatalf("system text lost:\n%s", out)
	}
}

func TestVisibleFullscreenTargetExplains(t *testing.T) {
	sc := liveCallScreen(t, 154, 44)
	l := sc.layoutFor()
	pg := videoPanelGeom(&sc, l, sc.camFeeds())
	if !pg.on {
		t.Fatal("panel must be on for the fullscreen target")
	}
	sc.handleMouse(mouseAt(pg.fsX0, pg.fsY))
	if !strings.Contains(sc.status, "fullscreen") {
		t.Fatalf("fullscreen target must explain itself, got %q", sc.status)
	}
}

func TestCallTimerFormat(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.callStart = time.Now().Add(-(1*time.Hour + 2*time.Minute + 37*time.Second))
	if got := c.callElapsed(); got != "01:02:37" {
		t.Fatalf("elapsed = %q; want 01:02:37", got)
	}
	c.callStart = time.Time{}
	if got := c.callElapsed(); got != "--:--:--" {
		t.Fatalf("idle elapsed = %q; want placeholder", got)
	}
}

// Live frames must not break the exact-row / max-width frame contract.
func TestCamStripExactRowsWithLiveFrames(t *testing.T) {
	const W, H = 100, 30
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.vp = *viewportPtr(60, 20)
	frame := "\x1b[38;2;10;20;30m\x1b[48;2;1;2;3m" + strings.Repeat("⣿", 60) + "\x1b[0m"
	lines := make([]string, 0, camPicRows+4)
	for i := 0; i < camPicRows+4; i++ {
		lines = append(lines, frame)
	}
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	sc.selfLines = lines
	sc.videoLines = lines
	got := sc.View()
	if rows := strings.Count(got, "\n") + 1; rows != H {
		t.Fatalf("painted %d rows with live frames; want exactly %d", rows, H)
	}
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > W {
			t.Fatalf("row width %d exceeds terminal %d: %q", w, W, ln)
		}
	}
}
