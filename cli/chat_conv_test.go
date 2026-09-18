package main

import (
	"strings"
	"testing"
)

// convOf builds the canonical pair bucket used across these tests.
func convOf(a, b string) string { return conversationKey(a, b) }

// ---------------------------------------------------------------------------
// Regression: DMs must NEVER appear in the general room view — for anyone,
// including the two participants. This is the reported bug.
// ---------------------------------------------------------------------------

func TestDMNeverPaintsInGeneralView(t *testing.T) {
	c := newFilterScreen("p1", "", "p1", "p2", "p3")
	c.vp = *viewportPtr(60, 20)

	c.addMessage(chatMessage{Seq: 1, Username: "p1", Kind: "chat", Text: "public hello", ConvID: generalConv})
	dm := chatMessage{Seq: 2, Username: "p2", Kind: "chat", Text: "secret", To: "p1", ConvID: convOf("p1", "p2")}
	c.addMessage(dm) // p1 receives their thread's message while in general view

	painted := strings.Join(c.lines, "\n")
	if strings.Contains(painted, "secret") {
		t.Fatalf("DM leaked into general room paint:\n%s", painted)
	}
	if !strings.Contains(painted, "public hello") {
		t.Error("public line lost")
	}
	// Raw store keeps it so the thread can render later.
	if len(c.history) != 2 {
		t.Fatalf("history must retain DM; got %d", len(c.history))
	}
}

func TestThreadViewIsolatedFromRoom(t *testing.T) {
	c := newFilterScreen("p1", "")
	c.vp = *viewportPtr(60, 20)

	c.addMessage(chatMessage{Seq: 1, Username: "p3", Kind: "chat", Text: "room noise", ConvID: generalConv})
	c.addMessage(chatMessage{Seq: 2, Username: "p2", Kind: "chat", Text: "hey you", To: "p1", ConvID: convOf("p1", "p2")})
	c.addMessage(chatMessage{Seq: 3, Username: "p1", Kind: "system", Text: "p3 joined", ConvID: generalConv})

	cmd := c.enterPrivate("p2")
	_ = cmd // always nil now (no server history to fetch)

	got := strings.Join(c.lines, "\n")
	if strings.Contains(got, "room noise") || strings.Contains(got, "p3 joined") {
		t.Errorf("room content leaked into thread view:\n%s", got)
	}
	if !strings.Contains(got, "hey you") {
		t.Error("thread history missing")
	}

	c.exitPrivate()
	got = strings.Join(c.lines, "\n")
	if strings.Contains(got, "hey you") || strings.Contains(got, "secret") {
		t.Errorf("thread content leaked back into room:\n%s", got)
	}
	if !strings.Contains(got, "room noise") {
		t.Error("room view not restored")
	}
}

func TestMultipleIndependentThreads(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	c.addMessage(chatMessage{Seq: 1, Username: "a", Kind: "chat", Text: "from-a", To: "me", ConvID: convOf("a", "me")})
	c.addMessage(chatMessage{Seq: 2, Username: "b", Kind: "chat", Text: "from-b", To: "me", ConvID: convOf("b", "me")})

	c.enterPrivate("a")
	if got := strings.Join(c.lines, "\n"); !strings.Contains(got, "from-a") || strings.Contains(got, "from-b") {
		t.Errorf("thread isolation broken for peer a:\n%s", got)
	}

	c.exitPrivate()
	c.targetUser = "b" // simulate direct switch path state
	c.rebuildView()
	if got := strings.Join(c.lines, "\n"); !strings.Contains(got, "from-b") || strings.Contains(got, "from-a") {
		t.Errorf("thread isolation broken for peer b:\n%s", got)
	}
}

func TestPendingEchoLivesInItsConversation(t *testing.T) {
	c := newFilterScreen("me", "alice")
	c.vp = *viewportPtr(60, 20)
	c.submitLine("in flight")
	if c.pending == nil || c.pending.conv != convOf("me", "alice") {
		t.Fatalf("pending conv = %+v", c.pending)
	}

	c.exitPrivate() // rebuild while in flight
	if c.pending == nil {
		t.Fatal("pending lost on rebuild")
	}

	c.settleSend(sendDoneMsg{text: "in flight", seq: 9, code: 201})
	for _, ll := range c.localLines {
		if ll.conv == convOf("me", "alice") && strings.Contains(ll.text, "[you →]") {
			t.Errorf("echo not resolved in its own thread: %q", ll.text)
		}
	}
	if c.pending != nil {
		t.Fatal("pending not cleared")
	}
}

// Threads open instantly: there is no server history to fetch, so
// enterPrivate must never schedule a fetch command.
func TestEnterPrivateSwitchesInstantly(t *testing.T) {
	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)

	if cmd := c.enterPrivate("alice"); cmd != nil {
		t.Fatal("thread open must not schedule any fetch (no server history)")
	}
	if c.targetUser != "alice" {
		t.Fatalf("targetUser = %q", c.targetUser)
	}
	if cmd := c.enterPrivate("alice"); cmd != nil {
		t.Fatal("re-open must also be fetch-free")
	}
}

// ---------------------------------------------------------------------------
// Regression: a CONFIRMED DM must land in its thread — never painted into
// the general room for either participant (reported bug #1).
// ---------------------------------------------------------------------------

func TestSettleRoutesDMIntoThreadNotGeneral(t *testing.T) {
	c := newFilterScreen("p1", "")
	c.vp = *viewportPtr(60, 20)
	c.enterPrivate("p2") // active conv = p1|p2

	c.submitLine("secret ping") // echo paints inside the thread
	if c.pending == nil || c.pending.conv != conversationKey("p1", "p2") {
		t.Fatalf("pending state wrong: %+v", c.pending)
	}

	c.exitPrivate() // user hops back to the room before confirmation lands

	// Confirmation arrives while we are in GENERAL view.
	c.settleSend(sendDoneMsg{text: "secret ping", to: "p2", seq: 42, code: 201})

	// History owns it under the pair bucket.
	found := false
	for _, h := range c.history {
		if h.Seq == 42 && h.ConvID == conversationKey("p1", "p2") && h.To == "p2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("DM not routed to pair history: %+v", c.history)
	}

	// General paint excludes it.
	if got := strings.Join(c.lines, "\n"); strings.Contains(got, "secret ping") {
		t.Fatalf("DM leaked into general paint after settle:\n%s", got)
	}

	// Thread paints it exactly once.
	c.enterPrivate("p2")
	got := strings.Join(c.lines, "\n")
	if n := strings.Count(got, "secret ping"); n != 1 {
		t.Fatalf("thread shows %d copies of the DM; want 1:\n%s", n, got)
	}
}

// ---------------------------------------------------------------------------
// Regression: navigation noise is BANNED from transcripts (bug #2). Context
// lives solely in the permanent header.
// ---------------------------------------------------------------------------

func TestNoNavigationNotesInTranscripts(t *testing.T) {
	c := newFilterScreen("me", "", "me", "a", "b")
	c.vp = *viewportPtr(60, 20)

	banned := []string{"Private chat with", "Back in the common", "Esc for common"}

	for _, peer := range []string{"a", "b", "a"} { // rapid hopping
		c.enterPrivate(peer)
		c.exitPrivate()
	}
	painted := strings.Join(c.lines, "\n")
	for _, b := range banned {
		if strings.Contains(painted, b) {
			t.Errorf("navigation note leaked into transcript: %q\n%s", b, painted)
		}
	}
	for _, ll := range c.localLines {
		for _, b := range banned {
			if strings.Contains(ll.text, b) {
				t.Errorf("navigation note persisted in locals: %+v", ll)
			}
		}
	}

	// Room header carries the context INSTEAD: silent in general, explicit
	// in DM. (The top bar stays neutral by design.)
	c.width = 100 // header needs a real width to render
	c.targetUser = ""
	if h := c.roomHeaderView(80); strings.Contains(h, "Private") || strings.Contains(h, "ESC") {
		t.Errorf("general room header must stay clean: %q", h)
	}
	c.targetUser = "a"
	h := c.roomHeaderView(80)
	if !strings.Contains(h, "Private with a") || !strings.Contains(h, "ESC = general") {
		t.Errorf("thread room header missing permanent nav heading: %q", h)
	}
}
