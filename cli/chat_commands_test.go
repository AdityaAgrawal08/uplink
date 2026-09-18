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

	// Ranked "" = [/admin, /audio, /download, /help, ...] (alphabetical);
	// down lands on /audio, up returns to the top (= /admin).
	c = down(c)
	if got := c.input.Value(); got != "/" || c.palette.sel != 1 {
		t.Fatalf("down did not move to second row: input=%q sel=%d", got, c.palette.sel)
	}
	c = up(c)
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyTab})
	if got := c.input.Value(); got != "/admin " {
		t.Fatalf("tab completion gave %q; want \"/admin \"", got)
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
	found := false
	for _, ll := range got.localLines {
		if strings.Contains(ll.text, "Commands:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("running /help must paint the command hint; localLines=%v", got.localLines)
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
	found := false
	for _, ll := range c.localLines {
		if strings.Contains(ll.text, "Commands:") {
			found = true
		}
	}
	if !found {
		t.Fatal("typed /HELP must paint the hint via runCommand")
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
	found := false
	for _, ll := range c2.localLines {
		if strings.Contains(ll.text, "/kick <username>") {
			found = true
		}
	}
	if !found {
		t.Fatal("bare /kick must paint usage")
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
