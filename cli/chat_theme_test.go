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

// The strip owns the render-size contract (like the old sidebar pane):
// syncViewport must push the true tile geometry so frames render at the
// width their tile paints. Narrow terminal → bottom strip mode.
func TestCamStripPushesRenderSize(t *testing.T) {
	const W, H = 100, 30
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.vp = *viewportPtr(60, 20)
	c.call = &mediaManager{}
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	l := sc.layoutFor()
	if l.vidPanelW != 0 {
		t.Fatal("narrow terminal must stay in strip mode")
	}
	if l.camRows == 0 {
		t.Fatal("strip must be on at this size")
	}
	wantInner := camTileInner(camStripContentW(W, l.frameOn), len(sc.camFeeds()))
	if minInner := tileMinFor(camStripContentW(W, l.frameOn)); wantInner < minInner {
		wantInner = minInner
	}
	wantRows := max(3, l.camRows-5)
	if sc.call.renderCols != wantInner || sc.call.renderRows != wantRows {
		t.Fatalf("render size = %dx%d; want tile %dx%d",
			sc.call.renderCols, sc.call.renderRows, wantInner, wantRows)
	}
}

// ---- adaptive density: every resize step must visibly rebalance --------

func TestTopBarCollapsesByWidth(t *testing.T) {
	full := topBarView(160)
	if !strings.Contains(full, "ephemeral") || !strings.Contains(full, "202") {
		t.Fatalf("wide top bar must show promises + date: %q", full)
	}
	mid := topBarView(90)
	if !strings.Contains(mid, "ephemeral") || strings.Contains(mid, "202") {
		t.Fatalf("mid top bar must keep promises, drop date: %q", mid)
	}
	narrow := topBarView(60)
	if strings.Contains(narrow, "ephemeral") {
		t.Fatalf("narrow top bar must drop promises: %q", narrow)
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
	if got := c.chatItemHeight(); got != 3 {
		t.Fatalf("roomy terminal must use 3-row items, got %d", got)
	}
	c.height = 20
	if got := c.chatItemHeight(); got != 1 {
		t.Fatalf("short terminal must collapse to 1-row items, got %d", got)
	}
	// Row budget collapses: three times the chats visible without scrolling.
	c.width, c.height = 120, 40
	full := c.chatItemRows(c.chatItems(), 24, "")
	c.height = 20
	compact := c.chatItemRows(c.chatItems(), 24, "")
	if len(compact)*3 != len(full) {
		t.Fatalf("compact rows %d must be a third of comfortable %d", len(compact), len(full))
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

func TestSendButtonShrinksOnNarrow(t *testing.T) {
	if got := sendBtnWidthFor(100); got != sendBtnWidth {
		t.Errorf("wide composer send width = %d; want %d", got, sendBtnWidth)
	}
	if got := sendBtnWidthFor(40); got != 6 {
		t.Errorf("narrow composer send width = %d; want 6", got)
	}
	b := sendButtonView(3, 6)
	if !strings.Contains(b, "➤") || strings.Contains(b, "Send") {
		t.Fatalf("icon button must show ➤, not Send:\n%s", stripANSI(b))
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

func TestRightPanelPlacement(t *testing.T) {
	const W, H = 154, 44
	sc := liveCallScreen(t, W, H)
	l := sc.layoutFor()
	if l.vidPanelW <= 0 {
		t.Fatal("wide terminal must open the right video panel")
	}
	if l.camRows != 0 {
		t.Fatal("bottom strip must yield while the panel is open")
	}
	got := sc.View()
	for _, want := range []string{"Video Call", "[M]", "[V]", "[S]", "[P]", "[X]",
		"intentionally blurred", "Chat", "Files", "⋮", "Video call started", "LIVE",
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

func TestPanelTileHandshake(t *testing.T) {
	const W, H = 154, 44
	sc := liveCallScreen(t, W, H)
	l := sc.layoutFor()
	pg := videoPanelGeom(&sc, l, sc.camFeeds())
	if sc.call.renderCols != pg.tileInner || sc.call.renderRows != pg.picH {
		t.Fatalf("render size = %dx%d; want panel tile %dx%d",
			sc.call.renderCols, sc.call.renderRows, pg.tileInner, pg.picH)
	}
	// Buttons live inside the painted panel, five across, X last.
	if len(pg.btns) != 5 {
		t.Fatalf("control bank must have 5 buttons, got %d", len(pg.btns))
	}
	for i, id := range []string{"M", "V", "S", "P", "X"} {
		if pg.btns[i].id != id {
			t.Fatalf("button %d = %q; want %q", i, pg.btns[i].id, id)
		}
		b := pg.btns[i]
		if b.x0 < pg.x0+1 || b.x1 > pg.x0+pg.w-1 || b.y0 < pg.topY+1 {
			t.Fatalf("button %q rect %+v escapes the panel", id, b)
		}
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

func TestRoomTabsAct(t *testing.T) {
	const W, H = 120, 40
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	l := sc.layoutFor()
	tc, tf, tm, ok := roomTabsGeoms(&sc, l)
	if !ok {
		t.Fatal("roomy terminal must hit-test room tabs")
	}
	// Files opens the received-files drawer (same as /download).
	sc.handleMouse(mouseAt(tf.x0, tf.y))
	if !sc.picker.isActive() || sc.picker.mode != modeFiles {
		t.Fatal("Files tab must open the files drawer")
	}
	// Chat returns to the transcript.
	sc.handleMouse(mouseAt(tc.x0, tc.y))
	if sc.picker.isActive() {
		t.Fatal("Chat tab must close the drawer")
	}
	// ⋮ focuses the command drawer.
	sc.handleMouse(mouseAt(tm.x0, tm.y))
	if sc.input.Value() != "/" {
		t.Fatalf("⋮ must open /, got %q", sc.input.Value())
	}
}

func TestCallCardShowsLive(t *testing.T) {
	sc := liveCallScreen(t, 154, 44)
	if l := sc.layoutFor(); l.callRows != callCardRows {
		t.Fatalf("live call must budget %d card rows, got %d", callCardRows, l.callRows)
	}
	got := stripANSI(sc.View())
	for _, want := range []string{"Video call started", "LIVE", "participant"} {
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
	if !strings.Contains(plain, "╭") || !strings.Contains(plain, "╯") {
		t.Fatalf("selected chat must carry a border:\n%s", plain)
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

func TestFullscreenHintHonest(t *testing.T) {
	sc := liveCallScreen(t, 154, 44)
	l := sc.layoutFor()
	pg := videoPanelGeom(&sc, l, sc.camFeeds())
	sc.handleMouse(mouseAt(pg.fsX0, pg.fsY))
	if !strings.Contains(sc.status, "terminal") {
		t.Fatalf("⛶ must explain panel sizing, got %q", sc.status)
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
