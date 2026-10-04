package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// /invite: the invitation window (group conversations only)
// ---------------------------------------------------------------------------

// inviteFixture builds a chat screen sitting inside a group conversation,
// with REAL engines for both the home room and the group session (so the
// client knows the existing members and sends actually POST /invites). The
// home room (123456) holds alice, carol and dave; the group holds alice and
// bob — bob is an existing GROUP member and must be EXCLUDED from the
// invitation list (as is alice, self).
func inviteFixture(t *testing.T) (*httptest.Server, chatScreen, string, *identityKey) {
	t.Helper()
	srv, _ := newGroupTestServer(t)
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// alice creates the group; the fake stores her pubkey as the creator's
	// signature anchor, so the invite POSTs pass the gate.
	asig := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	code, err := asig.createGroupRoom("alice", pubkeyB64(id), "Design", "", nil, "123456")
	if err != nil {
		t.Fatalf("createGroupRoom: %v", err)
	}
	// bob joins the group by code: an existing member, never inviteable.
	bobID, _ := generateIdentity()
	bob := &signalClient{serverURL: srv.URL, key: code, me: "bob"}
	if _, _, err := bob.joinRoom("bob", pubkeyB64(bobID), ""); err != nil {
		t.Fatalf("bob join: %v", err)
	}
	// Home room 123456: alice creates it, carol and dave join (the room
	// users the invitation window should list).
	homesig := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	if _, err := homesig.createRoom("alice", pubkeyB64(id), ""); err != nil {
		t.Fatalf("home create: %v", err)
	}
	homesig.key = "123456"
	for _, u := range []string{"carol", "dave"} {
		uid, _ := generateIdentity()
		usig := &signalClient{serverURL: srv.URL, key: "123456", me: u}
		if _, _, err := usig.joinRoom(u, pubkeyB64(uid), ""); err != nil {
			t.Fatalf("%s home join: %v", u, err)
		}
	}

	c := groupTestScreen("alice")
	c.id = id // the engines sign beats with THIS identity (the home creator's)
	c.users = []string{"alice", "bob", "carol", "dave"}
	c.width, c.height = 110, 40
	c.homeEng = c.newSessionEngine(homesig)
	c.homeEng.beatOnce() // seed home presence: alice (creator), carol, dave
	gsig := &signalClient{serverURL: srv.URL, key: code, me: "alice", id: id}
	g := &groupSession{code: code, name: "Design", sig: gsig}
	g.eng = c.newSessionEngine(gsig)
	g.eng.beatOnce() // seed group presence: alice (creator), bob
	c.groups[code] = g
	c.openGroup(code)
	return srv, c, code, id
}

func stepInvite(m inviteModel, msg tea.Msg) inviteModel {
	nxt, _ := m.Update(msg)
	return nxt.(inviteModel)
}

func stepInviteC(m inviteModel, msg tea.Msg) (inviteModel, tea.Cmd) {
	nxt, cmd := m.Update(msg)
	return nxt.(inviteModel), cmd
}

// TestInviteCommandGatedOutsideGroup: running /invite while NOT inside a
// group answers inline and never opens a window. Inside a group it opens
// the invitation window through rootModel.
func TestInviteCommandGatedOutsideGroup(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	t.Cleanup(srv.Close)
	c := newFilterScreen("alice", "")
	c.vp = *viewportPtr(80, 24)
	c.width, c.height = 80, 24
	if cmd := c.runCommand("/invite", ""); cmd != nil {
		t.Fatalf("outside a group /invite must not produce a window command, got %v", cmd)
	}
	if !strings.Contains(c.status, "/invite") {
		t.Fatalf("outside a group /invite must answer inline, status=%q", c.status)
	}

	// Typed through the palette: same inline answer, drawer closes.
	c2 := newFilterScreen("alice", "")
	c2.vp = *viewportPtr(80, 24)
	sc, _ := typeKeys(c2, "/invite")
	got, cmd := step(&sc, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("palette /invite outside a group must run locally, got cmd %v", cmd)
	}
	if got.palette.visible() {
		t.Fatal("selecting /invite must close the drawer")
	}
	if !strings.Contains(got.status, "/invite") {
		t.Fatalf("inline usage note missing: %q", got.status)
	}
}

// TestInviteWindowCandidatesExcludeMembers: the window lists the parent
// room's users minus the group's existing members and self, in stable
// username order (bob is already in the group, alice is self — neither may
// appear).
func TestInviteWindowCandidatesExcludeMembers(t *testing.T) {
	_, c, code, _ := inviteFixture(t)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	got := inviteNames(m.cands)
	if strings.Join(got, ",") != "carol,dave" {
		t.Fatalf("candidates = %v; want [carol dave] (bob=existing member, alice=self excluded)", got)
	}
	// None are pre-picked.
	if len(m.selectedUsers()) != 0 {
		t.Fatal("the window must open with no one selected")
	}
}

func inviteNames(cands []rosterMember) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Username)
	}
	return out
}

// TestInviteWindowKeyboardSend: select carol and dave one-by-one with
// highlight + Enter (window stays open throughout), Tab to the Send button,
// Enter POSTs /invites for BOTH; the fake server holds both invites.
func TestInviteWindowKeyboardSend(t *testing.T) {
	srv, c, code, _ := inviteFixture(t)
	fake := srv.Config.Handler.(*fakeSignalServer)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)

	// Enter on the highlighted row (sel 0 = carol) toggles it, window stays
	// open; Down + Enter toggles dave.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.picked["carol"] {
		t.Fatal("enter must toggle the highlighted member")
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.sel != 1 || m.cands[m.sel].Username != "dave" {
		t.Fatalf("down must move to dave, sel=%d cand=%q", m.sel, m.cands[m.sel].Username)
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.picked["dave"] {
		t.Fatal("enter must toggle the second member")
	}
	if len(m.picked) != 2 {
		t.Fatalf("picked = %v; want exactly carol+dave", m.picked)
	}

	// Enter again on dave UN-picks him (toggle), then re-picks.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.picked["dave"] {
		t.Fatal("enter must un-toggle a picked member")
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.picked["dave"] {
		t.Fatal("enter must re-toggle a picked member")
	}

	// Tab to the Send button; Enter sends every pick.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != 1 {
		t.Fatalf("tab must move to the Send button, focus=%d", m.focus)
	}
	m, cmd := stepInviteC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the Send button must produce a send command")
	}
	m = stepInvite(m, cmd().(inviteSentMsg))
	if m.busy {
		t.Fatal("send must resolve busy")
	}
	notes := strings.Join(m.notes, " · ")
	if !strings.Contains(notes, "invited carol") || !strings.Contains(notes, "invited dave") {
		t.Fatalf("notes = %q; want both invited", notes)
	}
	if fake.fakeInviteCount("carol") != 1 || fake.fakeInviteCount("dave") != 1 {
		t.Fatalf("invites = carol:%d dave:%d; want 1 each", fake.fakeInviteCount("carol"), fake.fakeInviteCount("dave"))
	}
	if fake.fakeInviteCount("bob") != 0 {
		t.Fatal("existing member bob must never hold an invite")
	}
	// Picks are consumed; the window stays open for the next round.
	if len(m.picked) != 0 || len(m.cands) != 2 {
		t.Fatalf("after send: picked=%v cands=%d; want cleared picks, window open", m.picked, len(m.cands))
	}
}

// TestInviteWindow409SkipsInline: a 409 (already in this session) for one
// member notes inline and the rest of the send continues.
func TestInviteWindow409SkipsInline(t *testing.T) {
	srv, c, code, _ := inviteFixture(t)
	fake := srv.Config.Handler.(*fakeSignalServer)
	fake.inviteConflict["dave"] = true // dave's invite answers 409
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)

	// Shift+Down range steps: anchor at carol (0) -> sel 1 (dave).
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyShiftDown})
	if m.anchor != 0 || m.sel != 1 {
		t.Fatalf("shift+down: anchor=%d sel=%d; want anchor 0 sel 1", m.anchor, m.sel)
	}
	// Enter applies the RANGE: carol + dave both picked at once.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.picked) != 2 || !m.picked["carol"] || !m.picked["dave"] {
		t.Fatalf("range enter must pick both: %v", m.picked)
	}
	if m.anchor != -1 {
		t.Fatal("applying the range must release the anchor")
	}

	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyTab}) // to the Send button
	m, cmd := stepInviteC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the Send button must produce a send command")
	}
	m = stepInvite(m, cmd().(inviteSentMsg))
	notes := strings.Join(m.notes, " · ")
	if !strings.Contains(notes, "dave is already in this session") {
		t.Fatalf("409 note missing: %q", notes)
	}
	if !strings.Contains(notes, "invited carol") {
		t.Fatalf("the rest must continue: %q", notes)
	}
	if fake.fakeInviteCount("dave") != 0 {
		t.Error("409 dave must NOT hold an invite")
	}
	if fake.fakeInviteCount("carol") != 1 {
		t.Errorf("carol must hold an invite: %d", fake.fakeInviteCount("carol"))
	}
	// Plain move clears the range anchor.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.anchor != -1 {
		t.Fatal("a plain move must clear the range anchor")
	}
}

// TestInviteWindowEscClosesWithoutSending: Esc at any point closes the
// window with zero invites POSTed.
func TestInviteWindowEscClosesWithoutSending(t *testing.T) {
	srv, c, code, _ := inviteFixture(t)
	fake := srv.Config.Handler.(*fakeSignalServer)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)

	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown})
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter}) // picked one
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyTab})   // on the Send button
	m, cmd := stepInviteC(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc must produce a close command")
	}
	var closeMsg closeOverlayMsg
	if got := cmd().(closeOverlayMsg); got != closeMsg {
		t.Fatalf("esc close = %#v", got)
	}
	if fake.fakeInviteCount("carol") != 0 || fake.fakeInviteCount("dave") != 0 || fake.fakeInviteCount("bob") != 0 {
		t.Fatal("esc must never send invites")
	}
}

// TestInviteWindowMouseParity: a click on a candidate row selects AND
// toggles it (window stays open), a click on the Send button sends; clicks
// on chrome rows do nothing.
func TestInviteWindowMouseParity(t *testing.T) {
	srv, c, code, _ := inviteFixture(t)
	fake := srv.Config.Handler.(*fakeSignalServer)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	g := m.geom()

	// Click the second candidate row: selects dave + toggles him.
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst+1))
	if m.sel != 1 || !m.picked["dave"] {
		t.Fatalf("row click must select+toggle dave: sel=%d picked=%v", m.sel, m.picked)
	}
	// Click the same row again: UN-toggles (toggle semantics).
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst+1))
	if m.picked["dave"] {
		t.Fatal("second click must un-toggle dave")
	}
	// The highlight stayed on dave (row 1): keyboard Enter re-picks him,
	// then the Send button click sends the pick.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.picked["dave"] {
		t.Fatal("enter must pick the highlighted member (dave)")
	}
	m, cmd := stepInviteC(m, mouseAt(g.btnFirstX+1, g.btnRow))
	if cmd == nil {
		t.Fatal("button click must produce a send command")
	}
	m = stepInvite(m, cmd().(inviteSentMsg))
	if fake.fakeInviteCount("dave") != 1 {
		t.Fatalf("button click must send dave, got %d invites", fake.fakeInviteCount("dave"))
	}
	// Chrome click (title row): no selection change, no send.
	before := m.sel
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst-2))
	if m.sel != before {
		t.Fatalf("chrome click must not move the selection: sel=%d", m.sel)
	}
}

// TestInviteWindowRenders: the window paints its title, the candidate rows
// with pick marks, the Send button and the keymap footer.
func TestInviteWindowRenders(t *testing.T) {
	_, c, code, _ := inviteFixture(t)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown}) // sel 1
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	view := m.View()
	for _, want := range []string{"◆ INVITE", "add members to group Design", "carol", "dave", "SEND", "enter toggle"} {
		if !strings.Contains(view, want) {
			t.Errorf("invite view missing %q", want)
		}
	}
}

// TestInviteOpenThroughRootModel: inside a group, typing /invite in the
// palette and pressing Enter opens the invitation window through rootModel;
// Esc closes back to the chat.
func TestInviteOpenThroughRootModel(t *testing.T) {
	srv, c, code, id := inviteFixture(t)
	_ = srv
	_ = id
	sc := c
	r := newRootModel(sc)

	// Type /invite, Enter: the palette runs the command -> openInviteMsg.
	sc2, _ := typeKeys(&sc, "/invite")
	r = newRootModel(sc2)
	rn, cmd := r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("selecting /invite in a group must produce the window-open command")
	}
	rn = runCmdStep(t, r, cmd)
	r = rn.(rootModel)
	im, ok := r.overlay.(inviteModel)
	if !ok {
		t.Fatalf("overlay = %T; want inviteModel", r.overlay)
	}
	if len(im.cands) != 2 {
		t.Fatalf("window candidates = %v; want the two inviteable room users", inviteNames(im.cands))
	}
	if code != im.code {
		t.Fatalf("window code = %q; want %q", im.code, code)
	}
	// Esc closes the window back to the chat.
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyEsc})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("esc in the window must produce the close command")
	}
	rn = runCmdStep(t, r, cmd)
	r = rn.(rootModel)
	if r.overlay != nil {
		t.Fatal("esc must close the invitation window")
	}
}