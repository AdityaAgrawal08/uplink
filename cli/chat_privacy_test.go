package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	if !strings.Contains(got, "public noise") || !strings.Contains(got, "Back in the common room") {
		t.Errorf("room restore wrong:\n%s", got)
	}
	if n := len(c.lines); n != 3 { // 2 history + back-note (enter-note stayed in thread)
		t.Fatalf("post-exit room rows = %d; want 3", n)
	}
}

func TestPendingEchoSurvivesModeSwitch(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.submitLine("in flight")
	if c.pending == nil {
		t.Fatal("precondition: send should be in flight")
	}

	c.enterPrivate("alice") // rebuild while a send is pending
	if c.pending == nil {
		t.Fatal("pending lost on rebuild")
	}
	want := len(c.lines) - 1
	if c.pending.lineIdx != want {
		t.Fatalf("pending idx %d not relocated (last=%d)", c.pending.lineIdx, want)
	}

	c.settleSend(sendDoneMsg{text: "in flight", seq: 9, code: 201})
	last := c.lines[len(c.lines)-1]
	if strings.Contains(last, "[you →]") {
		t.Errorf("settle missed relocated echo: %q", last)
	}
}

// ---------------------------------------------------------------------------
// Regression: sends from private view must carry the recipient over the wire
// ---------------------------------------------------------------------------

func TestSendTargetsCurrentPeer(t *testing.T) {
	var received [][]byte
	srv := newFakeChatServer(t, &received)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.client = newChatClient(srv.URL, "123456", "bob")
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

	if len(received) != 1 {
		t.Fatalf("wire posts = %d", len(received))
	}
	body := string(received[0])
	if !strings.Contains(body, `"to":"alice"`) {
		t.Errorf("payload missing private recipient: %s", body)
	}
}

// Common-room sends must NOT carry a recipient.
func TestCommonRoomSendsBroadcast(t *testing.T) {
	var received [][]byte
	srv := newFakeChatServer(t, &received)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.client = newChatClient(srv.URL, "123456", "bob")
	dispatch := c.submitLine("hello everyone")
	if dispatch == nil {
		t.Fatal("broadcast dispatch produced no command")
	}
	sd := dispatch().(sendDoneMsg) // performs the POST
	c.settleSend(sd)

	if len(received) != 1 {
		t.Fatalf("posts=%d", len(received))
	}
	if strings.Contains(string(received[0]), `"to"`) {
		t.Errorf("broadcast leaked a recipient field: %s", received[0])
	}
}

// ---------------------------------------------------------------------------
// Regression: roster freshness rides on polls, not just 15s heartbeats
// ---------------------------------------------------------------------------

func TestPollAdoptsRosterImmediately(t *testing.T) {
	var mu sync.Mutex
	users := []string{"bob"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		u := users
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		quoted := make([]string, len(u))
		for i, n := range u {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		fmt.Fprintf(w, `{"messages":[],"activeUsers":[%s],"ended":false}`, strings.Join(quoted, ","))
	}))
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	c.client = newChatClient(srv.URL, "123456", "bob")

	// A peer joins server-side between polls.
	mu.Lock()
	users = []string{"bob", "alice"}
	mu.Unlock()

	_, ended, err := c.client.pollOnce()
	if err != nil || ended {
		t.Fatalf("poll failed: ended=%v err=%v", ended, err)
	}
	if len(c.client.users) != 2 {
		t.Fatalf("roster after poll = %v; want [bob alice]", c.client.users)
	}

	// Everyone else leaves — roster must SHRINK too (no ghost users).
	mu.Lock()
	users = []string{"bob"}
	mu.Unlock()
	if _, _, err := c.client.pollOnce(); err != nil {
		t.Fatal(err)
	}
	if len(c.client.users) != 1 {
		t.Fatalf("roster shrink failed: %v", c.client.users)
	}
}
