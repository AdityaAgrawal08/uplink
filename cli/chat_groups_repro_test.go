package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestGroupFormTypingRepro drives the REAL /new-group path end to end:
// the window opens DIRECTLY on the creation form (no member-pick stage —
// members are added later through /invite) and typing into every field
// works THROUGH the model's Update (not by poking the textinputs directly).
// Regression for the reported bug: keystrokes never reach the
// Name/Description/Max-People fields.
func TestGroupFormTypingRepro(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, err := generateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	m := newGroupModel(srv.URL, "alice", id, "123456", 110, 40)

	// The window opens on the form, Name field focused.
	if !m.nameInput.Focused() || m.formFocus != 0 {
		t.Fatalf("window must open on the Name field, focus=%d", m.formFocus)
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

	// Esc closes the window outright — there is no earlier stage to return
	// to (the member multi-select stage is gone).
	m = stepGroup(m, tea.KeyMsg{Type: tea.KeyEsc})
	_, cmd = stepGroupC(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc must produce a close command")
	}
	var closeMsg closeOverlayMsg
	if got := cmd().(closeOverlayMsg); got != closeMsg {
		t.Fatalf("esc close command = %#v", got)
	}
}

// TestGroupFormBackspaceRepro proves editing keys (backspace) reach the
// focused field through Update too, not just runes.
func TestGroupFormBackspaceRepro(t *testing.T) {
	m := newGroupModel("", "alice", nil, "123456", 110, 40)
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

// TestNewGroupEndToEndThroughRootModel drives the REAL user path for the
// /new-group form typing bug: type "/new-group" in the composer, pick it in
// the palette (Enter), the root model opens the window DIRECTLY on the
// creation form (no member-pick stage), and typing into every field works —
// all through the program model, exactly like the live app.
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
	// The window opens DIRECTLY on the creation form: Name focused, no
	// member multi-select stage in between.
	if gm.formFocus != 0 || !gm.nameInput.Focused() {
		t.Fatalf("window must open on the Name field, focus=%d", gm.formFocus)
	}
	if !strings.Contains(gm.View(), "NEW GROUP") {
		t.Fatal("window must paint the creation form immediately")
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
