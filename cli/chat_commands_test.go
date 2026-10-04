package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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
		{Name: "/exit"}, {Name: "/quit"}, {Name: "/help", Group: "General"}, {Name: "/history", Group: "General"},
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

	// No query: plain dictionary order over everything (the drawer then
	// groups the display under its category headers).
	assertEq(names(""), "/exit", "/help", "/history", "/quit")

	// Fuzzy rank: matching commands float to the top (prefix + shortest
	// title breaking the score ties); non-matching commands VANISH —
	// fzf semantics, which is what lets the drawer reach its
	// "No results found" state.
	assertEq(names("/h"), "/help", "/history")
	assertEq(names("/q"), "/quit")
	assertEq(names("/zz")) // nothing matches: empty list

	// Case-insensitive prefix matching.
	assertEq(names("/H"), "/help", "/history")
}

// The ranking tiebreak chain: score desc, then title-prefix, then shorter
// title, then frecency, then stable order. Every branch is exercised with
// synthetic hits so the contract cannot drift when the fuzzy scorer changes.
func TestLessRankedTiebreaks(t *testing.T) {
	if !lessRanked(fuzzyHit{q: 5}, fuzzyHit{q: 3}, 0, 0) {
		t.Fatal("score desc: higher weighted score must win")
	}
	if !lessRanked(fuzzyHit{q: 2, prefixTitle: true, titleLen: 3}, fuzzyHit{q: 2, titleLen: 5}, 0, 0) {
		t.Fatal("prefix tiebreak: a title-prefix match beats a bare match at equal score")
	}
	if !lessRanked(fuzzyHit{q: 2, prefixTitle: true, titleLen: 3}, fuzzyHit{q: 2, prefixTitle: true, titleLen: 9}, 0, 0) {
		t.Fatal("shorter tiebreak: the shorter title wins at equal score+prefix")
	}
	if !lessRanked(fuzzyHit{q: 2, titleLen: 4}, fuzzyHit{q: 2, titleLen: 4}, 5, 1) {
		t.Fatal("frecency tiebreak: the hotter history wins the last tie")
	}
	if lessRanked(fuzzyHit{q: 2, titleLen: 4}, fuzzyHit{q: 2, titleLen: 4}, 0, 0) {
		t.Fatal("stability: equal items must never reorder")
	}
	if lessRanked(fuzzyHit{q: 2, titleLen: 4}, fuzzyHit{q: 2, titleLen: 4}, 2, 2) {
		t.Fatal("stability: equal frecency must never reorder")
	}
}

// Title weight (2x) must dominate the group weight (1x): a name match beats
// a category match of equal fuzzy quality, and the two combine additively.
func TestRankTitleGroupWeights(t *testing.T) {
	titleOnly := rankTitleGroup("mod", "mode", "")
	groupOnly := rankTitleGroup("mod", "", "Moderation")
	both := rankTitleGroup("mod", "mode", "Moderation")
	if !titleOnly.matched || !groupOnly.matched || !both.matched {
		t.Fatalf("all three must match: %+v %+v %+v", titleOnly, groupOnly, both)
	}
	if titleOnly.q <= groupOnly.q {
		t.Fatalf("title 2x must dominate group 1x: title=%d group=%d", titleOnly.q, groupOnly.q)
	}
	if both.q <= titleOnly.q {
		t.Fatalf("title+group must exceed title alone: both=%d title=%d", both.q, titleOnly.q)
	}
	if got := rankTitleGroup("zz", "mode", "Moderation"); got.matched {
		t.Fatal("a query matching neither title nor group must not match")
	}
	if got := rankTitleGroup("", "mode", "Moderation"); !got.matched || got.q != 0 {
		t.Fatal("an empty query matches everything with zero score")
	}
}

// Group-only matches still land in the list (fzf matches the whole line),
// but rank BELOW equal-quality title matches.
func TestPaletteGroupMatchRanksBelowTitleMatch(t *testing.T) {
	items := []slashCommand{
		{Name: "/zzz", Group: "Moderation"},
		{Name: "/mode", Group: "General"},
	}
	names := func(q string) []string {
		var out []string
		for _, cmd := range rankSlashCommands(items, q) {
			out = append(out, cmd.Name)
		}
		return out
	}
	if got := strings.Join(names("/mo"), ","); got != "/mode,/zzz" {
		t.Fatalf("title match must outrank group match: %v", got)
	}
}

// Frecency is the FINAL tiebreak: identical score/prefix/length pairs sort
// by pick history, and without history the stable registry order holds.
func TestRankSlashCommandsFrecencyTiebreak(t *testing.T) {
	items := []slashCommand{
		{Name: "/abc"}, {Name: "/abd"},
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	names := func(m map[string]frecEntry) string {
		var out []string
		for _, cmd := range rankSlashCommandsF(items, "/ab", m, now) {
			out = append(out, cmd.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(nil); got != "/abc,/abd" {
		t.Fatalf("no history: stable order must hold, got %v", got)
	}
	if got := names(map[string]frecEntry{"/abd": {freq: 3, last: now}}); got != "/abd,/abc" {
		t.Fatalf("frecency must float the hot item first, got %v", got)
	}
	// Decay: a fresh single pick outranks a huge but ancient history.
	ancient := now.Add(-30 * 24 * time.Hour)
	if got := names(map[string]frecEntry{
		"/abd": {freq: 10000, last: ancient},
		"/abc": {freq: 1, last: now},
	}); got != "/abc,/abd" {
		t.Fatalf("fresh pick must outrank decayed history, got %v", got)
	}
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

// UI contract: inline drawer with a single top border line, capped at
// min(termW-2, 80) — header contract (Bold title left + muted esc right),
// footer contract (keymap hints left + count right), border always on top.
func TestPalettePanelLayout(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")

	l := c.layoutFor()
	panel := c.paletteView(l.vpWidth + 2)
	if panel == "" {
		t.Fatal("panel must render while query starts with /")
	}
	if w := lipgloss.Width(panel); w != drawerMaxW(c.width, l.vpWidth+2) {
		t.Fatalf("panel width %d; want capped width %d", w, drawerMaxW(c.width, l.vpWidth+2))
	}
	// Single top border line + header: title left, esc right.
	if !strings.HasPrefix(panel, "\x1b") && !strings.Contains(panel, "─") {
		t.Fatal("panel must open with the single top border line")
	}
	if !strings.Contains(panel, "Commands") || !strings.Contains(panel, "esc") {
		t.Fatalf("header contract missing (title/esc): %q", panel)
	}
	// Footer contract: keymap hints left, count right.
	if !strings.Contains(panel, "navigate") || !strings.Contains(panel, "1/8") {
		t.Fatalf("footer contract missing (hints/count): %q", panel)
	}

	view := c.View()
	if !strings.Contains(view, "enter select") {
		t.Fatal("footer must be visible in the painted frame")
	}
	if !strings.Contains(view, "1/8") {
		t.Fatal("footer count must be visible in the painted frame")
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

	alice := newSignalTestClient(t, srv, "alice") // room creator = main admin
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	mk := func(u string) *signalClient {
		c := newSignalTestClient(t, srv, u)
		c.key = sid
		if _, _, err := c.joinRoom(u, pubkeyB64(c.id), ""); err != nil {
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
	// No query: plain dictionary order (case-insensitive) — the drawer then
	// groups the display under role headers.
	if got := strings.Join(names(""), ","); got != "alice,Bob,carol,zack" {
		t.Fatalf("ranking = %v", got)
	}
	// Fuzzy rank: title matches float to the top; the GROUP label matches
	// at 1x weight, so "b" (a letter of "Members") keeps the list visible
	// with the title match on top — the category never vanishes the list.
	if got := strings.Join(names("b"), ","); got != "Bob,carol,alice,zack" {
		t.Fatalf("ranking(/b) = %v", got)
	}
	// "ca" hits only carol's title (the other names have no ca run).
	if got := strings.Join(names("ca"), ","); got != "carol" {
		t.Fatalf("ranking(/ca) = %v", got)
	}
	// "zz" matches neither titles nor group labels: the list vanishes.
	if got := strings.Join(names("zz"), ","); got != "" {
		t.Fatalf("ranking(/zz) = %v; want the empty list", got)
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

	// Members see no mod commands: "/ki" matches nothing, so Enter stays in
	// the drawer on its muted "No results found" state — nothing runs,
	// nothing morphs (fzf semantics: non-matches vanish from the list).
	mem := newPaletteScreen()
	mem, _ = typeKeys(mem, "/ki")
	m3, _ := mem.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mem = m3.(chatScreen)
	if !mem.palette.visible() {
		t.Fatal("enter on an unmatched query must keep the drawer open")
	}
	if _, _, ok := mem.paletteUsers(); ok {
		t.Fatal("member must never enter user mode")
	}
	// Esc still dismisses the unmatched drawer.
	m4, _ := mem.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mem4 := m4.(chatScreen)
	if mem4.palette.visible() {
		t.Fatal("esc must dismiss the unmatched drawer")
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

	alice := newSignalTestClient(t, srv, "alice")
	sid, err := alice.createRoom("alice", pubkeyB64(alice.id), "")
	if err != nil {
		t.Fatal(err)
	}
	alice.key = sid
	bob := newSignalTestClient(t, srv, "bob")
	bob.key = sid
	if _, _, err := bob.joinRoom("bob", pubkeyB64(bob.id), ""); err != nil {
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

// ---------------------------------------------------------------------------
// Scroll window: 6 visible rows, window follows the highlight one row at a
// time in both directions.
// ---------------------------------------------------------------------------

func TestPaletteWindowScrollsDownOneRowAtATime(t *testing.T) {
	var p paletteState
	p.open = true
	const n = 10
	// Walk Down from 0: sel 1..5 keep window 0–5; sel 6 shifts to 1–6.
	for i := 1; i <= 5; i++ {
		p.moveDown(n)
		if p.sel != i || p.off != 0 {
			t.Fatalf("down %d: sel=%d off=%d; want sel=%d off=0", i, p.sel, p.off, i)
		}
	}
	p.moveDown(n) // sel 6: window must scroll to 1–6
	if p.sel != 6 || p.off != 1 {
		t.Fatalf("sel=%d off=%d; want sel=6 off=1 (window 2–7)", p.sel, p.off)
	}
	p.moveDown(n) // sel 7 → window 2–7
	if p.sel != 7 || p.off != 2 {
		t.Fatalf("sel=%d off=%d; want sel=7 off=2 (window 3–8)", p.sel, p.off)
	}
}

func TestPaletteWindowScrollsUpOneRowAtATime(t *testing.T) {
	var p paletteState
	p.open = true
	const n = 10
	p.sel, p.off = 7, 2 // window rows 3-8 (0-based 2-7), highlight on last visible
	// Walk Up through the window: no scrolling while sel stays inside 2-7.
	for want := 6; want >= 2; want-- {
		p.moveUp(n)
		if p.sel != want || p.off != 2 {
			t.Fatalf("sel=%d off=%d; want sel=%d off=2", p.sel, p.off, want)
		}
	}
	// sel sits on the 1st visible row (2); one more Up scrolls to 2-7.
	p.moveUp(n)
	if p.sel != 1 || p.off != 1 {
		t.Fatalf("sel=%d off=%d; want sel=1 off=1 (window 2-7)", p.sel, p.off)
	}
	p.moveUp(n) // sel 0 -> window 1-6
	if p.sel != 0 || p.off != 0 {
		t.Fatalf("sel=%d off=%d; want sel=0 off=0 (window 1-6)", p.sel, p.off)
	}
}

func TestPaletteWindowWrapJumpsToFarEnd(t *testing.T) {
	var p paletteState
	p.open = true
	const n = 10
	p.sel, p.off = 9, 4 // tail: window 5–10
	p.moveDown(n)       // wrap to 0 → window back to 1–6
	if p.sel != 0 || p.off != 0 {
		t.Fatalf("wrap down: sel=%d off=%d; want 0,0", p.sel, p.off)
	}
	p.moveUp(n) // wrap to 9 → window 5–10
	if p.sel != 9 || p.off != 4 {
		t.Fatalf("wrap up: sel=%d off=%d; want 9,4", p.sel, p.off)
	}
}

func TestPaletteWindowClampOnShrink(t *testing.T) {
	var p paletteState
	p.open = true
	p.sel, p.off = 8, 3
	p.clampSel(10) // no-op: still valid
	if p.sel != 8 || p.off != 3 {
		t.Fatalf("sel=%d off=%d; want 8,3", p.sel, p.off)
	}
	p.clampSel(4) // list shrank below the window: both pin into range
	if p.sel != 3 || p.off != 0 {
		t.Fatalf("shrink: sel=%d off=%d; want 3,0", p.sel, p.off)
	}
	p.sel, p.off = 5, 5
	p.clampSel(0)
	if p.sel != 0 || p.off != 0 {
		t.Fatalf("empty: sel=%d off=%d; want 0,0", p.sel, p.off)
	}
}

func TestPaletteWindowShortListNeverScrolls(t *testing.T) {
	var p paletteState
	p.open = true
	const n = 4
	for i := 0; i < 2*n; i++ {
		p.moveDown(n)
	}
	if p.off != 0 {
		t.Fatalf("off=%d; short lists must never scroll", p.off)
	}
	for i := 0; i < 2*n; i++ {
		p.moveUp(n)
	}
	if p.off != 0 || p.sel < 0 || p.sel >= n {
		t.Fatalf("sel=%d off=%d out of range", p.sel, p.off)
	}
}

// ---------------------------------------------------------------------------
// Home / End / page moves: jump (no wrap — only the arrows wrap) and clamp at
// the ends; the view centres the window on the destination afterwards.
// ---------------------------------------------------------------------------

func TestPaletteHomeEndPageMoves(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")
	const n = 8 // member-visible commands (11 registered minus 3 moderation)
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyEnd})
	if c.palette.sel != n-1 {
		t.Fatalf("End: sel=%d; want %d", c.palette.sel, n-1)
	}
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyHome})
	if c.palette.sel != 0 {
		t.Fatalf("Home: sel=%d; want 0", c.palette.sel)
	}
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyPgDown}) // +10 → clamped at the end
	if c.palette.sel != n-1 {
		t.Fatalf("PgDown: sel=%d; want clamped %d", c.palette.sel, n-1)
	}
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyPgUp}) // −10 → clamped at the top
	if c.palette.sel != 0 {
		t.Fatalf("PgUp: sel=%d; want clamped 0", c.palette.sel)
	}
	// State-level: the page step is ±10 with clamp, Home/End are exact.
	var p paletteState
	p.open = true
	p.movePage(50, 10)
	if p.sel != 10 {
		t.Fatalf("movePage(+10): sel=%d; want 10", p.sel)
	}
	p.movePage(50, -100)
	if p.sel != 0 {
		t.Fatalf("movePage clamp low: sel=%d; want 0", p.sel)
	}
	p.moveEnd(50)
	if p.sel != 49 {
		t.Fatalf("moveEnd: sel=%d; want 49", p.sel)
	}
	p.moveHome(50)
	if p.sel != 0 {
		t.Fatalf("moveHome: sel=%d; want 0", p.sel)
	}
}

// ---------------------------------------------------------------------------
// Window settling + centering on grouped plans (headers shift display rows).
// ---------------------------------------------------------------------------

func TestDrawerWindowSettleAndCenterGrouped(t *testing.T) {
	// plan rows: H G1(0) i0(1) i1(2) blank(3) H G2(4) i2(5) i3(6)
	plan := buildDrawerPlan(4, true, func(i int) string {
		if i < 2 {
			return "G1"
		}
		return "G2"
	}, 2)
	if plan.pos[0] != 1 || plan.pos[1] != 2 || plan.pos[2] != 5 || plan.pos[3] != 6 {
		t.Fatalf("pos = %v; want [1 2 5 6]", plan.pos)
	}
	// settle: sel on the last item (row 6), window 2 → first visible item
	// is the one at row ≥ 6-2+1 = 5 → i2.
	sel, off := 3, 0
	settleDrawerWindow(&sel, &off, plan)
	if off != 2 {
		t.Fatalf("settle: off=%d; want 2 (i2 at row 5)", off)
	}
	if sel != 3 {
		t.Fatalf("settle must not move sel: %d", sel)
	}
	// center: same destination for a 2-row window.
	sel, off = 3, 0
	centerDrawerWindow(&sel, &off, plan)
	if off != 2 {
		t.Fatalf("center: off=%d; want 2", off)
	}
	// The windowed painter keeps the group heading with its first item:
	// off=1 (i1 at row 2) pulls the window back over the G1 header only
	// when it is the row directly above; here row 1 is i0, so no pull.
	rows, below, above := windowDrawerPlan(plan, 1, 1)
	if len(rows) != 1 || rows[0].kind != drItem || rows[0].item != 1 {
		t.Fatalf("window(off=1) = %+v; want the single i1 row (blank clipped)", rows)
	}
	if below != 4 || above != 2 {
		t.Fatalf("window(off=1): below=%d above=%d; want 4/2", below, above)
	}
	// off on the FIRST item pulls the G1 header into the window.
	rows, _, _ = windowDrawerPlan(plan, 0, 0)
	if len(rows) != 2 || rows[0].kind != drHeader || rows[0].text != "G1" || rows[1].item != 0 {
		t.Fatalf("window(off=0) = %+v; want G1 header + i0", rows)
	}
}

// ---------------------------------------------------------------------------
// Grouped headers (Bold accent + blank separators) while the query is empty;
// flattened to a single ranked list while filtering.
// ---------------------------------------------------------------------------

func TestPaletteGroupedHeadersFlattenOnFilter(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/") // empty payload: grouped
	l := c.layoutFor()
	panel := c.paletteView(l.vpWidth + 2)
	for _, hdr := range []string{"Voice", "Files"} {
		if !strings.Contains(panel, hdr) {
			t.Fatalf("group header %q missing from the empty-query panel: %q", hdr, panel)
		}
	}
	// Blank-line separators BETWEEN groups (never rule lines): at least one
	// fully-blank painted row.
	blankRow := false
	for _, ln := range strings.Split(panel, "\n") {
		if strings.TrimSpace(stripANSI(ln)) == "" {
			blankRow = true
			break
		}
	}
	if !blankRow {
		t.Fatal("blank-line separators must separate groups (never rule lines)")
	}

	c2 := newPaletteScreen()
	c2, _ = typeKeys(c2, "/au") // filtering: flatten
	p2 := c2.paletteView(c2.layoutFor().vpWidth + 2)
	for _, gone := range []string{"Voice", "Files", "General", "Settings"} {
		if strings.Contains(p2, gone) {
			t.Fatalf("filtering must flatten the groups (header %q still painted): %q", gone, p2)
		}
	}
	if !strings.Contains(p2, "/audio") {
		t.Fatalf("/au must rank /audio into the flattened list: %q", p2)
	}
}

// ---------------------------------------------------------------------------
// Empty state: muted "No results found", budgeted, never flashing.
// ---------------------------------------------------------------------------

func TestPaletteEmptyStateNoResults(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/zz")
	if c.paletteRows() == 0 {
		t.Fatal("the empty state must still reserve drawer rows")
	}
	panel := c.paletteView(c.layoutFor().vpWidth + 2)
	if !strings.Contains(panel, "No results found") {
		t.Fatalf("empty state row missing: %q", panel)
	}
	// Enter swallows (OpenCode: nothing selected, drawer stays open)…
	got, cmd := step(c, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !got.palette.visible() {
		t.Fatal("enter on an unmatched query must keep the drawer open, no action")
	}
	// …Esc dismisses.
	got2, _ := step(got, tea.KeyMsg{Type: tea.KeyEsc})
	if got2.palette.visible() {
		t.Fatal("esc must dismiss the empty drawer")
	}
}

// ---------------------------------------------------------------------------
// Selection resets to the top on EVERY filter change (live sync), never on
// cursor moves.
// ---------------------------------------------------------------------------

func TestPaletteSelectionResetsOnFilterChange(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyDown})
	if c.palette.sel != 1 {
		t.Fatalf("precondition: sel=1, got %d", c.palette.sel)
	}
	c, _ = typeKeys(c, "h") // filter change
	if c.palette.sel != 0 {
		t.Fatal("typing must reset the selection to the top")
	}

	// Mention mirror: same contract on the "@" dropdown.
	dm := *newFilterScreen("bob", "", "alice", "carol", "dave")
	dm, _ = typeKeys(dm, "@")
	dm, _ = step(dm, tea.KeyMsg{Type: tea.KeyDown})
	if dm.mention.sel != 1 {
		t.Fatalf("precondition: mention sel=1, got %d", dm.mention.sel)
	}
	dm, _ = typeKeys(dm, "ca")
	if dm.mention.sel != 0 {
		t.Fatal("typing a mention fragment must reset the selection to the top")
	}
}

// ---------------------------------------------------------------------------
// Matched-char highlight: precomputed byte offsets, merged spans, the
// accent on unselected rows and the bar's ink on the selected one, skipped
// below 30 cells of width.
// ---------------------------------------------------------------------------

func TestPaletteMatchSpansAndHighlight(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256) // SGR sequences needed to assert spans
	defer lipgloss.SetColorProfile(prev)

	c := newPaletteScreen()
	c, _ = typeKeys(c, "/he")
	ranked := c.rankedCommands("/he")
	if len(ranked) == 0 || ranked[0].Name != "/help" {
		t.Fatalf("/he must rank /help first, got %+v", ranked)
	}
	hits := c.commandHits(ranked, palettePayload("/he"))
	if len(hits[0].matches) != 2 || hits[0].matches[0] != 1 || hits[0].matches[1] != 2 {
		t.Fatalf("match spans = %v; want [1 2] (byte offsets into \"/help\")", hits[0].matches)
	}

	// Contiguous run: "he" becomes ONE styled span (one accent seq).
	row := highlightMatches("/help", hits[0].matches, false, 60)
	seq := styleSeq(tuiPaletteMatchStyle)
	if seq == "" || strings.Count(row, seq) != 1 {
		t.Fatalf("contiguous matches must merge into a single span: %q", row)
	}
	// Selected rows wear the bar's ink instead of the accent.
	sel := highlightMatches("/help", hits[0].matches, true, 60)
	if selSeq := styleSeq(tuiPaletteMatchSelStyle); !strings.Contains(sel, selSeq) {
		t.Fatalf("selected match must use the bar ink: %q", sel)
	}
	// Skipped entirely below 30 cells.
	if plain := highlightMatches("/help", hits[0].matches, false, 20); plain != "/help" {
		t.Fatal("highlight must be skipped when the slot is narrower than 30 cells")
	}
}

// ---------------------------------------------------------------------------
// Cursor bar: ONE style per state, reused by every picker — the selected
// row spans edge to edge with the bar style, unselected rows stay plain.
// ---------------------------------------------------------------------------

func TestPaletteCursorBarStyleIdentity(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256) // SGR sequences needed to assert the bar
	defer lipgloss.SetColorProfile(prev)

	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")
	l := c.layoutFor()
	panel := c.paletteView(l.vpWidth + 2)
	inner := drawerMaxW(c.width, l.vpWidth+2)
	bar := styleSeq(tuiPaletteSelStyle)
	if bar == "" {
		t.Fatal("cursor-bar style must emit an SGR sequence")
	}
	lines := strings.Split(panel, "\n")
	foundSel, foundPlain := 0, 0
	for _, ln := range lines {
		plain := stripANSI(ln)
		if strings.HasPrefix(plain, "/audio") {
			if strings.Contains(ln, bar) {
				foundSel++
				if w := lipgloss.Width(ln); w != inner {
					t.Fatalf("the cursor bar must span the full row width: %d != %d", w, inner)
				}
			} else {
				foundPlain++
			}
		}
	}
	if foundSel != 1 || foundPlain != 0 {
		t.Fatalf("exactly the selected row must wear the bar (sel=%d): sel=%d plain=%d", c.palette.sel, foundSel, foundPlain)
	}
}

// ---------------------------------------------------------------------------
// Geometry caps: min(rows, termH/2-6, 10) rows, min(termW-2, 80) width.
// ---------------------------------------------------------------------------

func TestDrawerGeometryCaps(t *testing.T) {
	// Width: composer column, capped at termW-2 and at 80, floored at 6.
	for _, tc := range []struct{ termW, colW, want int }{
		{80, 56, 56},
		{40, 38, 38},
		{120, 120, 80},
		{200, 140, 80},
		{30, 28, 28},
		{20, 20, 18},
		{10, 10, 8},
		{0, 56, 6},
	} {
		if got := drawerMaxW(tc.termW, tc.colW); got != tc.want {
			t.Errorf("drawerMaxW(%d, %d) = %d; want %d", tc.termW, tc.colW, got, tc.want)
		}
	}
	// Height: min(10, termH/2-6), floored at 1.
	for _, tc := range []struct{ termH, want int }{
		{24, 6}, {40, 10}, {50, 10}, {10, 1}, {8, 1}, {0, 1},
	} {
		if got := drawerMaxRows(tc.termH); got != tc.want {
			t.Errorf("drawerMaxRows(%d) = %d; want %d", tc.termH, got, tc.want)
		}
	}

	// The full frame stays inside an 80x24 terminal with the drawer open.
	c := newFilterScreen("bob", "", "bob", "alice")
	c.vp = *viewportPtr(80, 24)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	sc.input.SetValue("/")
	ensurePaletteOpen(&sc)
	view := sc.View()
	if rows := strings.Count(view, "\n") + 1; rows > 24 {
		t.Fatalf("80x24: drawer frame painted %d rows", rows)
	}
	if mw := maxLineWidth(view); mw > 80 {
		t.Fatalf("80x24: widest row %d exceeds the terminal", mw)
	}
}

// ---------------------------------------------------------------------------
// Mouse + keyboard parity: click selects, hover follows after a one-tick
// lock post filter-change, wheel steps with wrap, Ctrl+C dismisses, and the
// input mode is tracked.
// ---------------------------------------------------------------------------

func TestPaletteMouseParity(t *testing.T) {
	c := newPaletteScreen()
	c, _ = typeKeys(c, "/")
	l := c.layoutFor()
	x := transcriptX0(l) + 2
	// Real item rows from the SAME plan the painter and the hit-test use
	// (the grouped display interleaves headers, so rows are not 2+i).
	itemRows := func(cc *chatScreen) []int {
		y0, _ := cc.drawerYRange(l)
		var ys []int
		for i, r := range cc.palettePanelRows() {
			if r.kind == drItem {
				ys = append(ys, y0+i)
			}
		}
		return ys
	}
	ys := itemRows(&c)
	if len(ys) < 2 {
		t.Fatalf("expected several item rows, got %v", ys)
	}

	// Click selects the row (Enter/Tab still activate).
	c.handleMouse(mouseAt(x, ys[0]))
	if c.palette.sel != 0 {
		t.Fatalf("click first item: sel=%d; want 0", c.palette.sel)
	}
	c.handleMouse(mouseAt(x, ys[1]))
	if c.palette.sel != 1 {
		t.Fatalf("click second item: sel=%d; want 1", c.palette.sel)
	}
	// A click on the chrome (header row) is consumed, selection untouched.
	y0, _ := c.drawerYRange(l)
	c.handleMouse(mouseAt(x, y0+1))
	if c.palette.sel != 1 {
		t.Fatalf("chrome click must not move the selection: sel=%d", c.palette.sel)
	}
	if !c.mouseActive {
		t.Fatal("inputMode tracking: mouse events must mark the mouse as active")
	}

	// Hover follows — but the FIRST motion after a filter change is ignored.
	c2 := newPaletteScreen()
	c2, _ = typeKeys(c2, "/") // filter change → hover lock 1
	if c2.drawerHoverLock != 1 {
		t.Fatalf("filter change must arm the one-tick hover lock, got %d", c2.drawerHoverLock)
	}
	hover := func(y int) {
		c2.handleMouse(tea.MouseMsg{Action: tea.MouseActionMotion, Type: tea.MouseMotion, X: x, Y: y})
	}
	ys2 := itemRows(&c2)
	// The display is group-major, so the RANKED index of the hovered row is
	// not its display position — resolve it from the same panel plan.
	want := func(cc *chatScreen, row int) int {
		y0, _ := cc.drawerYRange(l)
		for i, r := range cc.palettePanelRows() {
			if y0+i == row && r.kind == drItem {
				return r.item
			}
		}
		return -1
	}
	hover(ys2[2])
	if c2.palette.sel != 0 {
		t.Fatal("hover must be ignored 1 tick after a filter change")
	}
	if c2.drawerHoverLock != 0 {
		t.Fatal("the ignored motion must consume the lock")
	}
	hover(ys2[2])
	if c2.palette.sel != want(&c2, ys2[2]) {
		t.Fatalf("hover after the lock must select the hovered row: sel=%d; want %d", c2.palette.sel, want(&c2, ys2[2]))
	}

	// Wheel over the drawer steps the selection (arrow parity, wrap).
	c3 := newPaletteScreen()
	c3, _ = typeKeys(c3, "/")
	c3.handleMouse(tea.MouseMsg{Type: tea.MouseWheelDown, X: x, Y: itemRows(&c3)[0]})
	if c3.palette.sel != 1 {
		t.Fatalf("wheel down: sel=%d; want 1", c3.palette.sel)
	}

	// Ctrl+C dismisses the drawer — the app never quits mid-pick.
	got, cmd := step(c3, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil {
		t.Fatal("ctrl+c on an open drawer must not quit the session")
	}
	if got.palette.visible() {
		t.Fatal("ctrl+c must dismiss the drawer")
	}
	// Ctrl+C with the file browser open dismisses it too.
	p := newPaletteScreen()
	p.openPicker()
	got2, cmd2 := step(p, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd2 != nil || got2.picker.isActive() {
		t.Fatal("ctrl+c must dismiss the file browser, not quit")
	}
}

// ---------------------------------------------------------------------------
// Fuzzy rank on the member stage + the role-grouped plan.
// ---------------------------------------------------------------------------

func TestUsersPlanGroupsByRole(t *testing.T) {
	users := []rosterMember{
		{Username: "bob", Role: "admin"},
		{Username: "carol"},
		{Username: "alice", Role: "creator"},
	}
	plan := usersPlan(users, 10, true)
	var kinds []string
	var items []int
	for _, r := range plan.rows {
		switch r.kind {
		case drHeader:
			kinds = append(kinds, "H:"+r.text)
		case drBlank:
			kinds = append(kinds, "B")
		case drItem:
			kinds = append(kinds, "I")
			items = append(items, r.item)
		default:
			kinds = append(kinds, "?")
		}
	}
	want := []string{"H:Admins", "I", "I", "B", "H:Members", "I"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("plan = %v; want %v", kinds, want)
	}
	if strings.Join(itos(items), ",") != "0,2,1" {
		t.Fatalf("items = %v; want 0,2,1 (ranked order inside each group)", items)
	}
	if plan.pos[0] != 1 || plan.pos[1] != 5 || plan.pos[2] != 2 {
		t.Fatalf("pos = %v; want [1 5 2]", plan.pos)
	}
	// Flattened while filtering: no headers at all.
	flat := usersPlan(users, 10, false)
	for _, r := range flat.rows {
		if r.kind == drHeader || r.kind == drBlank {
			t.Fatal("filtering must flatten the member list")
		}
	}
}

func itos(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = fmt.Sprintf("%d", v)
	}
	return out
}
