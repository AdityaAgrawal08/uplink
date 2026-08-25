package main

import (
	"fmt"
	"net/http"
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
	_ = cmd // backlog fetch may be nil (fetchedConvs pre-seeded by helper)

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

// Lazy deep-fetch fires exactly once per thread and carries ?conv=.
func TestEnterPrivateFetchesThreadOnce(t *testing.T) {
	var queries []string
	srv := newFakeChatServer(t, nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("conv") != "" {
			queries = append(queries, r.URL.Query().Get("conv"))
			fmt.Fprintf(w, `{"messages":[{"seq":50,"username":"alice","kind":"chat","text":"old","convId":%q}],"activeUsers":[],"ended":false}`, convOf("me", "alice"))
			return
		}
		fmt.Fprint(w, `{"messages":[],"activeUsers":[],"ended":false}`)
	})
	defer srv.Close()

	c := newFilterScreen("me", "")
	c.vp = *viewportPtr(60, 20)
	c.client = newChatClient(srv.URL, "123456", "me")

	first := c.enterPrivate("alice")
	if first == nil {
		t.Fatal("first open must schedule thread fetch")
	}
	msg := first() // perform the wire call
	nm, _ := c.Update(msg)
	*c = nm.(chatScreen) // Update returns the mutated copy (Elm-style)
	if cmd := c.enterPrivate("alice"); cmd != nil {
		t.Fatal("second open must NOT refetch")
	}
	if len(queries) != 1 || queries[0] != convOf("me", "alice") {
		t.Fatalf("fetch queries = %v", queries)
	}
	found := false
	for _, m := range c.history {
		if m.Text == "old" && m.ConvID == convOf("me", "alice") {
			found = true
		}
	}
	if !found {
		t.Error("deep-fetched history not stored")
	}
}
