package main

// group_admin_test.go — /group-edit (rename + description, multi-admin
// transfer, creator dissolve behind confirmation), /group-members rendering,
// and the last-admin leave guard.

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func stepGroupEdit(m groupEditModel, msg tea.Msg) groupEditModel {
	nxt, _ := m.Update(msg)
	return nxt.(groupEditModel)
}

func stepGroupEditC(m groupEditModel, msg tea.Msg) (groupEditModel, tea.Cmd) {
	nxt, cmd := m.Update(msg)
	return nxt.(groupEditModel), cmd
}

// groupAdminFixture creates a group owned by bob with alice accepted as a
// member. Returns the code and both identities.
func groupAdminFixture(t *testing.T, srv string) (code string, aliceID, bobID *identityKey) {
	t.Helper()
	var err error
	aliceID, err = generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	bobID, err = generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	bsig := &signalClient{serverURL: srv, me: "bob", id: bobID}
	code, err = bsig.createGroupRoom("bob", pubkeyB64(bobID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := bsig.sendInvite("alice"); err != nil {
		t.Fatal(err)
	}
	asig := &signalClient{serverURL: srv, key: code, me: "alice", id: aliceID}
	if _, _, err := asig.acceptInvite(code, pubkeyB64(aliceID), ""); err != nil {
		t.Fatal(err)
	}
	return code, aliceID, bobID
}

// TestGroupEditRenameDescAndMember403: an admin renames + edits the
// description through the real PATCH route, the window settles on the
// server's response, and the local session + store pick it up immediately.
// A member gets the exact 403 from the server and the read-only window.
func TestGroupEditRenameDescAndMember403(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	code, aliceID, bobID := groupAdminFixture(t, srv.URL)

	// Member: direct client call -> exact 403.
	asig := &signalClient{serverURL: srv.URL, key: code, me: "alice", id: aliceID}
	nope := "Hijack"
	if _, _, err := asig.patchGroupMeta(&nope, nil); err == nil ||
		apiStatusCode(err) != 403 || !strings.Contains(err.Error(), "Only admins can change group details") {
		t.Fatalf("member PATCH = %v; want 403 'Only admins can change group details'", err)
	}
	// Member window: read-only, SAVE refused with the reason and no cmd.
	mm := newGroupEditModel(srv.URL, "alice", aliceID, code, "Design", "", "member",
		[]rosterMember{{Username: "bob", Role: "creator"}}, 110, 40)
	view := mm.View()
	if !strings.Contains(view, "Read-only") || !strings.Contains(view, "only admins can change group details") {
		t.Fatalf("member window must explain the read-only state:\n%s", view)
	}
	mm.focus = geFocusSave
	mm, cmd := stepGroupEditC(mm, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || mm.errMsg == "" {
		t.Fatalf("member SAVE must be refused locally: cmd=%v err=%q", cmd, mm.errMsg)
	}

	// Creator: rename + description through the real fake route.
	cm := newGroupEditModel(srv.URL, "bob", bobID, code, "Design", "", "creator",
		[]rosterMember{{Username: "alice", Role: "member"}}, 110, 40)
	cm.nameInput.SetValue("Design Team")
	cm.descInput.SetValue("the design crew")
	cmd = cm.submitMeta()
	if cmd == nil {
		t.Fatal("changed fields must produce a PATCH command")
	}
	msg := cmd().(groupMetaSavedMsg)
	if msg.err != nil || msg.name != "Design Team" || msg.desc != "the design crew" {
		t.Fatalf("PATCH result = %+v", msg)
	}
	meta, ok := fake.fakeGroup(code)
	if !ok || meta.name != "Design Team" || meta.desc != "the design crew" {
		t.Fatalf("server meta = %+v; want the rename + desc", meta)
	}
	// The editor's window settles on the response.
	cm = stepGroupEdit(cm, msg)
	if cm.name != "Design Team" || !strings.Contains(cm.notice, "saved") {
		t.Fatalf("window after save = name %q notice %q", cm.name, cm.notice)
	}

	// Local propagation: the live session name/desc + store update NOW.
	path := filepath.Join(t.TempDir(), "groups.json")
	scr := groupTestScreen("alice")
	scr.groupsPath = path
	scr.groups[code] = &groupSession{code: code, name: "Design", desc: ""}
	r := newRootModel(scr)
	nxt, _ := r.Update(msg)
	rr := nxt.(rootModel)
	g := rr.chat.groups[code]
	if g == nil || g.name != "Design Team" || g.desc != "the design crew" {
		t.Fatalf("local session not updated: %+v", g)
	}
	stored := loadGroupsForRoom(path, "111111")
	if len(stored) != 1 || stored[0].Name != "Design Team" || stored[0].Desc != "the design crew" {
		t.Fatalf("store not updated: %+v", stored)
	}

	// Invalid fields fail inline before any request.
	cm.nameInput.SetValue("")
	cm.descInput.SetValue("x")
	if cmd := cm.submitMeta(); cmd != nil || cm.errMsg == "" {
		t.Fatal("empty name must fail inline without a request")
	}
}

// TestGroupEditMultiAdminTransfer: the picker toggles several plain members
// and the batch runs one setRole call each (the exact /admin client path),
// with already-admins answering a note instead of a duplicate grant.
func TestGroupEditMultiAdminTransfer(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	code, _, bobID := groupAdminFixture(t, srv.URL)

	// Seed three more members + promote dave so the picker sees an admin.
	for _, u := range []string{"carol", "dave"} {
		uid, _ := generateIdentity()
		usig := &signalClient{serverURL: srv.URL, key: code, me: u, id: uid}
		if _, _, err := usig.joinRoom(u, pubkeyB64(uid), ""); err != nil {
			t.Fatalf("join %s: %v", u, err)
		}
	}
	owner := &signalClient{serverURL: srv.URL, key: code, me: "bob", id: bobID}
	if _, _, err := owner.setRole("dave", true); err != nil {
		t.Fatal(err)
	}

	m := newGroupEditModel(srv.URL, "bob", bobID, code, "Design", "", "creator",
		[]rosterMember{
			{Username: "dave", Role: "admin"},
			{Username: "alice", Role: "member"},
			{Username: "carol", Role: "member"},
		}, 110, 40)

	m.focus = geFocusTransfer
	m, cmd := stepGroupEditC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.mode != geModeAdmins {
		t.Fatalf("Enter on Transfer must open the picker: mode=%d cmd=%v", m.mode, cmd)
	}
	// Display order: Admins [dave], Members [alice, carol].
	m, _ = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeySpace}) // dave: already admin
	if m.notice == "" || len(m.picked) != 0 {
		t.Fatalf("toggling an existing admin must note, not pick: %q %v", m.notice, m.picked)
	}
	m, _ = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeySpace}) // alice
	m, _ = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeySpace}) // carol
	if len(m.picked) != 2 || !m.picked["alice"] || !m.picked["carol"] {
		t.Fatalf("picked = %v; want alice + carol", m.picked)
	}
	m, _ = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.pickApply {
		t.Fatal("Tab must move focus to the APPLY button")
	}
	m, cmd = stepGroupEditC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("APPLY must fire the batch")
	}
	msg := cmd().(groupAdminsAppliedMsg)
	if msg.err != nil || len(msg.promoted) != 2 {
		t.Fatalf("batch result = %+v; want both promoted", msg)
	}
	fake.mu.Lock()
	aliceRole := fake.members[code]["alice"].Role
	carolRole := fake.members[code]["carol"].Role
	daveRole := fake.members[code]["dave"].Role
	fake.mu.Unlock()
	if aliceRole != "admin" || carolRole != "admin" || daveRole != "admin" {
		t.Fatalf("roles alice=%s carol=%s dave=%s; want all admin", aliceRole, carolRole, daveRole)
	}
	// The window settles with notes and reflects the promotions locally.
	m = stepGroupEdit(m, msg)
	if m.mode != geModeForm || !strings.Contains(m.notice, "alice is now an admin") {
		t.Fatalf("window after apply = mode %d notice %q", m.mode, m.notice)
	}
	promotedLocal := false
	for _, mem := range m.members {
		if mem.Username == "alice" && mem.Role == "admin" {
			promotedLocal = true
		}
	}
	if !promotedLocal {
		t.Fatal("promoted members must update locally so they are not offered twice")
	}
}

// TestGroupEditDeleteConfirmationCreatorOnly pins the dissolve contract:
// the Delete action opens a confirmation whose text states exactly what
// happens, Esc cancels, Enter fires the creator DELETE (purge), and an admin
// who is not the creator gets the blocked reason instead.
func TestGroupEditDeleteConfirmationCreatorOnly(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	code, aliceID, bobID := groupAdminFixture(t, srv.URL)

	// Non-creator admin: blocked with the exact reason, no confirmation.
	am := newGroupEditModel(srv.URL, "alice", aliceID, code, "Design", "", "admin",
		[]rosterMember{{Username: "bob", Role: "creator"}}, 110, 40)
	am.focus = geFocusDelete
	am, cmd := stepGroupEditC(am, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || am.mode != geModeForm || !strings.Contains(am.errMsg, "Only the group creator can delete this group") {
		t.Fatalf("non-creator delete must be blocked: mode=%d err=%q cmd=%v", am.mode, am.errMsg, cmd)
	}

	cm := newGroupEditModel(srv.URL, "bob", bobID, code, "Design", "", "creator",
		[]rosterMember{{Username: "alice", Role: "member"}}, 110, 40)
	cm.focus = geFocusDelete
	cm, _ = stepGroupEditC(cm, tea.KeyMsg{Type: tea.KeyEnter})
	if cm.mode != geModeDelete {
		t.Fatalf("creator Delete must open the confirmation, mode=%d", cm.mode)
	}
	view := cm.View()
	if !strings.Contains(view, "DELETE GROUP?") ||
		!strings.Contains(view, "dissolves Design for everyone") ||
		!strings.Contains(view, "members lose access on their next heartbeat") {
		t.Fatalf("confirmation must state the exact dissolve consequences:\n%s", view)
	}

	// Esc backs out; the group is untouched.
	cm, _ = stepGroupEditC(cm, tea.KeyMsg{Type: tea.KeyEsc})
	if cm.mode != geModeForm {
		t.Fatal("Esc must cancel the confirmation")
	}
	if _, ok := fake.fakeGroup(code); !ok {
		t.Fatal("a cancelled confirmation must not delete the group")
	}

	// Enter through the confirmation -> real DELETE -> room purged.
	cm, _ = stepGroupEditC(cm, tea.KeyMsg{Type: tea.KeyEnter}) // reopen
	cm, cmd = stepGroupEditC(cm, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("confirm must fire the DELETE")
	}
	msg := cmd().(groupDeletedMsg)
	if msg.err != nil || msg.code != code || msg.name != "Design" {
		t.Fatalf("delete result = %+v", msg)
	}
	if _, ok := fake.fakeGroup(code); ok {
		t.Fatal("dissolve must purge the group meta")
	}
	fake.mu.Lock()
	_, stillMember := fake.members[code]["alice"]
	fake.mu.Unlock()
	if stillMember {
		t.Fatal("dissolve must purge the roster")
	}
	// A survivor's next join (the heartbeat path's fallback) finds it gone.
	asig := &signalClient{serverURL: srv.URL, key: code, me: "alice", id: aliceID}
	if _, _, err := asig.joinRoom("alice", pubkeyB64(aliceID), ""); err == nil || apiStatusCode(err) != 404 {
		t.Fatalf("survivor join after dissolve = %v; want 404 Session not found", err)
	}

	// rootModel applies the dissolve: live row drops to a tombstone, store
	// prunes, no overlay is left behind.
	path := filepath.Join(t.TempDir(), "groups.json")
	scr := groupTestScreen("alice")
	scr.groupsPath = path
	scr.groups[code] = &groupSession{code: code, name: "Design"}
	r := newRootModel(scr)
	nxt, _ := r.Update(msg)
	rr := nxt.(rootModel)
	if rr.overlay != nil {
		t.Fatal("a successful dissolve must close the edit window")
	}
	if _, live := rr.chat.groups[code]; live {
		t.Fatal("dissolved group must leave the live set")
	}
	if tmb, ok := rr.chat.tombstones[code]; !ok || tmb.name != "Design" {
		t.Fatalf("dissolve must leave a tombstone: %+v", rr.chat.tombstones)
	}
	if got := loadGroupsForRoom(path, "111111"); len(got) != 0 {
		t.Fatalf("dissolve must prune the store, got %+v", got)
	}
	if !strings.Contains(rr.chat.status, "dissolved for everyone") {
		t.Fatalf("status = %q; want the dissolve line", rr.chat.status)
	}
}

// TestGroupLeaveLastAdminBlocked: members may leave any time; an admin may
// leave while another admin remains; the LAST admin is refused with the
// promotion pointer so a group can never be left crownless by accident.
func TestGroupLeaveLastAdminBlocked(t *testing.T) {
	c := groupTestScreen("alice")
	id, _ := generateIdentity()
	c.id = id
	c.netCh = make(chan tea.Msg, 16)
	sig := &signalClient{serverURL: "http://127.0.0.1:1", key: "740001", me: "alice", id: id}
	eng := c.newSessionEngine(sig)
	c.groups["740001"] = &groupSession{code: "740001", name: "Design", sig: sig, eng: eng}
	c.activeGroup = "740001"

	// Last admin (creator alone): blocked with the exact pointer.
	eng.applyPushedRoster([]rosterMember{{Username: "alice", Role: "creator"}}, 1)
	if cmd := c.runCommand("/group-leave", ""); cmd != nil {
		t.Fatal("the last admin must not be able to leave")
	}
	if !strings.Contains(c.status, "promote another admin first") || !strings.Contains(c.status, "/group-edit") {
		t.Fatalf("blocked leave status = %q; want the promotion pointer", c.status)
	}
	c.status = ""

	// A second admin exists: the leave is allowed (cmd armed; not run).
	eng.applyPushedRoster([]rosterMember{
		{Username: "alice", Role: "creator"},
		{Username: "bob", Role: "admin"},
	}, 2)
	if cmd := c.runCommand("/group-leave", ""); cmd == nil {
		t.Fatal("an admin with a peer admin must be allowed to leave")
	}

	// Plain members may always leave.
	eng.applyPushedRoster([]rosterMember{
		{Username: "alice", Role: "member"},
		{Username: "bob", Role: "creator"},
	}, 3)
	if cmd := c.runCommand("/group-leave", ""); cmd == nil {
		t.Fatal("a plain member must be allowed to leave")
	}

	// Outside a group the command answers inline and never arms a leave.
	c.activeGroup = ""
	if cmd := c.runCommand("/group-leave", ""); cmd != nil || !strings.Contains(c.status, "/group-leave") {
		t.Fatalf("outside a group: cmd=%v status=%q", cmd, c.status)
	}
}

// TestGroupMembersRendering: the member list paints in the shared drawer
// craft with role headers, admins first, role tags, and the footer count.
func TestGroupMembersRendering(t *testing.T) {
	m := newGroupMembersModel("740001", "Design", []rosterMember{
		{Username: "carol", Role: "member"},
		{Username: "bob", Role: "creator"},
		{Username: "alice", Role: "admin"},
		{Username: "dave", Role: "member"},
	}, 110, 30)
	view := m.View()
	for _, want := range []string{
		"Members — Design", "esc", "bob", "(main admin)", "alice", "(admin)",
		"carol", "dave", "4 members",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("members view missing %q:\n%s", want, view)
		}
	}
	// Admins first: every admin row paints above every member row.
	bob, alice, carol, dave := strings.Index(view, "bob"), strings.Index(view, "alice"), strings.Index(view, "carol"), strings.Index(view, "dave")
	if bob < 0 || alice < 0 || carol < 0 || dave < 0 {
		t.Fatal("member rows missing")
	}
	if !(bob < carol && alice < carol && carol < dave) {
		t.Fatalf("admins must list first: bob=%d alice=%d carol=%d dave=%d", bob, alice, carol, dave)
	}
	// Esc closes the window.
	nxt, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_ = nxt
	if cmd == nil {
		t.Fatal("Esc must close the members window")
	}
	if _, ok := cmd().(closeOverlayMsg); !ok {
		t.Fatalf("Esc must emit closeOverlayMsg, got %T", cmd())
	}
}
