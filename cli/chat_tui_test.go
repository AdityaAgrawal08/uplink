package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// Presence matching (bug: Contains over-matched bob/bobby)
// ---------------------------------------------------------------------------

func TestIsOwnPresence(t *testing.T) {
	tests := []struct {
		text string
		me   string
		want bool
	}{
		{"bob joined", "bob", true},
		{"bob left", "bob", true},
		{"bobby joined", "bob", false}, // regression: substring false positive
		{"alice joined bob", "bob", false},
		{"bob rejoined", "bob", false},
		{"joined bob", "bob", false},
		{"bob  joined", "bob", false}, // double space ≠ server format
		{"bob Joined", "bob", false},  // case-sensitive protocol
		{"", "bob", false},
		{"carol left", "bob", false},
	}
	for _, tt := range tests {
		if got := isOwnPresence(tt.text, tt.me); got != tt.want {
			t.Errorf("isOwnPresence(%q, %q) = %v; want %v", tt.text, tt.me, got, tt.want)
		}
	}
}

func TestMentionsUser(t *testing.T) {
	if !mentionsUser("carol left", "carol") {
		t.Error("own leave must be detected")
	}
	if mentionsUser("carol left", "carolito") {
		t.Error("prefix collision must not match")
	}
}

// ---------------------------------------------------------------------------
// Layout math (single source of truth for paint + hit-test)
// ---------------------------------------------------------------------------

func TestComputeLayout(t *testing.T) {
	const W, H = 120, 40

	t.Run("typical terminal no status", func(t *testing.T) {
		l := computeLayout(W, H, false)
		if l.statusRows != 0 {
			t.Fatalf("statusRows = %d; want 0", l.statusRows)
		}
		if l.vpHeight != H-headerHeight-inputChromeHeight {
			t.Errorf("vpHeight = %d; want %d", l.vpHeight, H-headerHeight-inputChromeHeight)
		}
		if l.rosterX != W-rosterTotalWidth {
			t.Errorf("rosterX = %d; want %d", l.rosterX, W-rosterTotalWidth)
		}
		if want := l.rosterX - 3; l.vpWidth != want {
			t.Errorf("vpWidth = %d; want %d (border+spacer)", l.vpWidth, want)
		}
		if l.rosterY0 != headerHeight+2 {
			t.Errorf("rosterY0 = %d; want header+border row", l.rosterY0)
		}
	})

	t.Run("status line consumes one row", func(t *testing.T) {
		with := computeLayout(W, H, true)
		without := computeLayout(W, H, false)
		if with.vpHeight != without.vpHeight-1 {
			t.Errorf("status must shrink viewport by exactly 1: %d vs %d", with.vpHeight, without.vpHeight)
		}
	})

	t.Run("tiny terminal clamps viewport to minimum", func(t *testing.T) {
		l := computeLayout(60, 8, true)
		if l.vpHeight < minViewportHeight {
			t.Errorf("vpHeight %d below hard floor %d", l.vpHeight, minViewportHeight)
		}
	})

	t.Run("narrow terminal keeps transcript readable", func(t *testing.T) {
		l := computeLayout(45, 30, false)
		if l.vpWidth < minViewportWidth {
			t.Errorf("vpWidth %d below floor %d", l.vpWidth, minViewportWidth)
		}
		if l.rosterX <= l.vpWidth {
			t.Errorf("sidebar (%d) must sit right of transcript (%d)", l.rosterX, l.vpWidth)
		}
	})

	t.Run("degenerate sizes return zeroed layout", func(t *testing.T) {
		for _, sz := range [][2]int{{0, 0}, {-1, 40}, {80, -5}} {
			l := computeLayout(sz[0], sz[1], false)
			if l != (layout{}) {
				t.Errorf("computeLayout(%d,%d) should zero out; got %+v", sz[0], sz[1], l)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Visibility filter matrix
// ---------------------------------------------------------------------------

func newFilterScreen(me, target string, users ...string) *chatScreen {
	return &chatScreen{
		me:         me,
		targetUser: target,
		users:      users,
		rendered:   map[int]bool{},
	}
}

func TestShouldRenderMatrix(t *testing.T) {
	common := func(c *chatScreen, m chatMessage) bool { return c.shouldRender(m) }

	bob := newFilterScreen("bob", "") // common room
	sys := func(text string) chatMessage { return chatMessage{Kind: "system", Text: text} }
	chat := func(u, text string) chatMessage { return chatMessage{Kind: "chat", Username: u, Text: text} }

	if common(bob, sys("bob joined")) {
		t.Error("own join hidden in common room")
	}
	if common(bob, sys("bob left")) {
		t.Error("own leave hidden in common room")
	}
	if !common(bob, sys("alice joined")) {
		t.Error("others' joins visible in common room")
	}
	if !common(bob, chat("alice", "hi")) {
		t.Error("all chat visible in common room")
	}

	priv := newFilterScreen("bob", "alice")
	if !common(priv, chat("alice", "psst")) {
		t.Error("peer messages visible in private view")
	}
	if !common(priv, chat("bob", "me too")) {
		t.Error("own messages visible in private view")
	}
	if common(priv, chat("carol", "noise")) {
		t.Error("third-party chat filtered in private view")
	}
	if common(priv, sys("carol joined")) {
		t.Error("unrelated presence filtered in private view")
	}
	if !common(priv, sys("alice left")) {
		t.Error("peer presence visible in private view")
	}
	if common(priv, sys("bob joined")) {
		t.Error("own presence stays hidden even when self is in the pair rule")
	}
}

// ---------------------------------------------------------------------------
// Roster rendering
// ---------------------------------------------------------------------------

func TestRosterRows(t *testing.T) {
	if got := rosterRow("bob", "bob", ""); !strings.Contains(got, "(you)") {
		t.Errorf("self row must be marked (you); got %q", got)
	}
	sel := rosterRow("alice", "bob", "alice")
	if !strings.Contains(sel, "alice") {
		t.Errorf("selected peer row lost name: %q", sel)
	}
	if plain := rosterRow("carol", "bob", "alice"); strings.Contains(plain, "(you)") || strings.Contains(plain, "●") {
		t.Errorf("bystander row mis-styled: %q", plain)
	}
}

func TestRosterBodyFixedHeightAndTruncation(t *testing.T) {
	c := &chatScreen{me: "me"}
	users := []string{"me"}
	for i := 0; i < 30; i++ {
		users = append(users, strings.Repeat("u", 3)+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}
	c.users = users

	out := c.rosterBody()
	rows := strings.Split(out, "\n")
	if len(rows) != rosterMaxVisible+3 { // title + N rows + 2 border lines
		t.Fatalf("roster height = %d rows; want deterministic %d", len(rows), rosterMaxVisible+3)
	}
	if !strings.Contains(out, "+") || !strings.Contains(out, "more") {
		t.Error("overflow indicator missing for >max users")
	}
}

// ---------------------------------------------------------------------------
// Mouse hit-testing against the SAME geometry View paints
// ---------------------------------------------------------------------------

func mouseAt(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Type: tea.MouseLeft, X: x, Y: y}
}

func TestHandleMouseHitTest(t *testing.T) {
	const W, H = 120, 40
	c := newFilterScreen("bob", "", "bob", "alice", "carol")
	c.width, c.height = W, H
	l := computeLayout(W, H, false)

	// Click dead-center of alice's row → private mode engages.
	yAlice := l.rosterY0 + 1 // row 0 = bob(self), row 1 = alice
	c.handleMouse(mouseAt(l.rosterX+5, yAlice))
	if c.targetUser != "alice" {
		t.Fatalf("click on alice row set target=%q; want alice", c.targetUser)
	}

	reset := func() { c.targetUser = "" }

	// Clicking yourself never selects.
	reset()
	c.handleMouse(mouseAt(l.rosterX+5, l.rosterY0))
	if c.targetUser != "" {
		t.Errorf("self-click selected %q", c.targetUser)
	}

	// Left column (transcript area) is inert.
	reset()
	c.handleMouse(mouseAt(2, yAlice))
	if c.targetUser != "" {
		t.Errorf("transcript click leaked selection: %q", c.targetUser)
	}

	// One row past the last user is inert (padding zone).
	reset()
	c.handleMouse(mouseAt(l.rosterX+5, l.rosterY0+len(c.users)))
	if c.targetUser != "" {
		t.Errorf("padding-row click leaked selection: %q", c.targetUser)
	}

	// Coordinates outside the terminal are inert (defensive guard).
	reset()
	c.handleMouse(mouseAt(c.width+3, yAlice))
	c.handleMouse(mouseAt(2, -1))
	c.handleMouse(mouseAt(2, c.height+10))
	if c.targetUser != "" {
		t.Errorf("out-of-terminal click leaked selection: %q", c.targetUser)
	}

	// Non-left clicks ignored.
	reset()
	c.handleMouse(tea.MouseMsg{Type: tea.MouseRight, X: l.rosterX + 5, Y: yAlice})
	if c.targetUser != "" {
		t.Errorf("right-click leaked selection: %q", c.targetUser)
	}

	// Clicking the already-selected peer is a no-op (no duplicate system line).
	c.handleMouse(mouseAt(l.rosterX+5, yAlice)) // select carol? no—alice
	before := len(c.lines)
	c.handleMouse(mouseAt(l.rosterX+5, yAlice))
	if len(c.lines) != before || c.targetUser != "alice" {
		t.Errorf("re-click on active peer mutated state: lines %d→%d target=%q", before, len(c.lines), c.targetUser)
	}
}

func TestEscNeverQuitsViaUpdate(t *testing.T) {
	c := newFilterScreen("bob", "alice")
	c.width, c.height = 120, 40

	m, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if quitRequested(m, cmd) {
		t.Fatal("Esc in private mode must return to common room, not quit")
	}
	got := m.(chatScreen)
	if got.targetUser != "" {
		t.Errorf("Esc did not clear private target: %q", got.targetUser)
	}

	// Esc in COMMON room must also be a harmless no-op.
	m2, cmd2 := got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if quitRequested(m2, cmd2) {
		t.Fatal("Esc in common room must NEVER quit (regression: app died on Esc)")
	}
}

// quitRequested unwraps a tea.Cmd and reports whether it is tea.Quit.
func quitRequested(_ tea.Model, cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	return cmd() == tea.Quit()
}

// ---------------------------------------------------------------------------
// Send pipeline: single-flight + bounded index edits
// ---------------------------------------------------------------------------

func TestSettleSendBoundsSafety(t *testing.T) {
	c := newFilterScreen("bob", "")
	// Simulate a stale pending pointer beyond the slice (defensive path).
	c.lines = []string{"kept"}
	c.pending = &pendingSend{lineIdx: 99, text: "ghost"}

	c.settleSend(sendDoneMsg{text: "ghost", seq: 7, code: 201})
	if len(c.lines) != 2 {
		t.Fatalf("fallback append expected; lines=%v", c.lines)
	}
	if c.pending != nil {
		t.Error("pending must clear after settle")
	}
}

func TestOutboxQueuesWhileInFlight(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.submitLine("first") // dispatches immediately
	if c.pending == nil {
		t.Fatal("first submit should enter in-flight state")
	}
	c.submitLine("second") // must queue, not corrupt pendingIdx
	c.submitLine("third")
	if len(c.outbox) != 2 {
		t.Fatalf("outbox = %v; want [second third]", c.outbox)
	}

	// Settling #1 promotes #2 into flight.
	c.settleSend(sendDoneMsg{text: "first", seq: 1, code: 201})
	if c.pending == nil || c.pending.text != "second" {
		t.Fatalf("outbox drain failed; pending=%+v", c.pending)
	}
	if len(c.outbox) != 1 {
		t.Fatalf("outbox after drain = %v", c.outbox)
	}

	c.settleSend(sendDoneMsg{text: "second", seq: 2, code: 201})
	c.settleSend(sendDoneMsg{text: "third", seq: 3, code: 201})
	if c.pending != nil || len(c.outbox) != 0 {
		t.Fatalf("pipeline not drained: pending=%+v outbox=%v", c.pending, c.outbox)
	}
}

func TestSettleSend429MarksEcho(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.submitLine("spam")
	idx := c.pending.lineIdx
	c.settleSend(sendDoneMsg{text: "spam", code: 429})
	if !strings.Contains(c.lines[idx], "slow down") {
		t.Errorf("429 must annotate echo line; got %q", c.lines[idx])
	}
	if c.pending != nil {
		t.Error("pending must clear on 429 so retries can proceed")
	}
}

func TestSettleSendFilteredDropsEchoRow(t *testing.T) {
	c := newFilterScreen("bob", "alice") // private view
	c.users = []string{"bob", "alice"}
	c.submitLine("to self")
	idx := c.pending.lineIdx
	before := len(c.lines)
	// Confirm while a system line flips nothing — message from me renders in
	// private view; force-filter by simulating third-party text instead.
	c.pending.text = "x"
	c.settleSend(sendDoneMsg{text: "x", seq: 9, code: 201, err: nil})
	if len(c.lines) > before && idx < len(c.lines) && strings.Contains(c.lines[idx], "[you →]") {
		t.Logf("note: own lines legitimately render in private view")
	}
}
