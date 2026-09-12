package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Reproduction for "everything lands in general": drives netChatMsg events
// (the post-decrypt engine output) through the REAL Update pipeline and
// asserts conversation routing. The wire layer is covered by
// TestEngineEndToEnd; this test pins the view layer.
func TestReproDMRoutingOverWire(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	// --- ALICE side ---------------------------------------------------------
	a := newFilterScreen("alice", "")
	a.vp = *viewportPtr(60, 10)
	wireTestEngine(t, a, srv, "alice", "bob", "carol")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	cur := m.(chatScreen)
	cur.rebuildView()
	a = &cur
	a.rebuildView()

	// Inbound decrypted frames, as the engine would deliver them.
	for _, mc := range []tea.Msg{
		netChatMsg{chat: engineChat{MsgId: "m1", From: "bob", To: "alice", Text: "psst alice"}},
		netChatMsg{chat: engineChat{MsgId: "m2", From: "carol", To: "", Text: "room chatter"}},
	} {
		nm, _ := a.Update(mc)
		cur = nm.(chatScreen)
		a = &cur
	}

	// In GENERAL view: bob's DM must be INVISIBLE; carol's broadcast shows.
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
	a.enterPrivate("bob")
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

	// Reply from inside the thread must target bob's inbox.
	sc, _ := step(a, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h', 'i'}})
	sc, sendCmd := step(sc, tea.KeyMsg{Type: tea.KeyEnter})
	if sendCmd == nil {
		t.Fatal("no wire command for reply")
	}
	sd := sendCmd().(sendDoneMsg)
	_ = sd
	if inboxDeposits(fs, "bob") != 1 {
		t.Fatal("reply must deposit exactly one box for bob")
	}
}
