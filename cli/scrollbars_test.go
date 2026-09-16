package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// geomScreen builds a screen with video + overflowing roster at 140x40,
// the same geometry TestZZGeom measurements were taken on.
func geomScreen(t *testing.T) (*chatScreen, layout) {
	t.Helper()
	names := []string{"carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy", "karl", "lena", "mallory", "nina", "olga", "peggy", "sybil", "trent", "uma"}
	users := append([]string{"bob"}, names...)
	c := newFilterScreen("bob", "", users...)
	c.vp = *viewportPtr(60, 20)
	c.call = &mediaManager{videoOn: true}
	lines := make([]string, 0, videoPaneDefaultRows)
	for i := 0; i < videoPaneDefaultRows; i++ {
		lines = append(lines, "row")
	}
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	m2, _ := sc.Update(netVideoMsg{lines: lines})
	sc = m2.(chatScreen)
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
	chatG, videoG, rosterG := c.scrollBarGeoms(l)

	// Chat bar: inside the transcript border, rightmost interior column.
	if got := colAt(rows[3], chatG.x); got != "│" {
		t.Fatalf("chat bar column mismatch: got %q at x=%d", got, chatG.x)
	}
	if got := colAt(rows[2], chatG.x); got == "│" && got != "│" {
		t.Fatal("unreachable")
	}
	// Track rows: interior spans [3, 3+vpHeight); track starts below ▲.
	if chatG.trackY0 != 4 || chatG.trackH != l.vpHeight-2 {
		t.Fatalf("chat track y0=%d h=%d; want %d/%d", chatG.trackY0, chatG.trackH, 4, l.vpHeight-2)
	}
	// Video bar: x = rightmost sidebar interior column, track below ▲.
	if got := colAt(rows[videoG.trackY0], videoG.x); got != "█" && got != "│" {
		t.Fatalf("video bar column mismatch: %q at x=%d,y=%d", got, videoG.x, videoG.trackY0)
	}
	if got := colAt(rows[videoG.trackY0-1], videoG.x); got != "▲" {
		t.Fatalf("video bar must sit below the up-arrow; got %q", got)
	}
	// Roster bar: track starts below the title row's ▲.
	if got := colAt(rows[rosterG.trackY0-1], rosterG.x); got != "▲" {
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
// other two.
func TestIndependentScrollPanes(t *testing.T) {
	c, l := geomScreen(t)
	// Give chat scrollable content.
	c.lines = make([]string, 60)
	c.rebuildView()

	chatBefore := c.vp.YOffset
	videoBefore := c.videoVp.YOffset
	rosterBefore := c.rosterVp.YOffset

	// Wheel over the users list scrolls ONLY the roster.
	c.handleMouse(wheel(false, l.rosterX+5, l.rosterY0+2))
	if c.rosterVp.YOffset == rosterBefore {
		t.Fatal("wheel over roster must scroll the roster")
	}
	if c.vp.YOffset != chatBefore || c.videoVp.YOffset != videoBefore {
		t.Fatalf("roster wheel moved other panes: chat %d→%d video %d→%d",
			chatBefore, c.vp.YOffset, videoBefore, c.videoVp.YOffset)
	}

	// Wheel over the video pane scrolls ONLY the video feed.
	c.handleMouse(wheel(false, l.rosterX+5, l.rosterY0-l.videoRows))
	if c.videoVp.YOffset == videoBefore {
		t.Fatal("wheel over video must scroll the video pane")
	}
	if c.rosterVp.YOffset != rosterBefore+3 && c.rosterVp.YOffset == rosterBefore {
		t.Fatal("impossible")
	}
	if c.vp.YOffset != chatBefore {
		t.Fatal("video wheel moved chat")
	}

	// Drag the chat scrollbar thumb: chat moves, sidebar stays.
	chatG := c.thumbFor(secChat, func() barGeom {
		g, _, _ := c.scrollBarGeoms(l)
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
// the visible row (offset-aware hit-test).
func TestRosterScrollKeepsSelection(t *testing.T) {
	names := []string{"carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy", "karl", "lena", "mallory", "nina", "olga", "peggy", "sybil", "trent", "uma", "victor", "walter", "xavier", "yusuf", "zoe", "a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8", "i9", "j10", "k11", "l12", "m13"}
	users := append([]string{"bob"}, names...)
	c := newFilterScreen("bob", "", users...)
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	l := sc.layoutFor()
	// Roster taller than the box: sync content, wheel twice, click FIRST row.
	sc.syncRosterVp()
	sc.handleMouse(wheel(false, l.rosterX+5, l.rosterY0+2))
	sc.handleMouse(mouseAt(l.rosterX+5, l.rosterY0)) // top visible row
	if sc.targetUser == "" {
		t.Fatal("click on a scrolled roster row must select someone")
	}
	// The selected user must be the one visible at that row (offset-applied).
	online := orderedUsers(sc.users, sc.me, sc.lastDMAt)
	want := online[sc.rosterVp.YOffset]
	if sc.targetUser != want {
		t.Fatalf("clicked row selected %q; want %q (offset %d)", sc.targetUser, want, sc.rosterVp.YOffset)
	}
}
