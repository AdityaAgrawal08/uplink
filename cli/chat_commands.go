package main

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- slash commands ----------------------------------------------------------
//
// A single registry drives BOTH the "/" palette panel and command execution,
// so a command can never be listed but not runnable (or vice versa). New
// commands only need one entry here.

type slashCommand struct {
	Name string // canonical form including the leading "/"
	Desc string // one-line hint painted next to the name
}

// slashCommands is the full catalogue. Keep it the ONLY place a command is
// declared; execution switches on Name below.
var slashCommands = []slashCommand{
	{Name: "/help", Desc: "show available commands"},
	{Name: "/upload", Desc: "send file(s) into the room"},
	{Name: "/upload-review", Desc: "review buffered files before sending"},
	{Name: "/send", Desc: "send all buffered files now"},
	{Name: "/download", Desc: "fetch shared room files"},
}

// rankSlashCommands orders items for query "query" ("" = no filter).
//
// Ranking contract (what the user sees):
//  1. commands whose name STARTS WITH the query (case-insensitive) — these are
//     sorted alphabetically among themselves;
//  2. then every remaining command in plain dictionary order.
//
// Pure function => trivially unit-testable and shared by rendering + selection.
func rankSlashCommands(items []slashCommand, query string) []slashCommand {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]slashCommand, len(items))
	copy(out, items)

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Name, out[j].Name
		ap := strings.HasPrefix(a, q)
		bp := strings.HasPrefix(b, q)
		if ap != bp {
			return ap // prefix matches float to the top
		}
		return a < b // dictionary order inside each group
	})
	return out
}

// rankedSlashCommands orders the live registry for the current query.
func rankedSlashCommands(query string) []slashCommand {
	return rankSlashCommands(slashCommands, query)
}

// ---- palette styling ---------------------------------------------------------
//
// The drawer borrows the composer's rounded border + accent colour so the
// panel reads as if it lifted straight out of the input box.

var (
	tuiPaletteBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder(), true).
				BorderForeground(lipgloss.Color("62")) // composer accent

	tuiPaletteSelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("16")).
				Background(lipgloss.Color("62")) // accent chip: selected row

	tuiPaletteMatchStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("212")) // the typed prefix inside each name

	tuiPaletteDescStyle = lipgloss.NewStyle().Faint(true)

	tuiPaletteHintStyle = lipgloss.NewStyle().Faint(true)
)

// paletteMaxVisible caps how many command rows paint before "+N more".
const paletteMaxVisible = 6

// paletteFooterHints is the dim keymap legend under the list.
const paletteFooterHints = "↑↓ select · tab complete · enter run · esc dismiss"

// ---- palette state -----------------------------------------------------------

// paletteState is the OpenCode-style command drawer attached to the composer:
// typing "/" pops it out ABOVE the input box; it dissolves when the query no
// longer starts with "/", a row is picked, or Esc is pressed.
type paletteState struct {
	open bool
	sel  int // highlighted row into the last-ranked list
}

// sync recomputes visibility from the live composer text.
func (p *paletteState) sync(text string) {
	p.open = strings.HasPrefix(text, "/")
}

// visible reports whether the panel should paint right now.
func (p *paletteState) visible() bool { return p.open }

// clampSel keeps the highlight inside the list after every re-rank.
func (p *paletteState) clampSel(n int) {
	if n <= 0 {
		p.sel = 0
		return
	}
	if p.sel >= n {
		p.sel = n - 1
	}
}

// moveUp / moveDown shift the highlight with wrap-around (OpenCode behaviour).
func (p *paletteState) moveUp(n int) {
	if n <= 0 {
		p.sel = 0
		return
	}
	p.sel = ((p.sel-1)%n + n) % n
}

func (p *paletteState) moveDown(n int) {
	if n <= 0 {
		p.sel = 0
		return
	}
	p.sel = (p.sel + 1) % n
}

// close hides the drawer and forgets the stale cursor position.
func (p *paletteState) close() {
	p.open = false
	p.sel = 0
}

// paletteRows is the exact terminal-row budget the drawer consumes right now:
// one blank spacer above the panel, the visible command rows, the keymap
// footer, and the panel's own border. In file-browser mode (/upload) the
// picker's budget takes over. Width never affects it (rows truncate, they do
// not wrap), so this number is deterministic BEFORE layout math runs — which
// is what lets computeLayoutWithPalette reserve it up front.
func (c chatScreen) paletteRows() int {
	if c.picker.isActive() {
		return c.pickerRows()
	}
	if !c.palette.visible() {
		return 0
	}
	n := len(rankedSlashCommands(c.input.Value()))
	if n == 0 {
		return 0
	}
	rows := min(n, paletteMaxVisible)
	if n > rows {
		rows++ // "… +N more" overflow indicator
	}
	return 1 + rows + 1 + 2 // spacer + commands(+overflow) + hints footer + border
}

// layoutFor is THE geometry every paint/hit-test must agree on: it folds the
// live "/" drawer budget into the pure layout function.
func (c chatScreen) layoutFor() layout {
	return computeLayoutWithPalette(c.width, c.height, c.status != "", c.paletteRows())
}

// ---- palette view ------------------------------------------------------------

// drawerView renders whichever mode owns the drawer slot: the file browser
// (picker) or the "/" command list.
func (c chatScreen) drawerView(maxW int) string {
	if c.picker.isActive() {
		return c.pickerView(maxW)
	}
	return c.paletteView(maxW)
}

// paletteView renders the pop-out panel for the current composer text. maxW
// is the outer width budget — the composer's full outer width, so the panel
// reads as one piece with the input box. Every row is padded to the inner
// width (selected row's chip then spans edge to edge) and the whole panel is
// ANSI-truncated if it ever outgrows maxW, so the frame can never wrap.
// Returns "" when the drawer is closed or nothing matches the query.
func (c chatScreen) paletteView(maxW int) string {
	if !c.palette.visible() || maxW < 6 {
		return ""
	}
	ranked := rankedSlashCommands(c.input.Value())
	if len(ranked) == 0 {
		return ""
	}
	c.palette.clampSel(len(ranked))
	shown := min(len(ranked), paletteMaxVisible)

	inner := maxW - 2 // room for the box border
	query := strings.ToLower(strings.TrimSpace(c.input.Value()))

	// fit hard-clamps a styled row to the panel's inner width. Every row
	// must stay single-line or the painted height would drift from the
	// paletteRows() budget reserved inside the layout.
	fit := func(s string) string {
		if lipgloss.Width(s) > inner {
			return lipgloss.NewStyle().MaxWidth(inner).Render(s)
		}
		return s
	}

	nameCol := 0 // dynamic name column: longest visible name + gap
	for _, cmd := range ranked[:shown] {
		if w := lipgloss.Width(cmd.Name); w > nameCol {
			nameCol = w
		}
	}
	nameCol += 2

	rows := make([]string, 0, shown+2)
	for i := 0; i < shown; i++ {
		cmd := ranked[i]
		name := cmd.Name
		// Highlight the typed prefix inside the command name.
		if len(name) >= len(query) && len(query) > 0 &&
			strings.EqualFold(name[:len(query)], query) {
			name = tuiPaletteMatchStyle.Render(name[:len(query)]) + name[len(query):]
		}
		line := fit(padVisible(name, nameCol) + tuiPaletteDescStyle.Render(cmd.Desc))
		line = padVisible(line, inner) // full-width rows: chip reaches both edges
		if i == c.palette.sel {
			line = tuiPaletteSelStyle.Render(line)
		}
		rows = append(rows, line)
	}
	if hidden := len(ranked) - shown; hidden > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d more", hidden)), inner)))
	}
	rows = append(rows, fit(tuiPaletteHintStyle.Render(padVisible(paletteFooterHints, inner))))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(rows, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

// ---- palette key handling ----------------------------------------------------

// visibleCount is how many ranked rows the panel can currently paint; the
// highlight must never wander into hidden rows.
func visibleCount(ranked int) int { return min(ranked, paletteMaxVisible) }

// handlePaletteKeys intercepts keys while the drawer is open. It returns
// handled=true when the key was consumed (the caller must then SKIP normal
// editing), optionally returning an action to run afterwards.
func (c *chatScreen) handlePaletteKeys(msg tea.KeyMsg) (handled bool, action func() tea.Cmd) {
	if !c.palette.visible() {
		return false, nil
	}
	switch msg.Type {
	case tea.KeyUp:
		c.palette.moveUp(visibleCount(len(rankedSlashCommands(c.input.Value()))))
		return true, nil
	case tea.KeyDown:
		c.palette.moveDown(visibleCount(len(rankedSlashCommands(c.input.Value()))))
		return true, nil
	case tea.KeyTab:
		ranked := rankedSlashCommands(c.input.Value())
		c.palette.clampSel(visibleCount(len(ranked)))
		if c.palette.sel < len(ranked) {
			c.input.SetValue(ranked[c.palette.sel].Name + " ") // complete inline
			c.palette.close()
		}
		return true, nil
	case tea.KeyEnter:
		ranked := rankedSlashCommands(c.input.Value())
		c.palette.clampSel(visibleCount(len(ranked)))
		if c.palette.sel < len(ranked) {
			name := ranked[c.palette.sel].Name
			c.input.SetValue("")
			c.palette.close()
			return true, func() tea.Cmd { return c.runCommand(name) }
		}
		return true, nil
	case tea.KeyEsc:
		c.palette.close()
		return true, nil
	}
	return false, nil
}

// runCommand executes a slash command by canonical name. Leaving the session
// is deliberately NOT a command — Ctrl+C is the single exit path.
func (c *chatScreen) runCommand(name string) tea.Cmd {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "/help":
		names := make([]string, 0, len(slashCommands))
		for _, cmd := range slashCommands { // derived, so /help never goes stale
			names = append(names, cmd.Name)
		}
		hint := "* Commands: " + strings.Join(names, " · ") +
			" · type / for the picker · Ctrl+C leaves the session"
		c.appendLine(tuiSystemStyle.Render(hint))
		return nil
	case "/upload":
		return c.openPicker() // morphs the drawer into a file browser
	case "/upload-review":
		return c.openBufferReview() // show buffered files for review
	case "/send":
		return c.sendBuffered() // send all buffered files now
	case "/download":
		return c.openFilesDrawer() // morphs the drawer into the room's files
	default:
		return nil
	}
}

// ---- helpers -----------------------------------------------------------------

// padVisible right-pads a possibly-styled string to n printable cells
// (lipgloss.Width is ANSI-aware), so full-width rows align under borders.
func padVisible(s string, n int) string {
	if w := lipgloss.Width(s); w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-lipgloss.Width(s))
}

// ensurePaletteOpen re-syncs drawer visibility after any edit that was NOT
// intercepted by handlePaletteKeys (plain typing, backspace, paste…). Called
// on every non-intercepted KeyMsg so "/" toggling stays instantaneous.
func ensurePaletteOpen(c *chatScreen) {
	c.palette.sync(c.input.Value())
}
