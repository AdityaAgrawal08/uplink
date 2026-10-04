package main

// chat_mention_test.go — @mention receipt parsing, the composer dropdown,
// the desktop ping, and the in-bubble highlight (receipts: general-room
// only; highlight: every bubble, DMs included).

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ---------------------------------------------------------------------------
// Receipt parsing
// ---------------------------------------------------------------------------

func TestParseMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		me   string
		want []string
	}{
		{"plain text has no mentions", "hello world", "me", nil},
		{"empty text", "", "me", nil},
		{"single mention", "hi @alice", "me", []string{"alice"}},
		{"mention at start", "@alice hi", "me", []string{"alice"}},
		{"mention at end of message", "ping @alice", "me", []string{"alice"}},
		{"multi-mention each counts", "see @bob and @carol and @bob again", "me", []string{"bob", "carol", "bob"}},
		{"punctuation terminates", "hey @alice! what about (@bob)?", "me", []string{"alice", "bob"}},
		{"comma terminates", "thanks @alice, noted", "me", []string{"alice"}},
		{"invalid charset breaks the token", "mail @al-ice", "me", []string{"al"}},
		{"bare at alone", "just @ now", "me", nil},
		{"at followed by space", "just @ nobody", "me", nil},
		{"at at end with nothing after", "ping @", "me", nil},
		{"self ignored", "hey @me", "me", nil},
		{"self ignored among others", "@me and @bob", "me", []string{"bob"}},
		{"self ignored twice", "@me @me", "me", nil},
		{"case is preserved", "hi @ALICE", "me", []string{"ALICE"}},
		{"differently cased self is not self", "hi @ME", "me", []string{"ME"}},
		{"adjacent mentions", "@alice@bob", "me", []string{"alice", "bob"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMentions(tt.text, tt.me)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("parseMentions(%q, %q) = %v; want %v", tt.text, tt.me, got, tt.want)
			}
		})
	}
}

func TestMentionedInCaseSensitiveExact(t *testing.T) {
	if !mentionedIn("hey @alice check this", "alice") {
		t.Error("exact own mention must be detected")
	}
	if mentionedIn("hey @Alice", "alice") {
		t.Error("differently cased mention must NOT match (case-sensitive)")
	}
	if mentionedIn("hey @alice2", "alice") {
		t.Error("longer identifier must NOT match the shorter name")
	}
	if mentionedIn("hey", "alice") {
		t.Error("no mention must not match")
	}
	if mentionedIn("@alice", "") {
		t.Error("empty local username must never match")
	}
}

// ---------------------------------------------------------------------------
// Composer dropdown: open/filter/close by text shape
// ---------------------------------------------------------------------------

func TestMentionDropdownOpensWithAtAndClosesWithout(t *testing.T) {
	c := *newFilterScreen("zoe", "", "alice", "bob", "carol", "dave")
	if c.mention.visible() {
		t.Fatal("dropdown must start closed")
	}

	c, _ = typeKeys(c, "@")
	if !c.mention.visible() {
		t.Fatal("typing @ must open the member dropdown in the general room")
	}

	c, _ = typeKeys(c, "bo") // "@bo"
	if !c.mention.visible() {
		t.Fatal("dropdown must stay open while a fragment is being typed")
	}

	// Candidates filter by the fragment (prefix, alphabetical).
	_, users, ok := c.mentionCandidates()
	if !ok || len(users) != 1 || users[0].Username != "bob" {
		t.Fatalf("fragment @bo must filter to bob only, got %+v ok=%v", users, ok)
	}

	// Whitespace terminates the mention: dropdown dissolves.
	c, _ = typeKeys(c, " ") // "@bo "
	if c.mention.visible() {
		t.Fatal("whitespace after the fragment must close the dropdown")
	}

	// Punctuation terminates it too (mirrors the receipt parser).
	c.input.SetValue("@bob!")
	ensurePaletteOpen(&c)
	if c.mention.visible() {
		t.Fatal("a non-username character after the fragment must close the dropdown")
	}

	// A comma after the name, then more text: closed.
	c.input.SetValue("hey @bob, check")
	ensurePaletteOpen(&c)
	if c.mention.visible() {
		t.Fatal("mention already terminated by punctuation must stay closed")
	}

	// Removing the @ shape dissolves the dropdown.
	c.input.SetValue("hello")
	ensurePaletteOpen(&c)
	if c.mention.visible() {
		t.Fatal("plain text must close the dropdown")
	}
}

func TestMentionDropdownGeneralRoomOnly(t *testing.T) {
	dm := *newFilterScreen("bob", "alice", "alice", "carol")
	dm, _ = typeKeys(dm, "@")
	if dm.mention.visible() {
		t.Fatal("no @ dropdown inside a DM thread — @ is plain text there")
	}
	if dm.paletteRows() != 0 {
		t.Fatalf("DM thread must reserve no dropdown rows, got %d", dm.paletteRows())
	}

	// "/" command shapes own the drawer: the mention dropdown stays closed.
	c := *newFilterScreen("bob", "", "alice")
	c, _ = typeKeys(c, "/kick alice")
	if c.mention.visible() {
		t.Fatal("command shapes must keep the @ dropdown closed")
	}
}

func TestMentionDropdownExcludesSelf(t *testing.T) {
	c := *newFilterScreen("bob", "", "alice", "bob", "carol")
	c, _ = typeKeys(c, "@")
	_, users, _ := c.mentionCandidates()
	for _, u := range users {
		if u.Username == "bob" {
			t.Fatal("the local user must never appear as a mention candidate")
		}
	}
}

// ---------------------------------------------------------------------------
// Composer dropdown: keyboard
// ---------------------------------------------------------------------------

func TestMentionDropdownKeyboard(t *testing.T) {
	c := *newFilterScreen("bob", "", "alice", "carol", "dave")

	// Down/Up move the highlight with wrap-around.
	c, _ = typeKeys(c, "@")
	if handled, _ := c.handleMentionKeys(tea.KeyMsg{Type: tea.KeyDown}); !handled {
		t.Fatal("down must be consumed by the open dropdown")
	}
	if c.mention.sel != 1 {
		t.Fatalf("down moved selection to %d; want 1", c.mention.sel)
	}
	if handled, _ := c.handleMentionKeys(tea.KeyMsg{Type: tea.KeyUp}); !handled {
		t.Fatal("up must be consumed by the open dropdown")
	}
	if c.mention.sel != 0 {
		t.Fatalf("up moved selection to %d; want 0", c.mention.sel)
	}

	// Tab completes "@name " into the input and closes, with the cursor
	// parked AFTER the trailing space (ready to type the message).
	if handled, _ := c.handleMentionKeys(tea.KeyMsg{Type: tea.KeyTab}); !handled {
		t.Fatal("tab must be consumed by the open dropdown")
	}
	if got := c.input.Value(); got != "@alice " {
		t.Fatalf("tab completion gave %q; want \"@alice \"", got)
	}
	if pos := c.input.Position(); pos != len("@alice ") {
		t.Fatalf("cursor after tab completion = %d; want %d (after the trailing space)", pos, len("@alice "))
	}
	if c.mention.visible() {
		t.Fatal("tab completion must close the dropdown")
	}

	// Enter completes inline too — it never sends the message.
	c.input.SetValue("say @ca")
	ensurePaletteOpen(&c)
	c2, cmd := step(c, tea.KeyMsg{Type: tea.KeyEnter})
	if got := c2.input.Value(); got != "say @carol " {
		t.Fatalf("enter completion gave %q; want \"say @carol \"", got)
	}
	if pos := c2.input.Position(); pos != len("say @carol ") {
		t.Fatalf("cursor after enter completion = %d; want %d (after the trailing space)", pos, len("say @carol "))
	}
	if c2.mention.visible() {
		t.Fatal("enter completion must close the dropdown")
	}
	if c2.pending != nil || cmd != nil {
		t.Fatal("enter must complete the mention, not send the message")
	}

	// Completion preserves text before the @ (mid-sentence mention).
	c3 := *newFilterScreen("bob", "", "alice")
	c3.input.SetValue("please tell @al")
	ensurePaletteOpen(&c3)
	c3, _ = step(c3, tea.KeyMsg{Type: tea.KeyTab})
	if got := c3.input.Value(); got != "please tell @alice " {
		t.Fatalf("mid-sentence completion gave %q; want \"please tell @alice \"", got)
	}
	if pos := c3.input.Position(); pos != len("please tell @alice ") {
		t.Fatalf("cursor after mid-sentence completion = %d; want %d", pos, len("please tell @alice "))
	}
}

func TestMentionDropdownEscDismisses(t *testing.T) {
	c := *newFilterScreen("bob", "", "alice", "carol")
	c, _ = typeKeys(c, "@")
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyEsc})
	if c.mention.visible() {
		t.Fatal("esc must dismiss the dropdown")
	}
	if c.mention.sel != 0 || c.mention.off != 0 {
		t.Fatalf("esc must reset selection state: sel=%d off=%d", c.mention.sel, c.mention.off)
	}

	// Esc dismisses even when the fragment matches nobody.
	c2 := *newFilterScreen("bob", "", "alice")
	c2, _ = typeKeys(c2, "@zzz")
	if !c2.mention.visible() {
		t.Fatal("fragment @zzz must keep the (empty) dropdown open until esc")
	}
	c2, _ = step(c2, tea.KeyMsg{Type: tea.KeyEsc})
	if c2.mention.visible() {
		t.Fatal("esc must dismiss an empty-candidate dropdown too")
	}
}

func TestMentionDropdownEmptyCandidatesPassKeysThrough(t *testing.T) {
	c := *newFilterScreen("bob", "", "alice")
	c, _ = typeKeys(c, "@zzz")

	// Enter passes through: the message sends normally.
	c2, _ := step(c, tea.KeyMsg{Type: tea.KeyEnter})
	if c2.pending == nil {
		t.Fatal("enter with no candidates must send the message")
	}
	if c2.mention.visible() {
		t.Fatal("a real send must close the dropdown")
	}
	if got := c2.input.Value(); got != "" {
		t.Fatalf("send must clear the input, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Composer dropdown: painting + layout budget
// ---------------------------------------------------------------------------

func TestMentionDropdownPaintAndBudget(t *testing.T) {
	c := *newFilterScreen("bob", "", "alice", "carol", "dave")
	c.vp = *viewportPtr(40, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	c = m.(chatScreen)
	c, _ = typeKeys(c, "@")

	l := c.layoutFor()
	panel := c.mentionView(l.vpWidth + 2)
	if panel == "" {
		t.Fatal("dropdown must paint while a mention fragment is live")
	}
	if !strings.Contains(panel, mentionFooterHints) {
		t.Fatalf("keymap footer missing from dropdown: %q", panel)
	}
	for _, name := range []string{"alice", "carol", "dave"} {
		if !strings.Contains(panel, name) {
			t.Fatalf("member %s missing from dropdown", name)
		}
	}

	view := c.View()
	lines := strings.Split(view, "\n")
	palIdx, composerIdx := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "tab/enter complete") {
			palIdx = i
		}
		if strings.Contains(ln, "❯ @") {
			composerIdx = i
		}
	}
	if composerIdx < 0 {
		t.Fatal("composer row missing from view")
	}
	if palIdx < 0 {
		t.Fatal("dropdown rows missing from view while a fragment is live")
	}
	if palIdx >= composerIdx {
		t.Fatalf("dropdown must emerge ABOVE the composer: dropdown@%d composer@%d", palIdx, composerIdx)
	}

	// Budget dissolves with the dropdown: no candidates, no rows.
	c, _ = typeKeys(c, "zzz") // "@zzz"
	if c.paletteRows() != 0 {
		t.Fatalf("empty-candidate dropdown must reserve no rows, got %d", c.paletteRows())
	}
	if got := c.mentionView(l.vpWidth + 2); got != "" {
		t.Fatal("empty-candidate dropdown must paint nothing")
	}

	// DM threads: no dropdown rows either.
	dm := *newFilterScreen("bob", "alice", "alice")
	dm.vp = *viewportPtr(40, 10)
	dm, _ = step(dm, tea.WindowSizeMsg{Width: 80, Height: 24})
	dm, _ = typeKeys(dm, "@")
	if dm.paletteRows() != 0 {
		t.Fatalf("DM thread must reserve no dropdown rows, got %d", dm.paletteRows())
	}
}

func TestMentionDropdownClosesOnModeSwitches(t *testing.T) {
	c := *newFilterScreen("bob", "", "alice", "carol")
	c, _ = typeKeys(c, "@")
	if !c.mention.visible() {
		t.Fatal("precondition: dropdown open")
	}

	// Conversation switch (side rail / keyboard): dropdown goes with it.
	c.enterPrivate("alice")
	if c.mention.visible() {
		t.Fatal("entering a DM thread must close the dropdown")
	}

	c, _ = typeKeys(c, "@")
	c.exitPrivate()
	if c.mention.visible() {
		t.Fatal("leaving the room must not resurrect the dropdown")
	}

	// Picker open (/upload, /download): one drawer slot, dropdown yields.
	c, _ = typeKeys(c, "@")
	c.openPicker()
	if c.mention.visible() {
		t.Fatal("opening the file browser must close the dropdown")
	}

	c, _ = typeKeys(c, "@")
	c.openFilesDrawer()
	if c.mention.visible() {
		t.Fatal("opening the files drawer must close the dropdown")
	}
}

// ---------------------------------------------------------------------------
// Notification: ping on mention (general only), never for own/DM/no-mention
// ---------------------------------------------------------------------------

func TestMentionNotifyTriggers(t *testing.T) {
	var pings []string
	orig := mentionNotifier
	mentionNotifier = func(sender string) { pings = append(pings, sender) }
	defer func() { mentionNotifier = orig }()

	c := newFilterScreen("alice", "")
	c.handleNewMessage(chatMessage{
		Seq: 1, MsgId: "m1", Username: "bob", Kind: "chat",
		Text: "hey @alice check this", ConvID: generalConv, CreatedAt: "2026-10-04T10:00:00Z",
	})
	if len(pings) != 1 {
		t.Fatalf("mention must ping exactly once, got %v", pings)
	}
	if pings[0] != "bob" {
		t.Fatalf("ping sender %q; want bob", pings[0])
	}

	// A second message mentioning us pings again.
	c.handleNewMessage(chatMessage{
		Seq: 2, MsgId: "m2", Username: "carol", Kind: "chat",
		Text: "also @alice", ConvID: generalConv, CreatedAt: "2026-10-04T10:01:00Z",
	})
	if len(pings) != 2 {
		t.Fatalf("second mention must ping again, got %d pings", len(pings))
	}

	// Multiple mentions of us in ONE message still ping once per message.
	c.handleNewMessage(chatMessage{
		Seq: 3, MsgId: "m3", Username: "dave", Kind: "chat",
		Text: "@alice and again @alice", ConvID: generalConv, CreatedAt: "2026-10-04T10:02:00Z",
	})
	if len(pings) != 3 {
		t.Fatalf("one message = one ping even with repeated mentions, got %d", len(pings))
	}
}

func TestMentionNotifyNeverPings(t *testing.T) {
	var pings []string
	orig := mentionNotifier
	mentionNotifier = func(sender string) { pings = append(pings, sender) }
	defer func() { mentionNotifier = orig }()

	c := newFilterScreen("alice", "")

	// No mention at all.
	c.handleNewMessage(chatMessage{
		Seq: 1, MsgId: "m1", Username: "bob", Kind: "chat",
		Text: "just hello", ConvID: generalConv, CreatedAt: "2026-10-04T10:00:00Z",
	})

	// Own message mentioning myself: never pings.
	c.handleNewMessage(chatMessage{
		Seq: 2, MsgId: "m2", Username: "alice", Kind: "chat",
		Text: "hi @alice everybody", ConvID: generalConv, CreatedAt: "2026-10-04T10:01:00Z",
	})

	// DM naming me: @ is plain text there, no ping.
	c.handleNewMessage(chatMessage{
		Seq: 3, MsgId: "m3", Username: "bob", Kind: "chat",
		Text: "private @alice stuff", ConvID: conversationKey("alice", "bob"),
		CreatedAt: "2026-10-04T10:02:00Z",
	})

	// Legacy empty ConvID is still the general room: pings.
	c.handleNewMessage(chatMessage{
		Seq: 4, MsgId: "m4", Username: "carol", Kind: "chat",
		Text: "legacy @alice", CreatedAt: "2026-10-04T10:03:00Z",
	})

	// Case-sensitive: @Alice does not name alice.
	c.handleNewMessage(chatMessage{
		Seq: 5, MsgId: "m5", Username: "bob", Kind: "chat",
		Text: "hey @Alice", ConvID: generalConv, CreatedAt: "2026-10-04T10:04:00Z",
	})

	// Longer identifier does not name us.
	c.handleNewMessage(chatMessage{
		Seq: 6, MsgId: "m6", Username: "bob", Kind: "chat",
		Text: "hey @alice2", ConvID: generalConv, CreatedAt: "2026-10-04T10:05:00Z",
	})

	if len(pings) != 1 {
		t.Fatalf("expected the legacy-room ping only, got %v", pings)
	}
	if pings[0] != "carol" {
		t.Fatalf("legacy room ping sender = %q; want carol", pings[0])
	}
}

// TestMentionNotifyRealInboundPath drives the REAL inbound pipeline end to
// end: a decrypted room broadcast (netChatMsg, as the engine delivers it)
// enters the bubbletea Update loop and must ping the notifier. This pins the
// whole chain — netChatMsg handler -> convFor(ConvID) -> handleNewMessage
// gates -> mentionNotifier — not just the sink in isolation. The ping
// payload is the sender only; the desktop body is exactly
// "<sender> mentioned you in the chat." (no message excerpt).
func TestMentionNotifyRealInboundPath(t *testing.T) {
	var pings []string
	orig := mentionNotifier
	mentionNotifier = func(sender string) { pings = append(pings, sender) }
	defer func() { mentionNotifier = orig }()

	c := newFilterScreen("alice", "")
	_, _ = c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// A general-room mention arriving over the wire (To="" = broadcast).
	_, _ = c.Update(netChatMsg{chat: engineChat{MsgId: "m1", From: "bob", To: "", Text: "hey @alice check this"}})
	if len(pings) != 1 {
		t.Fatalf("mention over the real inbound path must ping exactly once, got %v", pings)
	}
	if pings[0] != "bob" {
		t.Fatalf("ping sender %q; want bob", pings[0])
	}

	// A DM naming us over the wire never pings (@ is plain text there).
	_, _ = c.Update(netChatMsg{chat: engineChat{MsgId: "m2", From: "carol", To: "alice", Text: "psst @alice"}})
	if len(pings) != 1 {
		t.Fatalf("DM mention must not ping, got %v", pings)
	}

	// The production sink formats the exact body: sender + period, nothing
	// else — no excerpt, no colon.
	var gotTitle, gotBody string
	origDesktop := notifyDesktop
	notifyDesktop = func(title, body string, icon any) error {
		gotTitle, gotBody = title, body
		return nil
	}
	defer func() { notifyDesktop = origDesktop }()
	notifyMentioned("bob")
	if gotBody != "bob mentioned you in the chat." {
		t.Fatalf("notification body %q; want exactly \"bob mentioned you in the chat.\"", gotBody)
	}
	if gotTitle != "Uplink-Delta" {
		t.Fatalf("notification title %q; want Uplink-Delta", gotTitle)
	}
}

// TestNotifyFallsBackWhenDesktopUnavailable pins the fire-and-forget ping
// contract's visible half: when the desktop notification path errors (no
// notification daemon / no session bus — the silent-failure mode behind the
// old "_ = beeep.Notify(...)" discards), the failure must surface on stderr
// with the underlying reason plus the terminal-bell BEL, never vanish.
func TestNotifyFallsBackWhenDesktopUnavailable(t *testing.T) {
	orig := notifyDesktop
	notifyDesktop = func(title, body string, icon any) error { return fmt.Errorf("no notification daemon") }
	defer func() { notifyDesktop = orig }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()

	notify("Uplink-Delta", "alice mentioned you in the chat")

	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	s := string(out)
	if !strings.Contains(s, "uplink: desktop notification unavailable") {
		t.Fatalf("failure must surface on stderr: %q", s)
	}
	if !strings.Contains(s, "no notification daemon") {
		t.Fatalf("stderr must name the underlying reason: %q", s)
	}
	if !strings.Contains(s, "\a") {
		t.Fatalf("terminal bell must ring as the fallback: %q", s)
	}
}

// ---------------------------------------------------------------------------
// Highlight: every valid @username token paints as a chip — own messages,
// inbound general-room messages, and DM threads alike (display-only in DMs;
// receipts stay general-room-only). Non-tokens stay plain.
// ---------------------------------------------------------------------------

// swapMentionStyler installs a test styler that visibly marks replacements
// and restores the production one. Needed because headless tests paint under
// an Ascii color profile, where color styles render as plain text.
func swapMentionStyler(t *testing.T) {
	t.Helper()
	orig := mentionStyler
	mentionStyler = lipgloss.NewStyle().PaddingLeft(2) // "x" -> "  x"
	t.Cleanup(func() { mentionStyler = orig })
}

func TestMentionHighlight(t *testing.T) {
	swapMentionStyler(t)
	c := *newFilterScreen("alice", "")

	// Inbound general message: ANY valid @token is replaced by the chip —
	// our own name and every other member's alike.
	body := c.renderedBody(chatMessage{
		Seq: 1, MsgId: "m1", Username: "bob", Kind: "chat",
		Text: "hi @alice!", ConvID: generalConv, CreatedAt: "2026-10-04T10:00:00Z",
	})
	if !strings.Contains(body, "  @alice!") {
		t.Fatalf("own mention must paint the mention chip:\n%s", body)
	}

	// Other users' mentions chip too — the blue is for every @username.
	peer := c.renderedBody(chatMessage{
		Seq: 2, MsgId: "m2", Username: "bob", Kind: "chat",
		Text: "hi @carol and @dave", ConvID: generalConv, CreatedAt: "2026-10-04T10:01:00Z",
	})
	if !strings.Contains(peer, "  @carol") || !strings.Contains(peer, "  @dave") {
		t.Fatalf("every member mention must paint the chip:\n%s", peer)
	}

	// Own message in the room chips every token too, self included.
	own := c.renderedBody(chatMessage{
		Seq: 3, MsgId: "m3", Username: "alice", Kind: "chat",
		Text: "@alice @bob", ConvID: generalConv, CreatedAt: "2026-10-04T10:02:00Z",
	})
	if !strings.Contains(own, "  @alice") || !strings.Contains(own, "  @bob") {
		t.Fatalf("own message tokens must all chip:\n%s", own)
	}

	// Longer identifiers are whole tokens under the parser's rules: they
	// chip in FULL ("@alice2" is a valid token, not "@alice" + "2").
	longer := c.renderedBody(chatMessage{
		Seq: 4, MsgId: "m4", Username: "bob", Kind: "chat",
		Text: "@alice2 and @alice_extra", ConvID: generalConv, CreatedAt: "2026-10-04T10:03:00Z",
	})
	if !strings.Contains(longer, "  @alice2") || !strings.Contains(longer, "  @alice_extra") {
		t.Fatalf("longer identifiers must chip as whole tokens:\n%s", longer)
	}

	// Markdown interplay: the mention inside bold still chips.
	bold := c.renderedBody(chatMessage{
		Seq: 5, MsgId: "m5", Username: "bob", Kind: "chat",
		Text: "**@alice** is here", ConvID: generalConv, CreatedAt: "2026-10-04T10:04:00Z",
	})
	if !strings.Contains(bold, "  @alice") {
		t.Fatal("mention inside bold must still chip")
	}

	// Non-tokens stay plain: a bare "@" (no charset char after it) and a
	// username without the "@" render unstyled; a token still ends at the
	// first non-charset byte ("@al" chips in full, "-ice" stays plain).
	plain := c.renderedBody(chatMessage{
		Seq: 6, MsgId: "m6", Username: "bob", Kind: "chat",
		Text: "just @ now, mail @al-ice, hi alice", ConvID: generalConv,
		CreatedAt: "2026-10-04T10:05:00Z",
	})
	if strings.Contains(plain, "  @ ") {
		t.Fatalf("a bare @ (nothing after it) must stay plain:\n%s", plain)
	}
	if !strings.Contains(plain, "  @al") {
		t.Fatalf("the char-set prefix of a broken token must still chip:\n%s", plain)
	}
	if strings.Contains(plain, "  -ice") {
		t.Fatalf("the non-charset continuation must stay plain:\n%s", plain)
	}
	if strings.Contains(plain, "  alice") {
		t.Fatalf("a username without @ must stay plain:\n%s", plain)
	}
}

func TestMentionHighlightDMsChipToo(t *testing.T) {
	swapMentionStyler(t)

	// DMs highlight every token for display (no receipts there — that gate
	// lives in handleNewMessage); the text still passes through otherwise.
	dm := newFilterScreen("alice", "bob")
	body := dm.renderedBody(chatMessage{
		Seq: 1, MsgId: "m1", Username: "bob", Kind: "chat",
		Text: "psst @alice watch @carol", ConvID: conversationKey("alice", "bob"),
		CreatedAt: "2026-10-04T10:00:00Z",
	})
	if !strings.Contains(body, "  @alice") || !strings.Contains(body, "  @carol") {
		t.Fatalf("DM tokens must chip like every other bubble:\n%s", body)
	}
	if !strings.Contains(body, "psst") {
		t.Fatalf("DM text must pass through otherwise untouched: %q", body)
	}
}

func TestMentionHighlightAnsiAware(t *testing.T) {
	swapMentionStyler(t)

	// Hand-crafted SGR input: escape sequences pass through untouched while
	// plain runs are scanned, so the pass composes with markdown styling.
	// Every valid token chips — @alice and @bob alike.
	got := mentionHighlighted("\x1b[1m@alice\x1b[0m and @bob")
	if !strings.Contains(got, "\x1b[1m  @alice\x1b[0m") {
		t.Fatalf("sequence must survive while the mention chips:\n%q", got)
	}
	if !strings.Contains(got, "  @bob") {
		t.Fatalf("peer mention must chip too:\n%q", got)
	}
	if !strings.Contains(got, "\x1b[1m") {
		t.Fatalf("escape sequence must survive the highlight pass:\n%q", got)
	}

	// Escape sequences nested around a longer identifier survive; the
	// whole token still chips inside the styling.
	got = mentionHighlighted("\x1b[1m@alice2\x1b[0m")
	if got != "\x1b[1m  @alice2\x1b[0m" {
		t.Fatalf("longer identifier inside styling must chip whole: %q", got)
	}
}

func TestMentionHighlightEscapesNeverBreak(t *testing.T) {
	swapMentionStyler(t)

	// A peer-controlled ESC byte must be sanitized before the highlight pass
	// (sanitizeDisplay runs inside renderMarkdown), so the ANSI walker can
	// never be confused by raw escapes in the text.
	c := *newFilterScreen("alice", "")
	body := c.renderedBody(chatMessage{
		Seq: 1, MsgId: "m1", Username: "bob", Kind: "chat",
		Text: "esc \x1b[31m @alice", ConvID: generalConv, CreatedAt: "2026-10-04T10:00:00Z",
	})
	if strings.Contains(body, "\x1b[31m") {
		t.Fatal("peer ESC must be stripped before rendering")
	}
	if !strings.Contains(body, "  @alice") {
		t.Fatal("mention after a stripped escape must still chip")
	}
}

// TestMentionStylerIsThemeFamilyBlue pins the production chip identity:
// bold in the theme family's blue — exactly colAccent, the same adaptive
// pair as the sidebar/topbar/link blue (#0b62c9 light / #4cc9f0 dark) — with
// NO background fill, so the chip reads on both dark and light bubbles.
// Needs a truecolor profile because headless tests paint Ascii (colors are
// stripped there).
func TestMentionStylerIsThemeFamilyBlue(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	probe := tuiMentionStyle.Render("x")
	if !strings.HasPrefix(probe, "\x1b[1;") {
		t.Fatalf("mention chip must carry the theme family's bold: %q", probe)
	}
	// The foreground must be precisely colAccent — reference-render it and
	// require the same SGR color sequence inside the chip's sequence.
	ref := lipgloss.NewStyle().Foreground(colAccent).Render("x")
	s := strings.Index(ref, "38;")
	e := strings.Index(ref[s:], "m")
	refColor := ref[s : s+e]
	if !strings.Contains(probe, refColor) {
		t.Fatalf("mention chip must use the theme blue %s: %q", refColor, probe)
	}
	if strings.Contains(probe, "48;") {
		t.Fatalf("mention chip must carry no background fill: %q", probe)
	}
}
