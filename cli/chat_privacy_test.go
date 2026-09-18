package main

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// Regression: private view must re-filter EXISTING history, not just new msgs
// ---------------------------------------------------------------------------

func TestPrivateViewRetroFiltersHistory(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)

	c.addMessage(chatMessage{Seq: 1, Username: "carol", Kind: "chat", Text: "public noise", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, Username: "alice", Kind: "chat", Text: "psst bob", To: "bob", ConvID: conversationKey("bob", "alice")})
	c.addMessage(chatMessage{Seq: 3, Username: "system", Kind: "system", Text: "carol joined", ConvID: generalConv})

	// General view paints ONLY its own bucket: public line + system event.
	// The DM is invisible here even though bob is a participant — the fix.
	if n := len(c.lines); n != 2 {
		t.Fatalf("general room should paint 2 rows; got %d:\n%s", n, strings.Join(c.lines, "\n"))
	}

	c.enterPrivate("alice")
	got := strings.Join(c.lines, "\n")
	if strings.Contains(got, "public noise") || strings.Contains(got, "carol joined") {
		t.Errorf("room content leaked into thread:\n%s", got)
	}
	if !strings.Contains(got, "psst bob") {
		t.Errorf("thread history missing:\n%s", got)
	}
	if len(c.history) != 3 {
		t.Fatalf("raw store truncated: %d", len(c.history))
	}

	c.exitPrivate()
	got = strings.Join(c.lines, "\n")
	if strings.Contains(got, "psst bob") {
		t.Errorf("DM leaked back into room:\n%s", got)
	}
	if !strings.Contains(got, "public noise") {
		t.Errorf("room restore wrong:\n%s", got)
	}
	if n := len(c.lines); n != 2 {
		t.Fatalf("post-exit room rows = %d; want exactly the 2 history rows (no nav notes)", n)
	}
}

func TestPendingEchoSurvivesModeSwitch(t *testing.T) {
	c := newFilterScreen("me", "alice")
	c.vp = *viewportPtr(40, 10)
	c.submitLine("in flight")
	if c.pending == nil {
		t.Fatal("precondition: send should be in flight")
	}

	c.exitPrivate() // rebuild while a send is pending
	if c.pending == nil {
		t.Fatal("pending lost on rebuild")
	}

	// Settle routes by the PENDING conversation, not the active view:
	// the thread's echo resolves even though we now sit in general.
	c.settleSend(sendDoneMsg{text: "in flight", seq: 9, code: 201})
	for _, ll := range c.localLines {
		if ll.conv != generalConv && strings.Contains(ll.text, "[you") {
			t.Fatalf("thread echo not resolved: %+v", ll)
		}
	}
	found := false
	for _, h := range c.history {
		if h.Seq == 9 && h.ConvID == conversationKey("me", "alice") && h.Text == "in flight" {
			found = true
		}
	}
	if !found {
		t.Fatal("confirmed DM missing from thread history")
	}
}

// ---------------------------------------------------------------------------
// Regression: sends from private view must target the peer over the wire.
// The deposit lands in alice's inbox (payload opaque: sealed box).
// ---------------------------------------------------------------------------

func TestSendTargetsCurrentPeer(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob", "alice")
	var m tea.Model = c
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	sc, ok := m.(chatScreen)
	if !ok {
		t.Fatalf("model type %T", m)
	}
	sc.enterPrivate("alice")

	for _, r := range []rune("secret hi") {
		var cm tea.Cmd
		m, cm = sc.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		sc = m.(chatScreen)
		_ = cm
	}
	m, cmd := sc.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("no send command produced from private view")
	}
	sd := cmd().(sendDoneMsg)
	_ = sd

	if inboxDeposits(fs, "alice") != 1 {
		t.Fatal("private send must deposit exactly one box for alice")
	}
}

// Common-room sends fan out per peer (one sealed box each). There is no
// single broadcast envelope anymore; assert every peer got exactly one.
func TestCommonRoomSendsBroadcast(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob", "alice", "carol")
	dispatch := c.submitLine("hello everyone")
	if dispatch == nil {
		t.Fatal("broadcast dispatch produced no command")
	}
	sd := dispatch().(sendDoneMsg) // performs the deposits
	c.settleSend(sd)

	if inboxDeposits(fs, "alice") != 1 || inboxDeposits(fs, "carol") != 1 {
		t.Fatal("broadcast must fan out one box per peer")
	}
}

// ---------------------------------------------------------------------------
// Regression: roster freshness rides on heartbeats, shrinking included
// ---------------------------------------------------------------------------

func TestPollAdoptsRosterImmediately(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob")

	// A peer joins server-side between beats.
	joiner := &signalClient{serverURL: srv.URL, key: "123456", me: "alice"}
	if _, _, err := joiner.joinRoom("alice", base64.StdEncoding.EncodeToString(make([]byte, 32)), ""); err != nil {
		t.Fatal(err)
	}

	c.eng.beatOnce()
	m, _ := c.Update(rosterTickMsg{})
	*c = m.(chatScreen)
	if len(c.users) != 2 {
		t.Fatalf("roster after beat = %v; want [bob alice]", c.users)
	}

	// Everyone else leaves — roster must SHRINK too (no ghost users).
	leaver := &signalClient{serverURL: srv.URL, key: "123456", me: "alice"}
	if err := leaver.leaveRoom(); err != nil {
		t.Fatal(err)
	}
	c.eng.beatOnce()
	m, _ = c.Update(rosterTickMsg{})
	*c = m.(chatScreen)
	if len(c.users) != 1 {
		t.Fatalf("roster shrink failed: %v", c.users)
	}
}
