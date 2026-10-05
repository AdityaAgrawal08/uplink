package main

// chat_settings_choice_test.go — the settings invite inbox's action targets:
// two explicit hit cells per row (✕ decline / ✓ accept), a ←/→ choice cursor
// on the focused row with Enter applying the highlighted target, and the
// mouse/keyboard parity contract. Every test drives rootModel.Update with
// REAL tea.MouseMsg / tea.KeyMsg so the whole rootModel → settings window
// path is exercised, and records which action ran through the fake
// signaling server's state (the same recorder the groups tests use):
// invite consumption, membership and the join handoff.

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// settingsRoot opens /settings through rootModel on a chat screen wired to
// srv and seeds the inbox with invites — the same openSettingsMsg →
// settingsModel.Update path the live app walks.
func settingsRoot(t *testing.T, srv *httptest.Server, invites []groupInvite) rootModel {
	t.Helper()
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	r := newRootModel(newChatScreen(srv.URL, "123456", "alice", id, ""))
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)
	rn, _ = r.Update(openSettingsMsg{})
	r = rn.(rootModel)
	rn, _ = r.Update(invitesFetchedMsg{invites: invites})
	return rn.(rootModel)
}

// settingsOverlay pulls the open settings window out of rootModel for
// geometry and state assertions.
func settingsOverlay(t *testing.T, r rootModel) settingsModel {
	t.Helper()
	sm, ok := r.overlay.(settingsModel)
	if !ok {
		t.Fatalf("overlay = %T; want settingsModel", r.overlay)
	}
	return sm
}

// settingsInvites parks one fixture pair of pending invites: row 0 = Design
// (bob), row 1 = Chess (carol).
func settingsInvites(t *testing.T, srv *httptest.Server) ([]groupInvite, string, string) {
	t.Helper()
	codeA := groupFixture(t, srv, "bob", "alice", "Design", nil)
	codeB := groupFixture(t, srv, "carol", "alice", "Chess", nil)
	invites := []groupInvite{
		{Code: codeA, GroupName: "Design", By: "bob", At: time.Now().UnixMilli()},
		{Code: codeB, GroupName: "Chess", By: "carol", At: time.Now().UnixMilli()},
	}
	return invites, codeA, codeB
}

// viewLineWith returns the painted (ANSI-carrying) line containing sub.
func viewLineWith(t *testing.T, view, sub string) string {
	t.Helper()
	for _, ln := range strings.Split(view, "\n") {
		if strings.Contains(stripANSI(ln), sub) {
			return ln
		}
	}
	t.Fatalf("painted view carries no line with %q:\n%s", sub, view)
	return ""
}

// cellOf returns the terminal CELL index of sub's first occurrence in a
// painted line (byte offsets lie once multi-byte glyphs/borders precede it).
func cellOf(t *testing.T, line, sub string) int {
	t.Helper()
	plain := stripANSI(line)
	i := strings.Index(plain, sub)
	if i < 0 {
		t.Fatalf("painted line carries no %q: %q", sub, plain)
	}
	return lipgloss.Width(plain[:i])
}

// finishAccept drives one accepted invite through the rootModel handoff:
// the acceptDoneMsg resolves, the emitted join handoff runs, and the window
// closes onto the opened group.
func finishAccept(t *testing.T, r rootModel, done acceptDoneMsg) (rootModel, acceptGroupDoneMsg) {
	t.Helper()
	rn, handoff := r.Update(done)
	r = rn.(rootModel)
	if handoff == nil {
		t.Fatal("accept success must emit the rootModel handoff")
	}
	msg := handoff()
	hm, ok := msg.(acceptGroupDoneMsg)
	if !ok {
		t.Fatalf("handoff = %T; want acceptGroupDoneMsg", msg)
	}
	rn, _ = r.Update(hm)
	return rn.(rootModel), hm
}

// TestSettingsInviteMouseWrongCellsInert: the ONLY live cells are the ✕ and
// ✓ glyphs. The row body, both chips' padding, the row edges and the chrome
// around the list run nothing, move nothing and touch nothing server-side.
func TestSettingsInviteMouseWrongCellsInert(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	invites, _, _ := settingsInvites(t, srv)
	r := settingsRoot(t, srv, invites)
	sm := settingsOverlay(t, r)
	g := sm.geom()

	wrong := []struct {
		x, y int
		why  string
	}{
		{g.left + 4, g.invFirst, "row body"},
		{g.declineX - 1, g.invFirst, "decline chip leading pad"},
		{g.declineX + 1, g.invFirst, "decline chip trailing pad"},
		{g.acceptX - 1, g.invFirst, "accept chip leading pad"},
		{g.acceptX + 1, g.invFirst, "accept chip trailing pad"},
		{g.left + 3 + g.rowW, g.invFirst, "row padding past the text area"},
		{g.declineX, g.invFirst - 1, "section chrome above the rows"},
	}
	for _, c := range wrong {
		rn, cmd := r.Update(mouseAt(c.x, c.y))
		r = rn.(rootModel)
		if cmd != nil {
			t.Fatalf("click on the %s must be inert, got a command", c.why)
		}
		got := settingsOverlay(t, r)
		if got.sel != sm.sel || got.choice != sm.choice {
			t.Fatalf("click on the %s must not move the cursor: sel %d→%d choice %d→%d",
				c.why, sm.sel, got.sel, sm.choice, got.choice)
		}
		if n := fake.fakeInviteCount("alice"); n != 2 {
			t.Fatalf("click on the %s must not touch the server: %d invites left", c.why, n)
		}
	}
}

// TestSettingsMouseAcceptTargetActsOnItsRow: ✓ on the SECOND row (the focus
// is on row 0) accepts Chess — the specific row that was clicked — and the
// join handoff opens that group; Design's invite is untouched.
func TestSettingsMouseAcceptTargetActsOnItsRow(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	invites, codeA, codeB := settingsInvites(t, srv)
	r := settingsRoot(t, srv, invites)
	sm := settingsOverlay(t, r)
	g := sm.geom()

	rn, cmd := r.Update(mouseAt(g.acceptX, g.invFirst+1))
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("clicking ✓ must start the accept flow")
	}
	done, ok := cmd().(acceptDoneMsg)
	if !ok || done.code != codeB {
		t.Fatalf("✓ on row 1 must accept Chess (%s), got %#v", codeB, done)
	}
	sm = settingsOverlay(t, r)
	if !sm.busy {
		t.Fatal("the accept attempt must mark the window busy")
	}
	if sm.sel != 1 {
		t.Fatalf("the clicked row must take the focus: sel=%d", sm.sel)
	}

	r, hm := finishAccept(t, r, done)
	if hm.code != codeB || hm.name != "Chess" {
		t.Fatalf("handoff = %+v; want Chess (%s)", hm, codeB)
	}
	if r.overlay != nil {
		t.Fatal("accept must close the settings window")
	}
	if r.chat.activeGroup != codeB {
		t.Fatalf("accept must open the accepted group: active=%q want %q", r.chat.activeGroup, codeB)
	}
	if r.chat.groups[codeB] == nil {
		t.Fatal("the accepted group must be attached")
	}
	defer r.chat.shutdownSessions()

	// Server truth: the clicked row's invite is consumed + joined; the other
	// row survives untouched.
	if fake.fakeHasInvite("alice", codeB) {
		t.Error("accept must consume Chess's invite")
	}
	if !fake.fakeHasInvite("alice", codeA) {
		t.Error("accepting Chess must leave Design's invite pending")
	}
	fake.mu.Lock()
	_, memberB := fake.members[codeB]["alice"]
	_, memberA := fake.members[codeA]["alice"]
	fake.mu.Unlock()
	if !memberB {
		t.Error("alice must be a member of the accepted group")
	}
	if memberA {
		t.Error("alice must NOT have joined the other row's group")
	}
}

// TestSettingsMouseDeclineTargetActsOnItsRow: ✕ on the second row declines
// Chess only; the Design row survives and nothing is joined.
func TestSettingsMouseDeclineTargetActsOnItsRow(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	invites, codeA, codeB := settingsInvites(t, srv)
	r := settingsRoot(t, srv, invites)
	sm := settingsOverlay(t, r)
	g := sm.geom()

	rn, cmd := r.Update(mouseAt(g.declineX, g.invFirst+1))
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("clicking ✕ must start the decline flow")
	}
	dd, ok := cmd().(declineDoneMsg)
	if !ok || dd.code != codeB {
		t.Fatalf("✕ on row 1 must decline Chess (%s), got %#v", codeB, dd)
	}
	rn, _ = r.Update(dd)
	r = rn.(rootModel)
	sm = settingsOverlay(t, r)
	if len(sm.invites) != 1 || sm.invites[0].Code != codeA {
		t.Fatalf("decline must consume only the clicked row: %+v", sm.invites)
	}
	if fake.fakeInviteCount("alice") != 1 || !fake.fakeHasInvite("alice", codeA) {
		t.Error("Design's invite must survive the Chess decline")
	}
	if fake.fakeHasInvite("alice", codeB) {
		t.Error("Chess's invite must be dropped server-side")
	}
	fake.mu.Lock()
	_, memberB := fake.members[codeB]["alice"]
	fake.mu.Unlock()
	if memberB {
		t.Error("declining must never join the group")
	}
}

// TestSettingsKeyboardChoiceDrivesBothTargets: ←/→ move the choice cursor on
// the focused row without acting; Enter applies the highlighted target.
// Down moves the focus, so the ✕ choice declines the SECOND row; the
// remaining row's ✓ accepts and opens its group.
func TestSettingsKeyboardChoiceDrivesBothTargets(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	invites, codeA, codeB := settingsInvites(t, srv)
	r := settingsRoot(t, srv, invites)
	sm := settingsOverlay(t, r)
	if sm.choice != inviteChoiceAccept {
		t.Fatalf("Enter must stay ✓ accept by default: choice=%d", sm.choice)
	}

	// Rows wrap with ↑/↓ (existing binding) and only move the focus.
	rn, cmd := r.Update(tea.KeyMsg{Type: tea.KeyUp})
	r = rn.(rootModel)
	if cmd != nil {
		t.Fatal("↑ must not act")
	}
	if got := settingsOverlay(t, r); got.sel != 1 {
		t.Fatalf("↑ must wrap the focus to the last row: sel=%d", got.sel)
	}
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyDown})
	r = rn.(rootModel)
	if cmd != nil {
		t.Fatal("↓ must not act")
	}
	if got := settingsOverlay(t, r); got.sel != 0 {
		t.Fatalf("↓ must wrap back to the first row: sel=%d", got.sel)
	}

	// The arrows only move the choice.
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyLeft})
	r = rn.(rootModel)
	if cmd != nil {
		t.Fatal("← must not act")
	}
	if got := settingsOverlay(t, r); got.choice != inviteChoiceDecline {
		t.Fatalf("← must choose ✕: choice=%d", got.choice)
	}
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyRight})
	r = rn.(rootModel)
	if cmd != nil {
		t.Fatal("→ must not act")
	}
	if got := settingsOverlay(t, r); got.choice != inviteChoiceAccept {
		t.Fatalf("→ must choose ✓: choice=%d", got.choice)
	}

	// Focus row 1, choose ✕, Enter: declines Chess only.
	rn, _ = r.Update(tea.KeyMsg{Type: tea.KeyDown})
	r = rn.(rootModel)
	rn, _ = r.Update(tea.KeyMsg{Type: tea.KeyLeft})
	r = rn.(rootModel)
	got := settingsOverlay(t, r)
	if got.sel != 1 || got.choice != inviteChoiceDecline {
		t.Fatalf("focused row 1 must carry the ✕ choice: sel=%d choice=%d", got.sel, got.choice)
	}
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("Enter must apply the highlighted choice")
	}
	dd, ok := cmd().(declineDoneMsg)
	if !ok || dd.code != codeB {
		t.Fatalf("Enter on ✕ must decline row 1 (Chess), got %#v", dd)
	}
	rn, _ = r.Update(dd)
	r = rn.(rootModel)
	sm = settingsOverlay(t, r)
	if len(sm.invites) != 1 || sm.invites[0].Code != codeA {
		t.Fatalf("decline must consume the focused row: %+v", sm.invites)
	}

	// → chooses ✓ on the surviving row; Enter accepts Design and opens it.
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyRight})
	r = rn.(rootModel)
	if cmd != nil {
		t.Fatal("→ must not act")
	}
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("Enter on ✓ must start the accept flow")
	}
	done, ok := cmd().(acceptDoneMsg)
	if !ok || done.code != codeA {
		t.Fatalf("Enter on ✓ must accept Design, got %#v", done)
	}
	r, hm := finishAccept(t, r, done)
	if hm.code != codeA {
		t.Fatalf("handoff = %+v; want Design (%s)", hm, codeA)
	}
	if r.overlay != nil || r.chat.activeGroup != codeA {
		t.Fatalf("accept must close the window onto the group: overlay=%v active=%q", r.overlay != nil, r.chat.activeGroup)
	}
	defer r.chat.shutdownSessions()

	if fake.fakeHasInvite("alice", codeA) || fake.fakeHasInvite("alice", codeB) {
		t.Error("both invite rows must be consumed")
	}
	fake.mu.Lock()
	_, memberA := fake.members[codeA]["alice"]
	fake.mu.Unlock()
	if !memberA {
		t.Error("alice must be a member of the accepted group")
	}
}

// TestSettingsMouseAcceptProtectedRunsModal: clicking ✓ on a protected group
// walks the SAME accept flow as the keyboard — the 401 opens the password
// modal and the typed password joins through the handoff.
func TestSettingsMouseAcceptProtectedRunsModal(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	creator, _ := generateIdentity()
	csig := &signalClient{serverURL: srv.URL, me: "bob", id: creator}
	code, err := csig.createGroupRoom("bob", pubkeyB64(creator), "Vault", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	meta := fake.groupMeta[code]
	meta.pass = "s3cret"
	fake.groupMeta[code] = meta
	invite := []groupInvite{{Code: code, GroupName: "Vault", By: "bob", At: time.Now().UnixMilli()}}
	fake.invites["alice"] = invite
	fake.mu.Unlock()

	r := settingsRoot(t, srv, invite)
	sm := settingsOverlay(t, r)
	g := sm.geom()

	rn, cmd := r.Update(mouseAt(g.acceptX, g.invFirst))
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("✓ click must start the accept flow")
	}
	done := cmd().(acceptDoneMsg)
	if done.err == nil {
		t.Fatal("fixture: the protected accept must answer 401")
	}
	rn, _ = r.Update(done)
	r = rn.(rootModel)
	sm = settingsOverlay(t, r)
	if !sm.passModal {
		t.Fatal("the 401-required answer must open the password modal")
	}

	// Type the password and submit exactly like the keyboard flow.
	for _, ch := range "s3cret" {
		rn, _ = r.Update(teaRune(ch))
		r = rn.(rootModel)
	}
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("Enter in the modal must submit the password")
	}
	done = cmd().(acceptDoneMsg)
	if done.err != nil || done.code != code {
		t.Fatalf("correct password must join: %+v", done)
	}
	r, hm := finishAccept(t, r, done)
	if hm.code != code || hm.password != "s3cret" {
		t.Fatalf("handoff = %+v; want the password carried for self-rejoin", hm)
	}
	if r.overlay != nil || r.chat.activeGroup != code {
		t.Fatalf("modal accept must close onto the group: overlay=%v active=%q", r.overlay != nil, r.chat.activeGroup)
	}
	defer r.chat.shutdownSessions()
}

// TestSettingsFocusAndChoicePaint: the focused row carries the cursor marker
// and its ACTIVE choice wears the shared cursor-bar style (tuiPaletteSelStyle);
// the other chip and every unfocused row stay in the resting tone. ←/→ flips
// the bar between ✕ and ✓, ↓ moves the focus (and the carried choice) down.
func TestSettingsFocusAndChoicePaint(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	srv, _ := newGroupTestServer(t)
	invites, _, _ := settingsInvites(t, srv)
	r := settingsRoot(t, srv, invites)
	g := settingsOverlay(t, r).geom()

	activeAccept := tuiPaletteSelStyle.Render(" ✓ ")
	activeDecline := tuiPaletteSelStyle.Render(" ✕ ")

	// Focused row 0, default choice ✓.
	design := viewLineWith(t, r.View(), "bob invited you to group Design")
	chess := viewLineWith(t, r.View(), "carol invited you to group Chess")
	if !strings.Contains(design, activeAccept) {
		t.Fatalf("focused row's active ✓ must wear the cursor bar: %q", design)
	}
	if strings.Contains(design, activeDecline) {
		t.Fatalf("the inactive ✕ must not wear the bar: %q", design)
	}
	if strings.Contains(chess, activeAccept) || strings.Contains(chess, activeDecline) {
		t.Fatalf("an unfocused row must not wear a choice bar: %q", chess)
	}
	if !strings.Contains(stripANSI(design), "> bob invited") {
		t.Fatalf("focused row must carry the cursor marker: %q", stripANSI(design))
	}
	if strings.Contains(stripANSI(chess), "> carol invited") {
		t.Fatalf("unfocused row must not carry the cursor marker: %q", stripANSI(chess))
	}
	// The painted ✓ cell IS the geometry's hit cell (paint ↔ hit-test truth).
	if x := cellOf(t, design, "✓"); x != g.acceptX {
		t.Fatalf("painted ✓ cell = %d; geometry accept cell = %d", x, g.acceptX)
	}

	// ← flips the bar onto ✕, at the geometry's decline cell.
	rn, _ := r.Update(tea.KeyMsg{Type: tea.KeyLeft})
	r = rn.(rootModel)
	design = viewLineWith(t, r.View(), "bob invited you to group Design")
	if !strings.Contains(design, activeDecline) {
		t.Fatalf("← must move the bar onto ✕: %q", design)
	}
	if strings.Contains(design, activeAccept) {
		t.Fatalf("← must clear the ✓ bar: %q", design)
	}
	if x := cellOf(t, design, "✕"); x != g.declineX {
		t.Fatalf("painted ✕ cell = %d; geometry decline cell = %d", x, g.declineX)
	}

	// ↓ moves the focus (carrying the ✕ choice) onto the Chess row.
	rn, _ = r.Update(tea.KeyMsg{Type: tea.KeyDown})
	r = rn.(rootModel)
	design = viewLineWith(t, r.View(), "bob invited you to group Design")
	chess = viewLineWith(t, r.View(), "carol invited you to group Chess")
	if strings.Contains(design, activeAccept) || strings.Contains(design, activeDecline) {
		t.Fatalf("the unfocused Design row must lose the bar: %q", design)
	}
	if !strings.Contains(chess, activeDecline) {
		t.Fatalf("the focused Chess row must carry the ✕ bar: %q", chess)
	}
	if !strings.Contains(stripANSI(chess), "> carol invited") {
		t.Fatalf("the focused Chess row must carry the cursor marker: %q", stripANSI(chess))
	}
	if strings.Contains(stripANSI(design), "> bob invited") {
		t.Fatalf("the unfocused Design row must lose the marker: %q", stripANSI(design))
	}
}

// TestSettingsEscClosesWithoutActing: Esc still closes the window and no
// invite is touched.
func TestSettingsEscClosesWithoutActing(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	invites, _, _ := settingsInvites(t, srv)
	r := settingsRoot(t, srv, invites)

	rn, cmd := r.Update(tea.KeyMsg{Type: tea.KeyEsc})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("Esc must close the settings window")
	}
	rn, _ = r.Update(cmd())
	r = rn.(rootModel)
	if r.overlay != nil {
		t.Fatal("Esc must close the overlay")
	}
	if fake.fakeInviteCount("alice") != 2 {
		t.Fatal("Esc must not act on any invite")
	}
}
