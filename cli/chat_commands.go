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
	{Name: "/video", Desc: "toggle camera to your DM peer / the room"},
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
	n := len(c.rankedCommands(c.input.Value()))
	if _, users, ok := c.paletteUsers(); ok {
		n = len(users) // member-picker stage budgets member rows, not commands
	}
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
	l := computeLayoutMedia(c.width, c.height, c.status != "", c.paletteRows(), false)
	// Sidebar video box is retired: feeds render in the bottom Live Cameras
	// strip. The sidebar column therefore always starts at the room-header
	// top (rosterY0 from the pure pass already accounts for that).
	l.videoRows = 0
	// Room header above the transcript (center column only): full two-row
	// heading when roomy, compact single row when short or narrow, hidden
	// when every row counts.
	l.headRows = 0
	if l.vpHeight > 6 {
		l.headRows = 1
		if l.vpHeight > 10 && c.width >= 80 {
			l.headRows = 2
		}
	}
	// The sidebar search box grows into the header zone: its rows sit above
	// the first scroll row, so rosterY0 shifts with it (paint + hit-test).
	l.rosterY0 += searchHeightFor(l.headRows, l.sidebarWidth-2) - 1
	// Live-call status card (pinned above the transcript while a call runs).
	l.callRows = 0
	if c.callActive() && l.vpHeight-l.headRows >= 12 {
		l.callRows = callCardRows
	}
	// Composer key-hints footer (truthful bindings only, see keyHintsView).
	l.hintRows = 0
	if c.width >= 70 && l.composerRows > 0 && l.vpHeight-l.headRows-l.callRows >= 8 {
		l.hintRows = 1
	}
	l.vpHeight -= l.headRows + l.callRows + l.hintRows
	if l.vpHeight < 0 {
		l.vpHeight = 0
	}
	// Video UI: right panel when wide (width split), bottom strip when
	// narrow (height split), hidden when neither fits.
	l.vidPanelW, l.camRows = c.videoChrome(l)
	if l.camRows > 0 {
		l.vpHeight -= l.camRows
		if l.vpHeight < 0 {
			l.vpHeight = 0
			l.camRows = 0
		}
	}
	// Right panel shrinks the transcript column (never below readable).
	if l.vidPanelW > 0 {
		l.vpWidth -= l.vidPanelW + 1 // panel + spacer
		if l.vpWidth < 10 {
			l.vpWidth = 10
		}
	}
	// Roster adapts to the settled viewport: search row eats one slot.
	l.rosterSlots = l.vpHeight - 1
	if l.rosterSlots > rosterMaxVisible {
		l.rosterSlots = rosterMaxVisible
	}
	if l.rosterSlots < 0 || !l.sidebarOn {
		l.rosterSlots = 0
	}
	return l
}

// videoPanelWidth is the right video column width (0 = too narrow). Pure:
// ~28% of the terminal, clamped to tile-usable bounds. The panel is the
// only video surface: narrow terminals keep a readable transcript instead
// of a squeezed strip below it.
func videoPanelWidth(termW int) int {
	if termW < 90 {
		return 0
	}
	w := termW * 28 / 100
	if w < 26 {
		w = 26
	}
	if w > 44 {
		w = 44
	}
	return w
}

// videoChrome decides the video UI placement for a settled layout: the
// right panel when it fits, otherwise nothing (the transcript keeps a
// readable floor in both modes). There is no bottom strip: video lives on
// the right, matching the reference layout.
func (c chatScreen) videoChrome(l layout) (panelW, stripRows int) {
	innerW := widthInsideFrame(c.width, l.frameOn)
	sideW := 0
	if l.sidebarOn {
		sideW = l.sidebarWidth + 1
	}
	bodyH := l.headRows + l.vpHeight
	if l.boxedTranscript && l.vpHeight > 0 {
		bodyH += transcriptBorder
	}
	// Right panel: needs width for tiles plus a tall-enough body column.
	// The panel splits width (never height), so even short terminals keep
	// it — tiles degrade gracefully to fewer rows (see videoPanelGeom).
	if w := videoPanelWidth(c.width); w > 0 && bodyH >= 14 {
		if avail := innerW - sideW - (w + 1) - transcriptBorder; avail >= 30 {
			return w, 0
		}
	}
	return 0, 0
}

// videoPaneGeom is the single source of truth for the ASCII picture size:
// the picture tracks the pane's real width (the old fixed-56-col render
// inside a ~20-col viewport cropped ~40% of every frame), with rows from
// the 4:3 aspect at the cell dot ratio (braille 2px×4px, half-block 1px×2px
// — both 3/8 rows per column), capped by the sidebar's half-height budget.
// Uses l.sidebarWidth, never sidebarInnerWidth (which re-derives the
// layout — infinite recursion).
func videoPaneGeom(c chatScreen, l layout) (cols, frameRows, streams int) {
	cols = l.sidebarWidth - 2
	streams = 1
	if len(c.selfLines) > 0 {
		streams = 2
	}
	frameRows = cols * 3 / 8
	budget := (l.vpHeight/2 - 4) / streams
	if frameRows > budget {
		frameRows = budget
	}
	if frameRows < 3 {
		frameRows = 3
	}
	return cols, frameRows, streams
}

// ---- palette view ------------------------------------------------------------

// drawerView renders whichever mode owns the drawer slot: the file browser
// (picker) or the "/" command list. (Remote video lives in the sidebar, not
// here.)
func (c chatScreen) drawerView(maxW int) string {
	if c.picker.isActive() {
		return c.pickerView(maxW)
	}
	return c.paletteView(maxW)
}

// videoActive reports whether the video pane should paint: publishing,
// receiving, or waiting for the first frame (the pane itself is the proof
// the UI path works — it must never stay invisible while video is on).
// Stale frames alone don't hold the split: flags own the layout.
func (c chatScreen) videoActive() bool {
	if c.call == nil {
		return false
	}
	return c.call.VideoOn() || c.call.RxOn() || c.call.Watching()
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

// paletteUsersView renders the member-picker stage: one row per candidate
// (username + role tag), same box/chip/overflow geometry as command rows so
// the height budget in paletteRows() still holds exactly.
func (c chatScreen) paletteUsersView(maxW int, users []rosterMember) string {
	if len(users) == 0 {
		return ""
	}
	c.palette.clampSel(len(users))
	shown := min(len(users), paletteMaxVisible)

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
	for _, u := range users[:shown] {
		if w := lipgloss.Width(u.Username); w > nameCol {
			nameCol = w
		}
	}
	nameCol += 2

	rows := make([]string, 0, shown+2)
	for i := 0; i < shown; i++ {
		u := users[i]
		name := u.Username
		if len(name) >= len(query) && len(query) > 0 &&
			strings.EqualFold(name[:len(query)], query) {
			name = tuiPaletteMatchStyle.Render(name[:len(query)]) + name[len(query):]
		}
		line := fit(padVisible(name, nameCol) + tuiPaletteDescStyle.Render(userRoleTag(u.Role)))
		line = padVisible(line, inner)
		if i == c.palette.sel {
			line = tuiPaletteSelStyle.Render(line)
		}
		rows = append(rows, line)
	}
	if hidden := len(users) - shown; hidden > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d more", hidden)), inner)))
	}
	rows = append(rows, fit(tuiPaletteHintStyle.Render(padVisible(paletteUserHints, inner))))

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
	// Member-picker stage: the same drawer completes usernames for the
	// pending moderation command. Empty candidate list falls through to
	// submitLine so a hand-typed (possibly stale-presence) name still
	// reaches the server for validation.
	if cmd, users, ok := c.paletteUsers(); ok && len(users) > 0 {
		switch msg.Type {
		case tea.KeyUp:
			c.palette.moveUp(visibleCount(len(users)))
			return true, nil
		case tea.KeyDown:
			c.palette.moveDown(visibleCount(len(users)))
			return true, nil
		case tea.KeyTab:
			c.palette.clampSel(visibleCount(len(users)))
			if c.palette.sel < len(users) {
				c.input.SetValue(cmd + " " + users[c.palette.sel].Username + " ")
				c.palette.close()
			}
			return true, nil
		case tea.KeyEnter:
			c.palette.clampSel(visibleCount(len(users)))
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
		c.palette.moveUp(visibleCount(len(c.rankedCommands(c.input.Value()))))
		return true, nil
	case tea.KeyDown:
		c.palette.moveDown(visibleCount(len(c.rankedCommands(c.input.Value()))))
		return true, nil
	case tea.KeyTab:
		ranked := c.rankedCommands(c.input.Value())
		c.palette.clampSel(visibleCount(len(ranked)))
		if c.palette.sel < len(ranked) {
			picked := ranked[c.palette.sel]
			c.input.SetValue(picked.Name + " ") // complete inline
			if picked.TakesUser {
				c.palette.sel = 0 // stay open: morph into the member picker
			} else {
				c.palette.close()
			}
		}
		return true, nil
	case tea.KeyEnter:
		ranked := c.rankedCommands(c.input.Value())
		c.palette.clampSel(visibleCount(len(ranked)))
		if c.palette.sel < len(ranked) {
			picked := ranked[c.palette.sel]
			if picked.TakesUser {
				// Stage two: don't fire with an empty arg (usage ping) —
				// complete the command and morph into the member picker.
				c.input.SetValue(picked.Name + " ")
				c.palette.sel = 0
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
		hint := "* Commands: " + strings.Join(names, " · ") +
			" · type / for the picker · Ctrl+C leaves the session"
		c.appendLocal(c.activeConv(), tuiSystemStyle.Render(hint))
		return nil
	case "/upload":
		return c.openPicker() // morphs the drawer into a file browser
	case "/download":
		return c.openFilesDrawer() // morphs the drawer into the room's files
	case "/video":
		return c.toggleVideo()
	case "/audio":
		return c.toggleAudio()
	case "/kick":
		target, ok := modTarget(arg)
		if !ok {
			c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* usage: /kick <username>"))
			return nil
		}
		return func() tea.Msg {
			roster, epoch, err := c.sig.kickUser(target)
			return kickDoneMsg{target: target, roster: roster, epoch: epoch, err: err}
		}
	case "/admin":
		target, ok := modTarget(arg)
		if !ok {
			c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* usage: /admin <username>"))
			return nil
		}
		return func() tea.Msg {
			roster, epoch, err := c.sig.setRole(target, true)
			return roleDoneMsg{target: target, admin: true, roster: roster, epoch: epoch, err: err}
		}
	case "/unadmin":
		target, ok := modTarget(arg)
		if !ok {
			c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* usage: /unadmin <username>"))
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
// on every non-intercepted KeyMsg so "/" toggling stays instantaneous.
func ensurePaletteOpen(c *chatScreen) {
	c.palette.sync(c.input.Value())
}
