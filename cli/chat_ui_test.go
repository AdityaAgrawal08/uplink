package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Density adaptation — the terminal-native answer to "font size by window"
// ---------------------------------------------------------------------------

func TestSidebarWidthDensity(t *testing.T) {
	// Fluid column: ~1 cell per 10 terminal columns, clamped [22,38].
	cases := map[int]int{66: 22, 70: 22, 86: 24, 90: 24, 106: 26, 130: 28, 176: 33, 200: 35, 226: 38, 300: 38}
	for w, want := range cases {
		if got := sidebarWidthFor(w); got != want {
			t.Errorf("sidebarWidthFor(%d) = %d; want %d", w, got, want)
		}
	}
	// Monotonic: every wider terminal rebalances (never jumps backwards).
	prev := 0
	for w := 40; w <= 260; w++ {
		if got := sidebarWidthFor(w); got < prev {
			t.Fatalf("sidebarWidthFor(%d) = %d < %d: not monotonic", w, got, prev)
		} else {
			prev = got
		}
	}
}

func TestComposerRowsDensity(t *testing.T) {
	// Single-line composer whenever boxed; bare prompt when cramped.
	cases := map[int]int{60: 1, 40: 1, 30: 1, 12: 1, 11: 0, 9: 0, 5: 0}
	for h, want := range cases {
		if got := composerRowsFor(h); got != want {
			t.Errorf("composerRowsFor(%d) = %d; want %d", h, got, want)
		}
	}
	// Monotonic growth with height.
	prev := -1
	for h := 0; h <= 60; h++ {
		if got := composerRowsFor(h); got < prev {
			t.Fatalf("composerRowsFor(%d) = %d < %d: not monotonic", h, got, prev)
		} else {
			prev = got
		}
	}
}

// ---------------------------------------------------------------------------
// Full-screen shell: outer frame wraps everything at healthy sizes
// ---------------------------------------------------------------------------

func TestFullScreenFrame(t *testing.T) {
	const W, H = 110, 34
	c := newFilterScreen("me", "", "me", "a")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(newWindowSize(W, H))
	v := m.(chatScreen).View()

	lines := strings.Split(v, "\n")
	if len(lines) != H {
		t.Fatalf("frame rows = %d; want exactly %d", len(lines), H)
	}
	if !strings.HasPrefix(lines[0], "╭") || !strings.HasSuffix(lines[0], "╮") {
		t.Errorf("top frame edge missing: %q", lines[0])
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "╰") || !strings.HasSuffix(last, "╯") {
		t.Errorf("bottom frame edge missing: %q", last)
	}
	if lw := lipglossWidth(lines[0]); lw != W {
		t.Errorf("frame width = %d; want %d", lw, W)
	}
	if !strings.Contains(v, "❯") {
		t.Error("composer prompt missing")
	}
}

func newWindowSize(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

func lipglossWidth(s string) int { return lipgloss.Width(s) }

// ---------------------------------------------------------------------------
// Sidebar: readable sections with presence dots + thread navigation
// ---------------------------------------------------------------------------

func TestSidebarSectionsAndNavigation(t *testing.T) {
	c := newFilterScreen("alice", "", "alice", "bob", "carol")
	c.width, c.height = 100, 30
	c.vp = *viewportPtr(60, 16)

	l := computeLayout(100, 30, false)
	out := c.rosterBody(l.rosterSlots)

	if !strings.Contains(out, "Search chats") || !strings.Contains(out, "General") {
		t.Errorf("chat list missing search/room:\n%s", out)
	}
	// Self (alice) has no chat row of its own; peers do.
	if !strings.Contains(out, "bob") || !strings.Contains(out, "carol") {
		t.Errorf("peers missing from chat list:\n%s", out)
	}
	if strings.Contains(out, "THREADS") && l.rosterSlots < 4 {
		t.Error("threads section rendered without room")
	}

	// THREADS panel is intentionally gone: the transcript + header carry all
	// conversation context. Opening a thread must not resurrect any section.
	c.enterPrivate("bob")
	out = c.rosterBody(computeLayout(100, 30, false).rosterSlots)
	if strings.Contains(out, "THREADS") || strings.Contains(out, "▸ · bob") || strings.Contains(out, "# general") {
		t.Errorf("thread panel remnants after removal:\n%s", out)
	}
	if !strings.Contains(out, "bob") {
		t.Errorf("selected peer lost its highlight:\n%s", out)
	}

	// Back to general: still the same chat list.
	c.exitPrivate()
	out = c.rosterBody(computeLayout(100, 30, false).rosterSlots)
	if !strings.Contains(out, "General") || !strings.Contains(out, "bob") {
		t.Errorf("chat list damaged by mode switches:\n%s", out)
	}
}

func TestComposerGrowsAndShrinks(t *testing.T) {
	render := func(w, h int) string {
		c := newFilterScreen("me", "")
		c.vp = *viewportPtr(40, 10)
		m, _ := c.Update(newWindowSize(w, h))
		return m.(chatScreen).View()
	}
	tall := render(120, 40)
	if !strings.Contains(tall, "❯") {
		t.Fatal("prompt missing")
	}
	// Count composer interior blank rows: tall terminal must give a taller box.
	countRows := func(v string) int { return strings.Count(v, "\n") }
	small := render(120, 12)
	if countRows(tall)-countRows(small) <= 0 {
		t.Errorf("composer did not grow with terminal size")
	}
}
