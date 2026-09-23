package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// geomScreen builds a screen with an overflowing chat list at 140x40.
func geomScreen(t *testing.T) (*chatScreen, layout) {
	t.Helper()
	names := []string{"carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy", "karl", "lena", "mallory", "nina", "olga", "peggy", "sybil", "trent", "uma"}
	users := append([]string{"bob"}, names...)
	c := newFilterScreen("bob", "", users...)
	c.vp = *viewportPtr(60, 20)
	c.call = &mediaManager{audioOn: true}
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	return &sc, sc.layoutFor()
}

func colAt(s string, col int) string {
	r := []rune(s)
	if col < len(r) {
		return string(r[col])
	}
	return ""
}

// TestScrollbarDragGeometry pins the scrollbar geometry to the painted
// view: every scrollbar's x column and track rows must match the glyphs
// View() actually paints (drag hit-tests read the same truth).
func TestScrollbarDragGeometry(t *testing.T) {
	c, l := geomScreen(t)
	view := c.View()
	rows := strings.Split(view, "\n")
	chatG, rosterG := c.scrollBarGeoms(l)

	// Chat bar: inside the transcript border, last interior column.
	// Interior top = frame + top bar + room header + call card + border;
	// the first interior row holds the up-arrow.
	frameOff, headOff := 0, 0
	if l.frameOn {
		frameOff = 1
	}
	if l.showHeader {
		headOff = 1
	}
	arrowRow := frameOff + headOff + l.headRows + 1
	if got := colAt(rows[arrowRow], chatG.x); got != "│" && got != "▲" && got != "█" {
		t.Fatalf("chat bar column mismatch: got %q at x=%d,y=%d", got, chatG.x, arrowRow)
	}
	// Track rows: track starts below ▲.
	if chatG.trackY0 != arrowRow+1 || chatG.trackH != l.vpHeight-2 {
		t.Fatalf("chat track y0=%d h=%d; want %d/%d", chatG.trackY0, chatG.trackH, arrowRow+1, l.vpHeight-2)
	}
	// Roster bar: x = rightmost sidebar interior column, track below the
	// first scroll row's ▲ (search row sits fixed above it).
	if rosterG.x != l.rosterX+l.sidebarWidth-2 {
		t.Fatalf("roster bar x=%d; want %d", rosterG.x, l.rosterX+l.sidebarWidth-2)
	}
	if got := colAt(rows[rosterG.trackY0-1], rosterG.x); got != "▲" && got != "│" && got != "█" {
		t.Fatalf("roster bar must sit below its up-arrow; got %q at y=%d", got, rosterG.trackY0-1)
	}
}

// wheel builds a wheel event at terminal coordinates.
func wheel(up bool, x, y int) tea.MouseMsg {
	t := tea.MouseWheelDown
	if up {
		t = tea.MouseWheelUp
	}
	return tea.MouseMsg{Type: t, X: x, Y: y}
}

// TestIndependentScrollPanes: wheel/drag on one pane must never move the
// other.
func TestIndependentScrollPanes(t *testing.T) {
	c, l := geomScreen(t)
	// Give chat scrollable content.
	c.lines = make([]string, 60)
	c.rebuildView()

	chatBefore := c.vp.YOffset
	rosterBefore := c.rosterVp.YOffset

	// Wheel over the chat list scrolls ONLY the roster.
	c.handleMouse(wheel(false, l.rosterX+5, l.rosterY0+2))
	if c.rosterVp.YOffset == rosterBefore {
		t.Fatal("wheel over chat list must scroll the roster")
	}
	if c.vp.YOffset != chatBefore {
		t.Fatalf("roster wheel moved chat: %d→%d", chatBefore, c.vp.YOffset)
	}

	// Drag the chat scrollbar thumb: chat moves, sidebar stays.
	chatG := c.thumbFor(secChat, func() barGeom {
		g, _ := c.scrollBarGeoms(l)
		return g
	}())
	down := 0
	if chatG.thumbTop < chatG.trackH-chatG.thumbH {
		c.handleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: chatG.x, Y: chatG.trackY0 + chatG.thumbH})
		c.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: chatG.x, Y: chatG.trackY0 + chatG.trackH - 1})
		if c.vp.YOffset == chatBefore {
			t.Fatal("dragging the chat thumb must scroll chat")
		}
		if c.rosterVp.YOffset != rosterBefore+3 {
			t.Fatalf("chat drag moved roster: %d → %d", rosterBefore, c.rosterVp.YOffset)
		}
		c.handleMouse(tea.MouseMsg{Action: tea.MouseActionRelease, X: chatG.x, Y: chatG.trackY0})
		if c.drag.active {
			t.Fatal("release must end the drag")
		}
	}
	_ = down
}

// TestRosterScrollKeepsSelection: with the list scrolled, clicks map to
// the visible row (offset-aware hit-test, two rows per chat item).
func TestRosterScrollKeepsSelection(t *testing.T) {
	names := []string{"carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy", "karl", "lena", "mallory", "nina", "olga", "peggy", "sybil", "trent", "uma", "victor", "walter", "xavier", "yusuf", "zoe", "a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8", "i9", "j10", "k11", "l12", "m13"}
	users := append([]string{"bob"}, names...)
	c := newFilterScreen("bob", "", users...)
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	l := sc.layoutFor()
	// Chat list taller than the box: sync content, wheel twice, click the
	// FIRST visible item row (below the fixed search row).
	sc.syncRosterVp()
	sc.handleMouse(wheel(false, l.rosterX+5, l.rosterY0+2))
	sc.handleMouse(wheel(false, l.rosterX+5, l.rosterY0+2))
	sc.handleMouse(mouseAt(l.rosterX+5, l.rosterY0)) // top visible item row
	if sc.targetUser == "" {
		t.Fatal("click on a scrolled chat-list row must select someone")
	}
	// The selected user must be the item visible at that row
	// (offset-applied, two rows per item).
	items := sc.chatItems()
	want := items[sc.rosterVp.YOffset/sc.chatItemHeight()].peer
	if want == "" {
		t.Fatal("scrolled top row landed on the room; test needs deeper scroll")
	}
	if sc.targetUser != want {
		t.Fatalf("clicked row selected %q; want %q (offset %d)", sc.targetUser, want, sc.rosterVp.YOffset)
	}
}
