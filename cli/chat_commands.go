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
	// Group is the command's category header, shown while the palette query
	// is empty (OpenCode-style grouped listing). It also feeds the fuzzy
	// rank at weight 1x (the name carries 2x).
	Group string
	// TakesUser marks commands whose argument is a room member (/kick bob):
	// once "<cmd> " is typed, the drawer morphs into a member picker.
	TakesUser bool
}

// slashCommands is the full catalogue. Keep it the ONLY place a command is
// declared; execution switches on Name below.
var slashCommands = []slashCommand{
	{Name: "/help", Desc: "show available commands", Group: "General"},
	{Name: "/reply", Desc: "reply to a message", Group: "General"},
	{Name: "/settings", Desc: "invites & notifications", Group: "Settings"},
	{Name: "/new-group", Desc: "create a group (add members with /invite)", Group: "Groups"},
	{Name: "/invite", Desc: "invite room users to this group", Group: "Groups"},
	{Name: "/group-edit", Desc: "rename, transfer admin, or delete this group", Group: "Groups"},
	{Name: "/group-members", Desc: "list group members & roles", Group: "Groups"},
	{Name: "/group-leave", Desc: "leave this group", Group: "Groups"},
	{Name: "/upload", Desc: "send file(s) into the room", Group: "Files"},
	{Name: "/download", Desc: "fetch shared room files", Group: "Files"},
	{Name: "/audio", Desc: "toggle mic to your DM peer / the room", Group: "Voice"},
	{Name: "/kick", Desc: "kick a user (creator/admin only)", Group: "Moderation", TakesUser: true},
	{Name: "/admin", Desc: "grant admin (room creator only)", Group: "Moderation", TakesUser: true},
	{Name: "/unadmin", Desc: "revoke admin (room creator only)", Group: "Moderation", TakesUser: true},
}

// rankSlashCommands orders items for query "query" ("" = no filter).
//
// Ranking contract (what the user sees):
//  1. an empty query keeps plain dictionary order — the drawer then displays
//     the commands GROUPED under their category headers;
//  2. a non-empty query runs the weighted fuzzy rank (title 2x + group 1x),
//     tie-broken by title-prefix, then shorter title, then frecency, then
//     stable registry order; non-matching commands vanish (fzf semantics).
//
// Menu text is matched against the command name without its leading "/"
// plus its group label, so "/ge" finds "General"-grouped commands too.
//
// Pure function => trivially unit-testable and shared by rendering + selection.
func rankSlashCommands(items []slashCommand, query string) []slashCommand {
	return rankSlashCommandsF(items, query, nil, time.Time{})
}

// rankSlashCommandsF is rankSlashCommands with a frecency history (used by
// the live screen; nil disables the tiebreak).
func rankSlashCommandsF(items []slashCommand, query string, frec map[string]frecEntry, now time.Time) []slashCommand {
	q := strings.TrimSpace(query)
	if q == "" || q == "/" {
		out := make([]slashCommand, len(items))
		copy(out, items)
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].Name < out[j].Name // dictionary order inside each group
		})
		return out
	}
	payload := strings.ToLower(strings.TrimPrefix(q, "/"))
	hitFor := func(i int) fuzzyHit {
		stripped := strings.TrimPrefix(items[i].Name, "/")
		h := rankTitleGroup(payload, stripped, items[i].Group)
		if h.matched && len(h.matches) > 0 {
			// display offsets shift by the leading "/" we stripped
			shifted := make([]int, len(h.matches))
			for mi, off := range h.matches {
				shifted[mi] = off + 1
			}
			h.matches = shifted
		}
		return h
	}
	return rankFuzzyList(items, hitFor, func(i int) float64 { return frecFor(frec, items[i].Name, now) })
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

// rankedCommands orders the commands visible to OUR role for the query,
// with the screen's frecency history as the final tiebreak.
// Every palette path (paint, budget, keys, Tab, Enter) must use this — never
// the raw registry — or hidden commands leak back in.
func (c *chatScreen) rankedCommands(query string) []slashCommand {
	return rankSlashCommandsF(visibleSlashCommands(c.myRole()), query, c.frec, time.Now())
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

// rankUsers orders candidates for a fragment: an empty fragment keeps the
// alphabetical order (the drawer then groups members under role headers);
// a non-empty fragment runs the same weighted fuzzy rank as commands —
// title (username) 2x, group (role label) 1x, tie-broken by prefix, shorter,
// frecency — and non-matching members vanish.
func rankUsers(users []rosterMember, query string) []rosterMember {
	return rankUsersF(users, query, nil, time.Time{})
}

// rankUsersF is rankUsers with a frecency history (nil disables it).
func rankUsersF(users []rosterMember, query string, frec map[string]frecEntry, now time.Time) []rosterMember {
	q := strings.TrimSpace(query)
	if q == "" {
		out := make([]rosterMember, len(users))
		copy(out, users)
		sort.SliceStable(out, func(i, j int) bool {
			return strings.ToLower(out[i].Username) < strings.ToLower(out[j].Username)
		})
		return out
	}
	return rankFuzzyList(users,
		func(i int) fuzzyHit { return rankTitleGroup(q, users[i].Username, userGroupLabel(users[i])) },
		func(i int) float64 { return frecFor(frec, users[i].Username, now) })
}

// paletteUsers reports the member-picker state: the command being completed
// plus ranked candidates. ok=false means command mode.
func (c *chatScreen) paletteUsers() (cmd string, users []rosterMember, ok bool) {
	cmd, query, ok := c.userArgTarget(c.input.Value())
	if !ok {
		return "", nil, false
	}
	return cmd, rankUsersF(c.userCandidates(), query, c.frec, time.Now()), true
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
// The drawer is an INLINE strip above the composer (not a boxed overlay):
// one accent top border line, a header, the list window and a footer. Every
// state owns exactly ONE reusable lipgloss style — the cursor bar, the
// plain row, the matched-char chip — so all four pickers share one visual
// language and one style identity.

var (
	// The single top border line borrows the composer's accent so the panel
	// reads as if it lifted straight out of the input box.
	tuiPaletteBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder(), true).
				BorderForeground(colAccent) // the anchored reaction card keeps its rounded box

	// drawerBorderLineStyle is the drawer's single top border line
	// (the inline strip contract — the drawer itself is never a box).
	drawerBorderLineStyle = lipgloss.NewStyle().Foreground(colAccent)

	// Cursor bar — the ONE selected-row style, reused by every picker:
	// accent background, contrasting ink, bold.
	tuiPaletteSelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#062a46"}).
				Background(colAccent)

	// Plain (unselected) rows — the ONE resting style: normal text tone.
	tuiPaletteRowStyle = lipgloss.NewStyle().Foreground(colText)

	// Header titles: bold, in the theme family's normal text tone.
	tuiPaletteTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(colText)

	// Matched-char highlight, fzf-style: accent on unselected rows…
	tuiPaletteMatchStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colAccent)
	// …and the accent-bright ink on the cursor bar.
	tuiPaletteMatchSelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#e8f9ff"})

	// Picked bar — the invite window's multi-select rows: the SAME accent
	// fill as the cursor bar with the ink run the other way (bright on
	// accent), so a picked row reads as a filled tile instantly distinct
	// from the hover bar. The two states must never blur: a row can be
	// picked AND focused at once, and the ✓/matched chips always ride the
	// bright contrast ink so they stay legible on either bar.
	tuiPalettePickStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "#062a46", Dark: "#e8f9ff"}).
				Background(colAccent)

	// Pulse flash — the one-tick "just toggled" flash a fresh pick wears:
	// the harshest flip the theme allows (panel text tone as the fill,
	// accent as ink), so the toggle registers before the eye lands on the
	// ✓. Next tick the row settles into the picked bar.
	tuiPalettePulseStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colAccent).
				Background(colText)

	tuiPaletteDescStyle = lipgloss.NewStyle().Foreground(colDim)

	tuiPaletteHintStyle = lipgloss.NewStyle().Foreground(colFaint)
)

// paletteMaxVisible is the DEFAULT scroll-window size for a bare paletteState
// (tests, pre-resize screens). The live app sizes the window from the
// terminal instead: drawerMaxRows — at most 10 rows, scaled by half the
// terminal height (see chat_drawer.go).
const paletteMaxVisible = 6

// paletteFooterHints is the dim keymap legend under the "/" command list,
// derived from the keys handlePaletteKeys actually binds.
const paletteFooterHints = "↑↓ navigate · tab complete · enter select · esc dismiss"

// paletteUserHints is the legend when the drawer morphed into the member
// picker: Esc steps back to the command instead of dismissing.
const paletteUserHints = "↑↓ navigate · tab complete · enter run · esc back"

// paletteTitleCommands / paletteTitleUsers are the drawer header titles.
const (
	paletteTitleCommands = "Commands"
	paletteTitleUsers    = "Members"
)

// palettePayload extracts the filter text of a "/" query: everything after
// the leading slash, trimmed. "" means the query is empty (grouped display,
// no fuzzy rank).
func palettePayload(input string) string {
	return strings.TrimSpace(strings.TrimPrefix(input, "/"))
}

// ---- palette state -----------------------------------------------------------

// paletteState is the OpenCode-style command drawer attached to the composer:
// typing "/" pops it out ABOVE the input box; it dissolves when the query no
// longer starts with "/", a row is picked, or Esc is pressed.
type paletteState struct {
	open  bool
	sel   int    // highlighted row into the last-ranked list
	off   int    // first visible row of the scroll window (item space)
	win   int    // live window size (0 = paletteMaxVisible default)
	query string // last-seen filter — a change resets the selection
}

// sync recomputes visibility from the live composer text. Filter changes
// (typing/backspace/paste) reset the selection to the top — the live
// synchronous filter contract; cursor moves never re-rank.
func (p *paletteState) sync(text string) {
	p.open = strings.HasPrefix(text, "/")
	q := palettePayload(text)
	if q != p.query {
		p.query = q
		p.sel, p.off = 0, 0
	}
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
	maxOff := max(n-p.effectiveWin(), 0)
	if p.off > maxOff {
		p.off = maxOff
	}
	if p.off < 0 {
		p.off = 0
	}
	if p.sel < p.off {
		p.off = p.sel
	}
	if p.sel > p.off+p.effectiveWin()-1 {
		p.off = p.sel - p.effectiveWin() + 1
	}
}

// effectiveWin is the scroll-window size this state is using right now.
func (p *paletteState) effectiveWin() int {
	if p.win > 0 {
		return p.win
	}
	return paletteMaxVisible
}

// moveUp / moveDown shift the highlight with wrap-around (OpenCode behaviour).
// The window follows: moving past the last visible row scrolls the window
// down by one, moving above the first visible row scrolls it up by one.
// Wrapping jumps the window to the far end.
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

// seqIndexOf locates sel inside a display-order item sequence (-1 when a
// stale cursor no longer appears — e.g. after a re-rank moved it out).
func seqIndexOf(seq []int, sel int) int {
	for i, it := range seq {
		if it == sel {
			return i
		}
	}
	return -1
}

// moveUpOrdered / moveDownOrdered step the highlight through the PAINTED
// item sequence (drawerPlan.displayOrder), with the same wrap-around as
// moveUp/moveDown. Grouped modes (empty "/" query under category headers,
// empty member fragment under role headers) reorder items on screen, so
// stepping the ranked array would jump across groups and skip rows; the
// display-order walk keeps every arrow press on the next visible row.
func (p *paletteState) moveUpOrdered(seq []int) {
	if n := len(seq); n <= 0 {
		p.sel, p.off = 0, 0
		return
	}
	at := seqIndexOf(seq, p.sel)
	if at < 0 {
		at = 0 // stale cursor: wrap to the far end
	}
	p.sel = seq[(at-1+len(seq))%len(seq)]
	p.followSel()
}

func (p *paletteState) moveDownOrdered(seq []int) {
	if n := len(seq); n <= 0 {
		p.sel, p.off = 0, 0
		return
	}
	at := seqIndexOf(seq, p.sel)
	if at < 0 {
		at = len(seq) - 1 // stale cursor: wrap to the top
	}
	p.sel = seq[(at+1)%len(seq)]
	p.followSel()
}

// moveHome / moveEnd / movePage jump the highlight without wrap: Home to the
// top, End to the bottom, pages step by ±10 and CLAMP at the ends (only the
// arrows wrap). The view centres the window on the destination afterwards.
func (p *paletteState) moveHome(n int) {
	if n <= 0 {
		p.sel, p.off = 0, 0
		return
	}
	p.sel = 0
}

func (p *paletteState) moveEnd(n int) {
	if n <= 0 {
		p.sel, p.off = 0, 0
		return
	}
	p.sel = n - 1
}

func (p *paletteState) movePage(n, d int) {
	if n <= 0 {
		p.sel, p.off = 0, 0
		return
	}
	p.sel += d
	if p.sel < 0 {
		p.sel = 0
	} else if p.sel >= n {
		p.sel = n - 1
	}
}

// followSel scrolls the window the minimum needed to keep sel visible.
func (p *paletteState) followSel() {
	if p.sel < p.off {
		p.off = p.sel
	} else if p.sel > p.off+p.effectiveWin()-1 {
		p.off = p.sel - p.effectiveWin() + 1
	}
}

// close hides the drawer and forgets the stale cursor position.
func (p *paletteState) close() {
	p.open = false
	p.sel = 0
	p.off = 0
	p.query = ""
}

// paletteRows is the exact terminal-row budget the drawer slot consumes
// right now: the file browser (/upload), the "/" command drawer, or the "@"
// member dropdown — one mode owns the slot at a time. In file-browser mode
// the picker's budget takes over. The reaction picker is NOT budgeted here:
// it paints as a transcript row anchored above its message, so the drawer
// slot stays free. Width never affects the budget (rows truncate, they do
// not wrap), so this number is deterministic BEFORE layout math runs — which
// is what lets computeLayoutWithPalette reserve it up front. The budget
// derives from the SAME panel rows the painter and the mouse hit-test emit
// (drawerPanelRows), so the reserved height can never drift from the
// painted height.
func (c chatScreen) paletteRows() int {
	if c.picker.isActive() {
		return c.pickerRows()
	}
	if c.palette.visible() {
		return 1 + len(c.palettePanelRows())
	}
	if c.mention.visible() {
		if _, _, ok := c.mentionCandidates(); !ok {
			return 0
		}
		return 1 + len(c.mentionPanelRows())
	}
	return 0
}

// quoteRows is the exact terminal-row budget a pinned reply citation
// consumes above the composer: one tinted row, or none.
func (c chatScreen) quoteRows() int {
	if c.composerQuote != nil && c.composerQuote.ReplyTo != "" {
		return 1
	}
	return 0
}

// layoutFor is THE geometry every paint/hit-test must agree on: it folds the
// live "/" drawer budget into the pure layout function.
func (c chatScreen) layoutFor() layout {
	l := computeLayoutMedia(c.width, c.height, c.status != "", c.paletteRows(), c.quoteRows())
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

// ---- drawer panels ----------------------------------------------------------
//
// drawerPanelRows is THE single source of the drawer's painted row plan:
// budget (paletteRows), paint (paletteView/drawerView) and the mouse
// hit-test (drawerSelAt) all derive from these rows, so the reserved height
// can never drift from the painted height and a click always maps to the row
// that is actually on screen.

// drawerPanelRows returns the exact panel rows the active mode paints
// (nil = the slot is free).
func (c chatScreen) drawerPanelRows() []drawerRow {
	if c.picker.isActive() {
		return c.pickerPanelRows(0) // count-only: rows never wrap, width is free
	}
	if c.palette.visible() {
		return c.palettePanelRows()
	}
	if c.mention.visible() {
		return c.mentionPanelRows()
	}
	return nil
}

// commandsPlan builds the display plan for the "/" command drawer: grouped
// under category headers while the query is empty, flat while filtering.
func commandsPlan(ranked []slashCommand, win int, grouped bool) drawerPlan {
	return buildDrawerPlan(len(ranked), grouped, func(i int) string { return ranked[i].Group }, win)
}

// usersPlan builds the display plan for a member list: grouped under
// role headers (Admins/Members) while the fragment is empty, flat while
// filtering.
func usersPlan(users []rosterMember, win int, grouped bool) drawerPlan {
	return buildDrawerPlan(len(users), grouped, func(i int) string { return userGroupLabel(users[i]) }, win)
}

// userGroupLabel is the header an empty-query member list groups under.
func userGroupLabel(u rosterMember) string {
	if u.Role == "creator" || u.Role == "admin" {
		return "Admins"
	}
	return "Members"
}

// palettePanelRows builds the painted rows of the "/" drawer (command stage
// or member-picker stage): border, header, windowed rows, overflow, footer.
func (c chatScreen) palettePanelRows() []drawerRow {
	if _, users, ok := c.paletteUsers(); ok {
		_, frag, _ := c.userArgTarget(c.input.Value())
		win := drawerMaxRows(c.height)
		plan := usersPlan(users, win, frag == "")
		plan.hits = c.userHits(users, frag)
		return c.listPanelRows(&c.palette, len(users), plan, paletteTitleUsers, paletteUserHints,
			func(i int) string { return users[i].Username }, func(i int) string { return userRoleTag(users[i].Role) })
	}
	ranked := c.rankedCommands(c.input.Value())
	payload := palettePayload(c.input.Value())
	grouped := payload == ""
	win := drawerMaxRows(c.height)
	plan := commandsPlan(ranked, win, grouped)
	plan.hits = c.commandHits(ranked, payload)
	return c.listPanelRows(&c.palette, len(ranked), plan, paletteTitleCommands, paletteFooterHints,
		func(i int) string { return ranked[i].Name }, func(i int) string { return ranked[i].Desc })
}

// commandHits precomputes the fuzzy match data (once per filter keystroke)
// for the ranked command list; display offsets shift by the leading "/"
// that the rank stripped.
func (c chatScreen) commandHits(ranked []slashCommand, payload string) []fuzzyHit {
	hits := make([]fuzzyHit, len(ranked))
	for i, cmd := range ranked {
		if payload == "" {
			hits[i] = fuzzyHit{matched: true}
			continue
		}
		h := rankTitleGroup(payload, strings.TrimPrefix(cmd.Name, "/"), cmd.Group)
		if h.matched && len(h.matches) > 0 {
			shifted := make([]int, len(h.matches))
			for mi, off := range h.matches {
				shifted[mi] = off + 1
			}
			h.matches = shifted
		}
		hits[i] = h
	}
	return hits
}

// userHits precomputes the fuzzy match data for a member list (fragment "" =
// plain rows, no highlight).
func (c chatScreen) userHits(users []rosterMember, frag string) []fuzzyHit {
	hits := make([]fuzzyHit, len(users))
	for i, u := range users {
		if frag == "" {
			hits[i] = fuzzyHit{matched: true}
			continue
		}
		hits[i] = rankTitleGroup(frag, u.Username, userGroupLabel(u))
	}
	return hits
}

// listPanelRows assembles the shared list panel around a display plan: the
// single top border, the header contract row, the windowed rows (grouped
// headers + blank separators, or the flat ranked list), the overflow marker
// and the keymap footer. itemSearchOf supplies each item's highlight source
// (title); the row carries the precomputed hit, so the matched-char
// highlight runs once per filter keystroke, not once per row.
func (c chatScreen) listPanelRows(state *paletteState, items int, plan drawerPlan, title, hints string, itemSearchOf, itemDescOf func(i int) string) []drawerRow {
	rows, below, above := windowDrawerPlan(plan, state.off, state.sel)
	panel := make([]drawerRow, 0, len(rows)+5)
	panel = append(panel, drawerRow{kind: drBorder}, drawerRow{kind: drHeader, text: title})
	if len(rows) == 0 {
		panel = append(panel, drawerRow{kind: drEmpty, text: drawerNoMatchText})
	} else {
		for _, r := range rows {
			if r.kind == drItem {
				r.text = itemSearchOf(r.item)
				r.desc = itemDescOf(r.item)
				r.hit = plan.hitOf(r.item)
			}
			panel = append(panel, r)
		}
		if below > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: below, text: "more"})
		} else if above > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: above, text: "above"})
		}
	}
	panel = append(panel, drawerRow{kind: drFooter, text: hints, n: items})
	return panel
}

// drawerNoMatchText is the muted empty-state row for the list pickers.
const drawerNoMatchText = "No results found"

// ---- window settling ---------------------------------------------------------

// settleActiveDrawer re-anchors the active picker's window (plan-aware:
// group headers shift display rows, so the item-space follow used by the
// bare paletteState is not enough here). Called from the key/mouse handlers
// (pointer receivers) so the settled state is in place before the next
// paint — budget, paint and hit-test then all agree on the same window.
func (c *chatScreen) settleActiveDrawer() {
	win := drawerMaxRows(c.height)
	switch {
	case c.picker.isActive():
		settleDrawerWindow(&c.picker.cursor, &c.picker.offset, c.pickerItemPlan(win))
	case c.palette.visible():
		if _, users, ok := c.paletteUsers(); ok {
			_, frag, _ := c.userArgTarget(c.input.Value())
			settleDrawerWindow(&c.palette.sel, &c.palette.off, usersPlan(users, win, frag == ""))
		} else {
			ranked := c.rankedCommands(c.input.Value())
			settleDrawerWindow(&c.palette.sel, &c.palette.off,
				commandsPlan(ranked, win, palettePayload(c.input.Value()) == ""))
		}
	case c.mention.visible():
		_, users, ok := c.mentionCandidates()
		if ok {
			frag, _ := mentionQuery(c.input.Value())
			settleDrawerWindow(&c.mention.sel, &c.mention.off, usersPlan(users, win, frag == ""))
		}
	}
}

// centerActiveDrawer re-anchors the window CENTERED on the current selection
// — the behaviour for typing resets, Home/End jumps and ±10 page moves.
func (c *chatScreen) centerActiveDrawer() {
	win := drawerMaxRows(c.height)
	switch {
	case c.picker.isActive():
		centerDrawerWindow(&c.picker.cursor, &c.picker.offset, c.pickerItemPlan(win))
	case c.palette.visible():
		if _, users, ok := c.paletteUsers(); ok {
			_, frag, _ := c.userArgTarget(c.input.Value())
			centerDrawerWindow(&c.palette.sel, &c.palette.off, usersPlan(users, win, frag == ""))
		} else {
			ranked := c.rankedCommands(c.input.Value())
			centerDrawerWindow(&c.palette.sel, &c.palette.off,
				commandsPlan(ranked, win, palettePayload(c.input.Value()) == ""))
		}
	case c.mention.visible():
		_, users, ok := c.mentionCandidates()
		if ok {
			frag, _ := mentionQuery(c.input.Value())
			centerDrawerWindow(&c.mention.sel, &c.mention.off, usersPlan(users, win, frag == ""))
		}
	}
}

// ---- rendering ---------------------------------------------------------------

// renderPanel paints a drawer row plan into the panel string — the shared
// drawerPanelView painter (chat_drawer.go), whose every row is single-line
// and pinned to the inner width, so the painted height equals the row count
// exactly (the paletteRows() budget) and nothing ever wraps.
func (c chatScreen) renderPanel(maxW int, rows []drawerRow, sel int) string {
	return drawerPanelView(maxW, rows, sel)
}

// paletteView renders the drawer for the current composer text: the member
// picker when "<cmd> <fragment>" is live, the "/" command list otherwise.
// maxW is the outer width budget; the panel caps itself at
// min(maxW, termW-2, 80) and never wraps. The empty state (nothing matches)
// paints its muted row instead of vanishing.
func (c chatScreen) paletteView(maxW int) string {
	if !c.palette.visible() || maxW < 6 {
		return ""
	}
	return c.renderPanel(drawerMaxW(c.width, maxW), c.palettePanelRows(), c.palette.sel)
}

// drawerItemSeq returns the DISPLAY-order item sequence for whichever list
// picker owns the drawer slot right now: the "/" command stage, its member
// stage, or the "@" dropdown. Grouped plans reorder items under headers, so
// arrow/wheel stepping must walk this sequence — never the raw ranked
// array — or the highlight jumps across groups.
func (c chatScreen) drawerItemSeq() []int {
	switch {
	case c.palette.visible():
		if _, users, ok := c.paletteUsers(); ok {
			_, frag, _ := c.userArgTarget(c.input.Value())
			return usersPlan(users, drawerMaxRows(c.height), frag == "").displayOrder()
		}
		ranked := c.rankedCommands(c.input.Value())
		return commandsPlan(ranked, drawerMaxRows(c.height), palettePayload(c.input.Value()) == "").displayOrder()
	case c.mention.visible():
		_, users, ok := c.mentionCandidates()
		if !ok {
			return nil
		}
		frag, _ := mentionQuery(c.input.Value())
		return usersPlan(users, drawerMaxRows(c.height), frag == "").displayOrder()
	}
	return nil
}

// ---- palette key handling ----------------------------------------------------

// handlePaletteKeys intercepts keys while the drawer is open. It returns
// handled=true when the key was consumed (the caller must then SKIP normal
// editing), optionally returning an action to run afterwards.
func (c *chatScreen) handlePaletteKeys(msg tea.KeyMsg) (handled bool, action func() tea.Cmd) {
	if !c.palette.visible() {
		return false, nil
	}
	// The live window derives from the terminal (min(10, termH/2-6)), and
	// every selection move re-settles the window plan-aware afterwards.
	c.palette.win = drawerMaxRows(c.height)
	defer c.settleActiveDrawer()
	// Member-picker stage: the same drawer completes usernames for the
	// pending moderation command. Empty candidate list falls through to
	// submitLine so a hand-typed (possibly stale-presence) name still
	// reaches the server for validation.
	if cmd, users, ok := c.paletteUsers(); ok && len(users) > 0 {
		switch msg.Type {
		case tea.KeyUp:
			c.palette.moveUpOrdered(c.drawerItemSeq())
			return true, nil
		case tea.KeyDown:
			c.palette.moveDownOrdered(c.drawerItemSeq())
			return true, nil
		case tea.KeyHome:
			c.palette.moveHome(len(users))
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyEnd:
			c.palette.moveEnd(len(users))
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyPgUp:
			c.palette.movePage(len(users), -10)
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyPgDown:
			c.palette.movePage(len(users), 10)
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyTab:
			c.palette.clampSel(len(users))
			if c.palette.sel < len(users) {
				user := users[c.palette.sel].Username
				c.input.SetValue(cmd + " " + user + " ")
				c.palette.close()
				c.bumpFrec(user)
			}
			return true, nil
		case tea.KeyEnter:
			c.palette.clampSel(len(users))
			if c.palette.sel < len(users) {
				user := users[c.palette.sel].Username
				c.input.SetValue("")
				c.palette.close()
				c.bumpFrec(user)
				return true, func() tea.Cmd { return c.runCommand(cmd, user) }
			}
			return true, nil
		case tea.KeyEsc:
			// Step back to the command stage, drawer stays open.
			c.input.SetValue(cmd)
			return true, nil
		case tea.KeyCtrlC:
			// OpenCode parity: Ctrl+C dismisses the drawer outright (Esc
			// steps back to the command stage).
			c.palette.close()
			return true, nil
		}
		return false, nil
	}
	switch msg.Type {
	case tea.KeyUp:
		c.palette.moveUpOrdered(c.drawerItemSeq())
		return true, nil
	case tea.KeyDown:
		c.palette.moveDownOrdered(c.drawerItemSeq())
		return true, nil
	case tea.KeyHome:
		c.palette.moveHome(len(c.rankedCommands(c.input.Value())))
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyEnd:
		c.palette.moveEnd(len(c.rankedCommands(c.input.Value())))
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyPgUp:
		c.palette.movePage(len(c.rankedCommands(c.input.Value())), -10)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyPgDown:
		c.palette.movePage(len(c.rankedCommands(c.input.Value())), 10)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyTab:
		ranked := c.rankedCommands(c.input.Value())
		c.palette.clampSel(len(ranked))
		if c.palette.sel < len(ranked) {
			picked := ranked[c.palette.sel]
			c.input.SetValue(picked.Name + " ") // complete inline
			c.bumpFrec(picked.Name)
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
			c.bumpFrec(name)
			return true, func() tea.Cmd { return c.runCommand(name, "") }
		}
		// Nothing matched. A query shaped like "<cmd> <args>" (a space in
		// the payload) still sends: close the drawer and let the composer
		// dispatch the line, so "/upload foo"+Enter keeps working. A bare
		// unmatched query stays open on its "No results found" state.
		payload := palettePayload(c.input.Value())
		if strings.Contains(payload, " ") {
			line := c.input.Value()
			c.input.SetValue("")
			c.palette.close()
			return true, func() tea.Cmd { return c.submitLine(line) }
		}
		return true, nil
	case tea.KeyEsc:
		c.palette.close()
		return true, nil
	case tea.KeyCtrlC:
		// OpenCode parity: Ctrl+C dismisses the drawer (the app itself
		// keeps running — Ctrl+C only quits with no drawer open).
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
	case "/reply":
		// Keyboard fallback for the right-click menu: enter selection mode
		// with the "<" pointer on the newest message; Up/Down move it,
		// Enter opens Reply / Reply-Privately on the pointed message.
		id := c.newestReactableMsgId()
		if id == "" {
			c.status = "nothing to reply to yet"
			c.rebuildView()
			return nil
		}
		return c.openReplyPick(id)
	case "/settings":
		// The full-screen settings window (invites inbox) opens over the
		// chat; the root model hosts it. Drawers close first — one modal
		// surface at a time.
		c.closeTransientsForWindow()
		return func() tea.Msg { return openSettingsMsg{} }
	case "/new-group":
		// Creation window (Name/Description/Max opens DIRECTLY — members
		// are added later through /invite); see groups_ui.go.
		c.closeTransientsForWindow()
		return func() tea.Msg { return openNewGroupMsg{} }
	case "/invite":
		// Invitation window (groups only): the composer's group view must
		// be open, otherwise the command answers inline and never opens.
		if c.activeGroup == "" {
			c.status = "/invite works inside a group — open the group conversation first"
			c.rebuildView()
			return nil
		}
		// The group view owns the screen's client/engine: park the message
		// and let rootModel build the window from the group session.
		c.closeTransientsForWindow()
		return func() tea.Msg { return openInviteMsg{} }
	case "/group-edit":
		// Administration window (groups only): rename/description, admin
		// transfer, and creator-only dissolve. Members open the same window
		// read-only with the reason.
		if c.activeGroup == "" {
			c.status = "/group-edit works inside a group — open the group conversation first"
			c.rebuildView()
			return nil
		}
		c.closeTransientsForWindow()
		return func() tea.Msg { return openGroupEditMsg{} }
	case "/group-members":
		// Member list (groups only) in the shared drawer craft.
		if c.activeGroup == "" {
			c.status = "/group-members works inside a group — open the group conversation first"
			c.rebuildView()
			return nil
		}
		c.closeTransientsForWindow()
		return func() tea.Msg { return openGroupMembersMsg{} }
	case "/group-leave":
		// Leave (groups only), refused for the LAST admin so a group is
		// never left crownless by accident.
		if c.activeGroup == "" {
			c.status = "/group-leave works inside a group — open the group conversation first"
			c.rebuildView()
			return nil
		}
		return c.leaveGroupCmd(c.activeGroup)
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

// closeTransientsForWindow dismisses every composer-adjacent transient
// before a full-screen window (/settings, /new-group, /invite) opens: the
// "/" and "@" drawers, the anchored reaction rows, the reply menu/pick and
// the pinned quote card. One surface at a time.
func (c *chatScreen) closeTransientsForWindow() {
	c.palette.close()
	c.mention.close()
	c.closeReactionAux()
	c.closeReplyMenu()
	c.closeReplyPick()
	c.composerQuote = nil
	c.sideFilter = ""
	c.input.SetValue("")
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
// Any such edit is a FILTER change, so the mouse hover is put on hold for
// one tick (the parity lock: the next motion event is ignored) — the list
// just re-ranked under the cursor and a stale hover must not fight it.
func ensurePaletteOpen(c *chatScreen) {
	c.palette.sync(c.input.Value())
	c.mention.syncMention(c.input.Value(), c.activeConv() == generalConv)
	c.drawerHoverLock = 1
	if c.palette.visible() {
		// One drawer slot: "/" owns it while open — the "@" dropdown and
		// the reaction aux rows yield.
		c.mention.close()
		c.closeReactionAux() // one drawer slot: commands replace the aux rows
	}
}
