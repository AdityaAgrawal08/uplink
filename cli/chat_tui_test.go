package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Presence matching (bug regression: Contains over-matched bob/bobby)
// ---------------------------------------------------------------------------

func TestIsOwnPresence(t *testing.T) {
	tests := []struct {
		text string
		me   string
		want bool
	}{
		{"bob joined", "bob", true},
		{"bob left", "bob", true},
		{"bobby joined", "bob", false},
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
// Layout math — HEIGHT INVARIANT is the headline contract
// ---------------------------------------------------------------------------

func TestComputeLayout(t *testing.T) {
	const W, H = 120, 40

	l := computeLayout(W, H, false)
	if !l.sidebarOn {
		t.Fatal("sidebar must be on at wide terminals")
	}
	if want := H - headerHeight - transcriptBorder - inputChromeHeight; l.vpHeight != want {
		t.Errorf("vpHeight = %d; want exact fit %d", l.vpHeight, want)
	}
	if l.totalRows() != H {
		t.Errorf("totalRows = %d; MUST equal termH exactly (%d)", l.totalRows(), H)
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

	t.Run("status line consumes exactly one row", func(t *testing.T) {
		with := computeLayout(W, H, true)
		without := computeLayout(W, H, false)
		if with.vpHeight != without.vpHeight-1 || with.statusRows != 1 {
			t.Errorf("status must shrink viewport by exactly 1: %+v vs %+v", with, without)
		}
		if with.totalRows() != H {
			t.Errorf("status frame overflows: total=%d termH=%d", with.totalRows(), H)
		}
	})

	t.Run("roster slots track viewport height", func(t *testing.T) {
		big := computeLayout(W, 60, false)
		if big.rosterSlots != rosterMaxVisible {
			t.Errorf("tall term should cap slots at %d; got %d", rosterMaxVisible, big.rosterSlots)
		}
		small := computeLayout(W, 14, false) // vpHeight = 14-6 = 8 → 7 slots
		if small.rosterSlots != small.vpHeight-1 {
			t.Errorf("slots %d must equal vpHeight-1 %d", small.rosterSlots, small.vpHeight-1)
		}
	})

	t.Run("tiny terminal degrades to zero-height viewport without overflow", func(t *testing.T) {
		for h := 1; h <= 8; h++ {
			l := computeLayout(W, h, true)
			if l.totalRows() > h {
				t.Fatalf("termH=%d overflows: totalRows=%d", h, l.totalRows())
			}
			if l.vpHeight < 0 {
				t.Fatalf("termH=%d negative vpHeight", h)
			}
		}
	})
}

// TestFrameNeverExceedsTerminal sweeps the whole realistic size matrix — this
// is the regression for "TUI exceeds the window height".
func TestFrameNeverExceedsTerminal(t *testing.T) {
	widths := []int{20, 40, 55, 61, 62, 63, 80, 100, 120, 160, 240}
	heights := []int{5, 8, 10, 12, 16, 20, 24, 30, 40, 50, 80}
	for _, w := range widths {
		for _, h := range heights {
			for _, st := range []bool{false, true} {
				l := computeLayout(w, h, st)
				if got := l.totalRows(); got > h {
					t.Errorf("w=%d h=%d status=%v: totalRows=%d EXCEEDS terminal", w, h, st, got)
				}
				if l.sidebarOn && l.rosterX+rosterTotalWidth > w {
					t.Errorf("w=%d: sidebar spills past right edge", w)
				}
				if !l.sidebarOn && l.vpWidth != w-2 && w >= 12 {
					t.Errorf("w=%d collapsed layout should use full width-2; got %d", w, l.vpWidth)
				}
			}
		}
	}
}

func TestSidebarResponsiveCollapse(t *testing.T) {
	if computeLayout(minSidebarTermW, 30, false).sidebarOn != true {
		t.Error("threshold width itself must keep the sidebar")
	}
	if computeLayout(minSidebarTermW-1, 30, false).sidebarOn != false {
		t.Error("one column below threshold must collapse the sidebar")
	}
	collapsed := computeLayout(50, 30, false)
	if collapsed.rosterX != 0 {
		t.Errorf("collapsed rosterX = %d; want 0", collapsed.rosterX)
	}
	if collapsed.vpWidth != 48 {
		t.Errorf("collapsed vpWidth = %d; want termW-2=48", collapsed.vpWidth)
	}
}

// ---------------------------------------------------------------------------
// Visibility filter matrix
// ---------------------------------------------------------------------------

func newFilterScreen(me, target string, users ...string) *chatScreen {
	ti := textinput.New()
	ti.Placeholder = "Type a message…"
	ti.Focus()
	ti.CharLimit = 500
	return &chatScreen{
		me:         me,
		targetUser: target,
		users:      users,
		rendered:   map[int]bool{},
		input:      ti,
	}
}

func TestShouldRenderMatrix(t *testing.T) {
	render := func(c *chatScreen, m chatMessage) bool { return c.shouldRender(m) }

	bob := newFilterScreen("bob", "")
	sys := func(text string) chatMessage { return chatMessage{Kind: "system", Text: text} }
	chat := func(u, text string) chatMessage { return chatMessage{Kind: "chat", Username: u, Text: text} }

	if render(bob, sys("bob joined")) || render(bob, sys("bob left")) {
		t.Error("own presence hidden in common room")
	}
	if !render(bob, sys("alice joined")) || !render(bob, chat("alice", "hi")) {
		t.Error("others visible in common room")
	}

	priv := newFilterScreen("bob", "alice")
	if !render(priv, chat("alice", "psst")) || !render(priv, chat("bob", "me too")) {
		t.Error("pair messages visible in private view")
	}
	if render(priv, chat("carol", "noise")) || render(priv, sys("carol joined")) {
		t.Error("third parties filtered in private view")
	}
	if !render(priv, sys("alice left")) {
		t.Error("peer presence visible in private view")
	}
	if render(priv, sys("bob joined")) {
		t.Error("own presence stays hidden even inside pair rule")
	}
}

// ---------------------------------------------------------------------------
// Roster rendering — adaptive slot count
// ---------------------------------------------------------------------------

func TestRosterRows(t *testing.T) {
	if got := rosterRow("bob", "bob", ""); !strings.Contains(got, "(you)") {
		t.Errorf("self row must be marked (you); got %q", got)
	}
	if plain := rosterRow("carol", "bob", "alice"); strings.Contains(plain, "(you)") || strings.Contains(plain, "●") {
		t.Errorf("bystander row mis-styled: %q", plain)
	}
}

func TestRosterBodyAdaptiveSlots(t *testing.T) {
	c := &chatScreen{me: "me"}
	users := []string{"me"}
	for i := 0; i < 30; i++ {
		users = append(users, fmt.Sprintf("user%02d", i))
	}
	c.users = users

	// Full slots: deterministic height, overflow indicator present.
	out := c.rosterBody(rosterMaxVisible)
	rows := strings.Split(out, "\n")
	if len(rows) != rosterMaxVisible+3 { // title + N slots + 2 border
		t.Fatalf("full roster height = %d rows; want %d", len(rows), rosterMaxVisible+3)
	}
	if !strings.Contains(out, "+") || !strings.Contains(out, "more") {
		t.Error("overflow indicator missing")
	}

	// Fewer slots than users: panel SHRINKS (never inflates short frames).
	shrunk := c.rosterBody(4)
	if rows := strings.Split(shrunk, "\n"); len(rows) != 4+3 {
		t.Fatalf("shrunken roster height = %d; want 7", len(rows))
	}

	// More slots than users: padded, no overflow marker.
	c2 := &chatScreen{me: "me", users: []string{"me", "alice"}}
	sparse := c2.rosterBody(8)
	if strings.Contains(sparse, "more") {
		t.Errorf("overflow indicator shown despite fitting: %q", sparse)
	}
	if rows := strings.Split(sparse, "\n"); len(rows) != 8+3 {
		t.Fatalf("sparse roster height = %d; want 11", len(rows))
	}

	c.users = []string{"me", "a"}
	zero := c.rosterBody(0)
	if h := lipgloss.Height(zero); h != 3 { // title row + 2 border rows
		t.Errorf("zero-slot roster height = %d; want 3", h)
	}
}

// ---------------------------------------------------------------------------
// Wrapping — long styled lines fold instead of clip on resize
// ---------------------------------------------------------------------------

func TestRefreshViewportWrapsToWidth(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(20, 10)

	long := tuiNameStyle.Render("alice") + ": " + strings.Repeat("word ", 30)
	c.lines = []string{long}
	c.refreshViewport()

	wrapped := strings.Split(c.vp.View(), "\n")
	if len(wrapped) < 2 {
		t.Fatal("long line did not wrap")
	}
	for _, ln := range wrapped {
		if lw := lipgloss.Width(ln); lw > 20 {
			t.Fatalf("wrapped line width %d exceeds viewport 20: %q", lw, ln)
		}
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

	yAlice := l.rosterY0 + 1 // row 0 = self, row 1 = alice
	reset := func() { c.targetUser = "" }

	c.handleMouse(mouseAt(l.rosterX+5, yAlice))
	if c.targetUser != "alice" {
		t.Fatalf("click on alice set target=%q", c.targetUser)
	}

	reset()
	c.handleMouse(mouseAt(l.rosterX+5, l.rosterY0)) // self row
	if c.targetUser != "" {
		t.Errorf("self-click selected %q", c.targetUser)
	}

	reset()
	c.handleMouse(mouseAt(2, yAlice)) // transcript area
	if c.targetUser != "" {
		t.Errorf("transcript click leaked selection: %q", c.targetUser)
	}

	reset()
	c.handleMouse(mouseAt(l.rosterX+5, l.rosterY0+l.rosterSlots)) // past last user
	if c.targetUser != "" {
		t.Errorf("padding-row click leaked selection: %q", c.targetUser)
	}

	reset()
	c.handleMouse(mouseAt(c.width+3, yAlice))
	c.handleMouse(mouseAt(2, -1))
	c.handleMouse(mouseAt(2, c.height+10))
	if c.targetUser != "" {
		t.Errorf("out-of-terminal click leaked selection: %q", c.targetUser)
	}

	reset()
	c.handleMouse(tea.MouseMsg{Type: tea.MouseRight, X: l.rosterX + 5, Y: yAlice})
	if c.targetUser != "" {
		t.Errorf("right-click leaked selection: %q", c.targetUser)
	}

	// Re-click on the ACTIVE peer is a complete no-op.
	c.handleMouse(mouseAt(l.rosterX+5, yAlice))
	before := len(c.lines)
	c.handleMouse(mouseAt(l.rosterX+5, yAlice))
	if len(c.lines) != before || c.targetUser != "alice" {
		t.Errorf("re-click mutated state: lines %d→%d target=%q", before, len(c.lines), c.targetUser)
	}
}

func TestMouseInertWhenSidebarCollapsed(t *testing.T) {
	const W, H = 50, 30 // below minSidebarTermW
	c := newFilterScreen("bob", "", "bob", "alice")
	c.width, c.height = W, H
	l := computeLayout(W, H, false)
	if l.sidebarOn {
		t.Fatal("expected collapsed sidebar at this width")
	}
	c.handleMouse(mouseAt(l.rosterY0+30, l.rosterY0+1)) // any coords
	if c.targetUser != "" {
		t.Errorf("collapsed-sidebar click selected %q", c.targetUser)
	}
}

func TestEscNeverQuitsViaUpdate(t *testing.T) {
	c := newFilterScreen("bob", "alice")
	c.width, c.height = 120, 40

	m, cmd := c.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if quitRequested(cmd) {
		t.Fatal("Esc in private mode must return to common room, not quit")
	}
	got := m.(chatScreen)
	if got.targetUser != "" {
		t.Errorf("Esc did not clear private target: %q", got.targetUser)
	}

	_, cmd2 := got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if quitRequested(cmd2) {
		t.Fatal("Esc in common room must NEVER quit (regression)")
	}
}

func quitRequested(cmd tea.Cmd) bool {
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
	c.vp = *viewportPtr(40, 10)
	c.appendLine("kept")
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
	c.vp = *viewportPtr(40, 10)

	c.submitLine("first")
	if c.pending == nil {
		t.Fatal("first submit should enter in-flight state")
	}
	c.submitLine("second")
	c.submitLine("third")
	if len(c.outbox) != 2 {
		t.Fatalf("outbox = %v; want [second third]", c.outbox)
	}

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
	c.vp = *viewportPtr(40, 10)
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

// viewportPtr is a tiny helper so tests can seed a real viewport model.
func viewportPtr(w, h int) *viewport.Model {
	vp := viewport.New(w, h)
	return &vp
}

// TestViewPaintedHeightNeverExceedsTerminal runs the FULL render path
// (WindowSizeMsg → View) across a size sweep and asserts the painted frame
// fits every terminal. This catches assembly bugs that pure layout math can't.
func TestViewPaintedHeightNeverExceedsTerminal(t *testing.T) {
	widths := []int{20, 40, 61, 62, 80, 100, 120, 200}
	heights := []int{1, 3, 5, 6, 8, 12, 16, 20, 24, 30, 40, 60}
	for _, w := range widths {
		for _, h := range heights {
			for _, st := range []string{"", "reconnecting… (2)"} {
				c := newFilterScreen("bob", "", "bob", "alice", "carol")
				c.vp = *viewportPtr(80, 20)
				c.status = st
				c.lines = []string{
					tuiNameStyle.Render("alice") + ": " + strings.Repeat("lorem ipsum dolor ", 8),
					tuiMeStyle.Render("[you →] hello"),
					tuiSystemStyle.Render("* alice joined"),
				}
				m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
				got := m.(chatScreen).View()
				rows := strings.Count(got, "\n") + 1
				if rows > h {
					t.Errorf("w=%d h=%d status=%q: painted %d rows > terminal", w, h, st, rows)
				}
				if mw := maxLineWidth(got); mw > w {
					t.Errorf("w=%d h=%d: painted line width %d exceeds terminal", w, h, mw)
				}
			}
		}
	}
}

// maxLineWidth measures ANSI-aware printable width of the widest row.
func maxLineWidth(s string) int {
	max := 0
	for _, ln := range strings.Split(s, "\n") {
		if w := lipgloss.Width(ln); w > max {
			max = w
		}
	}
	return max
}

// ---------------------------------------------------------------------------
// Wire-level send integration: Update(Enter) must produce a command whose
// invocation performs the HTTP POST. Regression for the silent-send bug where
// the optimistic echo painted but no request ever left the process.
// ---------------------------------------------------------------------------

func newFakeChatServer(t *testing.T, got *[][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if got != nil {
			*got = append(*got, body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"seq":%d}`, len(*got)+100)
	}))
}

func TestUpdateEnterActuallySendsOverWire(t *testing.T) {
	var received [][]byte
	srv := newFakeChatServer(t, &received)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.client = newChatClient(srv.URL, "123456", "bob")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter, Runes: []rune{}})
	_ = model
	if cmd != nil {
		cmd() // empty-input Enter is local-only; harmless if fires
	}

	// Type "hello" then press Enter.
	for _, r := range []rune("hello") {
		m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = m2
	}
	m3, sendCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if sendCmd == nil {
		t.Fatal("Update(KeyEnter) returned nil command — NOTHING WOULD BE SENT (regression)")
	}
	msg := sendCmd()
	sd, ok := msg.(sendDoneMsg)
	if !ok {
		t.Fatalf("send cmd yielded %T; want sendDoneMsg", msg)
	}
	if len(received) != 1 || !strings.Contains(string(received[0]), "hello") {
		t.Fatalf("wire payload wrong: %v", received)
	}

	// Feed the confirmation back through Update; echo must resolve.
	m4, promo := m3.(chatScreen).Update(sd)
	got := m4.(chatScreen)
	if got.pending != nil {
		t.Fatal("pending not cleared after settle")
	}
	if len(got.lines) != 1 || strings.Contains(got.lines[0], "[you →]") {
		t.Errorf("echo not replaced with confirmed line: %q", got.lines)
	}
	if promo != nil {
		t.Fatal("promotion cmd emitted with empty outbox")
	}
}

func TestOutboxPromotionGoesOverWire(t *testing.T) {
	var received [][]byte
	srv := newFakeChatServer(t, &received)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.client = newChatClient(srv.URL, "123456", "bob")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	scr := m.(chatScreen)

	type keyed interface {
		Update(tea.Msg) (tea.Model, tea.Cmd)
	}
	var km keyed = scr

	typeText := func(s string) {
		for _, r := range []rune(s) {
			nm, _ := km.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			km = nm.(keyed)
		}
	}
	enter := func() (keyed, tea.Cmd) {
		nm, cmd := km.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return nm.(keyed), cmd
	}

	typeText("one")
	km, c1 := enter()
	typeText("two")
	km, c2 := enter() // queued while one is in flight
	typeText("three")
	km, c3 := enter()

	if c1 == nil || c2 != nil || c3 != nil {
		t.Fatalf("only in-flight send may carry a cmd; got %v %v %v", c1, c2, c3)
	}
	sd1 := c1().(sendDoneMsg)
	km, promo1 := km.Update(sd1)
	if promo1 == nil {
		t.Fatal("settle must return promotion cmd for queued head (regression)")
	}
	sd2 := promo1().(sendDoneMsg)
	if sd2.text != "two" {
		t.Fatalf("promoted %q; want two", sd2.text)
	}
	km, promo2 := km.Update(sd2)
	sd3 := promo2().(sendDoneMsg)
	km, promo3 := km.Update(sd3)

	final := km.(chatScreen)
	if final.pending != nil || len(final.outbox) != 0 {
		t.Fatalf("pipeline undrained: pending=%+v outbox=%v", final.pending, final.outbox)
	}
	if promo3 != nil {
		t.Fatal("spurious promotion after last message")
	}
	if len(received) != 3 {
		t.Fatalf("wire saw %d posts; want 3 (%v)", len(received), received)
	}
	for i, want := range []string{"one", "two", "three"} {
		if !strings.Contains(string(received[i]), want) {
			t.Errorf("post %d = %q; want %q", i, received[i], want)
		}
	}
}
