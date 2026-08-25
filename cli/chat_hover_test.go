package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ---------------------------------------------------------------------------
// Regression for the reported colouring bug: with many users, colour must
// touch EXACTLY ONE row (the hovered one). Default is plain white for all;
// the selected peer is marked by SHAPE (filled dot), never by background.
// ---------------------------------------------------------------------------

func TestHoverPaintsExactlyOneRow(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	const W, H = 110, 36
	users := []string{"me", "u1", "u2", "u3", "u4", "u5", "u6"}
	c := newFilterScreen("me", "u3", users...) // 4th peer pre-selected
	c.width, c.height = W, H
	c.vp = *viewportPtr(60, 20)

	frag := func(u string) string { return tuiHoverStyle.Render("○ " + u) }

	render := func() string { return c.rosterBody(computeLayout(W, H, false).rosterSlots) }

	// Baseline: no hover → zero pink rows anywhere.
	c.hoverPeer = ""
	base := render()
	for _, u := range users {
		if u == "me" {
			continue
		}
		if strings.Contains(base, frag(u)) {
			t.Fatalf("pink leaked with no hover on %s", u)
		}
	}
	if strings.Contains(stripANSIRaw(base), "48;5;236") || strings.Contains(base, "\x1b[48;5;236m") {
		t.Error("legacy selection background still present")
	}

	// Hover the LAST user: its row is pink; every other peer's is not.
	c.hoverPeer = "u6"
	out := render()
	if !strings.Contains(out, frag("u6")) {
		t.Fatalf("hovered row not pink:\n%s", stripANSIRaw(out))
	}
	for _, u := range users {
		if u == "me" || u == "u6" {
			continue
		}
		if strings.Contains(out, frag(u)) {
			t.Fatalf("pink bled onto non-hovered row %s", u)
		}
	}

	// Selected 4th peer keeps only its SHAPE cue while someone else hovers.
	if !strings.Contains(stripANSIRaw(out), "● u3") {
		t.Error("selected peer row missing")
	}

	// Moving hover moves the single pink row.
	c.hoverPeer = "u1"
	out2 := render()
	if !strings.Contains(out2, frag("u1")) || strings.Contains(out2, frag("u6")) {
		t.Fatalf("hover move did not relocate the single pink row\n%s", stripANSIRaw(out2))
	}
}

func TestMotionUpdatesAndClearsHover(t *testing.T) {
	const W, H = 110, 34
	c := newFilterScreen("me", "", "me", "a", "b")
	m, _ := c.Update(newWindowSize(W, H))
	scr := m.(chatScreen)
	l := computeLayout(W, H, false)

	inside := l.rosterX + 4
	nm, _ := scr.Update(tea.MouseMsg{Type: tea.MouseMotion, X: inside, Y: l.rosterY0 + 1})
	got := nm.(chatScreen)
	if got.hoverPeer != "a" {
		t.Fatalf("hover = %q; want a", got.hoverPeer)
	}

	// Motion outside the column clears it.
	nm2, _ := got.Update(tea.MouseMsg{Type: tea.MouseMotion, X: 2, Y: l.rosterY0 + 1})
	got2 := nm2.(chatScreen)
	if got2.hoverPeer != "" {
		t.Fatalf("hover not cleared outside sidebar: %q", got2.hoverPeer)
	}
}

func stripANSIRaw(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
