package main

import (
	"net/http/httptest"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestGroupFormTypingRepro drives the REAL /new-group path end to end:
// member-pick stage → form stage → typing into every field THROUGH the
// model's Update (not by poking the textinputs directly). Regression for
// the reported bug: keystrokes never reach the Name/Description/Max-People
// fields.
func TestGroupFormTypingRepro(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	members := []rosterMember{
		{Username: "bob", Role: "member"},
		{Username: "carol", Role: "member"},
	}
	m := newGroupModel(srv.URL, "alice", id, "123456", 110, 40, members)

	// Stage 1: pick a member, Enter to the form.
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeySpace}) // toggle bob (sel 0)
	if !m.picked["bob"] {
		t.Fatal("space must pick the highlighted member")
	}
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.stage != groupStageForm {
		t.Fatalf("enter must open the form stage, got stage %d", m.stage)
	}

	// Name field: type through Update, rune by rune (the real key path).
	for _, r := range []rune("Design Team") {
		m = stepGroup(m, teaRune(r))
	}
	if got := m.nameInput.Value(); got != "Design Team" {
		t.Fatalf("name field = %q; want %q (typing through Update must reach the field)", got, "Design Team")
	}

	// Tab must move to Description and typing must land there.
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.formFocus != 1 {
		t.Fatalf("tab must move focus to Description, got focus %d", m.formFocus)
	}
	for _, r := range []rune("our design sync") {
		m = stepGroup(m, teaRune(r))
	}
	if got := m.descInput.Value(); got != "our design sync" {
		t.Fatalf("description field = %q; want %q", got, "our design sync")
	}

	// Tab to Max-People, type digits.
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.formFocus != 2 {
		t.Fatalf("tab must move focus to Max-People, got focus %d", m.formFocus)
	}
	for _, r := range []rune("12") {
		m = stepGroup(m, teaRune(r))
	}
	if got := m.maxInput.Value(); got != "12" {
		t.Fatalf("max field = %q; want %q", got, "12")
	}

	// Enter on the last field moves to Create; Enter again submits and
	// produces the create command (inline validation passes).
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.formFocus != 3 {
		t.Fatalf("enter on Max-People must move to Create, got focus %d", m.formFocus)
	}
	_, cmd := stepGroupC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on Create must produce a create command")
	}
	done := cmd().(createGroupDoneMsg)
	if done.code == "" || done.name != "Design Team" {
		t.Fatalf("create done = %+v; want a code and the typed name", done)
	}

	// Esc backs out per stage: from the form it returns to the member
	// pick (picks kept); from the pick it closes the window.
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.stage != groupStagePick {
		t.Fatal("esc in the form must return to the member pick stage")
	}
	if !m.picked["bob"] {
		t.Fatal("picks must survive esc back to the member pick")
	}
	_, cmd = stepGroupC(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in the pick stage must produce a close command")
	}
	var closeMsg closeOverlayMsg
	if got := cmd().(closeOverlayMsg); got != closeMsg {
		t.Fatalf("esc close command = %#v", got)
	}
}

// TestGroupFormEscStageTransitionRepro proves blur/focus sync across the
// pick→form→pick boundary: after Esc back to the pick stage, Enter must
// re-enter the form with the name field focused and typing live again.
func TestGroupFormEscStageTransitionRepro(t *testing.T) {
	members := []rosterMember{{Username: "bob", Role: "member"}}
	m := newGroupModel("", "alice", nil, "123456", 110, 40, members)
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // to form
	for _, r := range []rune("AB") {
		m = stepGroup(m, teaRune(r))
	}
	if got := m.nameInput.Value(); got != "AB" {
		t.Fatalf("name before esc = %q; want AB", got)
	}
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEsc}) // back to pick
	if m.stage != groupStagePick {
		t.Fatal("esc must return to pick stage")
	}
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // re-enter form
	if m.stage != groupStageForm || m.formFocus != 0 {
		t.Fatalf("re-enter form: stage=%d focus=%d; want form focus 0", m.stage, m.formFocus)
	}
	if !m.nameInput.Focused() {
		t.Fatal("name input must be focused after re-entering the form")
	}
	for _, r := range []rune("C") {
		m = stepGroup(m, teaRune(r))
	}
	if got := m.nameInput.Value(); got != "ABC" {
		t.Fatalf("name after re-enter = %q; want ABC (typing must survive the stage round-trip)", got)
	}
}

// TestNewGroupEndToEndThroughRootModel drives the REAL user path for the
// /new-group form typing bug: type "/new-group" in the composer, pick it in
// the palette (Enter), the root model opens the window, pick a member, and
// type into every form field — all through the program model, exactly like
// the live app. Regression for keystrokes never reaching the fields.
func TestNewGroupEndToEndThroughRootModel(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	t.Cleanup(srv.Close)
	c := newFilterScreen("alice", "")
	c.vp = *viewportPtr(80, 24)
	wireTestEngine(t, c, srv, "alice", "bob")
	c.eng.beatOnce()
	c.key = c.sig.key // the home session the /new-group window nests under
	c.id = c.sig.id   // the creator's device identity (signed create)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	scr := m.(chatScreen)
	r := newRootModel(scr)

	// Type "/new-group" into the composer; the "/" palette opens and
	// ranks it.
	sc, _ := typeKeys(&scr, "/new-group")
	r = newRootModel(sc)
	if !r.chat.palette.visible() {
		t.Fatal("typing /new-group must open the command palette")
	}
	// Enter in the palette runs the highlighted command → openNewGroupMsg.
	var cmd tea.Cmd
	rn, cmd := r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("selecting /new-group must produce the window-open command")
	}
	rn = runCmdStep(t, r, cmd)
	r = rn.(rootModel)
	gm, ok := r.overlay.(groupModel)
	if !ok {
		t.Fatalf("overlay = %T; want groupModel (the /new-group window)", r.overlay)
	}
	if gm.stage != groupStagePick {
		t.Fatalf("window must open on the member pick, stage=%d", gm.stage)
	}
	if len(gm.members) == 0 {
		t.Fatal("window must list room members to invite")
	}

	// Pick bob with Space, Enter to the form — keys still go through the
	// root model.
	rn, _ = r.Update(tea.KeyMsg{Type: tea.KeySpace})
	r = rn.(rootModel)
	if !r.overlay.(groupModel).picked["bob"] {
		t.Fatal("space must pick the highlighted member")
	}
	rn, _ = r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if r.overlay.(groupModel).stage != groupStageForm {
		t.Fatal("enter must move the window to the form stage")
	}

	// Type into Name: every keystroke must reach the field through the
	// root model's overlay routing.
	for _, ch := range []rune("Design Team") {
		r = stepRoot(r, teaRune(ch))
	}
	if got := r.overlay.(groupModel).nameInput.Value(); got != "Design Team" {
		t.Fatalf("name = %q; want %q (keystrokes must reach the form through rootModel)", got, "Design Team")
	}
	// Tab to Description, type; Tab to Max-People, type digits.
	r = stepRoot(r, tea.KeyMsg{Type: tea.KeyTab})
	for _, ch := range []rune("our design sync") {
		r = stepRoot(r, teaRune(ch))
	}
	r = stepRoot(r, tea.KeyMsg{Type: tea.KeyTab})
	for _, ch := range []rune("12") {
		r = stepRoot(r, teaRune(ch))
	}
	gm = r.overlay.(groupModel)
	if got := gm.descInput.Value(); got != "our design sync" {
		t.Fatalf("description = %q", got)
	}
	if got := gm.maxInput.Value(); got != "12" {
		t.Fatalf("max = %q", got)
	}
	// Enter on the last field moves to Create; Enter submits and closes
	// the window into a created group.
	r = stepRoot(r, tea.KeyMsg{Type: tea.KeyEnter})
	if r.overlay.(groupModel).formFocus != 3 {
		t.Fatalf("enter on Max-People must move to Create, focus=%d", r.overlay.(groupModel).formFocus)
	}
	rn, cmd = r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	r = rn.(rootModel)
	if cmd == nil {
		t.Fatal("enter on Create must submit the form")
	}
	rn = runCmdStep(t, r, cmd)
	r = rn.(rootModel)
	if r.overlay != nil {
		t.Fatal("a successful create must close the window")
	}
	if r.chat.activeGroup == "" || r.chat.key != r.chat.activeGroup {
		t.Fatalf("the created group must open: active=%q key=%q", r.chat.activeGroup, r.chat.key)
	}
	r.chat.shutdownSessions()
}

// stepRoot runs one Update on a rootModel and returns the new rootModel.
func stepRoot(r rootModel, msg tea.Msg) rootModel {
	nxt, _ := r.Update(msg)
	return nxt.(rootModel)
}

// TestGroupFormBackspaceRepro proves editing keys (backspace) reach the
// focused field through Update too, not just runes.
func TestGroupFormBackspaceRepro(t *testing.T) {
	members := []rosterMember{{Username: "bob", Role: "member"}}
	m := newGroupModel("", "alice", nil, "123456", 110, 40, members)
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEnter}) // to form
	for _, r := range []rune("abc") {
		m = stepGroup(m, teaRune(r))
	}
	if got := m.nameInput.Value(); got != "abc" {
		t.Fatalf("typed = %q; want abc", got)
	}
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if got := m.nameInput.Value(); got != "ab" {
		t.Fatalf("after backspace = %q; want ab", got)
	}
	if m.formFocus != 0 {
		t.Fatalf("backspace must stay in the focused field, got %d", m.formFocus)
	}
}
