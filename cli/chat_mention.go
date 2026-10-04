package main

// chat_mention.go — @mentions in chat.
//
// Mentions ride as PLAINTEXT inside ordinary E2E messages: nothing here
// mutates message text, adds frame types, or touches the server. Three
// client-side layers:
//
//  1. Composer dropdown — typing "@" opens a member-suggestion drawer above
//     the input (the "/" palette's mirror); Tab/Enter completes "@name "
//     with the cursor past the trailing space.
//  2. Receipt parsing — inbound general-room messages are scanned for
//     "@name" tokens so a mention of the local user can ping the desktop.
//     DMs never ping: there "@" is plain text as far as receipts go.
//  3. Highlight — every valid "@name" token paints as a distinct blue chip
//     inside bubbles, in the room, own messages, and DMs alike
//     (display-only in DMs: the receipt gate above still excludes them).

import (
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// mentionTokenRe matches one mention token: "@" plus the username charset.
// Whitespace, punctuation, or end-of-message terminates the token (the run
// stops at the first character outside [a-zA-Z0-9_]).
var mentionTokenRe = regexp.MustCompile(`@([a-zA-Z0-9_]+)`)

// isMentionRunChar reports whether a byte may appear inside a mention token
// (the username charset; usernames are [a-zA-Z0-9_] server-side, so every
// token a mention can render is single-width).
func isMentionRunChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// isGeneralConv reports whether a message belongs to the common room
// (legacy empty ConvID included, mirroring shouldRender).
func isGeneralConv(convID string) bool {
	return convID == "" || convID == generalConv
}

// ---- receipt parsing ---------------------------------------------------------

// parseMentions extracts every @username token from text, in order.
// Simple rules: a token starts at "@" and runs while characters stay inside
// the username charset, so whitespace, punctuation and end-of-message all
// terminate it — and multiple mentions of the same name each count. The
// local user's own name is skipped (@self ignored); tokens are returned
// verbatim, so a differently cased self-token ("@ME" vs me) still parses
// (case is matched against exact member names at the receipt site).
func parseMentions(text, me string) []string {
	var out []string
	for _, m := range mentionTokenRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		if name == "" || (me != "" && name == me) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// mentionedIn reports whether text names the local user in a mention token
// (exact, case-sensitive match) — the receipt condition for a desktop ping.
func mentionedIn(text, me string) bool {
	if me == "" {
		return false
	}
	for _, m := range mentionTokenRe.FindAllStringSubmatch(text, -1) {
		if m[1] == me {
			return true
		}
	}
	return false
}

// ---- highlight ---------------------------------------------------------------

// tuiMentionStyle paints a mention token inside a bubble: bold in the theme
// family's blue (colAccent — the same adaptive pair as the sidebar/topbar/
// link blue, #0b62c9 on light terminals and #4cc9f0 on dark, so the chip
// stays readable on both). No background fill: the blue foreground against
// the bubble fills (colOtherBg/colOwnBg) is the distinction, and no padding
// change keeps every width calculation exact (the token charset is
// single-width).
var tuiMentionStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(colAccent)

// mentionStyler is the chip style for mention tokens (production:
// tuiMentionStyle). The indirection mirrors mentionNotifier: color styles
// paint as plain text under an Ascii color profile (headless tests), so
// tests swap in a padding style that visibly marks replacements.
var mentionStyler = tuiMentionStyle

// mentionHighlighted wraps every valid "@token" of a rendered string in
// mentionStyler — the same token rules as the receipt parser (the "@" plus
// the username charset, terminated by anything outside it), so any @name in
// a chat message reads as a mention chip: inbound general-room messages,
// own sends, and DM threads (display-only there — receipts stay
// general-room-only). The scan is ANSI-aware: escape sequences pass through
// untouched and only plain-text runs are matched, so the pass composes with
// markdown and bubble styling already painted into the string. A mention is
// the whole token — "@me2" chips in full, not just its "@me" prefix.
func mentionHighlighted(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	var b strings.Builder
	rest := s
	for rest != "" {
		i := strings.Index(rest, "\x1b[")
		if i < 0 {
			b.WriteString(mentionStylePlain(rest))
			break
		}
		b.WriteString(mentionStylePlain(rest[:i]))
		j := strings.IndexByte(rest[i:], 'm')
		if j < 0 {
			b.WriteString(rest[i:]) // malformed sequence: pass through
			break
		}
		b.WriteString(rest[i : i+j+1])
		rest = rest[i+j+1:]
	}
	return b.String()
}

// mentionStylePlain styles every "@token" occurrence inside one ANSI-free
// run. Built on the same regex as the receipt parser, so token boundaries
// (charset, terminators, end-of-message) can never drift from parseMentions.
func mentionStylePlain(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range mentionTokenRe.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[0], m[1]
		b.WriteString(s[last:start])
		b.WriteString(mentionStyler.Render(s[start:end]))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// ---- composer dropdown -------------------------------------------------------

// mentionFooterHints is the dim keymap legend under the "@" member list,
// derived from the keys handleMentionKeys actually binds.
const mentionFooterHints = "↑↓ navigate · tab/enter complete · esc dismiss"

// mentionQuery extracts the live "@"-fragment from composer text (everything
// after the LAST "@") and reports whether a mention is being typed right
// now. ok=false when there is no "@", or the fragment already terminated —
// any character outside the username charset (whitespace included) ends it,
// mirroring the receipt parser, so the dropdown stays live exactly while
// the fragment could still grow into a username. A bare trailing "@" is
// live and shows the full member list.
func mentionQuery(text string) (frag string, ok bool) {
	at := strings.LastIndex(text, "@")
	if at < 0 {
		return "", false
	}
	frag = text[at+1:]
	for i := 0; i < len(frag); i++ {
		if !isMentionRunChar(frag[i]) {
			return "", false
		}
	}
	return frag, true
}

// syncMention derives mention-dropdown visibility from the live composer
// text: open only in the general room, only when the input is not a "/"
// command shape (the command palette owns that), and only while a mention
// fragment is being typed. A fragment change resets the selection to the
// top (the live synchronous filter contract).
func (m *paletteState) syncMention(text string, inGeneral bool) {
	_, live := mentionQuery(text)
	m.open = inGeneral && text != "" && !strings.HasPrefix(text, "/") && live
	frag := ""
	if f, ok := mentionQuery(text); ok {
		frag = f
	}
	if frag != m.query {
		m.query = frag
		m.sel, m.off = 0, 0
	}
}

// mentionCandidates resolves the dropdown state from the live composer:
// the fragment being completed plus the room members matching it — an empty
// fragment lists everyone (alphabetical; the view groups under role
// headers), a non-empty fragment runs the weighted fuzzy rank (members that
// do not contain the full fragment vanish) — self excluded. Source is
// userCandidates: eng.peers() live (refreshed by netRosterMsg), or the
// sidebar snapshot on bare screens. ok=false when no mention fragment is
// live.
func (c *chatScreen) mentionCandidates() (frag string, users []rosterMember, ok bool) {
	frag, ok = mentionQuery(c.input.Value())
	if !ok {
		return "", nil, false
	}
	users = rankUsersF(c.userCandidates(), frag, c.frec, time.Now())
	return frag, users, true
}

// mentionPanelRows builds the "@" dropdown's painted rows — the palette
// mirror: the same border/header/window/footer contract, one row per
// candidate with its role tag, grouped under role headers while the
// fragment is empty, flattened (and fuzzy-ranked, non-matches gone) while
// filtering. The muted "No results found" state paints when nothing matches,
// so the dropdown never flashes empty.
func (c chatScreen) mentionPanelRows() []drawerRow {
	frag, users, ok := c.mentionCandidates()
	if !ok {
		return nil
	}
	win := drawerMaxRows(c.height)
	plan := usersPlan(users, win, frag == "")
	plan.hits = c.userHits(users, frag)
	return c.listPanelRows(&c.mention, len(users), plan, paletteTitleUsers, mentionFooterHints,
		func(i int) string { return users[i].Username }, func(i int) string { return userRoleTag(users[i].Role) })
}

// mentionView renders the "@" member dropdown — the palette mirror: the same
// border/header/window/footer contract as the "/" drawer. Returns "" when
// the dropdown is closed (an unmatched fragment still paints its muted
// "No results found" row).
func (c chatScreen) mentionView(maxW int) string {
	if !c.mention.visible() || maxW < 6 {
		return ""
	}
	if _, _, ok := c.mentionCandidates(); !ok {
		return ""
	}
	return c.renderPanel(drawerMaxW(c.width, maxW), c.mentionPanelRows(), c.mention.sel)
}

// handleMentionKeys intercepts keys while the "@" member dropdown is open
// (mirrors handlePaletteKeys): Up/Down move the highlight, Tab/Enter
// complete "@name " into the composer, Home/End/PgUp/PgDn jump, Esc or
// Ctrl+C dismiss. handled=true means the caller must skip normal editing.
// With no matching candidates the dropdown paints its muted empty state and
// only Esc/Ctrl+C stay meaningful — every other key keeps normal editing,
// so Enter still sends and Tab still cycles focus.
func (c *chatScreen) handleMentionKeys(msg tea.KeyMsg) (handled bool, action func() tea.Cmd) {
	if !c.mention.visible() {
		return false, nil
	}
	c.mention.win = drawerMaxRows(c.height)
	defer c.settleActiveDrawer()
	_, users, ok := c.mentionCandidates()
	if !ok || len(users) == 0 {
		if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
			c.mention.close()
			return true, nil
		}
		return false, nil
	}
	switch msg.Type {
	case tea.KeyUp:
		c.mention.moveUp(len(users))
		return true, nil
	case tea.KeyDown:
		c.mention.moveDown(len(users))
		return true, nil
	case tea.KeyHome:
		c.mention.moveHome(len(users))
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyEnd:
		c.mention.moveEnd(len(users))
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyPgUp:
		c.mention.movePage(len(users), -10)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyPgDown:
		c.mention.movePage(len(users), 10)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyTab, tea.KeyEnter:
		c.mention.clampSel(len(users))
		if c.mention.sel < len(users) {
			c.completeMention(users[c.mention.sel].Username)
		}
		return true, nil
	case tea.KeyEsc, tea.KeyCtrlC:
		c.mention.close()
		return true, nil
	}
	return false, nil
}

// completeMention replaces the live "@fragment" with "@name " — the
// trailing space terminates the token, matching the receipt parser — parks
// the cursor AFTER that space (ready to type the message), and closes the
// dropdown. The completed name feeds the frecency tiebreak.
func (c *chatScreen) completeMention(name string) {
	value := c.input.Value()
	if at := strings.LastIndex(value, "@"); at >= 0 {
		completed := value[:at] + "@" + name + " "
		c.input.SetValue(completed)
		c.input.SetCursor(len(completed))
	}
	c.bumpFrec(name)
	c.mention.close()
}
