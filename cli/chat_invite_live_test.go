package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// LIVE-REPRO regression tests (branch Groups) — the existing invite unit
// probes step the models in isolation and miss the REAL wiring:
//
// TestInvitePollCmdRidesRosterTick calls pollInvitesCmd() directly and the
// settings tests hand the window a ready-made invitesFetchedMsg, so both
// halves pass while the LIVE chain (roster tick → signed GET /invites/mine
// → applyInvites → open /settings → the window's OWN fetch → Notifications
// row) can still be broken end to end.
// ---------------------------------------------------------------------------

// runBatchCmd executes one tea.Batch from an Update and collects every
// message its sub-commands produce (the program runtime fans a BatchMsg out
// into its sub-commands; tests do the same manually). Tick commands return
// nil immediately — they are skipped, exactly like the runtime ignores
// their (nil) synchronous return and only delivers the later message.
func runBatchCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command to run")
	}
	msg := cmd()
	if bm, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, sub := range bm {
			if sub == nil {
				continue
			}
			if m := sub(); m != nil {
				out = append(out, m)
			}
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// findMsg returns the first occurrence of a concrete message type produced
// by a batch run (ok=false when it never fired).
func findMsg[T any](msgs []tea.Msg) (T, bool) {
	var zero T
	for _, m := range msgs {
		if v, ok := m.(T); ok {
			return v, true
		}
	}
	return zero, false
}

// TestInviteeLiveChainIsComplete is the failing-first repro for the LIVE
// invite arrival path: the inviter signs POST /invites through the real
// HTTP surface, the server's per-user index holds it, the invitee's 2s
// roster tick polls GET /invites/mine, applyInvites badges + beeeps, and
// opening /settings through rootModel must arm the window's OWN fetch so
// the Notifications row paints. Today rootModel never runs the overlay's
// Init, so the settings window's fetch chain never starts and the row
// never appears.
func TestInviteeLiveChainIsComplete(t *testing.T) {
	srv, fake := newGroupTestServer(t)

	// ── Inviter side: create the group and sign the invite for bob.
	aliceID, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	inviter := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	code, err := inviter.createGroupRoom("alice", pubkeyB64(aliceID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatalf("createGroupRoom: %v", err)
	}
	if err := inviter.sendInvite("bob"); err != nil {
		t.Fatalf("sendInvite: %v", err)
	}
	if !fake.fakeHasInvite("bob", code) {
		t.Fatal("fixture: the per-user index holds no invite for bob")
	}

	// ── Invitee side: bob's live screen shape (sig + identity wired).
	var pings []string
	prev := inviteNotifier
	inviteNotifier = func(by, group string) { pings = append(pings, by+"|"+group) }
	defer func() { inviteNotifier = prev }()

	bobID, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	scr := newChatScreen(srv.URL, "123456", "bob", bobID, "")
	r := newRootModel(scr)
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)

	// ── The REAL 2s tick: rosterTickMsg must arm the invites poll.
	rn, cmd := r.Update(rosterTickMsg{})
	r = rn.(rootModel)
	msgs := runBatchCmd(t, cmd)
	polled, ok := findMsg[invitesPolledMsg](msgs)
	if !ok {
		t.Fatal("roster tick must arm the /invites/mine poll")
	}
	if polled.err != nil || len(polled.invites) != 1 || polled.invites[0].Code != code {
		t.Fatalf("invitee poll = %+v err=%v; want the fresh invite", polled.invites, polled.err)
	}
	rn, _ = r.Update(polled)
	r = rn.(rootModel)
	if r.chat.pendingInvites != 1 {
		t.Fatalf("invitee badge = %d; want 1 after the live poll", r.chat.pendingInvites)
	}
	if len(pings) != 1 || pings[0] != "alice|Design" {
		t.Fatalf("beeep = %v; want exactly one alice|Design", pings)
	}

	// ── /settings through rootModel: the window must arm its own fetch
	// IMMEDIATELY (its Init) — this is the piece the unit probes skip.
	rn, cmd = r.Update(openSettingsMsg{})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("opening /settings must arm the window's invites fetch (Init)")
	}
	msgs = runBatchCmd(t, cmd)
	fetched, ok := findMsg[invitesFetchedMsg](msgs)
	if !ok {
		t.Fatal("settings Init must fetch GET /invites/mine immediately")
	}
	if fetched.err != nil || len(fetched.invites) != 1 || fetched.invites[0].Code != code {
		t.Fatalf("settings fetch = %+v err=%v; want the pending invite", fetched.invites, fetched.err)
	}
	rn, _ = r.Update(fetched)
	r = rn.(rootModel)
	view := r.View()
	for _, want := range []string{"NOTIFICATIONS", "alice invited you to group Design"} {
		if !strings.Contains(view, want) {
			t.Errorf("settings view missing %q", want)
		}
	}
}