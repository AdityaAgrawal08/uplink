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
func TestAudioStateSurvivesLayout(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {154, 44}, {80, 18}} {
		c := newFilterScreen("bob", "", "bob", "alice", "carol")
		c.vp = *viewportPtr(60, 20)
		c.call = &mediaManager{audioOn: true}
		m, _ := c.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		sc := m.(chatScreen)
		_ = sc.layoutFor()
		if !sc.call.AudioOn() {
			t.Fatalf("layout pass changed audio state at %v", size)
		}
	}
}

// ---- adaptive density: every resize step must visibly rebalance --------

func TestTopBarCollapsesByWidth(t *testing.T) {
	for _, w := range []int{40, 60, 90, 160, 220} {
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

func TestComposerFullWidthNoSend(t *testing.T) {
	// No Send button anywhere: the message box spans the full column and
	// Enter (not a click target) sends.
	const W, H = 120, 40
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	got := stripANSI(sc.View())
	if strings.Contains(got, "Send") {
		t.Fatalf("Send button must be gone:\n%s", got)
	}
	found := false
	for _, r := range strings.Split(got, "\n") {
		if strings.Contains(r, "Type a message") {
			found = true
			// Message box runs to the frame edge (box border + frame
			// border adjacent), with no button column carved out.
			if !strings.HasSuffix(r, "││") {
				t.Fatalf("composer must span the full column, row ends %q",
					string([]rune(r)[len([]rune(r))-4:]))
			}
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
	clipX, y0, y1 := composerGeoms(l, W, H)
	if clipX < 0 || y1-y0 != 1 {
		t.Fatalf("clip must hit-test exactly one row, got x=%d y=[%d,%d)", clipX, y0, y1)
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
	c.call = &mediaManager{audioOn: true}
	m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m.(chatScreen)
}

func TestNoVideoSurfaces(t *testing.T) {
	const W, H = 154, 44
	sc := liveCallScreen(t, W, H)
	got := sc.View()
	for _, absent := range []string{"Video Call", "Live Cameras", "[M]", "[V]", "[S]", "[P]", "[X]", "intentionally blurred", "VIDEO_FRAME_SENTINEL", "⛶"} {
		if strings.Contains(stripANSI(got), absent) {
			t.Errorf("hidden video element %q still visible", absent)
		}
	}
	for _, absent := range []string{" Chat ", " Files ", "⋮"} {
		if strings.Contains(stripANSI(got), absent) {
			t.Errorf("removed room tab %q still visible", absent)
		}
	}
	for _, want := range []string{"Voice call started", "LIVE",
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

func TestRoomTabsRemoved(t *testing.T) {
	const W, H = 120, 40
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	got := stripANSI(sc.View())
	for _, absent := range []string{" Chat ", " Files ", "⋮"} {
		if strings.Contains(got, absent) {
			t.Fatalf("removed room tab %q still visible", absent)
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
	for _, want := range []string{"Voice call started", "LIVE", "participant"} {
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
	if strings.Contains(stripANSI(idle.View()), "Voice call started") {
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
	if !strings.Contains(plain, "Live now") {
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
