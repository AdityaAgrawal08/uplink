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
	if sc.call.renderCols != wantInner || sc.call.renderRows != camPicRows {
		t.Fatalf("render size = %dx%d; want tile %dx%d",
			sc.call.renderCols, sc.call.renderRows, wantInner, camPicRows)
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
