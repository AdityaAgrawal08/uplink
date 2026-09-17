package main

import (
	"strings"
	"testing"

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
// width their tile paints.
func TestCamStripPushesRenderSize(t *testing.T) {
	const W, H = 140, 40
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.vp = *viewportPtr(60, 20)
	c.call = &mediaManager{}
	m, _ := c.Update(tea.WindowSizeMsg{Width: W, Height: H})
	sc := m.(chatScreen)
	l := sc.layoutFor()
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

func TestCompactTranscriptDropsAvatar(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m := chatMessage{Seq: 1, Username: "alice", Kind: "chat", Text: "hi",
		ConvID: generalConv, CreatedAt: "2026-09-15T19:21:00Z"}
	c.width = 140
	wide := c.renderLine(m)
	if !strings.Contains(wide, " A ") {
		t.Fatalf("wide transcript must show the avatar chip:\n%s", stripANSI(wide))
	}
	c.width = 70
	c.renderCache = map[int]string{} // density flip invalidates below; render direct
	narrow := c.renderLine(m)
	if strings.Contains(narrow, " A ") {
		t.Fatalf("cramped transcript must drop the avatar chip:\n%s", stripANSI(narrow))
	}
	if !strings.Contains(stripANSI(narrow), "alice") {
		t.Fatalf("compact sender must keep the name:\n%s", stripANSI(narrow))
	}
}

func TestRenderCacheInvalidatesOnDensityFlip(t *testing.T) {
	c := newFilterScreen("bob", "", "bob", "alice")
	c.width = 70 // compact avatars off
	c.cacheForWidth(50)
	c.renderCache[7] = "stale"
	c.width = 140 // same viewport width, density flipped
	c.cacheForWidth(50)
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

// Live frames must not break the exact-row / max-width frame contract.
func TestCamStripExactRowsWithLiveFrames(t *testing.T) {
	const W, H = 140, 40
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
