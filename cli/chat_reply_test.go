package main

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// rightClick builds a right-button press at terminal (x, y) — the gesture
// that opens the floating reply menu.
func rightClick(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, Button: tea.MouseButtonRight, X: x, Y: y}
}

// msgFirstY is the terminal row of the FIRST painted row of a message entry
// — the quote-card row whenever the message carries a citation.
func msgFirstY(c *chatScreen, msgId string) int {
	l := c.layoutFor()
	row := c.firstRowOfMsg(msgId)
	if row < 0 {
		return -1
	}
	return transcriptTopRow(l) + row - c.vp.YOffset
}

// msgCardY is the terminal row of a message's quote-card row (the row a
// click must hit to jump), or -1 when the message has no painted card.
func msgCardY(c *chatScreen, msgId string) int {
	l := c.layoutFor()
	for row, id := range c.rowMsg {
		if id == msgId && row < len(c.rowCard) && c.rowCard[row] {
			return transcriptTopRow(l) + row - c.vp.YOffset
		}
	}
	return -1
}

// newReplyScreen builds a sized screen with the window settled (geometry
// truth live), ready for mouse/keyboard interaction.
func newReplyScreen(t *testing.T, me string) chatScreen {
	t.Helper()
	c := newFilterScreen(me, "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m.(chatScreen)
}

// ---------------------------------------------------------------------------
// Right-click menu: scope (own vs others), pin, Reply-Privately, dismissals
// ---------------------------------------------------------------------------

func TestReplyMenuScopeOwnVsOthers(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "theirs", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 2, MsgId: "m2", Username: "me", Kind: "chat", Text: "mine", ConvID: generalConv})

	// Right-click on alice's message: Reply + Reply-Privately.
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	if sc.replyMenu == nil {
		t.Fatal("right-click on a peer message must open the reply menu")
	}
	if got := strings.Join(sc.replyMenu.items, "|"); got != "Reply|Reply-Privately" {
		t.Fatalf("peer menu items = %q; want Reply|Reply-Privately", got)
	}
	if sc.replyMenu.msgId != "m1" || sc.replyMenu.sel != 0 {
		t.Fatalf("menu anchored wrong: msgId=%q sel=%d", sc.replyMenu.msgId, sc.replyMenu.sel)
	}

	// Own messages offer Reply only (no Reply-Privately, no self-notify).
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m2")))
	if sc.replyMenu == nil {
		t.Fatal("right-click on an own message must open the reply menu")
	}
	if got := strings.Join(sc.replyMenu.items, "|"); got != "Reply" {
		t.Fatalf("own menu items = %q; want exactly Reply", got)
	}

	// The menu paints over the frame near the click.
	out := stripANSI(sc.View())
	if !strings.Contains(out, "Reply") || strings.Contains(out, "Reply-Privately") {
		t.Fatalf("menu overlay missing/wrong:\n%s", out)
	}
}

func TestReplyMenuDismissals(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "theirs", ConvID: generalConv})

	// Esc dismisses.
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	if sc.replyMenu == nil {
		t.Fatal("setup: menu must open")
	}
	m, _ := sc.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.(chatScreen).replyMenu != nil {
		t.Fatal("Esc must dismiss the reply menu")
	}

	// Typing dismisses (and the keystroke still reaches the composer).
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	sc2, _ := step(sc, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	if sc2.replyMenu != nil {
		t.Fatal("typing must dismiss the reply menu")
	}
	if sc2.input.Value() != "h" {
		t.Fatalf("the keystroke must fall through to the composer, got %q", sc2.input.Value())
	}

	// Click-elsewhere dismisses AND is consumed: the underlying row must
	// not react (no reaction picker opens, no focus theft).
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	sc.handleMouse(mouseAt(10, transcriptTopRow(sc.layoutFor())+1))
	if sc.replyMenu != nil {
		t.Fatal("a click outside the menu must dismiss it")
	}
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("the same click must be consumed, not open a picker on %q", sc.pendingReactionMsgId)
	}

	// Right-clicking a second message re-anchors instead of stacking.
	sc.addMessage(chatMessage{Seq: 2, MsgId: "m2", Username: "alice", Kind: "chat", Text: "next", ConvID: generalConv})
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	sc.handleMouse(rightClick(40, msgLastY(&sc, "m2")))
	if sc.replyMenu == nil || sc.replyMenu.msgId != "m2" {
		t.Fatalf("second right-click must re-anchor the menu to m2, got %+v", sc.replyMenu)
	}
}

func TestReplyMenuReplyPinsQuoteCard(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat",
		Text: "a fairly long line\nwith a second line", ConvID: generalConv})

	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	// Click the first item row (inside the panel, off the border).
	sc.handleMouse(mouseAt(sc.replyMenu.x+3, sc.replyMenu.y+1))
	if sc.replyMenu != nil {
		t.Fatal("a pick must close the menu")
	}
	if sc.composerQuote == nil {
		t.Fatal("Reply must pin the composer quote card")
	}
	q := *sc.composerQuote
	if q.ReplyTo != "m1" || q.ReplyAuthor != "alice" {
		t.Fatalf("quote = %+v; want ReplyTo m1 / author alice", q)
	}
	if q.ReplyExcerpt != "a fairly long line" {
		t.Fatalf("excerpt must be the FIRST line only, got %q", q.ReplyExcerpt)
	}

	// The card paints above the composer with the X dismiss affordance.
	out := stripANSI(sc.View())
	if !strings.Contains(out, "Reply to alice") || !strings.Contains(out, "✕") {
		t.Fatalf("composer quote card missing:\n%s", out)
	}
	if rows := strings.Count(sc.View(), "\n") + 1; rows != 30 {
		t.Fatalf("quote card must keep the exact frame: painted %d rows, want 30", rows)
	}

	// Layout budget: the card consumes one row from the transcript.
	l := sc.layoutFor()
	if l.quoteRows != 1 {
		t.Fatalf("quoteRows = %d; want 1", l.quoteRows)
	}
	if l.totalRows() != 30 {
		t.Fatalf("layout totalRows = %d; want exactly 30", l.totalRows())
	}

	// The X at the card's right edge clears it.
	x0 := transcriptX0(l) + l.vpWidth + transcriptBorder - 1
	sc.handleMouse(mouseAt(x0, composerTopRows(l)-1))
	if sc.composerQuote != nil {
		t.Fatal("X click must clear the pinned quote")
	}
	if sc.layoutFor().quoteRows != 0 {
		t.Fatal("clearing the quote must release the layout row")
	}

	// A click on the card body (not the X) keeps the quote and focuses the
	// composer.
	sc.pinComposerQuote(chatMessage{MsgId: "m1", Username: "alice", Text: "again"})
	sc.handleMouse(mouseAt(20, composerTopRows(sc.layoutFor())-1))
	if sc.composerQuote == nil {
		t.Fatal("a body click must not clear the quote")
	}
	if sc.focus != focusComposer {
		t.Fatal("a body click must focus the composer")
	}
}

func TestReplyMenuReplyPrivatelyCarriesQuote(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "secret", ConvID: generalConv})

	sc.handleMouse(rightClick(40, msgLastY(&sc, "m1")))
	sc.handleMouse(mouseAt(sc.replyMenu.x+3, sc.replyMenu.y+2)) // second item
	if sc.menuOpenForTest() {
		t.Fatal("a pick must close the menu")
	}
	if sc.targetUser != "alice" {
		t.Fatalf("Reply-Privately must open the DM with alice, got target %q", sc.targetUser)
	}
	if sc.composerQuote == nil || sc.composerQuote.ReplyTo != "m1" {
		t.Fatalf("the quote card must ride into the DM composer: %+v", sc.composerQuote)
	}
	if !strings.Contains(stripANSI(sc.View()), "Reply to alice") {
		t.Fatal("the DM composer must still show the quote card")
	}

	// A DM message targets the thread's other participant.
	sc2 := newReplyScreen(t, "me")
	dm := conversationKey("me", "alice")
	sc2.enterPrivate("alice")
	sc2.addMessage(chatMessage{Seq: 1, MsgId: "d1", Username: "alice", Kind: "chat", Text: "in dm", To: "me", ConvID: dm})
	sc2.handleMouse(rightClick(40, msgLastY(&sc2, "d1")))
	if sc2.replyMenu == nil {
		t.Fatal("setup: right-click on the DM message must open the menu")
	}
	sc2.handleMouse(mouseAt(sc2.replyMenu.x+3, sc2.replyMenu.y+2))
	if sc2.targetUser != "alice" {
		t.Fatalf("DM Reply-Privately must target the thread peer, got %q", sc2.targetUser)
	}
}

// menuOpenForTest is a tiny assertion shim for the menu tests.
func (c *chatScreen) menuOpenForTest() bool { return c.replyMenu != nil }

// ---------------------------------------------------------------------------
// /reply selection mode: pointer, navigation, confirm, Esc, animation
// ---------------------------------------------------------------------------

func TestReplyPickOpensOnNewestAndNavigates(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "one", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 2, MsgId: "m2", Username: "alice", Kind: "chat", Text: "two", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 3, MsgId: "m3", Username: "me", Kind: "chat", Text: "three", ConvID: generalConv})

	if cmd := sc.runCommand("/reply", ""); cmd != nil {
		t.Fatal("plain runs must not arm a pointer timer")
	}
	if sc.replyPick == nil {
		t.Fatal("/reply must open selection mode")
	}
	if sc.replyPick.target != "m3" {
		t.Fatalf("default target = %q; want the newest message m3", sc.replyPick.target)
	}
	if sc.replyPick.anim != replyPickAnimFrames-1 {
		t.Fatalf("plain run must paint the settled pointer, anim = %d", sc.replyPick.anim)
	}
	if !strings.Contains(stripANSI(strings.Join(sc.lines, "\n")), "<") {
		t.Fatal("the pointer must paint beside the selected message")
	}

	// Up walks toward the oldest; Down toward the newest; both wrap.
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyUp})
	if sc.replyPick.target != "m2" {
		t.Fatalf("Up must move to m2, got %q", sc.replyPick.target)
	}
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyUp})
	if sc.replyPick.target != "m1" {
		t.Fatalf("Up must move to m1, got %q", sc.replyPick.target)
	}
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyUp})
	if sc.replyPick.target != "m3" {
		t.Fatalf("Up at the oldest must wrap to m3, got %q", sc.replyPick.target)
	}
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyDown})
	if sc.replyPick.target != "m1" {
		t.Fatalf("Down at the newest must wrap to m1, got %q", sc.replyPick.target)
	}

	// Enter on the pointed message opens the SAME menu (own message: Reply
	// only), anchored at the pointer's row.
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	if sc.replyPick != nil {
		t.Fatal("confirming must close selection mode")
	}
	if sc.replyMenu == nil || sc.replyMenu.msgId != "m1" {
		t.Fatalf("Enter must open the reply menu on the pointed message, got %+v", sc.replyMenu)
	}
	if got := strings.Join(sc.replyMenu.items, "|"); got != "Reply|Reply-Privately" {
		t.Fatalf("menu items = %q; want both options for a peer message", got)
	}

	// Esc from the menu, then Esc from a reopened pick, walk back cleanly.
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyEsc})
	if sc.replyMenu != nil {
		t.Fatal("Esc must dismiss the reply menu")
	}
	sc.runCommand("/reply", "")
	if sc.replyPick == nil {
		t.Fatal("setup: selection mode must reopen")
	}
	sc, _ = step(sc, tea.KeyMsg{Type: tea.KeyEsc})
	if sc.replyPick != nil {
		t.Fatal("Esc must exit selection mode")
	}
}

func TestReplyPickTypingExitsAndTypes(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "one", ConvID: generalConv})
	sc.runCommand("/reply", "")
	sc2, _ := step(sc, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if sc2.replyPick != nil {
		t.Fatal("typing must exit selection mode")
	}
	if sc2.input.Value() != "x" {
		t.Fatalf("the keystroke must still reach the composer, got %q", sc2.input.Value())
	}
}

func TestReplyPickPointerAnimationFramesConverge(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.animations = true
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m1", Username: "alice", Kind: "chat", Text: "one", ConvID: generalConv})

	cmd := sc.runCommand("/reply", "")
	if cmd == nil {
		t.Fatal("animated open must arm the pointer reveal tick")
	}
	if sc.replyPick.anim != 0 {
		t.Fatalf("open must start at frame 0, got %d", sc.replyPick.anim)
	}
	gen := sc.replyPick.gen
	for frame := 1; frame < replyPickAnimFrames; frame++ {
		var next tea.Cmd
		sc, next = step(sc, replyPickAnimMsg{gen: gen, frame: frame})
		if sc.replyPick.anim != frame {
			t.Fatalf("tick %d landed on frame %d", frame, sc.replyPick.anim)
		}
		if frame < replyPickAnimFrames-1 && next == nil {
			t.Fatalf("frame %d must re-arm the next tick", frame)
		}
		if frame == replyPickAnimFrames-1 && next != nil {
			t.Fatal("the final frame must not re-arm")
		}
	}
	// The settled pointer "<" sits beside the message (paint proof).
	plain := stripANSI(strings.Join(sc.lines, "\n"))
	if !strings.Contains(plain, "<") {
		t.Fatalf("settled pointer missing:\n%s", plain)
	}

	// A tick from a superseded session never repaints.
	sc.closeReplyPick()
	sc, _ = step(sc, replyPickAnimMsg{gen: gen, frame: 2})
	if sc.replyPick != nil {
		t.Fatal("a stale pointer tick must never resurrect the mode")
	}
}

// ---------------------------------------------------------------------------
// Protocol: quote frame round-trip, old-client compat, receipt into history
// ---------------------------------------------------------------------------

func TestReplyQuoteFrameRoundTripAndOldCompat(t *testing.T) {
	f := newFrame(frameChat, "mid1", "alice", "")
	f.Data = "hello"
	f.ReplyTo, f.ReplyAuthor, f.ReplyExcerpt = "orig1", "bob", "snippet"
	raw, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReplyTo != "orig1" || got.ReplyAuthor != "bob" || got.ReplyExcerpt != "snippet" {
		t.Fatalf("quote round-trip lost fields: %+v", got)
	}

	// Old-message compat: a quote-less frame decodes to an empty quote.
	old := `{"v":1,"type":"chat","msgId":"mid2","from":"alice","to":"","data":"plain"}`
	g2, err := decodeFrame([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if g2.ReplyTo != "" || g2.ReplyAuthor != "" || g2.ReplyExcerpt != "" {
		t.Fatalf("old frame must decode to an empty quote: %+v", g2)
	}
	// ...and plain frames still encode without the optional keys.
	plainRaw, _ := encodeFrame(newFrame(frameChat, "mid3", "alice", ""))
	if strings.Contains(string(plainRaw), "replyTo") {
		t.Fatalf("plain frames must omit quote keys: %s", plainRaw)
	}

	// Our NEW frames stay readable by an OLD-shaped decoder: unknown
	// fields are ignored by encoding/json.
	var legacy struct {
		V    int    `json:"v"`
		Type string `json:"type"`
		Msg  string `json:"msgId"`
		From string `json:"from"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("a legacy decoder must ignore the quote fields: %v", err)
	}
	if legacy.From != "alice" || legacy.Data != "hello" {
		t.Fatalf("legacy decode lost base fields: %+v", legacy)
	}
}

func TestReplyReceiptCarriesQuoteIntoHistory(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc2, _ := step(sc, netChatMsg{chat: engineChat{
		MsgId: "n1", From: "alice", To: "", Text: "hi there",
		ReplyTo: "orig1", ReplyAuthor: "me", ReplyExcerpt: "my words",
	}})
	if len(sc2.history) != 1 {
		t.Fatalf("received message missing from history: %+v", sc2.history)
	}
	h := sc2.history[0]
	if h.ReplyTo != "orig1" || h.ReplyAuthor != "me" || h.ReplyExcerpt != "my words" {
		t.Fatalf("receipt must carry the quote into chatMessage: %+v", h)
	}
	// The bubble render shows the citation card above the text.
	plain := stripANSI(strings.Join(sc2.lines, "\n"))
	if i, j := strings.Index(plain, "my words"), strings.Index(plain, "hi there"); i < 0 || j < 0 || i > j {
		t.Fatalf("quote card must paint above the message text:\n%s", plain)
	}
}

func TestReplySendConsumesQuoteAndSettlesCard(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "orig1", Username: "alice", Kind: "chat", Text: "my words", ConvID: generalConv})
	q := sc.composerQuoteFromPinForTest()

	cmd := sc.dispatchInConv(generalConv, "hello back")
	if cmd == nil {
		t.Fatal("dispatch must return the send command")
	}
	if sc.composerQuote != nil {
		t.Fatal("sending must consume the pinned citation (WhatsApp-style)")
	}
	if sc.pending == nil || sc.pending.text != "hello back" {
		t.Fatalf("pending send lost: %+v", sc.pending)
	}
	// The optimistic echo paints the citation card.
	plain := stripANSI(strings.Join(sc.lines, "\n"))
	if !strings.Contains(plain, "▎") || !strings.Contains(plain, "my words") {
		t.Fatalf("echo must paint the quote card:\n%s", plain)
	}

	sc.settleSend(sendDoneMsg{text: "hello back", seq: 2, code: 201, quote: q})
	if len(sc.history) < 2 {
		t.Fatalf("confirmed message missing: %+v", sc.history)
	}
	h := sc.history[len(sc.history)-1]
	if h.ReplyTo != "orig1" || h.ReplyAuthor != "alice" || h.ReplyExcerpt != "my words" {
		t.Fatalf("settled history must own the quote: %+v", h)
	}
	// Confirmed paint: card above text, one copy only.
	painted := stripANSI(strings.Join(sc.lines, "\n"))
	if n := strings.Count(painted, "hello back"); n != 1 {
		t.Fatalf("confirmed message must paint exactly once, got %d:\n%s", n, painted)
	}
	if i, j := strings.Index(painted, "my words"), strings.Index(painted, "hello back"); i < 0 || i > j {
		t.Fatalf("card must sit above the confirmed text:\n%s", painted)
	}
}

// composerQuoteFromPinForTest pins a quote from the seeded message and
// returns the captured citation (the value dispatchInConv must attach).
func (c *chatScreen) composerQuoteFromPinForTest() chatQuote {
	m, ok := c.msgById("orig1")
	if !ok {
		panic("setup: orig1 must exist")
	}
	c.pinComposerQuote(m)
	return *c.composerQuote
}

func TestReplyQuoteCardRendersGeneralAndDM(t *testing.T) {
	// General room.
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "g1", Username: "alice", Kind: "chat",
		Text: "body text", ConvID: generalConv, ReplyTo: "g0", ReplyAuthor: "bob", ReplyExcerpt: "cited words"})
	plain := stripANSI(strings.Join(sc.lines, "\n"))
	if !strings.Contains(plain, "▎") || !strings.Contains(plain, "bob") || !strings.Contains(plain, "cited words") {
		t.Fatalf("general bubble missing its quote card:\n%s", plain)
	}
	if i, j := strings.Index(plain, "cited words"), strings.Index(plain, "body text"); i < 0 || i > j {
		t.Fatalf("card must paint above the general message text:\n%s", plain)
	}

	// DM thread.
	sc2 := newReplyScreen(t, "me")
	dm := conversationKey("me", "alice")
	sc2.enterPrivate("alice")
	sc2.addMessage(chatMessage{Seq: 1, MsgId: "d1", Username: "alice", Kind: "chat",
		Text: "thread body", To: "me", ConvID: dm, ReplyTo: "d0", ReplyAuthor: "me", ReplyExcerpt: "my old words"})
	plain2 := stripANSI(strings.Join(sc2.lines, "\n"))
	if !strings.Contains(plain2, "▎") || !strings.Contains(plain2, "my old words") {
		t.Fatalf("DM bubble missing its quote card:\n%s", plain2)
	}
	if i, j := strings.Index(plain2, "my old words"), strings.Index(plain2, "thread body"); i < 0 || i > j {
		t.Fatalf("card must paint above the DM message text:\n%s", plain2)
	}

	// A plain message paints NO card (no stray bars).
	sc3 := newReplyScreen(t, "me")
	sc3.addMessage(chatMessage{Seq: 1, MsgId: "p1", Username: "alice", Kind: "chat", Text: "plain", ConvID: generalConv})
	if strings.Contains(stripANSI(strings.Join(sc3.lines, "\n")), "▎") {
		t.Fatal("a plain message must not paint a quote card")
	}
}

// ---------------------------------------------------------------------------
// Notify: exact body, only for the quoted author, self-replies silent
// ---------------------------------------------------------------------------

func TestReplyNotifyExactBodyAndSelfSilence(t *testing.T) {
	var pings []string
	prev := notifyReplyHandler
	notifyReplyHandler = func(from string) { pings = append(pings, from) }
	defer func() { notifyReplyHandler = prev }()

	sc := newReplyScreen(t, "me")

	// alice replies to MY message: ping "alice" (quoted author = me).
	sc, _ = step(sc, netChatMsg{chat: engineChat{MsgId: "r1", From: "alice", Text: "hi",
		ReplyTo: "mine1", ReplyAuthor: "me", ReplyExcerpt: "my words"}})
	if len(pings) != 1 || pings[0] != "alice" {
		t.Fatalf("ping = %v; want exactly [alice]", pings)
	}

	// A reply to someone else's message pings nobody here.
	sc, _ = step(sc, netChatMsg{chat: engineChat{MsgId: "r2", From: "carol", Text: "hey",
		ReplyTo: "bobs1", ReplyAuthor: "bob", ReplyExcerpt: "bob words"}})
	if len(pings) != 1 {
		t.Fatalf("a quote of a third party must stay silent, got %v", pings)
	}

	// Self-reply (sender quoting their own message): never a ping.
	sc, _ = step(sc, netChatMsg{chat: engineChat{MsgId: "r3", From: "carol", Text: "meh",
		ReplyTo: "carol1", ReplyAuthor: "carol", ReplyExcerpt: "own words"}})
	if len(pings) != 1 {
		t.Fatalf("a self-reply must never notify, got %v", pings)
	}

	// Plain messages never ping.
	sc, _ = step(sc, netChatMsg{chat: engineChat{MsgId: "r4", From: "dave", Text: "plain"}})
	if len(pings) != 1 {
		t.Fatalf("a plain message must never notify, got %v", pings)
	}

	// EXACT desktop body: "<from> replied to you" via the beeep entry point.
	var bodies, titles []string
	prevBeeep := beeepNotify
	beeepNotify = func(title, body string, icon any) error {
		titles = append(titles, title)
		bodies = append(bodies, body)
		return nil
	}
	defer func() { beeepNotify = prevBeeep }()
	notifyReplyTo("alice")
	if len(bodies) != 1 || bodies[0] != "alice replied to you" {
		t.Fatalf("notification body = %v; want exactly [alice replied to you]", bodies)
	}
	if len(titles) != 1 || titles[0] != "Uplink-Delta" {
		t.Fatalf("notification title = %v; want Uplink-Delta", titles)
	}
}

// ---------------------------------------------------------------------------
// Quote-click jump: scroll-top, 3s highlight expiry, trimmed fallback
// ---------------------------------------------------------------------------

func TestReplyQuoteJumpScrollsAndExpires(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m0", Username: "alice", Kind: "chat", Text: "the quoted original", ConvID: generalConv})
	// Fill the viewport so m0 scrolls out of view.
	for i := 0; i < 30; i++ {
		sc.addMessage(chatMessage{Seq: 100 + i, MsgId: "fill" + itoa(i), Username: "alice", Kind: "chat", Text: "filler", ConvID: generalConv})
	}

	// The replying message cites m0; its card row is on screen.
	sc.addMessage(chatMessage{Seq: 99, MsgId: "rep1", Username: "alice", Kind: "chat", Text: "replying",
		ConvID: generalConv, ReplyTo: "m0", ReplyAuthor: "alice", ReplyExcerpt: "the quoted original"})
	if row := sc.firstRowOfMsg("m0"); row >= sc.vp.YOffset {
		t.Fatalf("setup: m0 must be scrolled out of view (row %d, offset %d)", row, sc.vp.YOffset)
	}
	yCard := msgCardY(&sc, "rep1")
	if yCard < 0 {
		t.Fatal("setup: the replying message's card must be on screen")
	}

	cmd := sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, yCard))
	if cmd == nil {
		t.Fatal("a card click must arm the expiry tick")
	}
	if sc.quoteJump == nil || sc.quoteJump.msgId != "m0" || sc.quoteJump.left != quoteJumpTicks {
		t.Fatalf("jump state = %+v; want m0 / %d ticks", sc.quoteJump, quoteJumpTicks)
	}
	// The quoted message lands at the TOP of the viewport.
	if row := sc.firstRowOfMsg("m0"); sc.vp.YOffset != row || row != 0 {
		t.Fatalf("m0 must sit at the viewport top: row %d, offset %d", row, sc.vp.YOffset)
	}
	if sc.rowMsg[sc.vp.YOffset] != "m0" {
		t.Fatal("viewport top must be the quoted message")
	}
	// The jump closed the picker and left a fresh generation.
	gen := sc.quoteJump.gen

	// The ENTIRE row is blue: the group header now carries a background.
	hdr := sc.renderSender(sc.history[0])
	jhdr := sc.renderSenderJump(sc.history[0])
	if strings.Contains(hdr, "\x1b[48;5;") {
		t.Fatal("the plain header must not carry a background")
	}
	if !strings.Contains(jhdr, "\x1b[48;5;") {
		t.Fatalf("the jumped header must paint a blue background: %q", jhdr)
	}
	if plainH, plainJ := strings.TrimSpace(stripANSI(hdr)), strings.TrimSpace(stripANSI(jhdr)); plainH != plainJ {
		t.Fatalf("the highlight must not change the text: %q vs %q", plainH, plainJ)
	}

	// 3s expiry: ticks 1..2 keep the highlight, tick 3 clears it.
	for i := 1; i < quoteJumpTicks; i++ {
		sc, _ = step(sc, quoteJumpTickMsg{gen: gen})
		if sc.quoteJump == nil {
			t.Fatalf("highlight expired after %d/%d ticks", i, quoteJumpTicks)
		}
	}
	sc, next := step(sc, quoteJumpTickMsg{gen: gen})
	if sc.quoteJump != nil {
		t.Fatal("the final tick must clear the highlight")
	}
	if next != nil {
		t.Fatal("the final tick must not re-arm")
	}
	if strings.Contains(sc.renderSender(sc.history[0]), "\x1b[48;5;") {
		t.Fatal("after expiry the header must paint unhighlighted")
	}

	// A stale tick from the expired jump is a no-op.
	sc, _ = step(sc, quoteJumpTickMsg{gen: gen})
	if sc.quoteJump != nil {
		t.Fatal("a stale expiry tick must never resurrect the highlight")
	}

	// Re-jump bumps the generation: an old tick can't shorten the new one.
	sc.tableForTestResetScroll()
	cmd = sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgCardY(&sc, "rep1")))
	if cmd == nil {
		t.Fatal("re-jump must arm a fresh expiry tick")
	}
	gen2 := sc.quoteJump.gen
	if gen2 == gen {
		t.Fatal("re-jump must bump the generation")
	}
	sc, _ = step(sc, quoteJumpTickMsg{gen: gen})
	if sc.quoteJump == nil || sc.quoteJump.gen != gen2 {
		t.Fatal("a fenced tick must not touch the new jump")
	}
}

// tableForTestResetScroll scrolls the jump target back into view so a test
// can re-click its card.
func (c *chatScreen) tableForTestResetScroll() {
	c.vp.GotoBottom()
	if row := c.firstRowOfMsg("rep1"); row >= 0 && (row < c.vp.YOffset || row >= c.vp.YOffset+c.vp.Height) {
		c.vp.SetYOffset(row)
	}
}

func TestReplyQuoteJumpTrimmedFallback(t *testing.T) {
	sc := newReplyScreen(t, "me")
	// The cited original fell off history — only the reply survives.
	sc.addMessage(chatMessage{Seq: 2, MsgId: "rep1", Username: "alice", Kind: "chat", Text: "replying",
		ConvID: generalConv, ReplyTo: "ghost0", ReplyAuthor: "alice", ReplyExcerpt: "long gone"})
	yCard := msgCardY(&sc, "rep1")
	if yCard < 0 {
		t.Fatal("setup: card must be on screen")
	}
	cmd := sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, yCard))
	if cmd != nil {
		t.Fatal("a trimmed jump must not arm any tick")
	}
	if sc.quoteJump != nil {
		t.Fatal("a trimmed jump must not highlight anything")
	}
	plain := stripANSI(strings.Join(sc.lines, "\n"))
	if !strings.Contains(plain, "original message no longer in view") {
		t.Fatalf("the inline note is missing:\n%s", plain)
	}
}

func TestReplyQuoteCardClickDoesNotOpenReactionPicker(t *testing.T) {
	sc := newReplyScreen(t, "me")
	sc.addMessage(chatMessage{Seq: 1, MsgId: "m0", Username: "alice", Kind: "chat", Text: "original", ConvID: generalConv})
	sc.addMessage(chatMessage{Seq: 2, MsgId: "rep1", Username: "alice", Kind: "chat", Text: "replying",
		ConvID: generalConv, ReplyTo: "m0", ReplyAuthor: "alice", ReplyExcerpt: "original"})
	cmd := sc.handleMouse(mouseAt(transcriptX0(sc.layoutFor())+3, msgCardY(&sc, "rep1")))
	if cmd == nil {
		t.Fatal("card click must jump")
	}
	if sc.pendingReactionMsgId != "" {
		t.Fatalf("card click must not open the reaction picker on %q", sc.pendingReactionMsgId)
	}
	if sc.quoteJump == nil || sc.quoteJump.msgId != "m0" {
		t.Fatal("card click must jump to the quoted message")
	}

	// A click on the message TEXT (not the card) keeps the old meaning:
	// the reaction picker opens.
	sc.vp.GotoBottom()
	yText := -1
	l := sc.layoutFor()
	for row := range sc.rowMsg {
		if sc.rowMsg[row] == "rep1" && !sc.rowCard[row] {
			yText = transcriptTopRow(l) + row - sc.vp.YOffset
			break
		}
	}
	if yText < 0 {
		t.Fatal("setup: a text row of rep1 must be on screen")
	}
	sc.closeReplyMenu()
	sc.handleMouse(mouseAt(transcriptX0(l)+3, yText))
	if sc.pendingReactionMsgId != "rep1" {
		t.Fatalf("a text-row click must open the picker on rep1, got %q", sc.pendingReactionMsgId)
	}
}

// ---------------------------------------------------------------------------
// Helpers for the reply tests
// ---------------------------------------------------------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
