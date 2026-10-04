package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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
// on chrome rows (the group header, the title) do nothing.
func TestInviteWindowMouseParity(t *testing.T) {
	srv, c, code, _ := inviteFixture(t)
	fake := srv.Config.Handler.(*fakeSignalServer)
	m := settleInviteReveal(newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height))
	g := m.geom()
	// The grouped plan paints a "Members" header above the candidates:
	// row +0 is chrome, row +1 is carol, row +2 is dave.
	wrows, _, _ := m.inviteWindowRows()
	if wrows[1].kind != drItem {
		t.Fatalf("fixture: row +1 must be an item row, got %v", wrows[1].kind)
	}

	// Click the third painted row (dave): selects him AND toggles him.
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst+2))
	if m.sel != 1 || !m.picked["dave"] {
		t.Fatalf("row click must select+toggle dave: sel=%d picked=%v", m.sel, m.picked)
	}
	// Click the same row again: UN-toggles (toggle semantics).
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst+2))
	if m.picked["dave"] {
		t.Fatal("second click must un-toggle dave")
	}
	// The highlight stayed on dave (row 2): keyboard Enter re-picks him,
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
	// Chrome click (group header row): no selection change, no send.
	before := m.sel
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst))
	if m.sel != before {
		t.Fatalf("header click must not move the selection: sel=%d", m.sel)
	}
	// Chrome click (title row): no selection change, no send.
	m = stepInvite(m, mouseAt(g.left+5, g.rowFirst-2))
	if m.sel != before {
		t.Fatalf("chrome click must not move the selection: sel=%d", m.sel)
	}
}

// settleInviteReveal drives the window's staged open reveal to its settled
// frame — the paint/mouse tests run the window post-open, exactly like the
// reaction dropdown tests arm the reveal ticks explicitly.
func settleInviteReveal(m inviteModel) inviteModel {
	for frame := 1; frame < inviteRevealFrames; frame++ {
		m = stepInvite(m, inviteRevealMsg{gen: m.revealGen, frame: frame})
	}
	return m
}

// sgrBefore asserts the SGR sequence seq is the LAST paint BEFORE the
// glyph at byte index i in raw — i.e. the terminal renders that glyph with
// seq's ink, and no later override can steal it. (A bare "contains the
// glyph + contains the sequence somewhere" check passes even when the ink
// never reaches the glyph; this is the honest ink-order assertion.)
func sgrBefore(raw string, i int, seq string) bool {
	if i < 0 || i >= len(raw) || seq == "" {
		return false
	}
	return strings.HasSuffix(raw[:i], seq)
}

// TestInviteRowPickVisuals pins the three-state row language: the plain
// row, the focus CURSOR bar, the picked ACCENT bar with the bright ✓ and
// bold name, the focused+picked overlap (cursor bar wins the fill, the ✓
// keeps its bright ink — the old accent-on-accent invisibility), and the
// one-tick pulse flash.
func TestInviteRowPickVisuals(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256) // SGR sequences needed to assert the bars
	defer lipgloss.SetColorProfile(prev)

	base := inviteModel{
		cands:  []rosterMember{{Username: "carol", Role: "admin"}},
		picked: map[string]bool{},
		sel:    0,
	}
	row := func(m inviteModel) string { return m.inviteItemRowView(0, fuzzyHit{}, 60) }
	pickSeq := styleSeq(tuiPalettePickStyle)
	selSeq := styleSeq(tuiPaletteSelStyle)
	matchSelSeq := styleSeq(tuiPaletteMatchSelStyle)

	// Plain, unpicked, unfocused: no check, no bar.
	plain := row(base)
	if strings.Contains(stripANSI(plain), "✓") {
		t.Fatalf("unpicked row must not paint a check: %q", stripANSI(plain))
	}
	if pickSeq != "" && strings.Contains(plain, pickSeq) {
		t.Fatal("plain row must not wear the picked bar")
	}

	// Focused: the standard full-row cursor bar.
	focused := row(inviteModel{cands: base.cands, picked: map[string]bool{}, sel: 0})
	if selSeq == "" || !strings.Contains(focused, selSeq) {
		t.Fatalf("focused row must wear the cursor bar: %q", focused)
	}

	// Picked: the accent bar, bright ✓, bold name (the bar style bolds).
	picked := row(inviteModel{cands: base.cands, picked: map[string]bool{"carol": true}, sel: -1})
	if pickSeq == "" || !strings.Contains(picked, pickSeq) {
		t.Fatalf("picked row must wear the accent picked bar: %q", picked)
	}
	if !strings.Contains(stripANSI(picked), "✓") {
		t.Fatalf("picked row must paint the ✓: %q", stripANSI(picked))
	}
	// The ✓ must be painted BY the bright ink sequence (SGR order, not
	// mere presence): the ink right before the glyph is matchSel's.
	if i := strings.Index(picked, "✓"); !sgrBefore(picked, i, matchSelSeq) {
		t.Fatalf("the ✓ must ride the bright contrast ink (SGR directly before the glyph): %q", picked)
	}

	// Focused AND picked: the cursor bar wins the fill, the ✓ stays bright
	// (this exact spot used to render accent-on-accent and vanish).
	both := row(inviteModel{cands: base.cands, picked: map[string]bool{"carol": true}, sel: 0})
	if !strings.Contains(both, selSeq) {
		t.Fatalf("focused+picked row must keep the cursor bar: %q", both)
	}
	if pickSeq != "" && strings.Contains(both, pickSeq) {
		t.Fatal("the picked bar must never override the cursor bar")
	}
	if i := strings.Index(both, "✓"); i < 0 || !sgrBefore(both, i, matchSelSeq) {
		t.Fatalf("the ✓ must stay bright-ink on the cursor bar (SGR directly before the glyph): %q", both)
	}

	// Pulse flash: the just-toggled row wears the pulse style, not the bar.
	flashing := inviteModel{
		cands:      base.cands,
		picked:     map[string]bool{"carol": true},
		sel:        -1,
		pulseItem:  0,
		pulseGen:   1,
		pulseFrame: 0,
	}
	flash := row(flashing)
	if seq := styleSeq(tuiPalettePulseStyle); seq == "" || !strings.Contains(flash, seq) {
		t.Fatalf("flash frame must wear the pulse style: %q", flash)
	}
	if i := strings.Index(flash, "✓"); i < 0 || !sgrBefore(flash, i, matchSelSeq) {
		t.Fatalf("the flash must keep the ✓ bright: %q", flash)
	}
	// The pulse only touches its own row — a different item paints normal.
	other := flashing
	other.pulseItem = 7
	if strings.Contains(row(other), styleSeq(tuiPalettePulseStyle)) {
		t.Fatal("the pulse must only flash its row")
	}
}

// TestInviteSelectionPulse: every toggle — keyboard Enter, Shift+Down
// range Enter, mouse click — arms the generation-fenced 1-tick pulse on
// the toggled row; the last frame settles into the picked bar; stale
// generations stay inert; plain/CI runs settle immediately with no timers.
func TestInviteSelectionPulse(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	_, c, code, _ := inviteFixture(t)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	m.animations = true
	m = settleInviteReveal(m)

	// Keyboard toggle arms the pulse on the toggled row (sel 0 = carol).
	m, cmd := stepInviteC(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a toggle must arm the selection pulse tick")
	}
	if m.pulseItem != 0 || m.pulseFrame != 0 || m.pulseGen != 1 {
		t.Fatalf("pulse = item %d frame %d gen %d; want 0/0/1", m.pulseItem, m.pulseFrame, m.pulseGen)
	}
	// Frame 0 paints the flash; the frame-1 tick settles the state.
	flash := m.inviteItemRowView(m.pulseItem, m.invitePlan().hitOf(m.pulseItem), 60)
	if seq := styleSeq(tuiPalettePulseStyle); seq == "" || !strings.Contains(flash, seq) {
		t.Fatalf("flash frame must paint the pulse style: %q", flash)
	}
	m = stepInvite(m, invitePulseMsg{gen: m.pulseGen, frame: 1})
	if m.pulseItem != -1 || m.pulseFrame != 1 {
		t.Fatalf("settle frame must clear the pulse: item=%d frame=%d", m.pulseItem, m.pulseFrame)
	}

	// Range toggle pulses the range's END row (the highlight).
	m, c1 := stepInviteC(m, tea.KeyMsg{Type: tea.KeyShiftDown}) // anchor 0, sel 1
	if c1 != nil {
		t.Fatal("a plain move must not arm a pulse")
	}
	m, cmd = stepInviteC(m, tea.KeyMsg{Type: tea.KeyEnter}) // toggles 0..1
	if cmd == nil {
		t.Fatal("a range toggle must arm the pulse")
	}
	if m.pulseItem != 1 || m.pulseGen != 2 {
		t.Fatalf("range pulse = item %d gen %d; want 1/2", m.pulseItem, m.pulseGen)
	}
	m = stepInvite(m, invitePulseMsg{gen: m.pulseGen, frame: 1})

	// Mouse click toggle pulses the clicked row (dave, now repainted).
	g := m.geom()
	wrows, _, _ := m.inviteWindowRows()
	daveY := -1
	for i, r := range wrows {
		if r.kind == drItem && m.cands[r.item].Username == "dave" {
			daveY = g.rowFirst + i
			break
		}
	}
	if daveY < 0 {
		t.Fatal("fixture: dave row must be painted")
	}
	m, cmd = stepInviteC(m, mouseAt(g.left+5, daveY))
	if cmd == nil {
		t.Fatal("a mouse toggle must arm the pulse")
	}
	if m.pulseItem != 1 || m.pulseGen != 3 {
		t.Fatalf("mouse pulse = item %d gen %d; want 1/3", m.pulseItem, m.pulseGen)
	}
	// A stale generation's tick stays inert (never settles the newer pulse).
	m = stepInvite(m, invitePulseMsg{gen: 1, frame: 1})
	if m.pulseItem != 1 || m.pulseFrame != 0 {
		t.Fatalf("a stale pulse tick must never settle a newer pulse: item=%d frame=%d", m.pulseItem, m.pulseFrame)
	}
	m = stepInvite(m, invitePulseMsg{gen: m.pulseGen, frame: 1})

	// Plain runs settle immediately, no timers.
	p := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	p.animations = false
	p2, cmd := stepInviteC(p, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("plain runs must not arm pulse ticks")
	}
	if p2.pulseItem != 0 || p2.pulseFrame != invitePulseFrames-1 {
		t.Fatalf("plain settle = item %d frame %d; want 0/%d", p2.pulseItem, p2.pulseFrame, invitePulseFrames-1)
	}
}

// TestInviteFooterLiveCounter pins the spec footer verbatim: "n selected —
// Enter to send" in accent emphasis once ANY pick exists, "0 selected" in
// the hint tone before that, hints kept left, one row wide.
func TestInviteFooterLiveCounter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	inner := 76
	f0 := inviteFooterRow(inner, 0)
	s0 := stripANSI(f0)
	if !strings.Contains(s0, "0 selected") || strings.Contains(s0, "Enter to send") {
		t.Fatalf("empty footer = %q; want 0 selected, no send affordance", s0)
	}
	if seq := styleSeq(tuiPaletteHintStyle); seq != "" && !strings.Contains(f0, seq) {
		t.Fatal("the 0 counter rides the hint tone")
	}

	f2 := inviteFooterRow(inner, 2)
	s2 := stripANSI(f2)
	if !strings.Contains(s2, "2 selected — Enter to send") {
		t.Fatalf("picked footer = %q; want the spec counter", s2)
	}
	if seq := styleSeq(lipgloss.NewStyle().Bold(true).Foreground(colAccent)); seq != "" && !strings.Contains(f2, seq) {
		t.Fatal("the picked counter must ride accent bold")
	}
	if !strings.Contains(s2, "enter toggle") {
		t.Fatalf("the keymap hints must survive the counter at this width: %q", s2)
	}
	if w := lipgloss.Width(f2); w > inner {
		t.Fatalf("footer width %d > %d", w, inner)
	}

	// Live through the window: the toggle moves the counter 0 → 1 → 2.
	_, c, code, _ := inviteFixture(t)
	m := settleInviteReveal(newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height))
	view := func() string { return stripANSI(m.View()) }
	if !strings.Contains(view(), "0 selected") {
		t.Fatalf("window must open with 0 selected:\n%s", view())
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter}) // pick carol
	if !strings.Contains(view(), "1 selected — Enter to send") {
		t.Fatalf("counter must track the first pick:\n%s", view())
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown})
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter}) // pick dave
	if !strings.Contains(view(), "2 selected — Enter to send") {
		t.Fatalf("counter must track the second pick:\n%s", view())
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter}) // un-pick dave
	if !strings.Contains(view(), "1 selected — Enter to send") {
		t.Fatalf("counter must shrink on an un-toggle:\n%s", view())
	}
}

// TestInviteWindowRevealStaged: the open reveal is staged over tea.Tick
// frames — frame 0 paints bare chrome, each 50ms tick reveals more rows,
// the last frame is the settled window. Stale generations stay inert, and
// no-animation (plain/CI) models paint fully expanded with no timers.
func TestInviteWindowRevealStaged(t *testing.T) {
	_, c, code, _ := inviteFixture(t)
	m := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	m.animations = true

	// Init arms the first reveal tick; the window starts at frame 0.
	if cmd := m.Init(); cmd == nil {
		t.Fatal("animated open must arm the reveal tick")
	}
	if m.reveal != 0 {
		t.Fatalf("open must start at frame 0, got %d", m.reveal)
	}
	// Frame 0: bare chrome — no candidate rows yet.
	if view := m.View(); strings.Contains(view, "carol") {
		t.Fatalf("frame 0 must not paint candidates yet:\n%s", view)
	}
	// Frame 1: the group header + first row appear; the whole list not yet.
	m, cmd := stepInviteC(m, inviteRevealMsg{gen: m.revealGen, frame: 1})
	if cmd == nil {
		t.Fatal("a mid-reveal frame must re-arm the next tick")
	}
	if view := m.View(); !strings.Contains(view, "Members") || strings.Contains(view, "carol") {
		t.Fatalf("frame 1 must reveal the header first, got:\n%s", view)
	}
	// Frame 2: carol appears, dave still hidden.
	m = stepInvite(m, inviteRevealMsg{gen: m.revealGen, frame: 2})
	if view := m.View(); !strings.Contains(view, "carol") || strings.Contains(view, "dave") {
		t.Fatalf("frame 2 must reveal the first row, got:\n%s", view)
	}
	// Settled: fully expanded window.
	m = stepInvite(m, inviteRevealMsg{gen: m.revealGen, frame: inviteRevealFrames - 1})
	view := m.View()
	for _, want := range []string{"Members", "carol", "dave", "SEND"} {
		if !strings.Contains(view, want) {
			t.Errorf("settled invite view missing %q", want)
		}
	}
	// A stale reveal generation stays inert.
	stale := stepInvite(m, inviteRevealMsg{gen: 99, frame: 0})
	if stale.reveal != inviteRevealFrames-1 {
		t.Fatalf("a stale reveal tick must never rewind the window: reveal=%d", stale.reveal)
	}
	// No-animation models: fully expanded, Init arms nothing.
	m2 := newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height)
	m2.animations = false
	if cmd := m2.Init(); cmd != nil {
		t.Fatal("plain runs must not arm reveal ticks")
	}
	if view := m2.View(); !strings.Contains(view, "carol") {
		t.Fatalf("plain runs paint fully expanded:\n%s", view)
	}
}

// TestInviteWindowGroupedRoles: mixed-role candidates paint under the
// Admins/Members headers in the drawer standard (stable item order, headers
// inserted), the arrow keys walk every painted item row across the group
// boundary, and the footer carries the sel/count.
func TestInviteWindowGroupedRoles(t *testing.T) {
	c := groupTestScreen("alice")
	c.width, c.height = 110, 40
	m := settleInviteReveal(newInviteModel(c, "700001", "Design",
		&signalClient{serverURL: "http://x", key: "700001", me: "alice"}, 110, 40))
	m.cands = []rosterMember{
		{Username: "eve", Role: "admin"},
		{Username: "carol"},
		{Username: "dave"},
	}

	plan := m.invitePlan()
	var kinds []string
	for _, r := range plan.rows {
		switch r.kind {
		case drHeader:
			kinds = append(kinds, "H:"+r.text)
		case drBlank:
			kinds = append(kinds, "B")
		case drItem:
			kinds = append(kinds, "I")
		}
	}
	if got := strings.Join(kinds, ","); got != "H:Admins,I,B,H:Members,I,I" {
		t.Fatalf("grouped plan = %v; want H:Admins,I,B,H:Members,I,I", got)
	}
	if got := strings.Join(itos(plan.displayOrder()), ","); got != "0,1,2" {
		t.Fatalf("painted order = %v; want 0,1,2 (stable)", got)
	}

	view := m.View()
	for _, want := range []string{"Admins", "Members", "eve", "carol", "dave", "0 selected"} {
		if !strings.Contains(view, want) {
			t.Errorf("grouped view missing %q", want)
		}
	}
	// Arrows walk the painted rows across the group boundary.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.sel != 1 || m.cands[m.sel].Username != "carol" {
		t.Fatalf("down across the header: sel=%d %q; want carol", m.sel, m.cands[m.sel].Username)
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.sel != 2 || m.cands[m.sel].Username != "dave" {
		t.Fatalf("down into Members: sel=%d %q; want dave", m.sel, m.cands[m.sel].Username)
	}
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.sel != 1 {
		t.Fatalf("up must walk back to carol, sel=%d", m.sel)
	}
}

// TestInviteWindowRenders: the window paints in the drawer standard — the
// "Invite — Design" header with the esc hint, the role-grouped candidate
// rows with pick checks, the Send button and the keymap footer with the
// LIVE picked counter ("n selected — Enter to send").
func TestInviteWindowRenders(t *testing.T) {
	_, c, code, _ := inviteFixture(t)
	m := settleInviteReveal(newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height))
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown}) // sel 1
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	view := m.View()
	for _, want := range []string{"Invite — Design", "Members", "carol", "dave", "SEND", "enter toggle"} {
		if !strings.Contains(view, want) {
			t.Errorf("invite view missing %q", want)
		}
	}
	if !strings.Contains(view, "esc") {
		t.Error("invite header must paint the esc hint (drawer header contract)")
	}
	// The footer counts the LIVE picks in the spec string.
	if !strings.Contains(view, "1 selected — Enter to send") {
		t.Errorf("invite footer must paint %q, got:\n%s", "1 selected — Enter to send", view)
	}
	// Picked rows carry a legible check; the selected row wears the bar.
	if !strings.Contains(view, "✓") {
		t.Error("picked row must paint its ✓ check")
	}

	// Empty state: "No users to invite".
	c2 := groupTestScreen("alice")
	c2.width, c2.height = 110, 40
	c2.users = []string{"alice"}
	em := settleInviteReveal(newInviteModel(c2, code, "Design", &signalClient{serverURL: "x", key: code, me: "alice"}, 110, 40))
	if !strings.Contains(em.View(), "No users to invite") {
		t.Errorf("empty window must paint %q", "No users to invite")
	}
}

// TestInviteWindowStripCraft pins the window to the command drawer's craft
// (the reported "invite list still ugly vs the command panel"): the strip
// is the shared drawer language — the single top border line, the
// "Invite — <group>" header + esc hint, role-grouped Admins/Members
// headers with a blank separator, the muted rest rows, the full-row cursor
// bar, the footer hints + live counter — NOT a boxed card. It renders
// through the SHARED painter (drawerPanelView), caps at 80 cells wide, and
// never wraps the terminal.
func TestInviteWindowStripCraft(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	c := groupTestScreen("alice")
	c.width, c.height = 110, 40
	m := settleInviteReveal(newInviteModel(c, "700001", "Design",
		&signalClient{serverURL: "http://x", key: "700001", me: "alice"}, 110, 40))
	// Mixed roles: the Admins/Members grouping must paint with a blank
	// separator between the groups (stable candidate order).
	m.cands = []rosterMember{
		{Username: "eve", Role: "admin"},
		{Username: "carol"},
		{Username: "dave"},
	}
	view := m.View()
	lines := strings.Split(view, "\n")

	// (a) The strip is a small centered band — never a full-screen box:
	// the empty canvas surrounds a handful of content rows.
	nonEmpty := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			nonEmpty++
		}
	}
	if nonEmpty > 12 {
		t.Fatalf("the invite strip must stay a band, got %d content rows:\n%s", nonEmpty, view)
	}
	for _, box := range []string{"╭", "╮", "╰", "╯", "│"} {
		if strings.Contains(view, box) {
			t.Fatalf("the strip must not wear a box border (%q found):\n%s", box, view)
		}
	}

	// (b) The single top border line runs in accent — the drawer's one
	// border, above the header.
	var borderLine string
	for _, ln := range lines {
		if strings.Contains(ln, "─") {
			borderLine = ln
			break
		}
	}
	if borderLine == "" {
		t.Fatal("the strip must paint the single top border line")
	}
	if run := strings.TrimLeft(stripANSI(borderLine), " "); !strings.HasPrefix(run, "─") {
		t.Fatalf("the first line must be the border run: %q", borderLine)
	}
	if n := strings.Count(strings.TrimSpace(borderLine), "─"); n < 10 || n > 80 {
		t.Fatalf("border run = %d cells; want the 80-cell cap, well-formed", n)
	}
	// (c) The header contract: bold title + the esc hint.
	if !strings.Contains(view, "Invite — Design") || !strings.Contains(view, "esc") {
		t.Fatalf("header contract missing:\n%s", view)
	}
	// (d) The grouped member headers + the blank separator between groups.
	hdrIdx, memIdx := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "Admins") {
			hdrIdx = i
		}
		if strings.Contains(ln, "Members") {
			memIdx = i
		}
	}
	if hdrIdx < 0 || memIdx < 0 {
		t.Fatalf("Admins/Members headers missing:\n%s", view)
	}
	// Admins header at i: its item at i+1, the blank separator at i+2, the
	// Members header at i+3 — headers are separated by one blank row.
	if memIdx-hdrIdx != 3 {
		t.Fatalf("the groups must be separated by exactly one blank row (Admins at %d, Members at %d):\n%s", hdrIdx, memIdx, view)
	}
	// (e) The footer contract: nav hints left, the live counter right.
	if !strings.Contains(view, "0 selected") || !strings.Contains(view, "enter toggle") {
		t.Fatalf("footer hints/counter missing:\n%s", view)
	}
	// (f) 80-cell cap: no content row (margin stripped) exceeds 80 cells.
	for _, ln := range lines {
		if w := lipgloss.Width(strings.TrimSpace(ln)); w > 80 {
			t.Fatalf("strip row width %d exceeds the 80-cell cap: %q", w, ln)
		}
	}
	// (g) The row plan is the SHARED drawer plan: paint and hit-test walk
	// the same rows (mouse parity contract).
	rows := m.invitePanelRows()
	if rows[0].kind != drBorder || rows[1].kind != drHeader {
		t.Fatalf("strip must open with border + header, got %v/%v", rows[0].kind, rows[1].kind)
	}
	switch rows[len(rows)-1].kind {
	case drFooter:
	default:
		t.Fatalf("strip must close with the footer, got %v", rows[len(rows)-1].kind)
	}
}

// TestInvitePickedRowBrightInkInWindow drives the REAL paint path (the
// window's View through the shared painter): the ✓ on a picked row AND on
// a picked row under the cursor carries the bright contrast ink — asserted
// as the SGR sequence painted immediately before the glyph, the actual
// ink the terminal applies.
func TestInvitePickedRowBrightInkInWindow(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	_, c, code, _ := inviteFixture(t)
	m := settleInviteReveal(newInviteModel(c, code, "Design", c.groups[code].sig, c.width, c.height))
	matchSelSeq := styleSeq(tuiPaletteMatchSelStyle)
	if matchSelSeq == "" {
		t.Skip("color profile emits no SGR for the bright ink")
	}

	// Picked, cursor elsewhere: Enter picks carol (sel 0), then Down.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyDown})
	checkRow := func(label string) {
		t.Helper()
		view := m.View()
		for _, ln := range strings.Split(view, "\n") {
			if i := strings.Index(ln, "✓"); i >= 0 {
				if !sgrBefore(ln, i, matchSelSeq) {
					t.Fatalf("%s: the ✓ must be painted by the bright ink (SGR before the glyph): %q", label, ln)
				}
				return
			}
		}
		t.Fatalf("%s: no ✓ row painted in:\n%s", label, m.View())
	}
	checkRow("picked, cursor on another row")

	// Picked AND under the cursor: Enter toggles dave, Up back to carol —
	// the cursor bar sits ON the picked row; the ✓ must stay bright.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter}) // pick dave (sel 1)
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyUp})    // cursor on carol (picked)
	checkRow("cursor over the picked row")

	// The pulse flash keeps the bright ✓ too.
	m = stepInvite(m, tea.KeyMsg{Type: tea.KeyEnter}) // toggle carol -> flash
	view := m.View()
	found := false
	for _, ln := range strings.Split(view, "\n") {
		if i := strings.Index(ln, "✓"); i >= 0 {
			found = true
			if !sgrBefore(ln, i, matchSelSeq) {
				t.Fatalf("flash frame: the ✓ must stay bright-ink: %q", ln)
			}
		}
	}
	if !found {
		t.Fatalf("flash frame must still paint the ✓:\n%s", view)
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
