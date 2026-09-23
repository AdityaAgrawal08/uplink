package main

import (
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// step feeds one msg through Update and hands back the NEW model state —
// Update uses a value receiver, so the returned model always owns the truth.
func step(m tea.Model, msg tea.Msg) (chatScreen, tea.Cmd) {
	nm, cmd := m.Update(msg)
	return nm.(chatScreen), cmd
}

func typeKeys(m tea.Model, s string) (chatScreen, tea.Cmd) {
	var cmd tea.Cmd
	for _, r := range []rune(s) {
		m, cmd = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m.(chatScreen), cmd
}

func newPaletteScreenAt(me, target string) chatScreen {
	c := newFilterScreen(me, target)
	c.vp = *viewportPtr(40, 10)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m.(chatScreen)
}

func newPaletteScreen() chatScreen {
	return newPaletteScreenAt("bob", "")
}

// ---------------------------------------------------------------------------
// Ranking: prefix matches first, then dictionary order
// ---------------------------------------------------------------------------

func TestRankSlashCommandsPrefixFirst(t *testing.T) {
	// Synthetic registry exercises the ordering rules that the live one is
	// too small to show; keeps the contract pinned as commands are added.
	items := []slashCommand{
		{Name: "/exit"}, {Name: "/quit"}, {Name: "/help"}, {Name: "/history"},
	}
	names := func(q string) []string {
		var out []string
		for _, cmd := range rankSlashCommands(items, q) {
			out = append(out, cmd.Name)
		}
		return out
	}

	assertEq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("ranking = %v; want %v", got, want)
		}
	}

	// No query: plain dictionary order over everything.
	assertEq(names(""), "/exit", "/help", "/history", "/quit")

	// "/h": all prefix hits first (alphabetical), then the rest in order.
	assertEq(names("/h"), "/help", "/history", "/exit", "/quit")

	// "/q": single prefix hit floats up, others unchanged.
	assertEq(names("/q"), "/quit", "/exit", "/help", "/history")

	// Case-insensitive prefix matching.
	assertEq(names("/H"), "/help", "/history", "/exit", "/quit")
}

// /exit and /quit must be gone from the palette registry — Ctrl+C is the
// only exit path now.
func TestRegistryHasNoExitCommands(t *testing.T) {
	for _, cmd := range slashCommands {
		switch cmd.Name {
		case "/exit", "/quit":
			t.Errorf("registry still contains removed command %q", cmd.Name)
		}
	}
	foundHelp := false
	for _, cmd := range slashCommands {
		if cmd.Name == "/help" {
			foundHelp = true
		}
	}
	if !foundHelp {
		t.Error("registry lost /help")
	}
}

// ---------------------------------------------------------------------------
// Visibility: "/" opens, any removal of the slash closes
// ---------------------------------------------------------------------------

func TestPaletteOpensWithSlashAndClosesWithout(t *testing.T) {
	c := newPaletteScreen()
	if c.palette.visible() {
		t.Fatal("drawer must start closed")
	}

	c, _ = typeKeys(c, "/")
	if !c.palette.visible() {
		t.Fatal("typing / must open the drawer")
	}

	c, _ = typeKeys(c, "h")
	if !c.palette.visible() {
		t.Fatal("drawer must stay open while query starts with /")
	}

	c, _ = step(c, tea.KeyMsg{Type: tea.KeyBackspace}) // "/h" -> "/"
	if !c.palette.visible() {
		t.Fatal("plain \"/\" after edits must still be open")
	}

	c, _ = step(c, tea.KeyMsg{Type: tea.KeyBackspace}) // "/" -> ""
	if c.palette.visible() {
		t.Fatal("removing the leading / must close the drawer")
	}
}

func TestViewShowsPaletteAboveComposer(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/he")

	view := c.View()
	lines := strings.Split(view, "\n")
	palIdx, composerIdx := -1, -1
	for i, ln := range lines {
		if palIdx < 0 && strings.Contains(ln, "/help") {
			palIdx = i
		}
		if composerIdx < 0 && strings.Contains(ln, "❯ /he") {
			composerIdx = i
		}
	}
	if composerIdx < 0 {
		t.Fatal("composer row missing from view")
	}
	if palIdx < 0 {
		t.Fatal("palette rows missing from view while query starts with /")
	}
	if palIdx >= composerIdx {
		t.Fatalf("palette must emerge ABOVE the composer: palette@%d composer@%d", palIdx, composerIdx)
	}

	// Clearing the input dissolves the panel again.
	for range 3 {
		c, _ = step(c, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	if strings.Contains(c.View(), "show available commands") {
		t.Fatal("palette must not paint after the query is removed")
	}
}

// ---------------------------------------------------------------------------
// Key handling: arrows + wrap-around, Tab completion, Enter selection, Esc
// ---------------------------------------------------------------------------

func TestPaletteNavigationAndTabCompletes(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")

	down := func(m tea.Model) chatScreen { m, _ = step(m, tea.KeyMsg{Type: tea.KeyDown}); return m.(chatScreen) }
	up := func(m tea.Model) chatScreen { m, _ = step(m, tea.KeyMsg{Type: tea.KeyUp}); return m.(chatScreen) }

	// Unwired screen = member role: mod commands are hidden, so ranked ""
	// is [/audio, /download, /help, /upload, /video] (alphabetical); down
	// lands on /download, up returns to the top (= /audio).
	c = down(c)
	if got := c.input.Value(); got != "/" || c.palette.sel != 1 {
		t.Fatalf("down did not move to second row: input=%q sel=%d", got, c.palette.sel)
	}
	c = up(c)
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyTab})
	if got := c.input.Value(); got != "/audio " {
		t.Fatalf("tab completion gave %q; want \"/audio \"", got)
	}
	if c.palette.visible() {
		t.Fatal("tab completion must close the drawer")
	}

	// "/u" prefix-ranks /unadmin before /upload (alphabetical among
	// prefix matches); "/uplo" is unambiguous — Enter selects /upload
	// and OPENS THE PICKER.
	c2 := newPaletteScreen()
	c2, _ = typeKeys(c2, "/uplo")
	got2, cmd := step(c2, tea.KeyMsg{Type: tea.KeyEnter})
	if !got2.picker.isActive() {
		t.Fatal("selecting /upload must open the file browser")
	}
	if cmd != nil {
		t.Fatal("opening the picker is local-only")
	}
	if !strings.Contains(got2.View(), "~/") && !strings.Contains(got2.View(), breadcrumb(got2.picker.cwd, got2.picker.home)) {
		t.Fatal("browser must paint a breadcrumb header")
	}
}

func TestPaletteEnterRunsSelectedCommand(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/he") // top-ranked selection is /help

	got, cmd := step(c, tea.KeyMsg{Type: tea.KeyEnter})

	if cmd != nil {
		t.Fatal("/help must run locally without a network command")
	}
	if got.input.Value() != "" {
		t.Fatalf("input not cleared after selection: %q", got.input.Value())
	}
	if got.palette.visible() {
		t.Fatal("drawer must close after a selection")
	}
	if !strings.Contains(got.status, "Commands:") {
		t.Fatalf("running /help must park the command hint on status; status=%q localLines=%v", got.status, got.localLines)
	}
	for _, ll := range got.localLines {
		if strings.Contains(ll.text, "Commands:") {
			t.Fatalf("help must not paint transcript rows: %+v", got.localLines)
		}
	}
}

// With /exit removed from the registry, typing "/exit" falls through to a
// normal chat message instead of quitting.
func TestTypedExitIsNoLongerACommand(t *testing.T) {
	c := newPaletteScreen()
	cmd := c.submitLine("/exit")
	if cmd == nil {
		t.Fatal("/exit must fall through to dispatchSend now (non-nil wire cmd)")
	}
}

func TestPaletteEscClosesDrawerNotPrivateMode(t *testing.T) {
	c := newPaletteScreenAt("bob", "alice")

	c, _ = typeKeys(c, "/")
	if !c.palette.visible() {
		t.Fatal("drawer should be open before Esc")
	}
	got, _ := step(c, tea.KeyMsg{Type: tea.KeyEsc})
	if got.palette.visible() {
		t.Fatal("Esc must close the drawer")
	}
	if got.targetUser != "alice" {
		t.Fatalf("Esc on the drawer must NOT exit private mode; target=%q", got.targetUser)
	}
}

// UI contract: panel spans the composer's full width, carries the keymap
// footer, and its border matches the composer accent.
func TestPalettePanelLayout(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")

	l := c.layoutFor()
	panel := c.paletteView(l.vpWidth + 2)
	if panel == "" {
		t.Fatal("panel must render while query starts with /")
	}
	if w := lipgloss.Width(panel); w != l.vpWidth+2 {
		t.Fatalf("panel width %d; want composer outer width %d", w, l.vpWidth+2)
	}
	if !strings.Contains(panel, paletteFooterHints) {
		t.Fatalf("keymap footer missing from panel: %q", panel)
	}

	view := c.View()
	if !strings.Contains(view, "enter run") {
		t.Fatal("footer must be visible in the painted frame")
	}
}

// Palette-consumed keys must not leak into the viewport scroller.
func TestPaletteArrowsDoNotScrollTranscript(t *testing.T) {
	c := newPaletteScreen()
	c.lines = []string{"row one", "row two"}
	c.refreshViewport()
	c, _ = typeKeys(c, "/")

	before := c.vp.YOffset
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown})
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyUp})
	if got := c.vp.YOffset; got != before {
		t.Fatalf("viewport scrolled from %d to %d on palette navigation", before, got)
	}
}

// ---------------------------------------------------------------------------
// Registry delegation: typed commands behave like picked ones
// ---------------------------------------------------------------------------

func TestSubmitLineDelegatesRegistryCommands(t *testing.T) {
	c := newPaletteScreen()

	if cmd := c.submitLine("/HELP"); cmd != nil {
		t.Fatal("/HELP must resolve locally (case-insensitive)")
	}
	if !strings.Contains(c.status, "Commands:") {
		t.Fatalf("typed /HELP must park the hint on status; status=%q", c.status)
	}

	// Unknown slash text still goes to the wire as a normal message.
	cmd := c.submitLine("/foo hello")
	if cmd == nil {
		t.Fatal("unknown /foo must fall through to dispatchSend")
	}
}

func TestKickDoneAppliesRoster(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob")
	joiner := &signalClient{serverURL: srv.URL, key: "123456", me: "alice"}
	if _, _, err := joiner.joinRoom("alice", base64.StdEncoding.EncodeToString(make([]byte, 32)), ""); err != nil {
		t.Fatal(err)
	}
	c.eng.beatOnce()
	m, _ := c.Update(rosterTickMsg{})
	sc := m.(chatScreen)
	if len(sc.users) != 2 {
		t.Fatalf("users = %v; want [bob alice]", sc.users)
	}
	// A completed /kick drops the target from engine + sidebar at once.
	m, _ = sc.Update(kickDoneMsg{target: "alice",
		roster: []rosterMember{{Username: "bob", Online: true}}, epoch: 7})
	sc = m.(chatScreen)
	if len(sc.users) != 1 || sc.users[0] != "bob" {
		t.Fatalf("users after kick = %v; want [bob]", sc.users)
	}
	// Failures surface as status text, roster untouched.
	m, _ = sc.Update(kickDoneMsg{target: "alice", err: errors.New("403: crews only")})
	sc = m.(chatScreen)
	if len(sc.users) != 1 {
		t.Fatalf("failed kick must not touch users: %v", sc.users)
	}
}

func TestKickMatrixFakeServer(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := newSignalTestClient(srv, "alice") // room creator = main admin
	sid, err := alice.createRoom("alice", "pubkey-alice", "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	mk := func(u string) *signalClient {
		c := newSignalTestClient(srv, u)
		c.key = sid
		if _, _, err := c.joinRoom(u, "pubkey-"+u, ""); err != nil {
			t.Fatal(err)
		}
		return c
	}
	bob, carol, dave := mk("bob"), mk("carol"), mk("dave")

	mustFail := func(err error, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s must fail", what)
		}
	}
	// Members moderate nobody; non-creators cannot grant.
	mustFail(func() error { _, _, e := carol.kickUser("dave"); return e }(), "member kick")
	mustFail(func() error { _, _, e := bob.setRole("carol", true); return e }(), "non-creator grant")
	// Creator grants two admins; admins can never touch each other.
	if _, _, err := alice.setRole("bob", true); err != nil {
		t.Fatalf("creator grant: %v", err)
	}
	if _, _, err := alice.setRole("carol", true); err != nil {
		t.Fatalf("creator grant: %v", err)
	}
	mustFail(func() error { _, _, e := bob.kickUser("carol"); return e }(), "admin kick admin")
	mustFail(func() error { _, _, e := carol.kickUser("bob"); return e }(), "admin kick admin")
	mustFail(func() error { _, _, e := bob.setRole("carol", false); return e }(), "admin unadmin")
	// The creator is immune to everyone.
	mustFail(func() error { _, _, e := bob.kickUser("alice"); return e }(), "admin kick creator")
	mustFail(func() error { _, _, e := dave.kickUser("alice"); return e }(), "member kick creator")
	// Admins kick members; creator kicks admins.
	if _, _, err := bob.kickUser("dave"); err != nil {
		t.Fatalf("admin kick member: %v", err)
	}
	if _, _, err := alice.kickUser("bob"); err != nil {
		t.Fatalf("creator kick admin: %v", err)
	}
}

func TestModVisibilityByRole(t *testing.T) {
	names := func(role string) []string {
		out := []string{}
		for _, cmd := range visibleSlashCommands(role) {
			out = append(out, cmd.Name)
		}
		return out
	}
	has := func(role, name string) bool {
		for _, n := range names(role) {
			if n == name {
				return true
			}
		}
		return false
	}
	// Creator sees everything; admins see /kick only; members see none.
	for _, cmd := range slashCommands {
		if !has("creator", cmd.Name) {
			t.Fatalf("creator must see %s", cmd.Name)
		}
	}
	if !has("admin", "/kick") || has("admin", "/admin") || has("admin", "/unadmin") {
		t.Fatalf("admin visibility = %v; want only /kick among mod commands", names("admin"))
	}
	for _, mod := range []string{"/kick", "/admin", "/unadmin"} {
		if has("member", mod) || has("", mod) {
			t.Fatalf("role member/unknown must not see %s", mod)
		}
	}

	// Unwired engine fails closed to member (no mod rows).
	memberScreen := newPaletteScreen()
	if got := memberScreen.myRole(); got != "member" {
		t.Fatalf("unwired myRole = %q; want member", got)
	}
	if ranked := memberScreen.rankedCommands("/"); len(ranked) != len(slashCommands)-3 {
		t.Fatalf("member palette has %d rows; want %d (no mod commands)", len(ranked), len(slashCommands)-3)
	}

	// Wired creator sees their rank after a beat.
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob")
	c.eng.beatOnce()
	if got := c.myRole(); got != "creator" {
		t.Fatalf("room maker myRole = %q; want creator", got)
	}
	if ranked := c.rankedCommands("/"); len(ranked) != len(slashCommands) {
		t.Fatalf("creator palette has %d rows; want all %d", len(ranked), len(slashCommands))
	}
}

func TestRankUsersPrefixFirst(t *testing.T) {
	users := []rosterMember{
		{Username: "carol"}, {Username: "Bob"}, {Username: "alice"}, {Username: "zack"},
	}
	names := func(q string) []string {
		out := []string{}
		for _, u := range rankUsers(users, q) {
			out = append(out, u.Username)
		}
		return out
	}
	// No query: plain dictionary order (case-insensitive).
	if got := strings.Join(names(""), ","); got != "alice,Bob,carol,zack" {
		t.Fatalf("ranking = %v", got)
	}
	// Prefix hits float first, alphabetical inside each group.
	if got := strings.Join(names("b"), ","); got != "Bob,alice,carol,zack" {
		t.Fatalf("ranking(/b) = %v", got)
	}
	if got := strings.Join(names("C"), ","); got != "carol,alice,Bob,zack" {
		t.Fatalf("ranking(/C) = %v", got)
	}
}

// wireModRoom builds a creator screen (bob) with admin alice + member carol,
// presence refreshed so roles resolve. Alice shares the engine as an admin.
func wireModRoom(t *testing.T) (*httptest.Server, *chatScreen, *chatScreen) {
	t.Helper()
	srv := httptest.NewServer(newFakeSignalServer())
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(80, 24)
	wireTestEngine(t, c, srv, "bob", "alice", "carol")
	if _, _, err := c.sig.setRole("alice", true); err != nil {
		t.Fatal(err)
	}
	c.eng.beatOnce()
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	*c = m.(chatScreen)
	a := newFilterScreen("alice", "")
	a.vp = *viewportPtr(80, 24)
	a.eng = c.eng // shared presence: alice resolves as admin
	a.sig = c.sig
	return srv, c, a
}

func TestUserArgTargetParsing(t *testing.T) {
	srv, creator, admin := wireModRoom(t)
	defer srv.Close()

	if cmd, q, ok := creator.userArgTarget("/kick "); !ok || cmd != "/kick" || q != "" {
		t.Fatalf("creator /kick → (%q, %q, %v)", cmd, q, ok)
	}
	if cmd, q, ok := creator.userArgTarget("/ADMIN ca"); !ok || cmd != "/admin" || q != "ca" {
		t.Fatalf("creator /ADMIN ca → (%q, %q, %v)", cmd, q, ok)
	}
	for _, no := range []string{"/kick", "/help ", "/upload ", "/foo ", "kick ", "/kickbob "} {
		if _, _, ok := creator.userArgTarget(no); ok {
			t.Fatalf("creator %q must stay in command mode", no)
		}
	}
	// Admins complete /kick only; members complete nothing.
	if _, _, ok := admin.userArgTarget("/kick "); !ok {
		t.Fatal("admin must complete /kick users")
	}
	if _, _, ok := admin.userArgTarget("/admin "); ok {
		t.Fatal("admin must not complete /admin users")
	}
	member := newPaletteScreen()
	if _, _, ok := member.userArgTarget("/kick "); ok {
		t.Fatal("member must not enter user mode")
	}
}

func TestUserPickerFlow(t *testing.T) {
	srv, c, _ := wireModRoom(t)
	defer srv.Close()

	typeText := func(cs chatScreen, s string) chatScreen {
		for _, r := range s {
			m, _ := cs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			cs = m.(chatScreen)
		}
		return cs
	}
	usernames := func(us []rosterMember) []string {
		out := []string{}
		for _, u := range us {
			out = append(out, u.Username)
		}
		return out
	}

	// Typing "/kick " morphs the drawer into the member picker (self excluded).
	sc := typeText(*c, "/kick ")
	cmd, users, ok := sc.paletteUsers()
	if !ok || cmd != "/kick" {
		t.Fatalf("paletteUsers = (%q, %v, %v)", cmd, usernames(users), ok)
	}
	if got := strings.Join(usernames(users), ","); got != "alice,carol" {
		t.Fatalf("candidates = %v; want [alice carol] (no self)", usernames(users))
	}
	view := sc.View()
	if !strings.Contains(view, "alice") || !strings.Contains(view, "carol") {
		t.Fatal("picker must paint member rows")
	}
	if !strings.Contains(view, "admin") {
		t.Fatal("picker must annotate staff roles")
	}

	// Down + Enter dispatches /kick for the highlighted member (carol).
	m, _ := sc.Update(tea.KeyMsg{Type: tea.KeyDown})
	sc = m.(chatScreen)
	handled, action := sc.handlePaletteKeys(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || action == nil {
		t.Fatal("Enter on a member must dispatch")
	}
	done := action()()
	kick, isKick := done.(kickDoneMsg)
	if !isKick || kick.target != "carol" || kick.err != nil {
		t.Fatalf("dispatch = %#v; want kick carol", done)
	}

	// Tab completes the fragment inline and closes the drawer.
	sc2 := typeText(*c, "/kick c")
	m2, _ := sc2.Update(tea.KeyMsg{Type: tea.KeyTab})
	sc2 = m2.(chatScreen)
	if got := sc2.input.Value(); got != "/kick carol " {
		t.Fatalf("tab completed %q; want \"/kick carol \"", got)
	}
	if sc2.palette.visible() {
		t.Fatal("tab completion must close the drawer")
	}

	// Esc steps back to the command stage, drawer stays open.
	sc3 := typeText(*c, "/kick a")
	m3, _ := sc3.Update(tea.KeyMsg{Type: tea.KeyEsc})
	sc3 = m3.(chatScreen)
	if got := sc3.input.Value(); got != "/kick" {
		t.Fatalf("esc stepped back to %q; want \"/kick\"", got)
	}
	if !sc3.palette.visible() {
		t.Fatal("esc in user mode must keep the drawer open")
	}
	if _, _, ok := sc3.paletteUsers(); ok {
		t.Fatal("bare /kick must be back in command mode")
	}
}

func TestCommandMorphsIntoUserPicker(t *testing.T) {
	srv, c, _ := wireModRoom(t)
	defer srv.Close()

	typeText := func(cs chatScreen, s string) chatScreen {
		for _, r := range s {
			m, _ := cs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			cs = m.(chatScreen)
		}
		return cs
	}

	// Partial "/ki" + Enter: /kick is top-ranked, morphs into the picker
	// instead of firing with an empty arg.
	sc := typeText(*c, "/ki")
	m, _ := sc.Update(tea.KeyMsg{Type: tea.KeyEnter})
	sc = m.(chatScreen)
	if got := sc.input.Value(); got != "/kick " {
		t.Fatalf("enter morphed to %q; want \"/kick \"", got)
	}
	if !sc.palette.visible() {
		t.Fatal("morph must keep the drawer open")
	}
	if cmd, users, ok := sc.paletteUsers(); !ok || cmd != "/kick" || len(users) != 2 {
		t.Fatalf("morphed picker = (%q, %d users, %v)", cmd, len(users), ok)
	}

	// Partial "/adm" + Tab: same morph, inline completion.
	sc2 := typeText(*c, "/adm")
	m2, _ := sc2.Update(tea.KeyMsg{Type: tea.KeyTab})
	sc2 = m2.(chatScreen)
	if got := sc2.input.Value(); got != "/admin " {
		t.Fatalf("tab morphed to %q; want \"/admin \"", got)
	}
	if !sc2.palette.visible() {
		t.Fatal("tab morph must keep the drawer open")
	}
	if cmd, _, ok := sc2.paletteUsers(); !ok || cmd != "/admin" {
		t.Fatalf("tab morphed picker = (%q, %v)", cmd, ok)
	}

	// Members see no mod commands: "/ki" + Enter runs the top visible
	// command and closes, never morphs.
	mem := newPaletteScreen()
	mem, _ = typeKeys(mem, "/ki")
	m3, _ := mem.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mem = m3.(chatScreen)
	if mem.palette.visible() {
		t.Fatal("member enter must close the drawer (command ran)")
	}
	if _, _, ok := mem.paletteUsers(); ok {
		t.Fatal("member must never enter user mode")
	}
}

func TestModTargetParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"bob", "bob", true},
		{"@bob", "bob", true},
		{"  carol  ", "carol", true},
		{"", "", false},
		{"   ", "", false},
		{"two words", "", false},
		{strings.Repeat("x", 21), "", false},
	} {
		got, ok := modTarget(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("modTarget(%q) = (%q, %v); want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestKickAdminDispatchAndClient(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	alice := newSignalTestClient(srv, "alice")
	sid, err := alice.createRoom("alice", "pubkey-alice", "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	bob := newSignalTestClient(srv, "bob")
	bob.key = sid
	if _, _, err := bob.joinRoom("bob", "pubkey-bob", ""); err != nil {
		t.Fatal(err)
	}

	// Client methods parse roster + epoch.
	roster, epoch, err := alice.setRole("bob", true)
	if err != nil {
		t.Fatalf("setRole: %v", err)
	}
	if epoch == 0 {
		t.Fatal("setRole must report epoch")
	}
	roleOf := func(rs []rosterMember, u string) string {
		for _, m := range rs {
			if m.Username == u {
				return m.Role
			}
		}
		return ""
	}
	if roleOf(roster, "bob") != "admin" {
		t.Fatalf("bob role = %q; want admin", roleOf(roster, "bob"))
	}
	if _, _, err := alice.setRole("bob", false); err != nil {
		t.Fatalf("unadmin: %v", err)
	}
	roster, _, err = alice.kickUser("bob")
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	if roleOf(roster, "bob") != "" {
		t.Fatal("kicked bob must leave the roster")
	}
	if _, _, err := alice.kickUser("ghost"); err == nil {
		t.Fatal("kicking a stranger must fail")
	}

	// Dispatch: /kick with arg runs the command (async closure), bare
	// /kick prints usage locally, unknown /kick-extra is just chat.
	c := newPaletteScreen()
	if cmd := c.submitLine("/kick bob"); cmd == nil {
		t.Fatal("/kick bob must dispatch to runCommand")
	}
	c2 := newPaletteScreen()
	if cmd := c2.submitLine("/kick"); cmd != nil {
		t.Fatal("bare /kick must resolve locally (usage), not dispatch")
	}
	if !strings.Contains(c2.status, "/kick <username>") {
		t.Fatalf("bare /kick must park usage on status; status=%q", c2.status)
	}
	for _, ll := range c2.localLines {
		if strings.Contains(ll.text, "/kick <username>") {
			t.Fatal("usage must not paint transcript rows")
		}
	}
}

// ---------------------------------------------------------------------------
// Height/width contract with the palette open
// ---------------------------------------------------------------------------

func TestViewHeightContractWithPaletteOpen(t *testing.T) {
	widths := []int{20, 40, 61, 62, 80, 120}
	heights := []int{8, 12, 16, 20, 24, 30}
	for _, w := range widths {
		for _, h := range heights {
			c := newFilterScreen("bob", "", "bob", "alice")
			c.vp = *viewportPtr(80, 20)
			c.lines = []string{tuiNameStyle.Render("alice") + ": hi there"}
			m, _ := c.Update(tea.WindowSizeMsg{Width: w, Height: h})
			sc, _ := typeKeys(m, "/hel")

			view := sc.View()
			rows := strings.Count(view, "\n") + 1
			if rows > h {
				t.Errorf("w=%d h=%d: painted %d rows > terminal with palette open", w, h, rows)
			}
			if mw := maxLineWidth(view); mw > w {
				t.Errorf("w=%d h=%d: widest row %d exceeds terminal width", w, h, mw)
			}
			// The drawer may be legitimately dissolved on tiny terminals;
			// whenever the budget kept it, it must be visible.
			if sc.layoutFor().paletteRows > 0 && !strings.Contains(view, "/help") {
				t.Errorf("w=%d h=%d: palette budgeted but missing from view", w, h)
			}
		}
	}
}
