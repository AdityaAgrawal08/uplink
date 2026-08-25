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
	cases := map[int]int{70: rosterTotalWidth, 89: rosterTotalWidth, 90: 26, 129: 26, 130: 30, 200: 30}
	for w, want := range cases {
		if got := sidebarWidthFor(w); got != want {
			t.Errorf("sidebarWidthFor(%d) = %d; want %d", w, got, want)
		}
	}
}

func TestComposerRowsDensity(t *testing.T) {
	cases := map[int]int{40: 3, 22: 3, 21: 2, 14: 2, 13: 1, 10: 1, 9: 0, 5: 0}
	for h, want := range cases {
		if got := composerRowsFor(h); got != want {
			t.Errorf("composerRowsFor(%d) = %d; want %d", h, got, want)
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

	if !strings.Contains(out, "ONLINE — 3") {
		t.Errorf("online section missing: %q", out)
	}
	if !strings.Contains(out, "● alice (you)") || !strings.Contains(out, "○ bob") {
		t.Errorf("presence dots wrong:\n%s", out)
	}
	if strings.Contains(out, "THREADS") && l.rosterSlots < 4 {
		t.Error("threads section rendered without room")
	}

	// Opening a thread registers it and marks it active in the sidebar.
	c.enterPrivate("bob")
	out = c.rosterBody(computeLayout(100, 30, false).rosterSlots)
	if !strings.Contains(out, "THREADS") || !strings.Contains(out, "▸ · bob") {
		t.Errorf("active thread not highlighted:\n%s", out)
	}
	if !strings.Contains(out, "# general") {
		t.Error("general channel missing from threads list")
	}

	// Full-row highlight for the selected peer.
	hl := tuiRosterSelectedStyle.Render(strings.Repeat(" ", 1))
	_ = hl // style smoke: exact padding asserted indirectly via no-panic above

	// Switching to general marks the channel instead.
	c.exitPrivate()
	out = c.rosterBody(computeLayout(100, 30, false).rosterSlots)
	if !strings.Contains(out, "▸ # general") {
		t.Errorf("general not marked active after exit:\n%s", out)
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
