package main

import (
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// newGroupTestServer spins the fake signaling server (with the groups
// plane) up for one test.
func newGroupTestServer(t *testing.T) (*httptest.Server, *fakeSignalServer) {
	t.Helper()
	fake := newFakeSignalServer()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return srv, fake
}

// groupTestScreen builds a bare chat screen with the group state wired
// (no engine, no network): enough for sidebar/routing/open/exit assertions.
func groupTestScreen(me string) chatScreen {
	c := newFilterScreen(me, "")
	c.homeKey = "111111"
	c.key = "111111"
	c.groups = map[string]*groupSession{}
	c.lastGroupAt = map[string]time.Time{}
	if id, err := generateIdentity(); err == nil {
		c.id = id
	}
	return *c
}

// teaRune builds a printable-key KeyMsg for driving text inputs.
func teaRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// step helpers run one Update and hand back the concrete model (the models
// type-switch internally; Update's interface return needs the assertion).
func stepGroup(m groupModel, msg tea.Msg) groupModel {
	nxt, _ := m.Update(msg)
	return nxt.(groupModel)
}

func stepGroupC(m groupModel, msg tea.Msg) (groupModel, tea.Cmd) {
	nxt, cmd := m.Update(msg)
	return nxt.(groupModel), cmd
}

func stepSettings(m settingsModel, msg tea.Msg) settingsModel {
	nxt, _ := m.Update(msg)
	return nxt.(settingsModel)
}

func stepSettingsC(m settingsModel, msg tea.Msg) (settingsModel, tea.Cmd) {
	nxt, cmd := m.Update(msg)
	return nxt.(settingsModel), cmd
}

func stepChat(c chatScreen, msg tea.Msg) chatScreen {
	nxt, _ := c.Update(msg)
	return nxt.(chatScreen)
}

func stepChatC(c chatScreen, msg tea.Msg) (chatScreen, tea.Cmd) {
	nxt, cmd := c.Update(msg)
	return nxt.(chatScreen), cmd
}

// runCmdStep executes a tea.Cmd from an Update and feeds its message back,
// returning the resulting model (used to drive async HTTP flows inline).
func runCmdStep(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command to run")
	}
	msg := cmd()
	next, _ := m.Update(msg)
	return next
}

// groupFixture creates a group via the real HTTP surface (creator me) and
// invites a target user. Returns the group code.
func groupFixture(t *testing.T, srv *httptest.Server, me, target, name string, max *int) string {
	t.Helper()
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	sig := &signalClient{serverURL: srv.URL, me: me, id: id}
	code, err := sig.createGroupRoom(me, base64.StdEncoding.EncodeToString(id.publicKey()), name, "", max, "123456")
	if err != nil {
		t.Fatalf("createGroupRoom: %v", err)
	}
	if target != "" {
		if err := sig.sendInvite(target); err != nil {
			t.Fatalf("sendInvite: %v", err)
		}
	}
	return code
}

// ---------------------------------------------------------------------------
// Creation validation
// ---------------------------------------------------------------------------

func TestGroupCreateValidation(t *testing.T) {
	if e := validateGroupName(""); e == "" {
		t.Error("empty name must fail")
	}
	if e := validateGroupName(strings.Repeat("x", 65)); e == "" {
		t.Error("65-char name must fail")
	}
	if e := validateGroupName("Design Team"); e != "" {
		t.Errorf("valid name rejected: %v", e)
	}
	if e := validateGroupDesc(strings.Repeat("d", 257)); e == "" {
		t.Error("257-char desc must fail")
	}
	if e := validateGroupDesc(strings.Repeat("d", 256)); e != "" {
		t.Errorf("256-char desc is valid, got %v", e)
	}
	if n, e := validateMaxMembers(""); n != nil || e != "" {
		t.Errorf("empty max must mean unlimited: %v %v", n, e)
	}
	if n, e := validateMaxMembers("1"); n != nil || !strings.Contains(e, "at least 2") {
		t.Errorf("max 1 must fail inline: %v %v", n, e)
	}
	if n, e := validateMaxMembers("abc"); n != nil || e == "" {
		t.Errorf("non-numeric max must fail: %v %v", n, e)
	}
	if n, e := validateMaxMembers("2"); n == nil || *n != 2 || e != "" {
		t.Errorf("max 2 must parse: %v %v", n, e)
	}
}

// TestGroupCreateWindowInlineErrors drives the /new-group window: it opens
// DIRECTLY on the creation form (no member multi-select stage — members are
// added later through /invite), invalid forms never reach the server
// (inline errors), and the valid path creates the group and crowns the
// creator server-side.
func TestGroupCreateWindow(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}

	m := newGroupModel(srv.URL, "alice", id, "123456", 110, 40)

	// The window opens straight on the form: Name focused, no pick stage.
	if !m.nameInput.Focused() || m.formFocus != 0 {
		t.Fatalf("window must open on the Name field, focus=%d focused=%v", m.formFocus, m.nameInput.Focused())
	}
	if !strings.Contains(m.View(), "NEW GROUP") {
		t.Error("window must paint the creation form immediately")
	}

	// Empty name: inline error, no HTTP.
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // 0->1 (desc)
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // 1->2 (max)
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // 2->3 (Create button)
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // 3: submit -> inline error
	if m.errMsg == "" || !strings.Contains(m.errMsg, "1-64") {
		t.Fatalf("empty-name create must fail inline, got %q", m.errMsg)
	}

	// Fill the name; Max-People 1 must fail inline.
	m.formFocus = 0
	m.syncFormFocus()
	for _, r := range []rune("Design Team") {
		var cmd tea.Cmd
		m.nameInput, cmd = m.nameInput.Update(teaRune(r))
		_ = cmd
	}
	m.formFocus = 2
	m.syncFormFocus()
	for _, r := range []rune("1") {
		var cmd tea.Cmd
		m.maxInput, cmd = m.maxInput.Update(teaRune(r))
		_ = cmd
	}
	m.formFocus = 0
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter})     // 0->1
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter})     // 1->2
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter})     // 2->3 (Create button)
	m, _ = stepGroupC(m, tea.KeyMsg{Type: tea.KeyEnter}) // Enter on the button submits
	if m.errMsg == "" || !strings.Contains(m.errMsg, "at least 2") {
		t.Fatalf("max 1 must fail inline, got %q", m.errMsg)
	}

	// Fix max to 4 and submit for real.
	m.formFocus = 2
	m.maxInput.SetValue("4")
	m.formFocus = 3
	_, cmd := stepGroupC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("valid form must produce a create command")
	}
	done := cmd().(createGroupDoneMsg)
	if done.code == "" {
		t.Fatal("group create must return a session code")
	}
	if len(done.notes) != 0 {
		t.Fatalf("create must not invite anyone (members join via /invite): notes=%v", done.notes)
	}
	// Creator crowned automatically + group meta recorded server-side.
	meta, ok := fake.fakeGroup(done.code)
	if !ok {
		t.Fatalf("group %s not registered server-side", done.code)
	}
	if meta.name != "Design Team" || meta.max != 4 {
		t.Errorf("group meta = %+v; want name Design Team max 4", meta)
	}
	fake.mu.Lock()
	creator := fake.members[done.code]["alice"]
	fake.mu.Unlock()
	if creator.Role != "creator" {
		t.Errorf("creator role = %q; want creator (server-crowned)", creator.Role)
	}
}

// TestGroupCreateServerValidation proves the exact contract messages the
// CLI receives surface as user-visible errors.
func TestGroupCreateServerValidation(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, _ := generateIdentity()
	sig := &signalClient{serverURL: srv.URL, me: "alice"}
	one := 1
	_, err := sig.createGroupRoom("alice", base64.StdEncoding.EncodeToString(id.publicKey()), "G", "", &one, "")
	if err == nil || !strings.Contains(err.Error(), "maxMembers must be an integer of at least 2, or null for unlimited") {
		t.Errorf("max 1 create err = %v", err)
	}
	_, err = sig.createGroupRoom("alice", base64.StdEncoding.EncodeToString(id.publicKey()), "", "", nil, "123456")
	if err == nil || !strings.Contains(err.Error(), "groupName must be a string of 1-64 characters") {
		t.Errorf("empty-name create err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Invite send errors (the exact surfaces a creator sees)
// ---------------------------------------------------------------------------

func TestGroupInviteSendErrors(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()
	pk := base64.StdEncoding.EncodeToString(id.publicKey())

	// alice owns the home room; bob joins it; eve is a stranger.
	alice := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	if _, err := alice.createRoom("alice", pk, ""); err != nil {
		t.Fatal(err)
	}
	alice.key = "123456"
	bobID, _ := generateIdentity()
	bob := &signalClient{serverURL: srv.URL, key: "123456", me: "bob"}
	if _, _, err := bob.joinRoom("bob", base64.StdEncoding.EncodeToString(bobID.publicKey()), ""); err != nil {
		t.Fatal(err)
	}
	bob.key = "123456"

	err := alice.sendInvite("alice")
	if err == nil || !strings.Contains(err.Error(), "You cannot invite yourself") {
		t.Errorf("self-invite err = %v; want 'You cannot invite yourself'", err)
	}
	eve := &signalClient{serverURL: srv.URL, key: "123456", me: "eve"}
	err = eve.sendInvite("carol")
	if err == nil || apiStatusCode(err) != 401 ||
		!strings.Contains(err.Error(), "Invalid or missing request signature") {
		t.Errorf("non-member invite err = %v; want 401 signature rejection", err)
	}
	err = alice.sendInvite("bob")
	if err == nil || !strings.Contains(err.Error(), "User is already in this session") {
		t.Errorf("member invite err = %v; want 409", err)
	}
	ghost := &signalClient{serverURL: srv.URL, key: "999999", me: "alice"}
	err = ghost.sendInvite("carol")
	if err == nil || apiStatusCode(err) != 401 ||
		!strings.Contains(err.Error(), "Invalid or missing request signature") {
		t.Errorf("ghost-session invite err = %v; want 401 signature rejection", err)
	}
	if err := alice.sendInvite("carol"); err != nil {
		t.Errorf("valid invite failed: %v", err)
	}
	if fake.fakeInviteCount("carol") != 1 {
		t.Errorf("carol should hold 1 invite; got %d", fake.fakeInviteCount("carol"))
	}
}

// ---------------------------------------------------------------------------
// Settings window: accept / decline flows
// ---------------------------------------------------------------------------

// TestSettingsAcceptOpenGroup drives the ✓ flow on an open group: success
// joins the session, consumes the invite and emits the rootModel handoff.
func TestSettingsAcceptOpenGroup(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()
	code := groupFixture(t, srv, "bob", "alice", "Design", nil)

	m := newSettingsModel(srv.URL, "alice", id, 110, 30)
	m = stepSettings(m, invitesFetchedMsg{invites: []groupInvite{
		{Code: code, GroupName: "Design", By: "bob", At: time.Now().Format(time.RFC3339)},
	}})
	if len(m.invites) != 1 {
		t.Fatalf("invites = %d; want 1", len(m.invites))
	}

	m, cmd := stepSettingsC(m, tea.KeyMsg{Type: tea.KeyEnter}) // ✓ on the row
	next := runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if m.passModal {
		t.Fatal("open group must not open the password modal")
	}
	if m.doneCode != code || m.doneName != "Design" {
		t.Fatalf("done = %s/%s; want %s/Design", m.doneCode, m.doneName, code)
	}
	// Server-side: the invite is consumed and alice is a member.
	if fake.fakeInviteCount("alice") != 0 {
		t.Error("accept must consume the invite")
	}
	fake.mu.Lock()
	_, member := fake.members[code]["alice"]
	fake.mu.Unlock()
	if !member {
		t.Error("alice must be a member of the group after accept")
	}
	// The pending handoff command carries the join payload to rootModel.
	handoff := cmd
	if handoff == nil {
		t.Fatal("accept success must emit the rootModel handoff")
	}
	if got := handoff(); !strings.Contains(fmt.Sprintf("%v", got), code) {
		t.Fatalf("handoff msg = %v; want the session code", got)
	}
}

// TestSettingsAcceptProtectedPasswordModal drives the two-step modal: the
// first accept answers 401 "required" and opens the modal; a wrong password
// retries in place with "incorrect password"; Esc backs out; the right
// password joins.
func TestSettingsAcceptProtectedPasswordModal(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()
	creator, _ := generateIdentity()
	csig := &signalClient{serverURL: srv.URL, me: "bob", id: creator}
	code, err := csig.createGroupRoom("bob", base64.StdEncoding.EncodeToString(creator.publicKey()), "Vault", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	// Password-protect the group + park an invite for alice (test-side
	// seeding of the fake's group plane).
	fake.mu.Lock()
	meta := fake.groupMeta[code]
	meta.pass = "s3cret"
	fake.groupMeta[code] = meta
	fake.invites["alice"] = []groupInvite{{Code: code, GroupName: "Vault", By: "bob", At: time.Now().Format(time.RFC3339)}}
	fake.mu.Unlock()

	m := newSettingsModel(srv.URL, "alice", id, 110, 30)
	m = stepSettings(m, invitesFetchedMsg{invites: fake.invites["alice"]})

	// Step 1: accept without a password → 401 required → modal opens.
	m, cmd := stepSettingsC(m, tea.KeyMsg{Type: tea.KeyEnter})
	next := runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if !m.passModal {
		t.Fatal("protected group must open the password modal after 401-required")
	}
	if m.passErr != "" {
		t.Fatalf("first modal open must be clean, got %q", m.passErr)
	}

	// Wrong password → retry in place.
	m.passInput.SetValue("wrong")
	m, cmd = stepSettingsC(m, tea.KeyMsg{Type: tea.KeyEnter})
	next = runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if !m.passModal || m.passErr != "incorrect password" {
		t.Fatalf("wrong password must stay in the modal with 'incorrect password', got modal=%v err=%q", m.passModal, m.passErr)
	}

	// Esc backs out to the list, password cleared.
	m = stepSettings(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.passModal {
		t.Fatal("Esc must close the modal back to the list")
	}
	if m.passInput.Value() != "" {
		t.Fatal("Esc must clear the modal password")
	}
	if len(m.invites) != 1 {
		t.Fatal("the invite row survives the modal back-out")
	}

	// Re-accept, correct password → joined.
	m, cmd = stepSettingsC(m, tea.KeyMsg{Type: tea.KeyEnter})
	next = runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if !m.passModal {
		t.Fatal("re-accept of a protected group must open the modal again")
	}
	m.passInput.SetValue("s3cret")
	m, cmd = stepSettingsC(m, tea.KeyMsg{Type: tea.KeyEnter})
	next = runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if m.passModal || m.doneCode != code {
		t.Fatalf("correct password must join: modal=%v code=%q want %q", m.passModal, m.doneCode, code)
	}
	if m.donePassword != "s3cret" {
		t.Errorf("done password = %q; the engine needs it for self-rejoin", m.donePassword)
	}
	fake.mu.Lock()
	_, member := fake.members[code]["alice"]
	fake.mu.Unlock()
	if !member {
		t.Error("alice must be a member after the modal accept")
	}
}

// TestSettingsAcceptFullConsumesTerminal proves the max-allowance 403 is
// terminal: the row vanishes with the exact message and a re-accept 404s.
func TestSettingsAcceptFullConsumesTerminal(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()
	creator, _ := generateIdentity()
	csig := &signalClient{serverURL: srv.URL, me: "bob", id: creator}
	// Cap of 2, bob + carol already inside: alice's accept is full.
	max := 2
	code, err := csig.createGroupRoom("bob", base64.StdEncoding.EncodeToString(creator.publicKey()), "Full", "", &max, "123456")
	if err != nil {
		t.Fatal(err)
	}
	csig.key = code
	if err := csig.sendInvite("alice"); err != nil {
		t.Fatal(err)
	}
	carolID, _ := generateIdentity()
	carol := &signalClient{serverURL: srv.URL, key: code, me: "carol"}
	if _, _, err := carol.joinRoom("carol", base64.StdEncoding.EncodeToString(carolID.publicKey()), ""); err != nil {
		t.Fatal(err)
	}

	m := newSettingsModel(srv.URL, "alice", id, 110, 30)
	m = stepSettings(m, invitesFetchedMsg{invites: []groupInvite{
		{Code: code, GroupName: "Full", By: "bob", At: time.Now().Format(time.RFC3339)},
	}})
	m, cmd := stepSettingsC(m, tea.KeyMsg{Type: tea.KeyEnter})
	next := runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if len(m.invites) != 0 {
		t.Fatalf("full accept must consume the row; %d remain", len(m.invites))
	}
	if m.notice != "Maximum allowance is reached" {
		t.Fatalf("notice = %q; want the exact 403 message", m.notice)
	}
	// Terminal: the server consumed it; re-accept 404s.
	if fake.fakeInviteCount("alice") != 0 {
		t.Error("server must consume the invite on the full 403")
	}
	alice := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	if _, _, err := alice.acceptInvite(code, pubkeyB64(id), ""); err == nil ||
		!strings.Contains(err.Error(), "Invite not found") {
		t.Errorf("re-accept must 404, got %v", err)
	}
}

// TestSettingsDecline drives the X flow: 200 consumes the row; a second
// decline 404s (row already gone) without error noise.
func TestSettingsDecline(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()
	code := groupFixture(t, srv, "bob", "alice", "Chess", nil)

	m := newSettingsModel(srv.URL, "alice", id, 110, 30)
	m = stepSettings(m, invitesFetchedMsg{invites: []groupInvite{
		{Code: code, GroupName: "Chess", By: "bob", At: time.Now().Format(time.RFC3339)},
	}})
	// Mouse path: click the ✕ cell of the row (widths must match the
	// handler's lipgloss.Width math — ✕/✓ are multi-byte UTF-8).
	g := m.geom()
	tail := " ✕ ✓"
	tailX := g.left + 2 + g.innerW - lipgloss.Width(tail)
	m, cmd := stepSettingsC(m, tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionPress, X: tailX + 1, Y: g.invFirst})
	next := runCmdStep(t, m, cmd)
	m = next.(settingsModel)
	if len(m.invites) != 0 {
		t.Fatalf("decline must consume the row; %d remain", len(m.invites))
	}
	if fake.fakeInviteCount("alice") != 0 {
		t.Error("server must drop the invite on decline")
	}
	// Terminal: re-decline 404s ("Invite not found") — nothing to do.
	alice := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	if err := alice.declineInvite(code); err == nil ||
		!strings.Contains(err.Error(), "Invite not found") {
		t.Errorf("re-decline must 404, got %v", err)
	}
}

// TestSettingsDeclineErrorsRetainsRow: a transient failure keeps the row
// and surfaces the message on the notice line.
func TestSettingsDeclineErrorsRetainsRow(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, _ := generateIdentity()
	m := newSettingsModel(srv.URL, "alice", id, 110, 30)
	m = stepSettings(m, invitesFetchedMsg{invites: []groupInvite{
		{Code: "700999", GroupName: "Ghost", By: "bob", At: time.Now().Format(time.RFC3339)},
	}})
	m = stepSettings(m, declineDoneMsg{code: "700999", err: &apiStatusError{Code: 500, Msg: "boom"}})
	if len(m.invites) != 1 {
		t.Fatal("failed decline must retain the row")
	}
	if !strings.Contains(m.notice, "boom") {
		t.Fatalf("notice = %q; want the failure surfaced", m.notice)
	}
}

// TestSettingsRender lists the invite rows with their actions; the empty
// state paints too.
func TestSettingsRender(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, _ := generateIdentity()
	m := newSettingsModel(srv.URL, "alice", id, 110, 30)
	m = stepSettings(m, invitesFetchedMsg{invites: []groupInvite{
		{Code: "700001", GroupName: "Design", By: "bob", At: time.Now().Format(time.RFC3339)},
		{Code: "700002", GroupName: "", By: "carol", At: time.Now().Format(time.RFC3339)},
	}})
	view := m.View()
	for _, want := range []string{"◆ SETTINGS", "NOTIFICATIONS", "bob invited you to group Design", "carol invited you to group a group", "✕", "✓", "ACCOUNT"} {
		if !strings.Contains(view, want) {
			t.Errorf("settings view missing %q", want)
		}
	}
	m = stepSettings(m, invitesFetchedMsg{invites: []groupInvite{}})
	if !strings.Contains(m.View(), "No pending invitations.") {
		t.Error("empty settings view must paint the empty state")
	}
}

// ---------------------------------------------------------------------------
// Chat-side badge + beeep
// ---------------------------------------------------------------------------

// TestInviteBadgeAndBeeep proves the 2s poll reconcile: one beeep per new
// invite code (exact body), the badge reflecting the live list length, and
// the ⚙ chip painting in the sidebar header.
func TestInviteBadgeAndBeeep(t *testing.T) {
	var pings []string
	prev := inviteNotifier
	inviteNotifier = func(by, group string) { pings = append(pings, by+"|"+group) }
	defer func() { inviteNotifier = prev }()

	c := groupTestScreen("alice")
	c.applyInvites([]groupInvite{
		{Code: "700001", GroupName: "Design", By: "bob", At: "t"},
	})
	if c.pendingInvites != 1 {
		t.Fatalf("badge = %d; want 1", c.pendingInvites)
	}
	if len(pings) != 1 || pings[0] != "bob|Design" {
		t.Fatalf("pings = %v; want exactly one bob|Design", pings)
	}
	// Re-poll with the same list: no re-ring, badge unchanged.
	c.applyInvites([]groupInvite{
		{Code: "700001", GroupName: "Design", By: "bob", At: "t"},
	})
	if len(pings) != 1 {
		t.Fatalf("re-poll must not re-ring: %v", pings)
	}
	// A second invite rings with the unnamed-group fallback.
	c.applyInvites([]groupInvite{
		{Code: "700002", GroupName: "", By: "carol", At: "t"},
		{Code: "700001", GroupName: "Design", By: "bob", At: "t"},
	})
	if len(pings) != 2 || pings[1] != "carol|a group" {
		t.Fatalf("pings = %v; want carol|a group second", pings)
	}
	if c.pendingInvites != 2 {
		t.Fatalf("badge = %d; want 2", c.pendingInvites)
	}
	if !strings.Contains(c.sidebarHeader(30), "⚙") {
		t.Error("sidebar header must paint the ⚙ badge with invites pending")
	}
	c.applyInvites(nil)
	if c.pendingInvites != 0 {
		t.Fatalf("badge must drop to 0 when the list empties, got %d", c.pendingInvites)
	}
	if strings.Contains(c.sidebarHeader(30), "⚙") {
		t.Error("badge must vanish when no invites are pending")
	}
}

// TestInvitePollCmdRidesRosterTick proves the /invites/mine poll is
// piggybacked on the 2s roster tick against the fake server.
func TestInvitePollCmdRidesRosterTick(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	code := groupFixture(t, srv, "bob", "alice", "Design", nil)
	id, _ := generateIdentity()
	scr := newChatScreen(srv.URL, "123456", "alice", id, "")

	// myInvites surfaces the invite with the group name attached.
	for i := 0; i < 3; i++ {
		sig := &signalClient{serverURL: srv.URL, me: "alice", id: id}
		invites, err := sig.myInvites()
		if err != nil {
			t.Fatal(err)
		}
		if len(invites) == 1 && invites[0].Code == code && invites[0].GroupName == "Design" {
			break
		}
		if i == 2 {
			t.Fatalf("myInvites = %+v; want the fresh invite", invites)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The roster tick spawns the poll command, which resolves over HTTP.
	cmd := scr.pollInvitesCmd()
	if cmd == nil {
		t.Fatal("poll must be armed on wired screens")
	}
	msg := cmd().(invitesPolledMsg)
	if msg.err != nil || len(msg.invites) != 1 || msg.invites[0].Code != code {
		t.Fatalf("poll = %+v err=%v", msg.invites, msg.err)
	}
	scr.applyInvites(msg.invites)
	if scr.pendingInvites != 1 {
		t.Fatalf("badge = %d; want 1 after the poll", scr.pendingInvites)
	}
}

// ---------------------------------------------------------------------------
// Sidebar listing + conversation routing
// ---------------------------------------------------------------------------

// TestGroupSidebarListing pins the sidebar contract the reports asked for:
// joined groups list AFTER the General room and every user row
// ("General, user1, user2, ..., groupName") with a DM-style unread badge,
// and the row peer carries the group: namespace.
func TestGroupSidebarListing(t *testing.T) {
	c := groupTestScreen("alice")
	c.users = []string{"alice", "bob"}
	c.groups["740001"] = &groupSession{code: "740001", name: "Design", unread: 2}
	c.groups["740002"] = &groupSession{code: "740002", name: "Chess Club"}
	c.lastGroupAt["740001"] = time.Now().Add(-time.Minute)
	c.lastGroupAt["740002"] = time.Now()

	items := c.chatItems()
	var groupItems []chatItem
	for _, it := range items {
		if it.isGroup {
			groupItems = append(groupItems, it)
		}
	}
	if len(groupItems) != 2 {
		t.Fatalf("group rows = %d; want 2", len(groupItems))
	}
	if groupItems[0].name != "Chess Club" || groupItems[0].unread != 0 {
		t.Errorf("recency order wrong: %+v", groupItems[0])
	}
	if groupItems[1].name != "Design" || groupItems[1].unread != 2 {
		t.Errorf("Design row must carry its unread: %+v", groupItems[1])
	}
	if groupItems[1].active {
		t.Error("no group may be active in the home view")
	}
	if groupItems[1].peer != "group:740001" {
		t.Errorf("group peer = %q; want the group: prefix namespace", groupItems[1].peer)
	}
	// Full display order: General first, then users, then groups — a group
	// row can never appear above a user row.
	if len(items) < 4 || items[0].name != "General" {
		t.Fatalf("first row must be General, got %+v", items[0])
	}
	seenGroup := false
	for _, it := range items[1:] {
		if it.isGroup {
			seenGroup = true
			continue
		}
		if seenGroup {
			t.Fatalf("user row %q listed after a group row; order must be General, users..., groups", it.name)
		}
	}
	// Unread paints on the group row exactly like a DM row.
	rows := c.chatItemRows(items, 20, "")
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, circledNum(2)) {
		t.Errorf("group unread badge missing from the painted rows: %q", joined)
	}
}

// TestGroupSidebarSwitching drives the open-group switching the reports
// asked for: the keyboard cursor (hover Enter) AND the mouse row click both
// switch into the group conversation, unread clears, and the row is then
// active in the sidebar.
func TestGroupSidebarSwitching(t *testing.T) {
	c := groupTestScreen("alice")
	c.users = []string{"alice", "bob"}
	c.groups["740001"] = &groupSession{code: "740001", name: "Design", unread: 3}
	c.groups["740002"] = &groupSession{code: "740002", name: "Chess"}
	c.width, c.height = 120, 40
	l := c.layoutFor()

	// Keyboard: rail cursor walks General -> bob -> Chess -> Design, Enter
	// on the empty composer opens the highlighted group (Design, unread 3).
	c.moveRosterCursor(1)
	c.moveRosterCursor(1)
	c.moveRosterCursor(1)
	c = stepChat(c, tea.KeyMsg{Type: tea.KeyEnter})
	if c.activeGroup != "740001" {
		t.Fatalf("keyboard Enter must open the group under the cursor, active=%q", c.activeGroup)
	}
	if c.groups["740001"].unread != 0 {
		t.Fatal("opening the group must clear its unread")
	}
	opened := false
	for _, it := range c.chatItems() {
		if it.isGroup && it.name == "Design" && it.active {
			opened = true
		}
	}
	if !opened {
		t.Fatal("the opened group must paint as the active sidebar row")
	}

	// Mouse: click the Chess row (below Design) switches over.
	c = stepChat(c, tea.KeyMsg{Type: tea.KeyEsc}) // back to the room view
	items := c.chatItems()
	chessIdx := -1
	for i, it := range items {
		if it.isGroup && it.name == "Chess" {
			chessIdx = i
		}
	}
	if chessIdx < 0 {
		t.Fatalf("Chess row missing: %+v", items)
	}
	rowY := l.rosterY0 + chessIdx*c.chatItemHeight() + 1 // second row of the entry
	c = stepChat(c, mouseAt(l.rosterX+5, rowY))
	if c.activeGroup != "740002" {
		t.Fatalf("mouse click must open the Chess group, active=%q", c.activeGroup)
	}
	if c.groups["740002"].unread != 0 {
		t.Fatal("mouse-open must clear the group unread")
	}
}

// TestGroupMessageRouting: a background group message lands in the group
// bucket, badges the row, stays out of the home transcript, and paints once
// the group opens (unread clears); Esc returns home.
func TestGroupMessageRouting(t *testing.T) {
	c := groupTestScreen("alice")
	c.users = []string{"alice", "bob"}
	c.groups["740001"] = &groupSession{code: "740001", name: "Design"}

	c = stepChat(c, netChatMsg{key: "740001", chat: engineChat{MsgId: "g1", From: "bob", To: "", Text: "hi team"}})
	if len(c.history) != 1 || c.history[0].ConvID != "group:740001" {
		t.Fatalf("history = %+v; want one message in the group bucket", c.history)
	}
	if c.groups["740001"].unread != 1 {
		t.Fatalf("unread = %d; want 1", c.groups["740001"].unread)
	}
	if c.shouldRender(c.history[0]) {
		t.Error("group message must not render in the home transcript")
	}
	// Sidebar preview + badge.
	found := false
	for _, it := range c.chatItems() {
		if it.isGroup && it.name == "Design" {
			found = true
			if it.unread != 1 || !strings.Contains(it.preview, "hi team") {
				t.Errorf("group row = %+v; want unread 1 + preview", it)
			}
		}
	}
	if !found {
		t.Fatal("Design group missing from the sidebar")
	}
	// Duplicate delivery (at-least-once) must not double-add.
	c = stepChat(c, netChatMsg{key: "740001", chat: engineChat{MsgId: "g1", From: "bob", To: "", Text: "hi team"}})
	if len(c.history) != 1 {
		t.Fatalf("duplicate group message must dedup; history=%d", len(c.history))
	}

	// Open the group: active bucket switches, unread clears, message shows.
	c.openGroup("740001")
	if c.activeGroup != "740001" || c.key != "740001" {
		t.Fatalf("open group state wrong: active=%q key=%q", c.activeGroup, c.key)
	}
	if c.groups["740001"].unread != 0 {
		t.Fatal("opening the group must clear its unread")
	}
	if !c.shouldRender(c.history[0]) {
		t.Error("group message must render once the group is open")
	}
	// The room header names the group.
	if !strings.Contains(c.roomHeaderCompact(80), "Design") {
		t.Error("room header must name the open group")
	}
	// Esc closes the group view (still a member).
	c = stepChat(c, tea.KeyMsg{Type: tea.KeyEsc})
	if c.activeGroup != "" || c.key != "111111" {
		t.Fatalf("Esc must return home: active=%q key=%q", c.activeGroup, c.key)
	}
}

// TestGroupOpenEntryPoints: the sidebar cursor + empty Enter and the mouse
// row click both open a group; Esc returns home.
func TestGroupOpenEntryPoints(t *testing.T) {
	c := groupTestScreen("alice")
	c.users = []string{"alice", "bob"}
	c.groups["740001"] = &groupSession{code: "740001", name: "Design"}
	c.width, c.height = 120, 40
	// Keyboard: move the rail cursor onto the group row, then Enter on the
	// empty composer opens it.
	c.moveRosterCursor(1)
	c.moveRosterCursor(1)
	c = stepChat(c, tea.KeyMsg{Type: tea.KeyEnter})
	if c.activeGroup == "" {
		t.Fatalf("empty Enter must open the hovered entry; hover=%q", c.hoverPeer)
	}
	if c.activeGroup != "740001" {
		t.Fatalf("opened %q; want the Design group", c.activeGroup)
	}
	c = stepChat(c, tea.KeyMsg{Type: tea.KeyEsc})
	if c.activeGroup != "" {
		t.Fatal("Esc must close the group view")
	}
}

// TestGroupBroadcastSendDispatchesIntoGroupBucket: a send while the group is
// open targets the group engine (to="") and settles into the group bucket.
func TestGroupBroadcastSendDispatchesIntoGroupBucket(t *testing.T) {
	c := groupTestScreen("alice")
	c.groups["740001"] = &groupSession{code: "740001", name: "Design"}
	c.openGroup("740001")
	cmd := c.dispatchInConv(c.activeConv(), "hello group")
	if cmd == nil {
		t.Fatal("group send must produce a wire command")
	}
	if c.pending == nil || c.pending.conv != "group:740001" || c.pending.to != "" {
		t.Fatalf("pending = %+v; want the group bucket with a broadcast target", c.pending)
	}
}

// TestGroupConversationHelpers: conv/peer namespacing primitives.
func TestGroupConversationHelpers(t *testing.T) {
	if groupConv("740001") != "group:740001" {
		t.Error("groupConv mismatch")
	}
	if groupCodeOf("group:740001") != "740001" || groupCodeOf("general") != "" || groupCodeOf("a|b") != "" {
		t.Error("groupCodeOf must only match the group namespace")
	}
	if groupPeerCode("group:740001") != "740001" || groupPeerCode("bob") != "" {
		t.Error("groupPeerCode must only match sidebar group rows")
	}
}

// ---------------------------------------------------------------------------
// Join cap surface (plain /join enforces the same 403)
// ---------------------------------------------------------------------------

func TestGroupJoinMaxSurfaces403(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	creator, _ := generateIdentity()
	csig := &signalClient{serverURL: srv.URL, me: "bob"}
	// Cap of 2; bob already inside, carol fills it, alice hits the 403.
	max := 2
	code, err := csig.createGroupRoom("bob", base64.StdEncoding.EncodeToString(creator.publicKey()), "Full", "", &max, "123456")
	if err != nil {
		t.Fatal(err)
	}
	csig.key = code
	carolID, _ := generateIdentity()
	carol := &signalClient{serverURL: srv.URL, key: code, me: "carol"}
	if _, _, err := carol.joinRoom("carol", base64.StdEncoding.EncodeToString(carolID.publicKey()), ""); err != nil {
		t.Fatal(err)
	}
	aliceID, _ := generateIdentity()
	alice := &signalClient{serverURL: srv.URL, key: code, me: "alice"}
	_, _, err = alice.joinRoom("alice", base64.StdEncoding.EncodeToString(aliceID.publicKey()), "")
	if err == nil || !strings.Contains(err.Error(), "Maximum allowance is reached") {
		t.Fatalf("plain join of a full group must surface the exact 403, got %v", err)
	}
	if apiStatusCode(err) != 403 {
		t.Fatalf("status = %d; want 403", apiStatusCode(err))
	}
	_ = code
}

// ---------------------------------------------------------------------------
// Root model: attach + open with a live engine
// ---------------------------------------------------------------------------

// TestRootModelAttachGroupOpensEngine runs the full accept handoff path:
// rootModel attaches the group session (real engine against the fake
// server), opens it, and the engine's heartbeat proves the mesh wiring.
// Ctrl+C leaves the group (app keeps running) and drops it from the
// sidebar.
func TestRootModelAttachGroupOpensEngine(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, _ := generateIdentity()
	scr := newChatScreen(srv.URL, "123456", "alice", id, "")
	scr.sig.key = "" // createRoom mints the room (it parks the code itself)
	if _, err := scr.sig.createRoom("alice", pubkeyB64(id), ""); err != nil {
		t.Fatal(err)
	}
	scr.sig.key = "123456"
	// bob joins the home room so the group has a second member.
	bobID, _ := generateIdentity()
	bob := &signalClient{serverURL: srv.URL, me: "bob"}
	bob.key = "123456"
	if _, _, err := bob.joinRoom("bob", base64.StdEncoding.EncodeToString(bobID.publicKey()), ""); err != nil {
		t.Fatal(err)
	}

	code := groupFixture(t, srv, "bob", "alice", "Design", nil)
	r := newRootModel(scr)

	// The accept handoff message is what the settings window emits.
	nxt, _ := r.Update(acceptGroupDoneMsg{code: code, name: "Design", password: ""})
	r = nxt.(rootModel)
	if r.overlay != nil {
		t.Fatal("accept must close the overlay")
	}
	if r.chat.activeGroup != code || r.chat.key != code {
		t.Fatalf("root chat must be inside the group: active=%q key=%q", r.chat.activeGroup, r.chat.key)
	}
	g := r.chat.groups[code]
	if g == nil || g.eng == nil || g.name != "Design" {
		t.Fatalf("group session not attached: %+v", g)
	}
	// The engine beats: its presence roster seeds (alice + bob).
	deadline := time.Now().Add(3 * time.Second)
	seeded := false
	for time.Now().Before(deadline) {
		if len(g.eng.peers()) >= 2 {
			seeded = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seeded {
		t.Fatalf("group engine never seeded presence: %+v", g.eng.peers())
	}
	// A live group message routes into the group transcript.
	r.chat = stepChat(r.chat, netChatMsg{key: code, chat: engineChat{MsgId: "live1", From: "bob", To: "", Text: "welcome"}})
	if len(r.chat.history) != 1 || r.chat.history[0].ConvID != groupConv(code) {
		t.Fatalf("history = %+v; want the live group message", r.chat.history)
	}
	// Leaving via Ctrl+C leaves the group and returns home (the app keeps
	// running); the sidebar row drops.
	var cmd tea.Cmd
	r.chat, cmd = stepChatC(r.chat, tea.KeyMsg{Type: tea.KeyCtrlC})
	msg := cmd()
	r.chat = stepChat(r.chat, msg)
	if r.chat.activeGroup != "" || r.chat.key != "123456" {
		t.Fatalf("Ctrl+C in a group must return home: active=%q key=%q", r.chat.activeGroup, r.chat.key)
	}
	if _, ok := r.chat.groups[code]; ok {
		t.Fatal("left group must leave the sidebar")
	}
	// Teardown: stop every session engine (home included).
	r.chat.shutdownSessions()
}
