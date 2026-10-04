package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

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
	// TakesUser marks commands whose argument is a room member (/kick bob):
	// once "<cmd> " is typed, the drawer morphs into a member picker.
	TakesUser bool
}

// slashCommands is the full catalogue. Keep it the ONLY place a command is
// declared; execution switches on Name below.
var slashCommands = []slashCommand{
	{Name: "/help", Desc: "show available commands"},
	{Name: "/upload", Desc: "send file(s) into the room"},
	{Name: "/download", Desc: "fetch shared room files"},
	{Name: "/audio", Desc: "toggle mic to your DM peer / the room"},
	{Name: "/kick", Desc: "kick a user (creator/admin only)", TakesUser: true},
	{Name: "/admin", Desc: "grant admin (room creator only)", TakesUser: true},
	{Name: "/unadmin", Desc: "revoke admin (room creator only)", TakesUser: true},
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

// visibleSlashCommands filters the registry to what a role may use: the
// room creator (main admin) sees every moderation command, granted admins
// see /kick only, everyone else sees none. Visibility always mirrors
// capability — the server re-enforces every call, so a typed command for a
// hidden row fails closed with the server's 403.
func visibleSlashCommands(role string) []slashCommand {
	out := make([]slashCommand, 0, len(slashCommands))
	for _, cmd := range slashCommands {
		switch cmd.Name {
		case "/admin", "/unadmin":
			if role != "creator" {
				continue
			}
		case "/kick":
			if role != "creator" && role != "admin" {
				continue
			}
		}
		out = append(out, cmd)
	}
	return out
}

// myRole reports our roster role (creator|admin|member). Anything unknown —
// engine unwired, heartbeat not yet in, pre-roles server — fails closed to
// member: moderation rows stay hidden until a beat proves otherwise.
func (c *chatScreen) myRole() string {
	if c.eng == nil {
		return "member"
	}
	for _, m := range c.eng.peers() {
		if m.Username == c.me {
			if m.Role == "creator" || m.Role == "admin" {
				return m.Role
			}
			return "member"
		}
	}
	return "member"
}

// rankedCommands orders the commands visible to OUR role for the query.
// Every palette path (paint, budget, keys, Tab, Enter) must use this — never
// the raw registry — or hidden commands leak back in.
func (c *chatScreen) rankedCommands(query string) []slashCommand {
	return rankSlashCommands(visibleSlashCommands(c.myRole()), query)
}

// ---- second-stage member picker --------------------------------------------
//
// Moderation commands take a username, so the drawer works in two stages:
// "<cmd>" filters commands; "<cmd> <fragment>" morphs the same drawer into
// a member picker (up/down to move, Tab to complete, Enter to run, Esc to
// step back to the command). This is what makes a typed "/kick bob"+Enter
// work at all — without it the open drawer would swallow Enter as a command
// pick and the argument would never reach runCommand.

// userArgTarget parses "<cmd> <fragment>" from live composer text. It
// reports the visible TakesUser command + the username fragment, or ok=false
// when the drawer stays in command mode (no space yet, unknown or hidden
// command, command takes no user).
func (c *chatScreen) userArgTarget(input string) (cmd, query string, ok bool) {
	space := strings.Index(input, " ")
	if space < 0 {
		return "", "", false
	}
	head := strings.ToLower(input[:space])
	if !strings.HasPrefix(head, "/") {
		return "", "", false
	}
	for _, vc := range visibleSlashCommands(c.myRole()) {
		if vc.Name == head && vc.TakesUser {
			return vc.Name, strings.TrimLeft(input[space+1:], " "), true
		}
	}
	return "", "", false
}

// userCandidates lists pickable room members: online, never self, carrying
// roles for the row annotation. Engine presence is the source of truth; the
// sidebar snapshot covers unwired screens.
func (c *chatScreen) userCandidates() []rosterMember {
	var out []rosterMember
	if c.eng != nil {
		for _, m := range c.eng.peers() {
			if m.Online && m.Username != "" && m.Username != c.me {
				out = append(out, m)
			}
		}
	} else {
		for _, u := range c.users {
			if u != "" && u != c.me {
				out = append(out, rosterMember{Username: u, Online: true})
			}
		}
	}
	return out
}

// rankUsers orders candidates for a fragment: prefix matches first
// (case-insensitive), alphabetical inside each group — the same contract as
// rankSlashCommands, so both picker stages feel identical.
func rankUsers(users []rosterMember, query string) []rosterMember {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]rosterMember, len(users))
	copy(out, users)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Username), strings.ToLower(out[j].Username)
		ap, bp := strings.HasPrefix(a, q), strings.HasPrefix(b, q)
		if ap != bp {
			return ap // prefix matches float to the top
		}
		return a < b // dictionary order inside each group
	})
	return out
}

// paletteUsers reports the member-picker state: the command being completed
// plus ranked candidates. ok=false means command mode.
func (c *chatScreen) paletteUsers() (cmd string, users []rosterMember, ok bool) {
	cmd, query, ok := c.userArgTarget(c.input.Value())
	if !ok {
		return "", nil, false
	}
	return cmd, rankUsers(c.userCandidates(), query), true
}

// userRoleTag annotates picker rows: staff stand out, members stay clean.
func userRoleTag(role string) string {
	switch role {
	case "creator":
		return "main admin"
	case "admin":
		return "admin"
	default:
		return ""
	}
}

// ---- palette styling ---------------------------------------------------------
//
// The drawer borrows the composer's rounded border + accent colour so the
// panel reads as if it lifted straight out of the input box.

var (
	// The drawer borrows the composer's accent so the panel reads as if it
	// lifted straight out of the input box.
	tuiPaletteBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder(), true).
				BorderForeground(colAccent) // the focused surface wears the accent

	tuiPaletteSelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "15", Dark: "16"}).
				Background(colAccent) // accent chip: selected row

	tuiPaletteMatchStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "#9d174d", Dark: "#f0abfc"}) // the typed prefix

	tuiPaletteDescStyle = lipgloss.NewStyle().Foreground(colDim)

	tuiPaletteHintStyle = lipgloss.NewStyle().Foreground(colFaint)
)

// paletteMaxVisible is the scroll-window size: at most this many command
// rows paint at once. Longer lists scroll one row at a time with the
// highlight ("… +N more" below, "… +N above" at the tail).
const paletteMaxVisible = 6

// paletteFooterHints is the dim keymap legend under the list.
const paletteFooterHints = "↑↓ select · tab complete · enter run · esc dismiss"

// paletteUserHints is the legend when the drawer morphed into the member
// picker: Esc steps back to the command instead of dismissing.
const paletteUserHints = "↑↓ select user · tab complete · enter run · esc back"

// ---- palette state -----------------------------------------------------------

// paletteState is the OpenCode-style command drawer attached to the composer:
// typing "/" pops it out ABOVE the input box; it dissolves when the query no
// longer starts with "/", a row is picked, or Esc is pressed.
type paletteState struct {
	open bool
	sel  int // highlighted row into the last-ranked list
	off  int // first visible row of the 6-row scroll window
}

// sync recomputes visibility from the live composer text.
func (p *paletteState) sync(text string) {
	p.open = strings.HasPrefix(text, "/")
}

// visible reports whether the panel should paint right now.
func (p *paletteState) visible() bool { return p.open }

// clampSel keeps the highlight inside the list after every re-rank, and
// keeps the scroll window valid with the highlight visible inside it.
func (p *paletteState) clampSel(n int) {
	if n <= 0 {
		p.sel = 0
		p.off = 0
		return
	}
	if p.sel >= n {
		p.sel = n - 1
	}
	if p.sel < 0 {
		p.sel = 0
	}
	maxOff := max(n-paletteMaxVisible, 0)
	if p.off > maxOff {
		p.off = maxOff
	}
	if p.off < 0 {
		p.off = 0
	}
	if p.sel < p.off {
		p.off = p.sel
	}
	if p.sel > p.off+paletteMaxVisible-1 {
		p.off = p.sel - paletteMaxVisible + 1
	}
}

// moveUp / moveDown shift the highlight with wrap-around (OpenCode behaviour).
// The 6-row window follows: moving past the last visible row scrolls the
// window down by one (…5 → 2–7 window), moving above the first visible row
// scrolls it up by one. Wrapping jumps the window to the far end.
func (p *paletteState) moveUp(n int) {
	if n <= 0 {
		p.sel = 0
		p.off = 0
		return
	}
	p.sel = ((p.sel-1)%n + n) % n
	p.followSel()
}

func (p *paletteState) moveDown(n int) {
	if n <= 0 {
		p.sel = 0
		p.off = 0
		return
	}
	p.sel = (p.sel + 1) % n
	p.followSel()
}

// followSel scrolls the window the minimum needed to keep sel visible.
func (p *paletteState) followSel() {
	if p.sel < p.off {
		p.off = p.sel
	} else if p.sel > p.off+paletteMaxVisible-1 {
		p.off = p.sel - paletteMaxVisible + 1
	}
}

// close hides the drawer and forgets the stale cursor position.
func (p *paletteState) close() {
	p.open = false
	p.sel = 0
	p.off = 0
}

// drawerRowsBudget converts a candidate count into the drawer slot budget:
// one blank spacer above the panel, the visible rows (plus the overflow
// indicator), the keymap footer, and the panel's own border.
func drawerRowsBudget(n int) int {
	if n <= 0 {
		return 0
	}
	rows := min(n, paletteMaxVisible)
	if n > rows {
		rows++ // "… +N more" overflow indicator
	}
	return 1 + rows + 1 + 2 // spacer + commands(+overflow) + hints footer + border
}

// paletteRows is the exact terminal-row budget the drawer slot consumes
// right now: the file browser (/upload), the "/" command drawer, or the "@"
// member dropdown — one mode owns the slot at a time. In file-browser mode
// the picker's budget takes over. The reaction picker is NOT budgeted here:
// it paints as a transcript row anchored above its message, so the drawer
// slot stays free. Width never affects the budget (rows truncate, they do
// not wrap), so this number is deterministic BEFORE layout math runs — which
// is what lets computeLayoutWithPalette reserve it up front.
func (c chatScreen) paletteRows() int {
	if c.picker.isActive() {
		return c.pickerRows()
	}
	if c.palette.visible() {
		n := len(c.rankedCommands(c.input.Value()))
		if _, users, ok := c.paletteUsers(); ok {
			n = len(users) // member-picker stage budgets member rows, not commands
		}
		return drawerRowsBudget(n)
	}
	if c.mention.visible() {
		_, users, ok := c.mentionCandidates()
		if !ok {
			return 0
		}
		return drawerRowsBudget(len(users))
	}
	return 0
}

// layoutFor is THE geometry every paint/hit-test must agree on: it folds the
// live "/" drawer budget into the pure layout function.
func (c chatScreen) layoutFor() layout {
	l := computeLayoutMedia(c.width, c.height, c.status != "", c.paletteRows())
	// Chat header above the transcript (main column only): full two-row
	// heading when roomy, compact single row when short or narrow, hidden
	// when every row counts.
	l.headRows = 0
	if l.vpHeight > 6 {
		l.headRows = 1
		if l.vpHeight > 10 && c.width >= 80 {
			l.headRows = 2
		}
	}
	// Composer key-hints footer (truthful bindings only, see keyHintsView);
	// it rides directly under the composer box.
	l.hintRows = 0
	if c.width >= 70 && l.composerRows > 0 && l.vpHeight-l.headRows >= 8 {
		l.hintRows = 1
	}
	l.vpHeight -= l.headRows + l.hintRows
	if l.vpHeight < 0 {
		l.vpHeight = 0
	}
	return l
}

// ---- palette view ------------------------------------------------------------

// drawerView renders whichever mode owns the drawer slot above the composer:
// the file browser (picker) or the "/" command list — or the "@" member
// dropdown, the palette's mirror for mentions. The reaction picker no
// longer competes for this slot — it is anchored in the transcript.
func (c chatScreen) drawerView(maxW int) string {
	if c.picker.isActive() {
		return c.pickerView(maxW)
	}
	if c.palette.visible() {
		return c.paletteView(maxW)
	}
	if c.mention.visible() {
		return c.mentionView(maxW)
	}
	return ""
}

// ---- reaction picker ---------------------------------------------------------

// reactionBarItems is the painted choice order: the six allowlisted emoji
// plus the trailing "+" affordance.
func reactionBarItems() []string {
	out := make([]string, 0, len(reactionEmojis)+1)
	out = append(out, reactionEmojis...)
	return append(out, "+")
}

// reactionCellAt maps a cell offset inside the picker row to its choice
// (ok=false on the brackets and on chip padding). Paint and hit-test share
// reactionBarItems and reactionChip's horizontal-only padding, so they can
// never drift apart.
func reactionCellAt(offset int) (string, bool) {
	pos := 1 // past the leading "["
	for _, item := range reactionBarItems() {
		w := lipgloss.Width(item)
		if offset >= pos+1 && offset < pos+1+w { // +1: the chip's left pad cell
			return item, true
		}
		pos += w + 2 // left pad + item + right pad
	}
	return "", false
}

// reactionPickerRow paints the anchored picker: the six allowlisted emoji plus
// the "+" affordance on one compact row, floating directly above the message
// it targets (not in the drawer slot). My current pick wears the palette's
// selected chip; idle chips reuse the timestamp tone; the brackets reuse the
// drawer's hint tone. Every chip shares the badge's horizontal-only padding
// (reactionChip), so the two surfaces stay sizewise in lockstep. Always one
// line — the row is inserted into the transcript as its own entry.
func (c chatScreen) reactionPickerRow(m chatMessage) string {
	mine := c.myReactions[m.MsgId]
	parts := make([]string, 0, len(reactionEmojis)+1)
	for _, e := range reactionEmojis {
		st := thMsgTimeStyle // idle choice: same faint tone as transcript times
		if e == mine {
			st = tuiPaletteSelStyle // my current pick reads as the selected chip
		}
		parts = append(parts, reactionChip(e, st))
	}
	parts = append(parts, reactionChip("+", tuiPaletteDescStyle))
	return tuiPaletteHintStyle.Render("[") + strings.Join(parts, "") + tuiPaletteHintStyle.Render("]")
}

// reactionPickerView aligns the picker row to the side of the bubble it floats
// above — right for own messages, left for peers — and pins it to the
// transcript width so it can never wrap into a second (backgrounded) row.
func (c chatScreen) reactionPickerView(m chatMessage) string {
	row := c.reactionPickerRow(m)
	w := c.transcriptW()
	if lipgloss.Width(row) > w {
		row = truncateByWidth(row, w) // absurdly narrow: one clipped row beats a wrap
	}
	if m.Username == c.me {
		return lipgloss.NewStyle().Width(w).Align(lipgloss.Right).Render(row)
	}
	return row
}

// ---- reaction detail dropdown ------------------------------------------------

// reactionDetailNames returns the reactor names for one message+emoji from the
// server breakdown; empty when the breakdown is unknown (old server) or does
// not cover this emoji, which sends the caller to the counts+mine fallback.
func (c chatScreen) reactionDetailNames(msgId, emoji string) []string {
	for _, d := range c.reactionDetails[msgId] {
		if d.Emoji != emoji {
			continue
		}
		var names []string
		for _, n := range d.Usernames {
			if n != "" {
				names = append(names, n)
			}
		}
		return names
	}
	return nil
}

// reactionDetailAnimFrames is the number of painted stages in the dropdown's
// open reveal: 0 is the bare border (frame 0), the last stage is fully
// expanded. reactionDetailAnimStep spaces the tea.Tick frames ~150ms apart.
const (
	reactionDetailAnimFrames = 4
	reactionDetailAnimStep   = 50 * time.Millisecond
)

// reactionDetailAnimMsg advances the open dropdown to painted stage frame.
// gen fences out ticks from a superseded open (a dismiss, a reopen, a second
// chip): only the generation currently anchored may repaint.
type reactionDetailAnimMsg struct{ gen, frame int }

// scheduleReactionDetailAnim arms the next reveal stage for generation gen.
func scheduleReactionDetailAnim(gen, frame int) tea.Cmd {
	return tea.Tick(reactionDetailAnimStep, func(time.Time) tea.Msg {
		return reactionDetailAnimMsg{gen: gen, frame: frame}
	})
}

// reactionDetailPlainRows builds the dropdown card's visible text rows for one
// message+emoji, final form: a header "<emoji> <count>", then one "• name" per
// reactor from the server breakdown (which caps names per emoji, so any
// remainder the counts prove is spelled out as "+N more"). When the breakdown
// is missing it falls back to counts+mine — my own pick is named, the rest of
// the tally stays unprovable and is spelled out as the remainder. Empty when
// the emoji has no reactors left.
func (c chatScreen) reactionDetailPlainRows(m chatMessage, emoji string) []string {
	count := c.reactionCounts[m.MsgId][emoji]
	if count <= 0 {
		return nil
	}
	rows := []string{fmt.Sprintf("%s %d", emoji, count)}
	named := 0
	if names := c.reactionDetailNames(m.MsgId, emoji); len(names) > 0 {
		for _, name := range names {
			rows = append(rows, "• "+name)
		}
		named = len(names)
	} else if c.myReactions[m.MsgId] == emoji {
		rows = append(rows, "• "+c.me)
		named = 1
	}
	if extra := count - named; extra > 0 {
		rows = append(rows, fmt.Sprintf("+%d more", extra))
	}
	return rows
}

// reactionDetailInterior paints the plain rows through the badge/palette
// styles: the header wears the badge's unread chip tone, my own row takes the
// palette's selected highlight (padded edge to edge), the cap row stays faint,
// and a faint divider separates the header from the reactors. Returns the
// styled rows (header first, divider second) plus the widest visible row so
// the card border can size itself.
func (c chatScreen) reactionDetailInterior(m chatMessage, emoji string) ([]string, int) {
	plain := c.reactionDetailPlainRows(m, emoji)
	if len(plain) == 0 {
		return nil, 0
	}
	type cardRow struct {
		text     string
		rendered string
		own      bool
	}
	rows := make([]cardRow, 0, len(plain))
	for i, r := range plain {
		switch {
		case i == 0:
			rows = append(rows, cardRow{text: r, rendered: reactionChip(r, tuiUnreadStyle)})
		case c.me != "" && r == "• "+c.me:
			rows = append(rows, cardRow{text: r, rendered: tuiPaletteSelStyle.Render(r), own: true})
		case strings.HasPrefix(r, "+"):
			rows = append(rows, cardRow{text: r, rendered: tuiPaletteHintStyle.Render(r)})
		default:
			rows = append(rows, cardRow{text: r, rendered: r})
		}
	}
	width := 0
	for _, r := range rows {
		if w := lipgloss.Width(r.rendered); w > width {
			width = w
		}
	}
	divider := tuiPaletteHintStyle.Render(strings.Repeat("─", width))
	out := make([]string, 0, len(rows)+1)
	for i, r := range rows {
		if i == 0 {
			out = append(out, r.rendered, divider)
			continue
		}
		if r.own {
			out = append(out, tuiPaletteSelStyle.Render(padVisible(r.text, width)))
			continue
		}
		out = append(out, r.rendered)
	}
	return out, width
}

// reactionDetailPaint renders the dropdown card at animation frame: the
// rounded box anchored below its bubble, showing the first reveal(frame)
// interior rows — frame 0 paints the bare borders (sized for the full card),
// the last frame paints everything. The block is right-aligned for own
// bubbles and flush left for peers, exactly like the picker floating above,
// and clipped to the transcript width so it can never wrap.
func (c chatScreen) reactionDetailPaint(m chatMessage, emoji string, frame int) []string {
	interior, width := c.reactionDetailInterior(m, emoji)
	if len(interior) == 0 {
		return nil
	}
	if frame < 0 {
		frame = 0
	}
	if frame > reactionDetailAnimFrames-1 {
		frame = reactionDetailAnimFrames - 1
	}
	reveal := len(interior) * frame / (reactionDetailAnimFrames - 1)
	shown := interior[:reveal]

	avail := c.transcriptW()
	inner := width
	if maxInner := avail - 4; inner > maxInner { // 2 border + 2 padding cells
		inner = maxInner
	}
	if inner < 1 {
		inner = 1
	}
	for i, s := range shown {
		shown[i] = truncateByWidth(s, inner)
	}
	box := tuiPaletteBoxStyle.Padding(0, 1).Width(inner + 2).
		Render(strings.Join(shown, "\n"))

	lines := strings.Split(box, "\n")
	for i, ln := range lines {
		if lipgloss.Width(ln) > avail {
			ln = truncateByWidth(ln, avail)
		}
		if m.Username == c.me {
			ln = lipgloss.NewStyle().Width(avail).Align(lipgloss.Right).Render(ln)
		}
		lines[i] = ln
	}
	return lines
}

// reactionDetailRows is the fully expanded dropdown for one message+emoji:
// one string per painted line, aligned to the side of the bubble it hangs
// under. Empty when there is nothing to show.
func (c chatScreen) reactionDetailRows(m chatMessage, emoji string) []string {
	return c.reactionDetailPaint(m, emoji, reactionDetailAnimFrames-1)
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
	if _, users, ok := c.paletteUsers(); ok {
		return c.paletteUsersView(maxW, users)
	}
	ranked := c.rankedCommands(c.input.Value())
	if len(ranked) == 0 {
		return ""
	}
	c.palette.clampSel(len(ranked))
	off := c.palette.off
	end := min(off+paletteMaxVisible, len(ranked))

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
	for _, cmd := range ranked[off:end] {
		if w := lipgloss.Width(cmd.Name); w > nameCol {
			nameCol = w
		}
	}
	nameCol += 2

	rows := make([]string, 0, paletteMaxVisible+2)
	for idx := off; idx < end; idx++ {
		cmd := ranked[idx]
		name := cmd.Name
		// Highlight the typed prefix inside the command name.
		if len(name) >= len(query) && len(query) > 0 &&
			strings.EqualFold(name[:len(query)], query) {
			name = tuiPaletteMatchStyle.Render(name[:len(query)]) + name[len(query):]
		}
		line := fit(padVisible(name, nameCol) + tuiPaletteDescStyle.Render(cmd.Desc))
		line = padVisible(line, inner) // full-width rows: chip reaches both edges
		if idx == c.palette.sel {
			line = tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle))
		}
		rows = append(rows, line)
	}
	// Overflow indicator: items below the window keep the old "+N more"
	// wording; at the tail (nothing below, items above) it reads "+N above".
	// The row exists whenever the list exceeds the window, so the painted
	// height always matches the paletteRows() budget.
	if below := len(ranked) - end; below > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d more", below)), inner)))
	} else if above := off; above > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d above", above)), inner)))
	}
	rows = append(rows, fit(tuiPaletteHintStyle.Render(padVisible(paletteFooterHints, inner))))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(rows, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

// paletteUsersView renders the member-picker stage: one row per candidate
// (username + role tag), same box/chip/overflow geometry as command rows so
// the height budget in paletteRows() still holds exactly.
func (c chatScreen) paletteUsersView(maxW int, users []rosterMember) string {
	if len(users) == 0 {
		return ""
	}
	c.palette.clampSel(len(users))
	off := c.palette.off
	end := min(off+paletteMaxVisible, len(users))

	inner := maxW - 2 // room for the box border
	query := strings.ToLower(strings.TrimSpace(c.input.Value()))
	if i := strings.Index(query, " "); i >= 0 {
		query = strings.TrimLeft(query[i+1:], " ") // match the fragment, not "<cmd> "
	}

	fit := func(s string) string {
		if lipgloss.Width(s) > inner {
			return lipgloss.NewStyle().MaxWidth(inner).Render(s)
		}
		return s
	}

	nameCol := 0
	for _, u := range users[off:end] {
		if w := lipgloss.Width(u.Username); w > nameCol {
			nameCol = w
		}
	}
	nameCol += 2

	rows := make([]string, 0, paletteMaxVisible+2)
	for idx := off; idx < end; idx++ {
		u := users[idx]
		name := u.Username
		if len(name) >= len(query) && len(query) > 0 &&
			strings.EqualFold(name[:len(query)], query) {
			name = tuiPaletteMatchStyle.Render(name[:len(query)]) + name[len(query):]
		}
		line := fit(padVisible(name, nameCol) + tuiPaletteDescStyle.Render(userRoleTag(u.Role)))
		line = padVisible(line, inner)
		if idx == c.palette.sel {
			line = tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle))
		}
		rows = append(rows, line)
	}
	if below := len(users) - end; below > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d more", below)), inner)))
	} else if above := off; above > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d above", above)), inner)))
	}
	rows = append(rows, fit(tuiPaletteHintStyle.Render(padVisible(paletteUserHints, inner))))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(rows, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

// ---- palette key handling ----------------------------------------------------

// handlePaletteKeys intercepts keys while the drawer is open. It returns
// handled=true when the key was consumed (the caller must then SKIP normal
// editing), optionally returning an action to run afterwards.
func (c *chatScreen) handlePaletteKeys(msg tea.KeyMsg) (handled bool, action func() tea.Cmd) {
	if !c.palette.visible() {
		return false, nil
	}
	// Member-picker stage: the same drawer completes usernames for the
	// pending moderation command. Empty candidate list falls through to
	// submitLine so a hand-typed (possibly stale-presence) name still
	// reaches the server for validation.
	if cmd, users, ok := c.paletteUsers(); ok && len(users) > 0 {
		switch msg.Type {
		case tea.KeyUp:
			c.palette.moveUp(len(users))
			return true, nil
		case tea.KeyDown:
			c.palette.moveDown(len(users))
			return true, nil
		case tea.KeyTab:
			c.palette.clampSel(len(users))
			if c.palette.sel < len(users) {
				c.input.SetValue(cmd + " " + users[c.palette.sel].Username + " ")
				c.palette.close()
			}
			return true, nil
		case tea.KeyEnter:
			c.palette.clampSel(len(users))
			if c.palette.sel < len(users) {
				user := users[c.palette.sel].Username
				c.input.SetValue("")
				c.palette.close()
				return true, func() tea.Cmd { return c.runCommand(cmd, user) }
			}
			return true, nil
		case tea.KeyEsc:
			// Step back to the command stage, drawer stays open.
			c.input.SetValue(cmd)
			return true, nil
		}
		return false, nil
	}
	switch msg.Type {
	case tea.KeyUp:
		c.palette.moveUp(len(c.rankedCommands(c.input.Value())))
		return true, nil
	case tea.KeyDown:
		c.palette.moveDown(len(c.rankedCommands(c.input.Value())))
		return true, nil
	case tea.KeyTab:
		ranked := c.rankedCommands(c.input.Value())
		c.palette.clampSel(len(ranked))
		if c.palette.sel < len(ranked) {
			picked := ranked[c.palette.sel]
			c.input.SetValue(picked.Name + " ") // complete inline
			if picked.TakesUser {
				c.palette.sel = 0 // stay open: morph into the member picker
				c.palette.off = 0
			} else {
				c.palette.close()
			}
		}
		return true, nil
	case tea.KeyEnter:
		ranked := c.rankedCommands(c.input.Value())
		c.palette.clampSel(len(ranked))
		if c.palette.sel < len(ranked) {
			picked := ranked[c.palette.sel]
			if picked.TakesUser {
				// Stage two: don't fire with an empty arg (usage ping) —
				// complete the command and morph into the member picker.
				c.input.SetValue(picked.Name + " ")
				c.palette.sel = 0
				c.palette.off = 0
				return true, nil
			}
			name := picked.Name
			c.input.SetValue("")
			c.palette.close()
			return true, func() tea.Cmd { return c.runCommand(name, "") }
		}
		return true, nil
	case tea.KeyEsc:
		c.palette.close()
		return true, nil
	}
	return false, nil
}

// runCommand executes a slash command by canonical name + raw argument
// ("/kick bob" → name "/kick", arg "bob"). Leaving the session is
// deliberately NOT a command — Ctrl+C is the single exit path.
func (c *chatScreen) runCommand(name, arg string) tea.Cmd {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "/help":
		visible := visibleSlashCommands(c.myRole())
		names := make([]string, 0, len(visible))
		for _, cmd := range visible { // derived, so /help never goes stale
			names = append(names, cmd.Name)
		}
		c.status = "Commands: " + strings.Join(names, " · ") +
			" · type / for the picker · Ctrl+C leaves the session"
		c.rebuildView()
		return nil
	case "/upload":
		return c.openPicker() // morphs the drawer into a file browser
	case "/download":
		return c.openFilesDrawer() // morphs the drawer into the room's files
	case "/audio":
		return c.toggleAudio()
	case "/kick":
		target, ok := modTarget(arg)
		if !ok {
			c.status = "usage: /kick <username>"
			c.rebuildView()
			return nil
		}
		return func() tea.Msg {
			roster, epoch, err := c.sig.kickUser(target)
			return kickDoneMsg{target: target, roster: roster, epoch: epoch, err: err}
		}
	case "/admin":
		target, ok := modTarget(arg)
		if !ok {
			c.status = "usage: /admin <username>"
			c.rebuildView()
			return nil
		}
		return func() tea.Msg {
			roster, epoch, err := c.sig.setRole(target, true)
			return roleDoneMsg{target: target, admin: true, roster: roster, epoch: epoch, err: err}
		}
	case "/unadmin":
		target, ok := modTarget(arg)
		if !ok {
			c.status = "usage: /unadmin <username>"
			c.rebuildView()
			return nil
		}
		return func() tea.Msg {
			roster, epoch, err := c.sig.setRole(target, false)
			return roleDoneMsg{target: target, admin: false, roster: roster, epoch: epoch, err: err}
		}
	default:
		return nil
	}
}

// modTarget normalizes a moderation-command argument: one username, with an
// optional leading @ forgiven. Usernames are [a-zA-Z0-9_]{3,20} server-side;
// the server re-validates strictly, this just catches empty/garbage early.
func modTarget(arg string) (string, bool) {
	t := strings.TrimPrefix(strings.TrimSpace(arg), "@")
	if t == "" || strings.ContainsAny(t, " \t") || len(t) > 20 {
		return "", false
	}
	return t, true
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
// on every non-intercepted KeyMsg so "/" and "@" toggling stay instantaneous.
func ensurePaletteOpen(c *chatScreen) {
	c.palette.sync(c.input.Value())
	c.mention.syncMention(c.input.Value(), c.activeConv() == generalConv)
	if c.palette.visible() {
		// One drawer slot: "/" owns it while open — the "@" dropdown and
		// the reaction aux rows yield.
		c.mention.close()
		c.closeReactionAux() // one drawer slot: commands replace the aux rows
	}
}
