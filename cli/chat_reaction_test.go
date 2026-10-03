package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// Local toggle semantics: same emoji removes, different replaces
// ---------------------------------------------------------------------------

func TestReactionToggleReplaceRemove(t *testing.T) {
	c := newFilterScreen("me", "")
	c.applyLocalReaction("m1", "👍")
	if c.myReactions["m1"] != "👍" {
		t.Fatalf("my reaction = %q; want 👍", c.myReactions["m1"])
	}
	if got := c.reactionCounts["m1"]["👍"]; got != 1 {
		t.Fatalf("👍 count = %d; want 1", got)
	}

	// Same emoji toggles off — counts and mine both clear.
	c.applyLocalReaction("m1", "👍")
	if _, ok := c.myReactions["m1"]; ok {
		t.Fatal("same emoji must remove the reaction")
	}
	if len(c.reactionCounts["m1"]) != 0 {
		t.Fatalf("removal must leave no counts: %v", c.reactionCounts["m1"])
	}

	// A different emoji replaces the prior pick.
	c.applyLocalReaction("m1", "👍")
	c.applyLocalReaction("m1", "❤️")
	if c.myReactions["m1"] != "❤️" {
		t.Fatalf("replace failed: %q", c.myReactions["m1"])
	}
	counts := c.reactionCounts["m1"]
	if counts["❤️"] != 1 || counts["👍"] != 0 {
		t.Fatalf("replace left wrong counts: %v", counts)
	}
}

func TestReactionFailedPostRollsBack(t *testing.T) {
	c := newFilterScreen("me", "")
	c.applyLocalReaction("m1", "❤️")
	c.applyLocalReaction("m1", "👍") // replace: optimistic state is 👍

	m, _ := c.Update(reactionDoneMsg{msgId: "m1", emoji: "👍", prev: "❤️", err: errTestSink})
	sc := m.(chatScreen)
	if sc.myReactions["m1"] != "❤️" {
		t.Fatalf("rollback must restore prior pick, got %q", sc.myReactions["m1"])
	}
	if sc.reactionCounts["m1"]["❤️"] != 1 || sc.reactionCounts["m1"]["👍"] != 0 {
		t.Fatalf("rollback left wrong counts: %v", sc.reactionCounts["m1"])
	}
	if !strings.Contains(sc.status, "reaction failed") {
		t.Fatalf("failed POST must park on the status line, got %q", sc.status)
	}

	// Removal rolls back to nothing.
	c2 := newFilterScreen("me", "")
	c2.applyLocalReaction("m1", "👍")
	m2, _ := c2.Update(reactionDoneMsg{msgId: "m1", emoji: "👍", prev: "", err: errTestSink})
	sc2 := m2.(chatScreen)
	if _, ok := sc2.myReactions["m1"]; ok {
		t.Fatal("failed add must roll back to no reaction")
	}
	if len(sc2.reactionCounts["m1"]) != 0 {
		t.Fatalf("failed add left counts: %v", sc2.reactionCounts["m1"])
	}
}

// ---------------------------------------------------------------------------
// Badge rendering under the bubble (and the cache-eviction contract)
// ---------------------------------------------------------------------------

func TestReactionBadgeUnderBubble(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	c.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "me", Kind: "chat", Text: "hello", ConvID: generalConv})
	if painted := strings.Join(c.lines, "\n"); strings.Contains(painted, "👍") {
		t.Fatal("no reaction means no badge")
	}
	c.reactionCounts["m1"] = map[string]int{"👍": 2, "❤️": 1}
	// Cached bubble must be evicted or the stale paint sticks.
	c.evictRenderCache("m1")
	c.rebuildView()
	painted := strings.Join(c.lines, "\n")
	if !strings.Contains(painted, "👍2") || !strings.Contains(painted, "❤️1") {
		t.Fatalf("badge missing from paint:\n%s", painted)
	}
	// Allowlist order: 👍 before ❤️.
	if strings.Index(painted, "👍2") > strings.Index(painted, "❤️1") {
		t.Fatalf("badge order drifted from the allowlist:\n%s", painted)
	}
	if w := maxLineWidth(c.vp.View()); w > 60 {
		t.Fatalf("badge widened the transcript beyond the viewport: %d", w)
	}
}

// ---------------------------------------------------------------------------
// Conversation isolation: state is keyed by MsgId, paint follows activeConv
// ---------------------------------------------------------------------------

func TestReactionConvIsolation(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	dm := conversationKey("me", "alice")
	c.addMessage(chatMessage{Seq: 1, MsgId: "g1", Username: "me", Kind: "chat", Text: "roommsg", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, MsgId: "d1", Username: "alice", Kind: "chat", Text: "dmmsg", To: "me", ConvID: dm})

	c.applyFetchedReactions([]string{"g1", "d1"}, []reactionSummary{
		{MsgId: "g1", Counts: map[string]int{"👍": 1}, Mine: []string{"👍"}},
		{MsgId: "d1", Counts: map[string]int{"❤️": 2}},
	})
	c.rebuildView()

	room := strings.Join(c.lines, "\n")
	if !strings.Contains(room, "👍1") || strings.Contains(room, "❤️2") {
		t.Fatalf("room must paint only its own reaction:\n%s", room)
	}
	c.enterPrivate("alice")
	thread := strings.Join(c.lines, "\n")
	if !strings.Contains(thread, "❤️2") || strings.Contains(thread, "👍1") {
		t.Fatalf("thread must paint only its own reaction:\n%s", thread)
	}
	// State itself is per message, untouched by the view switch.
	if c.myReactions["g1"] != "👍" || c.myReactions["d1"] != "" {
		t.Fatalf("conv switch corrupted my-reaction state: %v", c.myReactions)
	}

	// A fetch scope of ids replaces truth wholesale: vanished reactions clear.
	c2 := newFilterScreen("me", "")
	c2.applyFetchedReactions([]string{"x"}, []reactionSummary{{MsgId: "x", Counts: map[string]int{"😂": 3}, Mine: []string{"😂"}}})
	if c2.reactionCounts["x"]["😂"] != 3 {
		t.Fatal("seed fetch failed")
	}
	if changed := c2.applyFetchedReactions([]string{"x"}, nil); !changed {
		t.Fatal("empty truth must clear (report changed)")
	}
	if len(c2.reactionCounts["x"]) != 0 || c2.myReactions["x"] != "" {
		t.Fatalf("empty truth did not clear: %v / %v", c2.reactionCounts["x"], c2.myReactions["x"])
	}
}

// ---------------------------------------------------------------------------
// Click mapping: message row -> drawer bar -> emoji toggle
// ---------------------------------------------------------------------------

func TestReactionClickOpenToggleAndDismiss(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	wireTestEngine(t, c, srv, "me", "alice")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "me", Kind: "chat", Text: "hello", ConvID: generalConv})

	row := -1
	for i, id := range sc.rowMsg {
		if id == "m1" {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("message missing from the row index: %v", sc.rowMsg)
	}
	l := sc.layoutFor()
	y := transcriptTopRow(l) + row - sc.vp.YOffset
	sc.handleMouse(mouseAt(transcriptX0(l)+3, y))
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("click must open the reaction bar for m1, got %q", sc.pendingReactionMsgId)
	}
	// Opening the bar reserved drawer rows; the bar sits just under the
	// (shrunk) transcript.
	l2 := sc.layoutFor()
	barY := reactionBarRow(l2)
	cmd := sc.handleMouse(mouseAt(transcriptX0(l2)+1, barY)) // offset 1 = first emoji
	if cmd == nil {
		t.Fatal("emoji click must return the persist command")
	}
	if sc.myReactions["m1"] != reactionEmojis[0] {
		t.Fatalf("click did not toggle %s: %v", reactionEmojis[0], sc.myReactions)
	}
	for _, msg := range drainCmds(cmd) {
		if rd, ok := msg.(reactionDoneMsg); ok && rd.err != nil {
			t.Fatalf("reaction POST failed: %v", rd.err)
		}
	}
	fs.mu.Lock()
	saved := len(fs.reactions["123456"])
	fs.mu.Unlock()
	if saved != 1 {
		t.Fatalf("server recorded %d reactions; want 1", saved)
	}

	// Same emoji again removes it (server toggle mirrors the local one).
	cmd = sc.handleMouse(mouseAt(transcriptX0(l2)+1, barY))
	if _, ok := sc.myReactions["m1"]; ok {
		t.Fatal("second click on the same emoji must remove the reaction")
	}
	for _, msg := range drainCmds(cmd) {
		if rd, ok := msg.(reactionDoneMsg); ok && rd.err != nil {
			t.Fatalf("removal POST failed: %v", rd.err)
		}
	}
	fs.mu.Lock()
	saved = len(fs.reactions["123456"])
	fs.mu.Unlock()
	if saved != 0 {
		t.Fatalf("server kept %d reactions after removal; want 0", saved)
	}

	// A second click on the message itself dismisses the bar.
	sc.handleMouse(mouseAt(transcriptX0(l)+3, y))
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("second click on the message must dismiss, got %q", sc.pendingReactionMsgId)
	}
}

// ---------------------------------------------------------------------------
// Keyboard fallback: r targets the newest message, 1-6 toggle
// ---------------------------------------------------------------------------

func TestReactionKeyboardFallback(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	c.addMessage(chatMessage{Seq: 1, MsgId: "old", Username: "alice", Kind: "chat", Text: "old", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, MsgId: "new", Username: "me", Kind: "chat", Text: "new", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 3, MsgId: "dm1", Username: "alice", Kind: "chat", Text: "dm", To: "me", ConvID: conversationKey("me", "alice")})
	c.focus = focusTranscript

	m, _ := c.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	sc := m.(chatScreen)
	if sc.pendingReactionMsgId != "new" {
		t.Fatalf("r must target the newest message, got %q", sc.pendingReactionMsgId)
	}
	m2, _ := sc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	sc2 := m2.(chatScreen)
	if sc2.myReactions["new"] != reactionEmojis[1] {
		t.Fatalf("key 2 must toggle %s, got %q", reactionEmojis[1], sc2.myReactions["new"])
	}
	// Esc dismisses the bar without touching the transcript.
	m3, _ := sc2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m3.(chatScreen).pendingReactionMsgId != "" {
		t.Fatal("Esc must close the reaction bar")
	}
	// r on a DM view targets that thread's newest message.
	sc2.enterPrivate("alice")
	sc2.focus = focusTranscript
	m4, _ := sc2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if got := m4.(chatScreen).pendingReactionMsgId; got != "dm1" {
		t.Fatalf("thread r must target the thread's message, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Server truth: GET /reactions poll, narrowed per active conversation
// ---------------------------------------------------------------------------

func TestReactionPollFetchesServerTruth(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "me", "alice")
	dm := conversationKey("me", "alice")
	c.addMessage(chatMessage{Seq: 1, MsgId: "g1", Username: "alice", Kind: "chat", Text: "room", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, MsgId: "d1", Username: "alice", Kind: "chat", Text: "dm", To: "me", ConvID: dm})

	peer := &signalClient{serverURL: srv.URL, key: "123456", me: "alice"}
	if err := peer.react("g1", "👍"); err != nil {
		t.Fatalf("peer react: %v", err)
	}
	if err := peer.react("d1", "❤️"); err != nil {
		t.Fatalf("peer react: %v", err)
	}

	// In the room, the poll asks only about room messages.
	var fetched reactionsFetchedMsg
	ok := false
	for _, msg := range drainCmds(c.fetchReactionsCmd()) {
		if f, is := msg.(reactionsFetchedMsg); is {
			fetched, ok = f, true
		}
	}
	if !ok {
		t.Fatal("poll produced no result")
	}
	if fetched.err != nil {
		t.Fatalf("poll failed: %v", fetched.err)
	}
	if len(fetched.ids) != 1 || fetched.ids[0] != "g1" {
		t.Fatalf("room poll ids = %v; want [g1] (DM must stay out)", fetched.ids)
	}
	m, _ := c.Update(fetched)
	sc := m.(chatScreen)
	if sc.reactionCounts["g1"]["👍"] != 1 || sc.reactionCounts["d1"] != nil {
		t.Fatalf("room truth applied wrong: %v", sc.reactionCounts)
	}
	if len(sc.myReactions) != 0 {
		t.Fatalf("peer reaction must not become mine: %v", sc.myReactions)
	}

	// In the thread, the poll narrows to the DM message.
	sc.enterPrivate("alice")
	var dmFetched reactionsFetchedMsg
	ok = false
	for _, msg := range drainCmds(sc.fetchReactionsCmd()) {
		if f, is := msg.(reactionsFetchedMsg); is {
			dmFetched, ok = f, true
		}
	}
	if !ok || len(dmFetched.ids) != 1 || dmFetched.ids[0] != "d1" {
		t.Fatalf("thread poll ids = %v; want [d1]", dmFetched.ids)
	}
	m2, _ := sc.Update(dmFetched)
	sc2 := m2.(chatScreen)
	if sc2.reactionCounts["d1"]["❤️"] != 1 {
		t.Fatalf("thread truth not applied: %v", sc2.reactionCounts)
	}
}

func TestReactionNudgeTriggersFetch(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "me", "alice")
	c.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hi", ConvID: generalConv})

	_, cmd := c.Update(netReactionMsg{reaction: engineReaction{MsgId: "r1", From: "alice", Target: "m1", Emoji: "👍"}})
	found := false
	for _, msg := range drainCmds(cmd) {
		if _, ok := msg.(reactionsFetchedMsg); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("a reaction nudge must schedule an immediate reactions fetch")
	}
}

// ---------------------------------------------------------------------------
// Wire plumbing: frame round-trip, dispatch, best-effort send
// ---------------------------------------------------------------------------

func TestReactionFrameDispatch(t *testing.T) {
	f := newFrame(frameReaction, "rid", "alice", "bob")
	f.Target = "m1"
	f.Emoji = "❤️"
	raw, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != frameReaction || got.Target != "m1" || got.Emoji != "❤️" {
		t.Fatalf("reaction round-trip mismatch: %+v", got)
	}

	var seen []engineReaction
	e := &engine{cb: engineCallbacks{onReaction: func(r engineReaction) { seen = append(seen, r) }}}
	e.dispatch(f)
	if len(seen) != 1 || seen[0].Target != "m1" || seen[0].Emoji != "❤️" || seen[0].From != "alice" {
		t.Fatalf("dispatch mismatch: %+v", seen)
	}
	// Malformed nudges (missing target / non-allowlisted emoji) are dropped.
	e.dispatch(newFrame(frameReaction, "rid2", "alice", ""))
	bad := newFrame(frameReaction, "rid3", "alice", "")
	bad.Target = "m1"
	bad.Emoji = "🚀"
	e.dispatch(bad)
	if len(seen) != 1 {
		t.Fatalf("malformed reactions must be dropped, got %+v", seen)
	}
}

func TestSendReactionNudgesPeers(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "")
	wireTestEngine(t, c, srv, "me", "alice")
	if err := c.eng.sendReaction("", "m1", "👍"); err != nil {
		t.Fatalf("sendReaction: %v", err)
	}
	if n := inboxDeposits(fs, "alice"); n != 1 {
		t.Fatalf("nudge deposits = %d; want 1", n)
	}
	if err := c.eng.sendReaction("", "m1", "🚀"); err == nil {
		t.Fatal("allowlist must reject unknown emoji")
	}
	if err := c.eng.sendReaction("", "", "👍"); err == nil {
		t.Fatal("empty target must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Height contract: the reaction bar reserves exactly its painted rows
// ---------------------------------------------------------------------------

func TestReactionBarKeepsFrameContract(t *testing.T) {
	for _, wh := range [][2]int{{80, 24}, {100, 30}, {62, 20}, {120, 40}, {240, 50}} {
		w, h := wh[0], wh[1]
		c := newFilterScreen("me", "", "me", "alice")
		c.vp = *viewportPtr(40, 10)
		c.pendingReactionMsgId = "m1"
		m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
		sc := m.(chatScreen)
		sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "me", Kind: "chat", Text: "hello", ConvID: generalConv})
		v := sc.View()
		if rows := strings.Count(v, "\n") + 1; rows > h {
			t.Fatalf("w=%d h=%d: bar-open frame painted %d rows", w, h, rows)
		}
		if mw := maxLineWidth(v); mw > w {
			t.Fatalf("w=%d h=%d: bar-open frame width %d", w, h, mw)
		}
		// Pure layout agrees with the painter's budget.
		if l := computeLayoutMedia(w, h, false, 2); l.totalRows() > h {
			t.Fatalf("w=%d h=%d: palette budget overflows (%d rows)", w, h, l.totalRows())
		}
	}
}

// Bar hit cells must match the painted separators for every choice.
func TestReactionBarHitCells(t *testing.T) {
	items := reactionBarItems()
	if len(items) != len(reactionEmojis)+1 || items[len(items)-1] != "+" {
		t.Fatalf("unexpected bar items: %v", items)
	}
	pos := 1 // past "["
	for _, item := range items {
		got, ok := reactionCellAt(pos)
		if !ok || got != item {
			t.Fatalf("cell %d = %q,%v; want %q", pos, got, ok, item)
		}
		pos += lipglossWidth(item) + 1
	}
	if _, ok := reactionCellAt(0); ok {
		t.Fatal("bracket offset must not hit a choice")
	}
}
