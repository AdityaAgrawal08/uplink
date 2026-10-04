package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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
// Click mapping: message entry -> anchored picker -> emoji toggle
// ---------------------------------------------------------------------------

// msgTopY is the terminal row of the first painted row of a message entry
// (rowMsg maps every wrapped row of the entry to its MsgId).
func msgTopY(c *chatScreen, msgId string) int {
	l := c.layoutFor()
	for row, id := range c.rowMsg {
		if id == msgId {
			return transcriptTopRow(l) + row - c.vp.YOffset
		}
	}
	return -1
}

// pickerHitX maps a choice to the terminal X of its first painted cell in the
// anchored picker (mirrors handleMouse's offset math).
func pickerHitX(c *chatScreen, choice string) int {
	l := c.layoutFor()
	for off := 0; off < 64; off++ {
		if got, ok := reactionCellAt(off); ok && got == choice {
			return transcriptX0(l) + 1 + c.reactionPickerX0() + off
		}
	}
	return -1
}

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

	// Click the message: the picker opens anchored directly ABOVE the
	// message entry (one content row earlier), not in the drawer slot.
	top := msgTopY(&sc, "m1")
	if top < 0 {
		t.Fatalf("message missing from the row index: %v", sc.rowMsg)
	}
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, top))
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("click must open the picker for m1, got %q", sc.pendingReactionMsgId)
	}
	if got := sc.paletteRows(); got != 0 {
		t.Fatalf("anchored picker must not budget drawer rows, got %d", got)
	}
	pickerY := sc.reactionPickerY(sc.layoutFor())
	if anchoredTop := msgTopY(&sc, "m1"); pickerY != anchoredTop-1 {
		t.Fatalf("picker row = %d; want %d (directly above the message entry)", pickerY, anchoredTop-1)
	}

	// Click 👍 on the picker: it toggles, persists through the fake server,
	// and closes the picker.
	cmd := sc.handleMouse(mouseAt(pickerHitX(&sc, reactionEmojis[0]), pickerY))
	if cmd == nil {
		t.Fatal("emoji click must return the persist command")
	}
	if sc.myReactions["m1"] != reactionEmojis[0] {
		t.Fatalf("click did not toggle %s: %v", reactionEmojis[0], sc.myReactions)
	}
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("a pick must close the anchored picker, still open on %q", sc.pendingReactionMsgId)
	}
	if sc.reactionPickerY(sc.layoutFor()) != -1 {
		t.Fatal("picker row still mapped after the pick closed it")
	}
	if sc.detailMsgId != "" || sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatalf("a pick must not open the detail bar, got %q/%q", sc.detailMsgId, sc.detailEmoji)
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

	// Re-open and click the same emoji: same emoji removes (server toggle
	// mirrors the local one).
	top = msgTopY(&sc, "m1")
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, top))
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("re-click must re-anchor the picker, got %q", sc.pendingReactionMsgId)
	}
	pickerY = sc.reactionPickerY(sc.layoutFor())
	cmd = sc.handleMouse(mouseAt(pickerHitX(&sc, reactionEmojis[0]), pickerY))
	if _, ok := sc.myReactions["m1"]; ok {
		t.Fatal("second pick on the same emoji must remove the reaction")
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

	// A second click on the message itself dismisses without picking.
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgTopY(&sc, "m1")))
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("click must re-open the picker, got %q", sc.pendingReactionMsgId)
	}
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgTopY(&sc, "m1")))
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("second click on the message must dismiss, got %q", sc.pendingReactionMsgId)
	}
}

// ---------------------------------------------------------------------------
// Reactor detail bar: badge click -> <user> : <emoji> rows directly below the
// message, single aux row, counts+mine fallback
// ---------------------------------------------------------------------------

// msgLastY is the terminal row of the LAST painted row of a message entry —
// the badge row whenever the message has one.
func msgLastY(c *chatScreen, msgId string) int {
	l := c.layoutFor()
	last := -1
	for row, id := range c.rowMsg {
		if id == msgId {
			last = row
		}
	}
	if last < 0 {
		return -1
	}
	return transcriptTopRow(l) + last - c.vp.YOffset
}

// badgeHitX maps one badge emoji to the terminal X of its first content cell
// (mirrors reactionBadgeEmojiAt's geometry: chip span + side alignment).
func badgeHitX(c *chatScreen, m chatMessage, emoji string) int {
	line, hits := c.reactionBadgeCore(m)
	if line == "" {
		return -1
	}
	x0 := transcriptX0(c.layoutFor())
	if m.Username == c.me {
		x0 += maxInt(c.transcriptW()-lipgloss.Width(line), 0)
	}
	for _, h := range hits {
		if h.emoji == emoji {
			return x0 + h.start + 1 // +1: skip the chip's left pad
		}
	}
	return -1
}

// detailCardText returns the open dropdown's painted lines (border included),
// in paint order, or nil when no card is painted right now. A helper so tests
// never have to re-derive the detailLineIdx/detailN window by hand.
func detailCardText(c *chatScreen) []string {
	if c.detailLineIdx < 0 || c.detailN <= 0 || c.detailLineIdx+c.detailN > len(c.lines) {
		return nil
	}
	return c.lines[c.detailLineIdx : c.detailLineIdx+c.detailN]
}

func TestReactionDetailBadgeClickOpensBelowEntry(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hello", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"m1"}, []reactionSummary{{
		MsgId:  "m1",
		Counts: map[string]int{"👍": 2, "❤️": 1},
		Details: []reactionDetail{
			{Emoji: "👍", Usernames: []string{"alice", "bob"}},
			{Emoji: "❤️", Usernames: []string{"carol"}},
		},
	}})
	sc.rebuildView()

	msg, ok := sc.msgById("m1")
	if !ok {
		t.Fatal("message missing from history")
	}
	badgeY := msgLastY(&sc, "m1")
	x := badgeHitX(&sc, msg, "❤️")
	if x < 0 {
		t.Fatal("❤️ chip not painted in the badge")
	}
	sc.handleMouse(mouseAt(x, badgeY))

	if sc.detailMsgId != "m1" || sc.detailEmoji != "❤️" {
		t.Fatalf("badge click must open detail for m1/❤️, got %q/%q", sc.detailMsgId, sc.detailEmoji)
	}
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("opening the detail must not also open the picker, got %q", sc.pendingReactionMsgId)
	}
	l := sc.layoutFor()
	if y := sc.reactionDetailY(l); y != badgeY+1 {
		t.Fatalf("detail first row = %d; want %d (directly below the badge)", y, badgeY+1)
	}
	card := detailCardText(&sc)
	if len(card) == 0 {
		t.Fatal("dropdown card not painted")
	}
	painted := stripANSI(strings.Join(card, "\n"))
	if !strings.Contains(painted, "❤️ 1") {
		t.Fatalf("card header must carry the emoji and tally, painted:\n%s", painted)
	}
	if !strings.Contains(painted, "• carol") {
		t.Fatalf("card must list the reactor, painted:\n%s", painted)
	}
	if !strings.Contains(painted, "╭") || !strings.Contains(painted, "╰") {
		t.Fatalf("card must wear its rounded border, painted:\n%s", painted)
	}
	body := strings.Join(sc.lines, "\n")
	if strings.Index(body, "hello") > strings.Index(body, "• carol") {
		t.Fatalf("card must paint below its message, painted:\n%s", body)
	}

	// Reselecting the same chip closes the detail (one aux row discipline).
	sc.handleMouse(mouseAt(x, msgLastY(&sc, "m1")))
	if sc.detailMsgId != "" || sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatal("reselecting the badge chip must close the detail")
	}

	// A bubble-row click keeps its old meaning: open the picker above, with
	// no detail bar left behind.
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgTopY(&sc, "m1")))
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("bubble click must open the picker, got %q", sc.pendingReactionMsgId)
	}
	if sc.detailMsgId != "" {
		t.Fatal("opening the picker must close the detail")
	}
}

func TestReactionDetailPickerPickTogglesOnlyBadgeOpensDetail(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	_, ids := wireTestEngine(t, c, srv, "me", "alice")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "me", Kind: "chat", Text: "hello", ConvID: generalConv})

	top := msgTopY(&sc, "m1")
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, top))
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("click must open the picker, got %q", sc.pendingReactionMsgId)
	}
	pickerY := sc.reactionPickerY(sc.layoutFor())
	cmd := sc.handleMouse(mouseAt(pickerHitX(&sc, reactionEmojis[1]), pickerY))
	for _, msg := range drainCmds(cmd) {
		if rd, ok := msg.(reactionDoneMsg); ok && rd.err != nil {
			t.Fatalf("reaction POST failed: %v", rd.err)
		}
	}
	if sc.myReactions["m1"] != reactionEmojis[1] {
		t.Fatalf("picker click must toggle %s, got %q", reactionEmojis[1], sc.myReactions["m1"])
	}
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("picker must close after the pick, got %q", sc.pendingReactionMsgId)
	}
	// A pick is a toggle only: no detail may auto-open behind it.
	if sc.detailMsgId != "" || sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatalf("picker pick must not open the detail, got %q/%q", sc.detailMsgId, sc.detailEmoji)
	}

	// The explicit inspection path: click the ❤️ badge chip the pick just
	// painted. The detail opens directly below the entry, optimistically
	// naming my own pick.
	msg, ok := sc.msgById("m1")
	if !ok {
		t.Fatal("message missing from history")
	}
	x := badgeHitX(&sc, msg, reactionEmojis[1])
	if x < 0 {
		t.Fatal("picked emoji chip not painted in the badge")
	}
	badgeY := msgLastY(&sc, "m1")
	sc.handleMouse(mouseAt(x, badgeY))
	if sc.detailMsgId != "m1" || sc.detailEmoji != reactionEmojis[1] {
		t.Fatalf("badge click must open detail for m1/%s, got %q/%q", reactionEmojis[1], sc.detailMsgId, sc.detailEmoji)
	}
	if y := sc.reactionDetailY(sc.layoutFor()); y != badgeY+1 {
		t.Fatalf("detail first row = %d; want %d (directly below the badge)", y, badgeY+1)
	}
	// Optimistic fallback: my own pick is the only reactor known so far.
	painted := stripANSI(strings.Join(detailCardText(&sc), "\n"))
	if !strings.Contains(painted, "❤️ 1") || !strings.Contains(painted, "• me") {
		t.Fatalf("optimistic card must name my pick with its tally, painted:\n%s", painted)
	}
	if y := sc.reactionDetailY(sc.layoutFor()); y < 0 {
		t.Fatal("detail not painted inside the viewport")
	}

	// Server truth lands: the breakdown replaces the fallback wholesale and
	// the detail stays open (reconciled, not dismissed).
	peer := &signalClient{serverURL: srv.URL, key: "123456", me: "alice", id: ids["alice"]}
	if err := peer.react("m1", "❤️"); err != nil {
		t.Fatalf("peer react: %v", err)
	}
	summaries, err := sc.sig.reactions([]string{"m1"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(summaries) != 1 || len(summaries[0].Details) != 1 || summaries[0].Details[0].Emoji != "❤️" {
		t.Fatalf("wire must carry the breakdown, got %+v", summaries)
	}
	if names := summaries[0].Details[0].Usernames; len(names) != 2 || names[0] != "alice" || names[1] != "me" {
		t.Fatalf("breakdown names = %v; want [alice me]", names)
	}
	sc.applyFetchedReactions([]string{"m1"}, summaries)
	sc.rebuildView()
	painted = stripANSI(strings.Join(detailCardText(&sc), "\n"))
	if !strings.Contains(painted, "• alice") || !strings.Contains(painted, "• me") {
		t.Fatalf("reconciled card must name both reactors, painted:\n%s", painted)
	}
	if !strings.Contains(painted, "❤️ 2") {
		t.Fatalf("reconciled header must carry the tally, painted:\n%s", painted)
	}
	if strings.Contains(painted, "more") || strings.Contains(painted, "×") {
		t.Fatal("a known breakdown must not paint the counts fallback")
	}
	if sc.detailMsgId != "m1" {
		t.Fatal("the poll must reconcile the detail, not close it")
	}
}

func TestReactionDetailFallbackWithoutBreakdown(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	c.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "one", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, MsgId: "m2", Username: "alice", Kind: "chat", Text: "two", ConvID: generalConv})
	c.applyFetchedReactions([]string{"m1", "m2"}, []reactionSummary{
		{MsgId: "m1", Counts: map[string]int{"❤️": 2}, Mine: []string{"❤️"}},
		{MsgId: "m2", Counts: map[string]int{"👍": 1}},
	})
	c.rebuildView()

	// Old server: no breakdown. The card still opens and derives rows from
	// counts+mine — my pick is named, the rest of the tally is spelled out.
	c.openReactionDetail("m1", "❤️")
	if c.detailMsgId != "m1" {
		t.Fatal("detail must open without a breakdown")
	}
	painted := stripANSI(strings.Join(detailCardText(c), "\n"))
	if !strings.Contains(painted, "❤️ 2") || !strings.Contains(painted, "• me") ||
		!strings.Contains(painted, "+1 more") {
		t.Fatalf("fallback must name my pick and spell out the remainder, painted:\n%s", painted)
	}

	// Peer-only reaction: no name is provable, only the tally.
	c.openReactionDetail("m2", "👍")
	painted = stripANSI(strings.Join(detailCardText(c), "\n"))
	if !strings.Contains(painted, "👍 1") {
		t.Fatalf("peer-only fallback must show the count, painted:\n%s", painted)
	}
	if strings.Contains(painted, "•") || strings.Contains(painted, " : 👍") {
		t.Fatalf("no username may be invented for an unknown reactor, painted:\n%s", painted)
	}

	// An emoji with no reactors left has nothing to show: stays closed.
	c.openReactionDetail("m2", "❤️")
	if c.detailMsgId != "" {
		t.Fatal("empty detail must not open")
	}

	// A capped breakdown lists the named reactors and spells out the
	// remainder the counts still prove (server caps names per emoji).
	c.addMessage(chatMessage{Seq: 3, MsgId: "m3", Username: "alice", Kind: "chat", Text: "three", ConvID: generalConv})
	c.applyFetchedReactions([]string{"m3"}, []reactionSummary{{
		MsgId: "m3", Counts: map[string]int{"😂": 23},
		Details: []reactionDetail{{Emoji: "😂", Usernames: []string{"alice", "bob"}}},
	}})
	c.rebuildView()
	c.openReactionDetail("m3", "😂")
	painted = stripANSI(strings.Join(detailCardText(c), "\n"))
	if !strings.Contains(painted, "😂 23") || !strings.Contains(painted, "• alice") ||
		!strings.Contains(painted, "• bob") {
		t.Fatalf("capped breakdown must name every reported reactor, painted:\n%s", painted)
	}
	if !strings.Contains(painted, "+21 more") {
		t.Fatalf("capped breakdown must spell out the remainder, painted:\n%s", painted)
	}
}

// The dropdown is a rounded card: border, header chip with the tally, a
// divider, bulleted reactor rows (mine highlighted), and a faint +N more cap
// when the server breakdown is known to be partial.
func TestReactionDetailDropdownBoxRenders(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hello", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"m1"}, []reactionSummary{{
		MsgId:  "m1",
		Counts: map[string]int{"👍": 1, "❤️": 3},
		Mine:   []string{"❤️"},
		Details: []reactionDetail{
			{Emoji: "❤️", Usernames: []string{"alice", "me", "bob"}},
			{Emoji: "👍", Usernames: []string{"carol"}},
		},
	}})
	sc.rebuildView()

	msg, _ := sc.msgById("m1")
	cmd := sc.handleMouse(mouseAt(badgeHitX(&sc, msg, "❤️"), msgLastY(&sc, "m1")))
	if cmd != nil {
		t.Fatal("plain runs (animations off) must open fully expanded with no timer")
	}
	if sc.detailEmoji != "❤️" {
		t.Fatalf("badge click anchored %q; want ❤️", sc.detailEmoji)
	}
	card := detailCardText(&sc)
	if len(card) != 7 { // top border, header, divider, 3 reactors, bottom border
		t.Fatalf("card rows = %d; want 7:\n%s", len(card), strings.Join(card, "\n"))
	}
	if top := stripANSI(card[0]); !strings.Contains(top, "╭") || !strings.Contains(top, "╮") {
		t.Fatalf("card must open with a rounded top border: %q", top)
	}
	if bottom := stripANSI(card[len(card)-1]); !strings.Contains(bottom, "╰") || !strings.Contains(bottom, "╯") {
		t.Fatalf("card must close with a rounded bottom border: %q", bottom)
	}
	header := card[1]
	if !strings.Contains(stripANSI(header), "❤️ 3") {
		t.Fatalf("header must carry the emoji and tally: %q", stripANSI(header))
	}
	if seq := styleSeq(tuiUnreadStyle); seq != "" && !strings.Contains(header, seq) {
		t.Fatalf("header must wear the unread chip tone: %q", header)
	}
	if !strings.Contains(stripANSI(card[2]), "─") {
		t.Fatalf("divider missing: %q", stripANSI(card[2]))
	}

	// Reactor rows, in server order, bulleted; my own row highlighted.
	meRow := ""
	for _, r := range card[3:6] {
		text := stripANSI(r)
		switch {
		case strings.Contains(text, "• me"):
			meRow = r
		case strings.Contains(text, "• alice"), strings.Contains(text, "• bob"):
		default:
			t.Fatalf("unexpected card row: %q", text)
		}
	}
	if meRow == "" {
		t.Fatal("own reactor row missing")
	}
	seq := styleSeq(tuiPaletteSelStyle)
	if seq == "" || !strings.Contains(meRow, seq) {
		t.Fatalf("own row must take the palette highlight: %q (seq %q)", meRow, seq)
	}
	if a, b := lipgloss.Width(meRow), lipgloss.Width(header); a != b {
		t.Fatalf("highlighted row width = %d; want the full card width %d", a, b)
	}

	// A capped breakdown adds the faint "+N more" row instead of extra names.
	sc.addMessage(chatMessage{Seq: 2, MsgId: "m2", Username: "alice", Kind: "chat", Text: "big", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"m2"}, []reactionSummary{{
		MsgId: "m2", Counts: map[string]int{"😂": 23},
		Details: []reactionDetail{{Emoji: "😂", Usernames: []string{"alice", "bob"}}},
	}})
	sc.rebuildView()
	sc.openReactionDetail("m2", "😂")
	card = detailCardText(&sc)
	capRow := ""
	for _, r := range card {
		if strings.Contains(stripANSI(r), "+21 more") {
			capRow = r
		}
	}
	if capRow == "" {
		t.Fatalf("capped breakdown must paint the +N more row:\n%s", strings.Join(card, "\n"))
	}
	if seq := styleSeq(tuiPaletteHintStyle); seq != "" && !strings.Contains(capRow, seq) {
		t.Fatalf("cap row must stay faint: %q", capRow)
	}
}

// The open reveal is staged over tea.Tick frames: frame 0 paints the bare
// border, each tick cascades header/divider/rows, and the last frame equals
// the fully expanded card. Stale generations stay inert.
func TestReactionDetailDropdownAnimationFramesConverge(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.animations = true
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hello", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"m1"}, []reactionSummary{{
		MsgId: "m1", Counts: map[string]int{"❤️": 2},
		Details: []reactionDetail{{Emoji: "❤️", Usernames: []string{"alice", "bob"}}},
	}})
	sc.rebuildView()

	msg, _ := sc.msgById("m1")
	cmd := sc.handleMouse(mouseAt(badgeHitX(&sc, msg, "❤️"), msgLastY(&sc, "m1")))
	if cmd == nil {
		t.Fatal("animated open must arm the reveal tick")
	}
	if sc.detailAnim != 0 {
		t.Fatalf("open must start at frame 0, got %d", sc.detailAnim)
	}
	gen := sc.detailAnimGen
	full := len(sc.reactionDetailRows(msg, "❤️"))
	if full != 6 {
		t.Fatalf("full card = %d rows; want 6", full)
	}

	// Frame 0: the bare border box, already sized for the full card.
	height := len(detailCardText(&sc))
	if height != 3 { // top border, empty interior, bottom border
		t.Fatalf("frame 0 must paint the bare border box (3 rows), got %d", height)
	}
	if plain := stripANSI(strings.Join(detailCardText(&sc), "\n")); strings.Contains(plain, "•") ||
		strings.Contains(plain, "❤️") {
		t.Fatalf("frame 0 must not paint content yet:\n%s", plain)
	}

	last := height
	for frame := 1; frame < reactionDetailAnimFrames; frame++ {
		var next tea.Cmd
		sc, next = step(sc, reactionDetailAnimMsg{gen: gen, frame: frame})
		if sc.detailAnim != frame {
			t.Fatalf("tick %d landed on frame %d", frame, sc.detailAnim)
		}
		got := len(detailCardText(&sc))
		if got < last {
			t.Fatalf("frame %d shrank the card: %d -> %d", frame, last, got)
		}
		last = got
		if frame < reactionDetailAnimFrames-1 && next == nil {
			t.Fatalf("frame %d must re-arm the next tick", frame)
		}
		if frame == reactionDetailAnimFrames-1 && next != nil {
			t.Fatal("the final frame must not re-arm")
		}
	}
	if last != full {
		t.Fatalf("animation converged to %d rows; want the full card %d", last, full)
	}
	plain := stripANSI(strings.Join(detailCardText(&sc), "\n"))
	if !strings.Contains(plain, "❤️ 2") || !strings.Contains(plain, "• alice") || !strings.Contains(plain, "• bob") {
		t.Fatalf("converged card incomplete:\n%s", plain)
	}

	// A tick from a superseded generation never repaints.
	sc.closeReactionDetail()
	sc, _ = step(sc, reactionDetailAnimMsg{gen: gen, frame: 2})
	if sc.detailMsgId != "" || len(detailCardText(&sc)) != 0 {
		t.Fatal("a stale reveal tick must never resurrect the card")
	}
	cmd = sc.toggleReactionDetail("m1", "❤️")
	if cmd == nil {
		t.Fatal("reopen must arm a fresh tick")
	}
	if sc.detailAnimGen == gen {
		t.Fatal("reopen must bump the reveal generation")
	}
	sc, _ = step(sc, reactionDetailAnimMsg{gen: gen, frame: 2})
	if sc.detailAnim != 0 {
		t.Fatalf("a fenced tick advanced the new reveal to frame %d", sc.detailAnim)
	}
}

// UPLINK_CHAT_PLAIN=1 is the no-animation contract: the card paints fully
// expanded on open and no timer is ever armed.
func TestReactionDetailAnimationRespectsPlainMode(t *testing.T) {
	t.Setenv("UPLINK_CHAT_PLAIN", "1")
	if chatAnimationsEnabled() {
		t.Fatal("UPLINK_CHAT_PLAIN=1 must disable transient animation")
	}
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.animations = chatAnimationsEnabled() // newChatScreen wires exactly this
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hello", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"m1"}, []reactionSummary{{
		MsgId: "m1", Counts: map[string]int{"👍": 1},
		Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"alice"}}},
	}})
	sc.rebuildView()

	msg, _ := sc.msgById("m1")
	cmd := sc.handleMouse(mouseAt(badgeHitX(&sc, msg, "👍"), msgLastY(&sc, "m1")))
	if cmd != nil {
		t.Fatal("plain mode must not arm a reveal timer")
	}
	if sc.detailAnim != reactionDetailAnimFrames-1 {
		t.Fatalf("plain open must jump to the final frame, got %d", sc.detailAnim)
	}
	if got := len(detailCardText(&sc)); got != 5 {
		t.Fatalf("plain open must paint the full card, got %d rows", got)
	}
}

// A card wider than the transcript is clipped line by line: never a wrap,
// never a row wider than the viewport.
func TestReactionDetailDropdownClipsToTranscript(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(14, 6)
	c.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hi", ConvID: generalConv})
	c.applyFetchedReactions([]string{"m1"}, []reactionSummary{{
		MsgId: "m1", Counts: map[string]int{"👍": 1},
		Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"averylongreactorname"}}},
	}})
	c.rebuildView()
	c.openReactionDetail("m1", "👍")
	m, _ := c.msgById("m1")
	rows := c.reactionDetailRows(m, "👍")
	if len(rows) == 0 {
		t.Fatal("narrow card must still paint")
	}
	w := c.transcriptW()
	for _, row := range rows {
		if lipgloss.Height(row) != 1 {
			t.Fatalf("narrow card line wrapped: %q", row)
		}
		if lipgloss.Width(row) > w {
			t.Fatalf("narrow card line %d cells wide; transcript is %d: %q", lipgloss.Width(row), w, row)
		}
	}
	if mw := maxLineWidth(c.vp.View()); mw > w {
		t.Fatalf("painted viewport widened to %d; transcript is %d", mw, w)
	}
}

func TestReactionDetailClosesOnEscScrollAndPickAway(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(40, 8)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 90, Height: 20})
	sc := m.(chatScreen)
	for i := 0; i < 30; i++ {
		sc.addMessage(chatMessage{
			Seq: i + 1, MsgId: fmt.Sprintf("m%d", i+1), Username: "alice", Kind: "chat",
			Text: fmt.Sprintf("scroll %d", i), ConvID: generalConv,
		})
	}
	sc.applyFetchedReactions([]string{"m1", "m2", "m29", "m30"}, []reactionSummary{
		{MsgId: "m1", Counts: map[string]int{"👍": 1}, Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"alice"}}}},
		{MsgId: "m2", Counts: map[string]int{"👍": 1}, Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"alice"}}}},
		{MsgId: "m29", Counts: map[string]int{"👍": 1}},
		{MsgId: "m30", Counts: map[string]int{"👍": 1}},
	})
	sc.rebuildView()
	sc.vp.GotoBottom()
	openDetail := func(s chatScreen, id string) chatScreen {
		s.openReactionDetail(id, "👍")
		if s.detailMsgId != id {
			t.Fatalf("detail did not open for %s", id)
		}
		if s.reactionDetailY(s.layoutFor()) < 0 {
			t.Fatalf("detail for %s not painted inside the viewport", id)
		}
		return s
	}

	// Esc closes.
	sc = openDetail(sc, "m30")
	m2, _ := sc.Update(tea.KeyMsg{Type: tea.KeyEsc})
	sc = m2.(chatScreen)
	if sc.detailMsgId != "" || sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatal("Esc must close the detail bar")
	}

	// Wheel scroll inside the transcript closes (the row is anchored).
	sc.vp.GotoBottom()
	sc = openDetail(sc, "m30")
	l := sc.layoutFor()
	sc.handleMouse(tea.MouseMsg{Type: tea.MouseWheelUp, X: transcriptX0(l) + 2, Y: transcriptTopRow(l) + 1})
	if sc.detailMsgId != "" {
		t.Fatalf("wheel scroll must close the detail, still open on %q", sc.detailMsgId)
	}
	if sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatal("detail row still mapped after the scroll closed it")
	}

	// Keyboard scroll (transcript focus) closes too.
	sc.vp.GotoBottom()
	sc = openDetail(sc, "m30")
	sc.focus = focusTranscript
	m3, _ := sc.Update(tea.KeyMsg{Type: tea.KeyUp})
	sc = m3.(chatScreen)
	if sc.detailMsgId != "" {
		t.Fatalf("keyboard scroll must close the detail, still open on %q", sc.detailMsgId)
	}

	// Pick-away: clicking another message closes the detail and opens that
	// message's picker (still exactly one aux row).
	sc.vp.GotoBottom()
	sc = openDetail(sc, "m30")
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgTopY(&sc, "m29")))
	if sc.detailMsgId != "" {
		t.Fatal("clicking another message must drop the detail")
	}
	if sc.pendingReactionMsgId != "m29" {
		t.Fatalf("pick-away must open the other message's picker, got %q", sc.pendingReactionMsgId)
	}

	// A reveal tick armed before the dismissal must stay inert: the closed
	// card never resurrects and nothing repaints.
	sc.vp.GotoBottom()
	sc = openDetail(sc, "m30")
	gen := sc.detailAnimGen
	sc.closeReactionDetail()
	sc, _ = step(sc, reactionDetailAnimMsg{gen: gen, frame: 2})
	if sc.detailMsgId != "" || sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatal("a stale reveal tick must not resurrect a dismissed dropdown")
	}
}

func TestReactionDetailSuppressesWhenPickerOpens(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hello", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"m1"}, []reactionSummary{{
		MsgId: "m1", Counts: map[string]int{"👍": 1},
		Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"alice"}}},
	}})
	sc.rebuildView()

	// Click the badge, then open the picker on the same message: the picker
	// replaces the detail (never both).
	msg, _ := sc.msgById("m1")
	x := badgeHitX(&sc, msg, "👍")
	sc.handleMouse(mouseAt(x, msgLastY(&sc, "m1")))
	if sc.detailMsgId != "m1" {
		t.Fatal("badge click must open the detail")
	}
	sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgTopY(&sc, "m1")))
	if sc.detailMsgId != "" {
		t.Fatal("opening the picker must close the detail")
	}
	if sc.pendingReactionMsgId != "m1" {
		t.Fatalf("picker must own the aux row, got %q", sc.pendingReactionMsgId)
	}

	// Clicking the badge again while the picker is open hands the row back
	// to the detail.
	msg, _ = sc.msgById("m1")
	x = badgeHitX(&sc, msg, "👍")
	sc.handleMouse(mouseAt(x, msgLastY(&sc, "m1")))
	if sc.pendingReactionMsgId != "" {
		t.Fatal("opening the detail must close the picker")
	}
	if sc.detailMsgId != "m1" || sc.detailEmoji != "👍" {
		t.Fatalf("detail did not take the aux row, got %q/%q", sc.detailMsgId, sc.detailEmoji)
	}
}

func TestReactionDetailAlignmentGeneralAndDM(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(50, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "own", Username: "me", Kind: "chat", Text: "mine", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 2, MsgId: "peer", Username: "alice", Kind: "chat", Text: "theirs", ConvID: generalConv})
	sc.applyFetchedReactions([]string{"own", "peer"}, []reactionSummary{
		{MsgId: "own", Counts: map[string]int{"👍": 1}, Mine: []string{"👍"},
			Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"me"}}}},
		{MsgId: "peer", Counts: map[string]int{"👍": 1},
			Details: []reactionDetail{{Emoji: "👍", Usernames: []string{"alice"}}}},
	})

	ownMsg, _ := sc.msgById("own")
	peerMsg, _ := sc.msgById("peer")
	w := sc.transcriptW()
	// A one-reactor card: top border, header, divider, reactor row, bottom
	// border. Every line is single-height, so the paint height is exact.
	ownRows := sc.reactionDetailRows(ownMsg, "👍")
	if len(ownRows) != 5 {
		t.Fatalf("own card must paint border+header+divider+row+border, got %d rows: %v", len(ownRows), ownRows)
	}
	for _, row := range ownRows {
		if lipgloss.Height(row) != 1 {
			t.Fatalf("own card line must be single-height: %q", row)
		}
		if lipgloss.Width(row) != w || !strings.HasPrefix(stripANSI(row), " ") {
			t.Fatalf("own card must be right-aligned and pinned to %d: %q", w, row)
		}
	}
	peerRows := sc.reactionDetailRows(peerMsg, "👍")
	if len(peerRows) != 5 {
		t.Fatalf("peer card must paint border+header+divider+row+border, got %d rows: %v", len(peerRows), peerRows)
	}
	for _, row := range peerRows {
		if lipgloss.Height(row) != 1 || lipgloss.Width(row) >= w || strings.HasPrefix(stripANSI(row), " ") {
			t.Fatalf("peer card must be left-aligned, compact, single-height: %q", row)
		}
	}
	if !strings.Contains(stripANSI(strings.Join(peerRows, "\n")), "• alice") {
		t.Fatalf("peer card must name the reactor: %v", peerRows)
	}

	// DM thread: identical anchoring and alignment for the thread's messages.
	sc.enterPrivate("alice")
	dm := conversationKey("me", "alice")
	sc.addMessage(chatMessage{Seq: 3, MsgId: "dm1", Username: "alice", Kind: "chat", Text: "hi", To: "me", ConvID: dm})
	sc.addMessage(chatMessage{Seq: 4, MsgId: "dm2", Username: "me", Kind: "chat", Text: "yo", To: "alice", ConvID: dm})
	sc.applyFetchedReactions([]string{"dm1", "dm2"}, []reactionSummary{
		{MsgId: "dm1", Counts: map[string]int{"❤️": 1}, Details: []reactionDetail{{Emoji: "❤️", Usernames: []string{"alice"}}}},
		{MsgId: "dm2", Counts: map[string]int{"❤️": 1}, Mine: []string{"❤️"},
			Details: []reactionDetail{{Emoji: "❤️", Usernames: []string{"me"}}}},
	})
	sc.rebuildView()
	dmPeer, _ := sc.msgById("dm1")
	dmOwn, _ := sc.msgById("dm2")
	if rows := sc.reactionDetailRows(dmPeer, "❤️"); len(rows) != 5 ||
		strings.HasPrefix(stripANSI(rows[0]), " ") {
		t.Fatalf("DM peer card must be left-aligned: %v", rows)
	}
	if rows := sc.reactionDetailRows(dmOwn, "❤️"); len(rows) != 5 || lipgloss.Width(rows[0]) != w {
		t.Fatalf("DM own card must be right-aligned: %v", rows)
	}
	sc.openReactionDetail("dm1", "❤️")
	if y := sc.reactionDetailY(sc.layoutFor()); y < 0 {
		t.Fatal("DM detail not painted inside the viewport")
	}
	if sc.detailMsgId != "dm1" {
		t.Fatalf("DM anchor resolved the wrong message: %q", sc.detailMsgId)
	}

	// Back in the room, a detail anchored to a DM message paints nothing
	// (activeConv/shouldRender filter), and vice versa.
	sc.exitPrivate()
	sc.rebuildView()
	if sc.detailLineIdx != -1 || sc.reactionDetailY(sc.layoutFor()) != -1 {
		t.Fatal("a DM detail must not paint in the room view")
	}
	painted := strings.Join(sc.lines, "\n")
	if strings.Contains(painted, "• alice") {
		t.Fatalf("room view leaked the DM detail:\n%s", painted)
	}
}

func TestReactionSummaryParsesOptionalBreakdown(t *testing.T) {
	// Canonical shape: details as {emoji, usernames} objects.
	var s reactionSummary
	if err := json.Unmarshal([]byte(`{"msgId":"m1","counts":{"❤️":2},"mine":["❤️"],`+
		`"details":[{"emoji":"❤️","usernames":["alice","me"]},{"emoji":"👍","usernames":["bob"]}]}`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Details) != 2 || s.Details[0].Emoji != "❤️" || s.Details[1].Usernames[0] != "bob" {
		t.Fatalf("canonical breakdown not parsed: %+v", s.Details)
	}

	// Missing field: counts+mine still parse, details stay unknown (nil).
	s = reactionSummary{} // a fresh slice element starts zeroed in production
	if err := json.Unmarshal([]byte(`{"msgId":"m2","counts":{"👍":1},"mine":[]}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.MsgId != "m2" || s.Counts["👍"] != 1 || s.Details != nil {
		t.Fatalf("missing breakdown must degrade to counts+mine: %+v", s)
	}

	// Tolerant aliases: `breakdown` + `users`, and the flat map form.
	s = reactionSummary{}
	if err := json.Unmarshal([]byte(`{"msgId":"m3","counts":{"👍":1},`+
		`"breakdown":[{"emoji":"👍","users":["carol"]}]}`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Details) != 1 || s.Details[0].Usernames[0] != "carol" {
		t.Fatalf("breakdown/users alias not parsed: %+v", s.Details)
	}
	s = reactionSummary{}
	if err := json.Unmarshal([]byte(`{"msgId":"m4","counts":{"👍":1},"reactors":{"👍":["dave"]}}`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Details) != 1 || s.Details[0].Usernames[0] != "dave" {
		t.Fatalf("flat reactors map not parsed: %+v", s.Details)
	}

	// Malformed breakdown leaves the count/mine payload intact.
	s = reactionSummary{}
	if err := json.Unmarshal([]byte(`{"msgId":"m5","counts":{"👍":1},"mine":["👍"],"details":"garbage"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Counts["👍"] != 1 || len(s.Mine) != 1 || len(s.Details) != 0 {
		t.Fatalf("malformed breakdown must not poison counts+mine: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// Keyboard fallback: r targets the newest message, 1-6 toggle + close
// ---------------------------------------------------------------------------

func TestReactionKeyboardFallback(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	wireTestEngine(t, c, srv, "me", "alice")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "old", Username: "alice", Kind: "chat", Text: "old", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 2, MsgId: "new", Username: "me", Kind: "chat", Text: "new", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 3, MsgId: "dm1", Username: "alice", Kind: "chat", Text: "dm", To: "me", ConvID: conversationKey("me", "alice")})
	sc.focus = focusTranscript

	// r anchors the picker above the newest message of the active view.
	m2, _ := sc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	sc2 := m2.(chatScreen)
	if sc2.pendingReactionMsgId != "new" {
		t.Fatalf("r must target the newest message, got %q", sc2.pendingReactionMsgId)
	}
	if y := sc2.reactionPickerY(sc2.layoutFor()); y != msgTopY(&sc2, "new")-1 {
		t.Fatalf("r picker row = %d; want directly above the newest entry (%d)", y, msgTopY(&sc2, "new")-1)
	}
	// Number key toggles through the fake server and closes the picker.
	m3, cmd := sc2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	sc3 := m3.(chatScreen)
	if sc3.myReactions["new"] != reactionEmojis[1] {
		t.Fatalf("key 2 must toggle %s, got %q", reactionEmojis[1], sc3.myReactions["new"])
	}
	if sc3.pendingReactionMsgId != "" {
		t.Fatalf("a keyboard pick must close the picker, still open on %q", sc3.pendingReactionMsgId)
	}
	for _, msg := range drainCmds(cmd) {
		if rd, ok := msg.(reactionDoneMsg); ok && rd.err != nil {
			t.Fatalf("keyboard reaction POST failed: %v", rd.err)
		}
	}
	fs.mu.Lock()
	saved := len(fs.reactions["123456"])
	fs.mu.Unlock()
	if saved != 1 {
		t.Fatalf("server recorded %d reactions; want 1", saved)
	}

	// r re-opens; Esc dismisses without touching the transcript.
	m4, _ := sc3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	sc4 := m4.(chatScreen)
	if sc4.pendingReactionMsgId != "new" {
		t.Fatalf("r must re-open the picker, got %q", sc4.pendingReactionMsgId)
	}
	m5, _ := sc4.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m5.(chatScreen).pendingReactionMsgId != "" {
		t.Fatal("Esc must close the anchored picker")
	}
	// r on a DM view targets that thread's newest message; a conversation
	// switch never carries the anchor across.
	sc5 := m5.(chatScreen)
	sc5.enterPrivate("alice")
	if sc5.pendingReactionMsgId != "" {
		t.Fatal("a conversation switch must drop the anchored picker")
	}
	sc5.focus = focusTranscript
	m6, _ := sc5.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if got := m6.(chatScreen).pendingReactionMsgId; got != "dm1" {
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
	_, ids := wireTestEngine(t, c, srv, "me", "alice")
	dm := conversationKey("me", "alice")
	c.addMessage(chatMessage{Seq: 1, MsgId: "g1", Username: "alice", Kind: "chat", Text: "room", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, MsgId: "d1", Username: "alice", Kind: "chat", Text: "dm", To: "me", ConvID: dm})

	peer := &signalClient{serverURL: srv.URL, key: "123456", me: "alice", id: ids["alice"]}
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
// Frame contract + geometry: the anchored picker reserves exactly its row and
// leaves the drawer slot to the "/" palette
// ---------------------------------------------------------------------------

func TestReactionPickerKeepsFrameContract(t *testing.T) {
	for _, wh := range [][2]int{{80, 24}, {100, 30}, {62, 20}, {120, 40}, {240, 50}} {
		w, h := wh[0], wh[1]
		c := newFilterScreen("me", "", "me", "alice")
		c.vp = *viewportPtr(40, 10)
		m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
		sc := m.(chatScreen)
		sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "me", Kind: "chat", Text: "hello", ConvID: generalConv})
		sc.focus = focusTranscript
		m2, _ := sc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
		sc2 := m2.(chatScreen)
		if sc2.pendingReactionMsgId != "m1" {
			t.Fatalf("w=%d h=%d: picker did not open", w, h)
		}
		// The picker consumes NO drawer rows: the palette slot stays free.
		if rows := sc2.paletteRows(); rows != 0 {
			t.Fatalf("w=%d h=%d: picker must not budget drawer rows, got %d", w, h, rows)
		}
		v := sc2.View()
		if rows := strings.Count(v, "\n") + 1; rows > h {
			t.Fatalf("w=%d h=%d: picker-open frame painted %d rows", w, h, rows)
		}
		if mw := maxLineWidth(v); mw > w {
			t.Fatalf("w=%d h=%d: picker-open frame width %d", w, h, mw)
		}
		if l := sc2.layoutFor(); l.totalRows() > h {
			t.Fatalf("w=%d h=%d: layout overflows (%d rows)", w, h, l.totalRows())
		}
	}
}

// Picker hit cells must match the painted chips (brackets and padding are
// not choices) for every item.
func TestReactionPickerHitCells(t *testing.T) {
	items := reactionBarItems()
	if len(items) != len(reactionEmojis)+1 || items[len(items)-1] != "+" {
		t.Fatalf("unexpected picker items: %v", items)
	}
	pos := 1 // past "["
	for _, item := range items {
		// The chip's own cells sit one pad cell past its start.
		got, ok := reactionCellAt(pos + 1)
		if !ok || got != item {
			t.Fatalf("cell %d = %q,%v; want %q", pos+1, got, ok, item)
		}
		if _, ok := reactionCellAt(pos); ok {
			t.Fatalf("%q's left pad at %d must not hit a choice", item, pos)
		}
		pos += lipglossWidth(item) + 2 // left pad + item + right pad
	}
	if _, ok := reactionCellAt(0); ok {
		t.Fatal("bracket offset must not hit a choice")
	}
}

// The picker floats above its message and hugs the same side of the
// transcript: right for own bubbles, left for peers — room and DM alike.
func TestReactionPickerAnchoredAlignment(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(50, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	sc := m.(chatScreen)
	sc.addMessage(chatMessage{Seq: 1, MsgId: "own", Username: "me", Kind: "chat", Text: "mine", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 2, MsgId: "peer", Username: "alice", Kind: "chat", Text: "theirs", ConvID: generalConv})

	sc.pendingReactionMsgId = "own"
	sc.rebuildView()
	ownMsg, ok := sc.reactionTarget()
	if !ok {
		t.Fatal("own target missing")
	}
	rowW := lipgloss.Width(sc.reactionPickerRow(ownMsg))
	if x0 := sc.reactionPickerX0(); x0 != sc.transcriptW()-rowW {
		t.Fatalf("own picker x0 = %d; want right-aligned %d", x0, sc.transcriptW()-rowW)
	}
	ownPaint := sc.reactionPickerView(ownMsg)
	if h := lipgloss.Height(ownPaint); h != 1 {
		t.Fatalf("own picker height = %d; want 1", h)
	}
	if gw := lipgloss.Width(ownPaint); gw != sc.transcriptW() {
		t.Fatalf("own picker width = %d; want %d (pinned to the transcript)", gw, sc.transcriptW())
	}

	sc.pendingReactionMsgId = "peer"
	sc.rebuildView()
	peerMsg, ok := sc.reactionTarget()
	if !ok {
		t.Fatal("peer target missing")
	}
	if x0 := sc.reactionPickerX0(); x0 != 0 {
		t.Fatalf("peer picker x0 = %d; want left-aligned 0", x0)
	}
	peerPaint := sc.reactionPickerView(peerMsg)
	if h := lipgloss.Height(peerPaint); h != 1 {
		t.Fatalf("peer picker height = %d; want 1", h)
	}
	if gw, rw := lipgloss.Width(peerPaint), lipgloss.Width(sc.reactionPickerRow(peerMsg)); gw != rw {
		t.Fatalf("peer picker width = %d; want unpadded %d", gw, rw)
	}

	// DM views anchor exactly the same way.
	dm := conversationKey("me", "alice")
	sc.enterPrivate("alice")
	sc.addMessage(chatMessage{Seq: 3, MsgId: "dm1", Username: "alice", Kind: "chat", Text: "hi", To: "me", ConvID: dm})
	sc.pendingReactionMsgId = "dm1"
	sc.rebuildView()
	dmMsg, ok := sc.reactionTarget()
	if !ok {
		t.Fatal("DM target missing")
	}
	if x0 := sc.reactionPickerX0(); x0 != 0 {
		t.Fatalf("DM peer picker x0 = %d; want 0", x0)
	}
	if y := sc.reactionPickerY(sc.layoutFor()); y < 0 {
		t.Fatal("DM picker not painted inside the viewport")
	}
	if got := dmMsg.ConvID; got != dm {
		t.Fatalf("DM anchor resolved the wrong conversation: %q", got)
	}
}

// The picker follows the viewport index: under a scrolled transcript it lands
// exactly on the screen row the clicked entry used to occupy.
func TestReactionPickerFollowsScroll(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(50, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	sc := m.(chatScreen)
	for i := 0; i < 40; i++ {
		who := "alice"
		if i%2 == 1 {
			who = "me"
		}
		sc.addMessage(chatMessage{
			Seq: i + 1, MsgId: fmt.Sprintf("m%d", i+1), Username: who, Kind: "chat",
			Text: fmt.Sprintf("bubble %d %s", i, strings.Repeat("x", 20)), ConvID: generalConv,
		})
	}
	sc.vp.GotoTop()
	l := sc.layoutFor()
	// First entry whose top row is visible below the pane's first row.
	target, topY := "", -1
	for row := 1; row < l.vpHeight; row++ {
		id := sc.rowMsg[sc.vp.YOffset+row]
		if id == "" || sc.rowMsg[sc.vp.YOffset+row-1] == id {
			continue
		}
		target, topY = id, transcriptTopRow(l)+row
		break
	}
	if target == "" {
		t.Fatal("no visible message entry found")
	}
	sc.handleMouse(mouseAt(transcriptX0(l)+3, topY))
	if sc.pendingReactionMsgId != target {
		t.Fatalf("click anchored %q; want %q", sc.pendingReactionMsgId, target)
	}
	// Under a scrolled transcript the picker lands exactly on the row the
	// clicked entry used to occupy, i.e. immediately above its new top row.
	if y := sc.reactionPickerY(sc.layoutFor()); y != msgTopY(&sc, target)-1 {
		t.Fatalf("picker row = %d; want %d (one row above the entry)", y, msgTopY(&sc, target)-1)
	}
}

// Scroll closes the picker instead of letting it trail its message; scrolling
// the rail (a different pane) must not.
func TestReactionPickerClosesOnScroll(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(40, 8)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 90, Height: 20})
	sc := m.(chatScreen)
	for i := 0; i < 30; i++ {
		sc.addMessage(chatMessage{
			Seq: i + 1, MsgId: fmt.Sprintf("m%d", i+1), Username: "alice", Kind: "chat",
			Text: strings.Repeat("scroll ", 6), ConvID: generalConv,
		})
	}
	sc.focus = focusTranscript
	open := func(s chatScreen) chatScreen {
		m, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
		return m.(chatScreen)
	}

	sc = open(sc)
	if sc.pendingReactionMsgId == "" {
		t.Fatal("r must open the picker")
	}
	l := sc.layoutFor()
	// Wheel up inside the transcript: the picker dismisses with the scroll.
	sc.handleMouse(tea.MouseMsg{Type: tea.MouseWheelUp, X: transcriptX0(l) + 2, Y: transcriptTopRow(l) + 1})
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("wheel scroll must close the picker, still open on %q", sc.pendingReactionMsgId)
	}
	if sc.reactionPickerY(sc.layoutFor()) != -1 {
		t.Fatal("picker row still mapped after the scroll closed it")
	}

	// Keyboard scroll (focus is on the transcript) closes it too.
	sc = open(sc)
	if sc.pendingReactionMsgId == "" {
		t.Fatal("r must reopen the picker")
	}
	m2, _ := sc.Update(tea.KeyMsg{Type: tea.KeyUp})
	sc = m2.(chatScreen)
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("keyboard scroll must close the picker, still open on %q", sc.pendingReactionMsgId)
	}

	// A rail-wheel scroll moves another pane: the anchor stays put.
	sc = open(sc)
	if sc.pendingReactionMsgId == "" {
		t.Fatal("r must reopen the picker")
	}
	sc.handleMouse(tea.MouseMsg{Type: tea.MouseWheelUp, X: 2, Y: sc.layoutFor().rosterY0 + 1})
	if sc.pendingReactionMsgId == "" {
		t.Fatal("rail scroll must leave the anchored picker open")
	}
}

// Badge and picker chips share one geometry: horizontal-only padding, always
// one row tall, double-width safe.
func TestReactionChipGeometry(t *testing.T) {
	plain := lipgloss.NewStyle()
	chip := reactionChip("👍", plain)
	if h := lipgloss.Height(chip); h != 1 {
		t.Fatalf("chip height = %d; want 1", h)
	}
	if w := lipgloss.Width(chip); w != 4 {
		t.Fatalf("chip width = %d; want 4 (pad + double-width emoji + pad)", w)
	}
	// The badge's count chip, the picker's selected/idle chips and the "+"
	// affordance all keep the same one-row geometry.
	for _, s := range []string{
		reactionChip("👍2", tuiUnreadStyle),
		reactionChip("❤️", thMsgTimeStyle),
		reactionChip("❤️", tuiPaletteSelStyle),
		reactionChip("+", tuiPaletteDescStyle),
	} {
		if h := lipgloss.Height(s); h != 1 {
			t.Fatalf("chip height = %d; want 1 (%q)", h, s)
		}
	}
	// A narrow transcript clips the anchored row instead of wrapping it into
	// a second backgrounded row.
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(12, 6)
	c.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "hi", ConvID: generalConv})
	c.pendingReactionMsgId = "m1"
	c.rebuildView()
	target, ok := c.reactionTarget()
	if !ok {
		t.Fatal("target missing")
	}
	paint := c.reactionPickerView(target)
	if h := lipgloss.Height(paint); h != 1 {
		t.Fatalf("narrow picker height = %d; want 1", h)
	}
	if w := lipgloss.Width(paint); w > c.transcriptW() {
		t.Fatalf("narrow picker width %d exceeds transcript %d", w, c.transcriptW())
	}
}
