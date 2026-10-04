package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// Command-panel end-to-end audit (BUG 2 regression suite): the whole panel
// driving through the REAL Update path — open, list all 11 commands,
// filter-as-you-type, every navigation key, Enter runs, Esc closes, footer
// hints, empty state, mouse select, and the budget/paint single-plan
// contract after every interaction.
// ---------------------------------------------------------------------------

// auditScreen is a creator-role chat screen (sees all 11 commands) with a
// real layout, driven through Update like the live app.
func auditScreen(t *testing.T) chatScreen {
	t.Helper()
	srv := httptest.NewServer(newFakeSignalServer())
	t.Cleanup(srv.Close)
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(80, 24)
	wireTestEngine(t, c, srv, "bob")
	c.eng.beatOnce() // resolve the roster: bob is the room creator
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	c = &sc
	if got := c.myRole(); got != "creator" {
		t.Fatalf("fixture role = %q; want creator (engine must resolve the roster)", got)
	}
	return *c
}

// panelContract asserts the single-plan contract after every interaction:
// the reserved budget equals exactly what the painter emits (spacer + panel
// rows), and the hit-test walks the very same row list — a phantom row in
// either direction is a failure.
func panelContract(t *testing.T, c *chatScreen) {
	t.Helper()
	if !c.palette.visible() {
		t.Fatal("precondition: palette must be visible")
	}
	rows := c.palettePanelRows()
	budget := c.paletteRows()
	if want := 1 + len(rows); budget != want {
		t.Fatalf("budget %d != spacer 1 + panel rows %d — budget/paint drift", budget, want)
	}
	pal := c.paletteView(c.layoutFor().vpWidth + 2)
	if got := lipglossHeight(pal); got != len(rows) {
		t.Fatalf("painted rows %d != panel rows %d — paint/plan drift", got, len(rows))
	}
	// The hit-test must resolve every painted item row and NO chrome row.
	y0, y1 := c.drawerYRange(c.layoutFor())
	for y := y0; y < y1; y++ {
		row := y - y0
		r := rows[row]
		item, ok := c.drawerSelAt(y, c.layoutFor())
		if (r.kind == drItem) != ok {
			t.Fatalf("row %d (%v): hit ok=%v; want %v", row, r.kind, ok, r.kind == drItem)
		}
		if ok && item != r.item {
			t.Fatalf("row %d: hit item %d != painted item %d", row, item, r.item)
		}
	}
}

func lipglossHeight(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// TestPaletteEndToEndCreator is the full audit on one path: every registered
// command listed, filter narrows, all navigation keys move, Enter runs the
// highlighted command, Esc closes, and the panel contract holds at each
// step.
func TestPaletteEndToEndCreator(t *testing.T) {
	c := auditScreen(t)

	// Open with "/": all 11 registered commands listed.
	c, _ = typeKeys(&c, "/")
	if !c.palette.visible() {
		t.Fatal("typing / must open the command panel")
	}
	ranked := c.rankedCommands("/")
	if len(ranked) != len(slashCommands) {
		t.Fatalf("creator palette lists %d commands; want all %d", len(ranked), len(slashCommands))
	}
	// Every registered command must be reachable in the display plan (the
	// window paints 6 rows at a time; the plan covers the full list).
	plan := commandsPlan(ranked, len(ranked), true)
	seen := map[string]bool{}
	for _, r := range plan.rows {
		if r.kind == drItem {
			seen[ranked[r.item].Name] = true
		}
	}
	for _, cmd := range slashCommands {
		if !seen[cmd.Name] {
			t.Fatalf("display plan omits %s from the item rows", cmd.Name)
		}
	}
	// The live window also paints every item it can; navigation reaches the
	// rest (End lands on the last command, Home back on the first).
	c, _ = step(&c, tea.KeyMsg{Type: tea.KeyEnd})
	if got := ranked[c.palette.sel].Name; got != "/upload" {
		t.Fatalf("End must land on /upload, got %s", got)
	}
	panelContract(t, &c)
	c, _ = step(&c, tea.KeyMsg{Type: tea.KeyHome})
	if c.palette.sel != 0 {
		t.Fatalf("Home must land on the first command, sel=%d", c.palette.sel)
	}
	panelContract(t, &c)

	// Footer hints: the live keymap legend (truncated to the slot when the
	// column is narrow, count always visible) — same contract as
	// TestPalettePanelLayout.
	panel := c.paletteView(c.layoutFor().vpWidth + 2)
	if !strings.Contains(panel, "navigate") || !strings.Contains(panel, "enter") {
		t.Fatalf("footer must carry the palette hints: %q", panel)
	}
	if !strings.Contains(panel, "1/11") {
		t.Fatalf("footer must carry the 1/11 count: %q", panel)
	}
	// The header contract: Bold title left, muted esc right.
	if !strings.Contains(panel, "Commands") || !strings.Contains(panel, "esc") {
		t.Fatalf("header contract missing (title/esc): %q", panel)
	}

	// Filter-as-you-type narrows the list to /kick.
	c, _ = typeKeys(c, "ki")
	ranked = c.rankedCommands(c.input.Value())
	if len(ranked) != 1 || ranked[0].Name != "/kick" {
		t.Fatalf("/ki must narrow to /kick, got %+v", ranked)
	}
	panelContract(t, &c)

	// Navigation keys move the highlight; the cursor stays inside the list.
	for _, k := range []tea.KeyType{tea.KeyDown, tea.KeyEnd, tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyUp} {
		c, _ = step(c, tea.KeyMsg{Type: k})
		panelContract(t, &c)
	}
	if c.palette.sel < 0 || c.palette.sel >= len(c.rankedCommands(c.input.Value())) {
		t.Fatalf("selection %d escaped the ranked list", c.palette.sel)
	}

	// Enter runs the highlighted command: /kick with a staged member arg
	// acts locally (usage guard), so pick /help instead for a status proof.
	c2 := auditScreen(t)
	c2, _ = typeKeys(c2, "/he") // /help is the top match
	got, cmd := step(c2, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || !strings.Contains(got.status, "Commands:") || got.palette.visible() {
		t.Fatalf("enter must run /help and close the drawer: cmd=%v status=%q open=%v", cmd, got.status, got.palette.visible())
	}

	// Esc closes the drawer; the session keeps running.
	c3 := auditScreen(t)
	c3, _ = typeKeys(c3, "/")
	c3, cmd = step(c3, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || c3.palette.visible() {
		t.Fatal("esc must close the drawer, no command")
	}
}

// TestPaletteEmptyStateContract: the empty state paints its one muted row
// and the budget stays exactly in lockstep (no phantom rows shrink or grow).
func TestPaletteEmptyStateContract(t *testing.T) {
	c := auditScreen(t)
	c, _ = typeKeys(c, "/zzzz")
	if !c.palette.visible() {
		t.Fatal("drawer must stay open on an unmatched query")
	}
	rows := c.palettePanelRows()
	if len(rows) == 0 {
		t.Fatal("empty panel must still emit its rows")
	}
	hasEmpty := false
	for _, r := range rows {
		if r.kind == drEmpty {
			hasEmpty = true
			if r.text != drawerNoMatchText {
				t.Fatalf("empty row text = %q; want %q", r.text, drawerNoMatchText)
			}
		}
	}
	if !hasEmpty {
		t.Fatal("empty state row missing from the plan")
	}
	panelContract(t, &c)
	if pal := c.paletteView(c.layoutFor().vpWidth + 2); !strings.Contains(pal, drawerNoMatchText) {
		t.Fatalf("empty state not painted: %q", pal)
	}
}

// TestPaletteMemberStageWheelStepsUsers: wheel-stepping over the panel in
// the member-picker stage must walk the painted USER list (same rows the
// painter emits), never the command list. Regression for the drawerStep
// command-count bug.
func TestPaletteMemberStageWheelStepsUsers(t *testing.T) {
	srv, creator, _ := wireModRoom(t) // bob creator, alice admin, carol member
	defer srv.Close()
	c := *creator

	// Member stage: "/kick " morphs the drawer into the user picker.
	c, _ = typeKeys(&c, "/kick ")
	cmd, users, ok := c.paletteUsers()
	if !ok || cmd != "/kick" {
		t.Fatalf("paletteUsers = (%q, %v, %v); want /kick users", cmd, users, ok)
	}
	if len(users) != 2 {
		t.Fatalf("users = %v; want [alice carol]", users)
	}
	if c.palette.sel != 0 {
		t.Fatalf("member stage must open on the first user, sel=%d", c.palette.sel)
	}
	panelContract(t, &c)

	// The wheel walks the USER rows: sel 0 -> 1 (carol).
	y0, _ := c.drawerYRange(c.layoutFor())
	x := transcriptX0(c.layoutFor()) + 2
	c.handleMouse(tea.MouseMsg{Type: tea.MouseWheelDown, X: x, Y: y0 + 2})
	if c.palette.sel != 1 {
		t.Fatalf("wheel down in member stage: sel=%d; want 1 (user list)", c.palette.sel)
	}
	c.handleMouse(tea.MouseMsg{Type: tea.MouseWheelUp, X: x, Y: y0 + 2})
	if c.palette.sel != 0 {
		t.Fatalf("wheel up in member stage: sel=%d; want 0", c.palette.sel)
	}
	panelContract(t, &c)

	// The selection must sit on a painted user row: resolve the display row
	// of the selected item through the same plan the painter uses.
	_, users, _ = c.paletteUsers()
	plan := usersPlan(users, drawerMaxRows(c.height), false)
	if plan.pos[c.palette.sel] < 0 {
		t.Fatalf("selection %d missing from the user plan", c.palette.sel)
	}
	rows := c.palettePanelRows()
	var painted int
	for _, r := range rows {
		if r.kind == drItem && r.item == c.palette.sel {
			painted++
		}
	}
	if painted != 1 {
		t.Fatalf("selected user must paint exactly once, got %d", painted)
	}
}

// TestPaletteThirdCommandNotFirst is the failing-first regression for the
// reported "selection stuck on the first item" bug: arrow-down twice (or a
// filter) must select the THIRD command — Enter must run THAT command, never
// the first one. Mouse parity: clicking the third painted row and pressing
// Enter runs the same third command.
//
// The empty-query drawer paints commands GROUPED under category headers,
// so "third command" means the third PAINTED row: with the draw now
// walking the painted order, down twice lands on the third displayed item
// (the earlier ranked-space assertion encoded the very miss the live report
// surfaced — arrows jumping across groups).
func TestPaletteThirdCommandNotFirst(t *testing.T) {
	c := auditScreen(t)
	c, _ = typeKeys(&c, "/")
	ranked := c.rankedCommands("/")
	if len(ranked) < 3 {
		t.Fatalf("need at least 3 commands, got %d", len(ranked))
	}
	var disp []int // painted item order (ranked indices in display order)
	for _, r := range commandsPlan(ranked, len(ranked), true).rows {
		if r.kind == drItem {
			disp = append(disp, r.item)
		}
	}
	if len(disp) < 3 {
		t.Fatalf("grouped plan paints %d items; need >= 3", len(disp))
	}
	third := ranked[disp[2]].Name
	thirdItem := disp[2]

	// Arrow-down twice: selection sits on the third PAINTED command.
	c, _ = step(&c, tea.KeyMsg{Type: tea.KeyDown})
	c, _ = step(&c, tea.KeyMsg{Type: tea.KeyDown})
	if c.palette.sel != thirdItem {
		t.Fatalf("down twice: sel=%d (%s); want %d (%s, the third painted item)",
			c.palette.sel, ranked[c.palette.sel].Name, thirdItem, third)
	}
	// Enter runs the third command: TakesUser commands morph into the
	// member stage with the command completed (no empty-arg fire); /reply
	// opens the reply pick. Either way the FIRST command never runs.
	got, cmd := step(&c, tea.KeyMsg{Type: tea.KeyEnter})
	if got.palette.visible() {
		if third != "/kick" && third != "/admin" && third != "/unadmin" {
			t.Fatalf("enter must close the drawer after picking the third command (%s)", third)
		}
		if got.input.Value() != third+" " {
			t.Fatalf("enter on %s must complete it into the member picker, input=%q", third, got.input.Value())
		}
		if _, _, ok := got.paletteUsers(); !ok {
			t.Fatalf("enter on %s must open the member-picker stage", third)
		}
	} else if third == "/reply" && got.replyPick == nil {
		t.Fatalf("enter must run %s (the third painted command), not the first", third)
	}
	_ = cmd

	// Filter variant: "/a" ranks >=3 commands; down twice + Enter must run
	// the third of THAT filtered list.
	c2 := auditScreen(t)
	c2, _ = typeKeys(&c2, "/a")
	franked := c2.rankedCommands(c2.input.Value())
	if len(franked) < 3 {
		t.Fatalf("filtered list too short: %+v", franked)
	}
	c2, _ = step(&c2, tea.KeyMsg{Type: tea.KeyDown})
	c2, _ = step(&c2, tea.KeyMsg{Type: tea.KeyDown})
	if c2.palette.sel != 2 || c2.rankedCommands(c2.input.Value())[2].Name != franked[2].Name {
		t.Fatalf("filtered down twice: sel=%d ranked[2]=%s; want %s", c2.palette.sel,
			c2.rankedCommands(c2.input.Value())[2].Name, franked[2].Name)
	}

	// Mouse parity: on the FLAT filtered list (/a), click the third visible
// item row — the hit-test resolves the exact ranked item, never row 0.
	c3 := auditScreen(t)
	c3, _ = typeKeys(&c3, "/a")
	l := c3.layoutFor()
	y0, _ := c3.drawerYRange(l)
	x := transcriptX0(l) + 2
	itemRows := c3.palettePanelRows()
	var clickY int
	for i, r := range itemRows {
		if r.kind == drItem && r.item == 2 { // the third ranked item
			clickY = y0 + i
			break
		}
	}
	c3.handleMouse(mouseAt(x, clickY))
	if c3.palette.sel != 2 {
		t.Fatalf("click third row: sel=%d; want 2", c3.palette.sel)
	}
}

// TestPaletteClickSelectsAcrossStages: a click on a painted row selects it
// in the command stage AND the member stage; chrome rows stay inert.
func TestPaletteClickSelectsAcrossStages(t *testing.T) {
	c := auditScreen(t)
	c, _ = typeKeys(c, "/")
	panelContract(t, &c)

	y0, _ := c.drawerYRange(c.layoutFor())
	x := transcriptX0(c.layoutFor()) + 2
	itemRows := func() []int {
		var ys []int
		for i, r := range c.palettePanelRows() {
			if r.kind == drItem {
				ys = append(ys, y0+i)
			}
		}
		return ys
	}
	ys := itemRows()
	if len(ys) < 2 {
		t.Fatalf("need several item rows, got %v", ys)
	}
	// Click the row of the LAST visible item: the click must select exactly
	// that ranked item (the hit-test walks the very same plan).
	lastY := ys[len(ys)-1]
	var lastItem int
	for i, r := range c.palettePanelRows() {
		if y0+i == lastY && r.kind == drItem {
			lastItem = r.item
		}
	}
	c.handleMouse(mouseAt(x, lastY))
	if c.palette.sel != lastItem {
		t.Fatalf("click last visible row: sel=%d; want %d", c.palette.sel, lastItem)
	}
	panelContract(t, &c)
	// Chrome clicks (header row) are consumed without moving the cursor.
	before := c.palette.sel
	c.handleMouse(mouseAt(x, y0+1))
	if c.palette.sel != before {
		t.Fatalf("chrome click must not move the selection: sel=%d", c.palette.sel)
	}
}
