package main

import (
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

	// Single-entry registry: navigation clamps to the one row, Tab completes.
	c = down(c)
	c = down(c)
	c = up(c)
	if c.palette.sel != 0 {
		t.Fatalf("selection escaped the single visible row: %d", c.palette.sel)
	}
	c, _ = step(c, tea.KeyMsg{Type: tea.KeyTab})
	if got := c.input.Value(); got != "/help " {
		t.Fatalf("tab completion gave %q; want \"/help \"", got)
	}
	if c.palette.visible() {
		t.Fatal("tab completion must close the drawer")
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
			if !strings.Contains(view, "/help") {
				t.Errorf("w=%d h=%d: palette missing from view", w, h)
			}
		}
	}
}
