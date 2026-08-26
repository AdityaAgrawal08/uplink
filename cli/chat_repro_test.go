package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Reproduction for "everything lands in general": drives the REAL wire layer
// (chatClient.pollOnce JSON) + the REAL Update pipeline against a server that
// tags convId exactly like the production route does.
func TestReproDMRoutingOverWire(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	postSeq := 100
	sendPayloads := map[string]string{} // client -> last raw body

	mux.HandleFunc("/api/v1/session/123456/messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var p struct {
				Text string `json:"text"`
				To   string `json:"to"`
			}
			json.NewDecoder(r.Body).Decode(&p)
			raw, _ := json.Marshal(p)
			sendPayloads[r.Header.Get("X-Uplink-Username")] = string(raw)
			postSeq++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]int{"seq": postSeq})
		case http.MethodGet:
			// Whatever the poll cursor: return one DM bob->alice tagged by
			// the server exactly like appendMessage/toMessageDTO do.
			dm := map[string]any{
				"seq": 1, "username": "bob", "kind": "chat",
				"text": "psst alice", "createdAt": "2026-08-26T02:00:00Z",
				"to": "alice", "convId": conversationKey("bob", "alice"),
			}
			gen := map[string]any{
				"seq": 2, "username": "carol", "kind": "chat",
				"text": "room chatter", "createdAt": "2026-08-26T02:00:01Z",
				"convId": "general",
			}
			json.NewEncoder(w).Encode(map[string]any{
				"messages":    []map[string]any{dm, gen},
				"activeUsers": []string{"alice", "bob", "carol"},
				"ended":       false,
			})
		}
	})

	// --- ALICE side ---------------------------------------------------------
	a := newFilterScreen("alice", "")
	a.vp = *viewportPtr(60, 10)
	a.client = newChatClient(srv.URL, "123456", "alice")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	cur := m.(chatScreen)
	cur.rebuildView()
	a = &cur
	a.rebuildView()

	// Real pollOnce → real JSON parse.
	newMsgs, ended, err := a.client.pollOnce()
	if err != nil || ended {
		t.Fatalf("poll failed: %v ended=%v", err, ended)
	}
	for _, msg := range newMsgs {
		t.Logf("parsed: seq=%d from=%s to=%q convId=%q text=%q",
			msg.Seq, msg.Username, msg.To, msg.ConvID, msg.Text)
	}
	if len(newMsgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(newMsgs))
	}
	if newMsgs[0].ConvID != conversationKey("bob", "alice") {
		t.Fatalf("WIRE LAYER DROPPED convId: got %q", newMsgs[0].ConvID)
	}

	// Route through the model like pollDoneMsg handling does.
	for _, msg := range newMsgs {
		a.handleNewMessage(msg)
	}

	// In GENERAL view: neither bob's DM nor... carol's broadcast shows; only
	// general chatter paints. Bob's DM must be INVISIBLE here.
	var view strings.Builder
	for _, ln := range a.lines {
		view.WriteString(ln + "\n")
	}
	if strings.Contains(view.String(), "psst alice") {
		t.Fatal("BUG REPRODUCED: DM painted in general view")
	}
	if !strings.Contains(view.String(), "room chatter") {
		t.Fatal("general broadcast missing from general view")
	}

	// Open the thread: the DM appears, the room chatter disappears.
	cmd := a.enterPrivate("bob")
	if cmd != nil {
		cmd()
	}
	a.rebuildView()
	view.Reset()
	for _, ln := range a.lines {
		view.WriteString(ln + "\n")
	}
	if !strings.Contains(view.String(), "psst alice") {
		t.Fatalf("DM missing inside thread view: %v", a.lines)
	}
	if strings.Contains(view.String(), "room chatter") {
		t.Fatal("general chatter leaked into thread view")
	}

	// Reply from inside the thread must carry to=bob on the wire.
	sc, _ := step(a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h', 'i'}})
	sc, sendCmd := step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	if sendCmd == nil {
		t.Fatal("no wire command for reply")
	}
	sd := sendCmd().(sendDoneMsg)
	_ = sd
	if !strings.Contains(sendPayloads["alice"], `"to":"bob"`) {
		t.Fatalf("reply payload lost its recipient: %s", sendPayloads["alice"])
	}
}
