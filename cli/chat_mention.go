package main

// chat_mention.go — @mentions in the general room.
//
// Mentions ride as PLAINTEXT inside ordinary E2E messages: nothing here
// mutates message text, adds frame types, or touches the server. Three
// client-side layers:
//
//  1. Composer dropdown — typing "@" opens a member-suggestion drawer above
//     the input (the "/" palette's mirror); Tab/Enter completes "@name ".
//  2. Receipt parsing — inbound general messages are scanned for "@name"
//     tokens so a mention of the local user can ping the desktop.
//  3. Highlight — "@<own-username>" paints as a distinct chip inside
//     bubbles.
//
// DMs are excluded everywhere: there "@" is plain text.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

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

// mentionExcerptMax caps an @mention notification body so a desktop ping
// stays one tidy line.
const mentionExcerptMax = 80

// mentionExcerpt slims a message for the notification body: whitespace runs
// (newlines included) collapse to single spaces, then the excerpt truncates.
func mentionExcerpt(text string) string {
	one := strings.Join(strings.Fields(text), " ")
	if r := []rune(one); len(r) > mentionExcerptMax {
		one = string(r[:mentionExcerptMax])
	}
	return one
}

// ---- highlight ---------------------------------------------------------------

// tuiMentionStyle paints an own-username mention inside a bubble: the
// palette's selected-chip family (bold + accent background — same values as
// tuiPaletteSelStyle), so a name that pings you reads as a distinct chip.
// No padding change: the token charset is single-width, so every width
// calculation stays exact.
var tuiMentionStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.AdaptiveColor{Light: "15", Dark: "16"}).
	Background(colAccent)

// mentionStyler is the chip style for own-username mentions (production:
// tuiMentionStyle). The indirection mirrors mentionNotifier: color styles
// paint as plain text under an Ascii color profile (headless tests), so
// tests swap in a padding style that visibly marks replacements.
var mentionStyler = tuiMentionStyle

// mentionHighlighted wraps every exact "@me" token of a rendered string in
// mentionStyler. The scan is ANSI-aware: escape sequences pass through
// untouched and only plain-text runs are matched, so the pass composes with
// markdown and bubble styling already painted into the string. A mention is
// the whole token — "@me" inside "@me2" stays plain.
func mentionHighlighted(s, me string) string {
	if me == "" || !strings.Contains(s, "@"+me) {
		return s
	}
	var b strings.Builder
	rest := s
	for rest != "" {
		i := strings.Index(rest, "\x1b[")
		if i < 0 {
			b.WriteString(mentionStylePlain(rest, me))
			break
		}
		b.WriteString(mentionStylePlain(rest[:i], me))
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

// mentionStylePlain styles "@me" occurrences inside one ANSI-free run.
func mentionStylePlain(s, me string) string {
	needle := "@" + me
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, needle)
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := i + len(needle)
		if j < len(rest) && isMentionRunChar(rest[j]) {
			// A charset char follows the match: this "@me" is only the
			// prefix of a longer identifier ("@me2") — copy the whole
			// token through unstyled.
			k := j
			for k < len(rest) && isMentionRunChar(rest[k]) {
				k++
			}
			b.WriteString(rest[:k])
			rest = rest[k:]
			continue
		}
		b.WriteString(rest[:i])
		b.WriteString(mentionStyler.Render(needle))
		rest = rest[j:]
	}
	return b.String()
}

// ---- composer dropdown -------------------------------------------------------

// mentionFooterHints is the dim keymap legend under the "@" member list.
const mentionFooterHints = "↑↓ select · tab/enter complete · esc dismiss"

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
// fragment is being typed.
func (m *paletteState) syncMention(text string, inGeneral bool) {
	_, live := mentionQuery(text)
	m.open = inGeneral && text != "" && !strings.HasPrefix(text, "/") && live
}

// mentionCandidates resolves the dropdown state from the live composer:
// the fragment being completed plus the room members whose name matches it
// (case-insensitive prefix of the typed fragment, alphabetical; an empty
// fragment lists everyone) — self excluded. Source is userCandidates:
// eng.peers() live (refreshed by netRosterMsg), or the sidebar snapshot on
// bare screens. ok=false when no mention fragment is live.
func (c *chatScreen) mentionCandidates() (frag string, users []rosterMember, ok bool) {
	frag, ok = mentionQuery(c.input.Value())
	if !ok {
		return "", nil, false
	}
	q := strings.ToLower(frag)
	matched := make([]rosterMember, 0, len(c.users)+1)
	for _, u := range c.userCandidates() {
		if q == "" || strings.HasPrefix(strings.ToLower(u.Username), q) {
			matched = append(matched, u)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].Username < matched[j].Username
	})
	return frag, matched, true
}

// mentionView renders the "@" member dropdown — the palette mirror: the
// same box/chip/scroll-window geometry as the "/" drawer, one row per
// candidate with its role tag, the typed fragment highlighted. Returns ""
// when the dropdown is closed or nothing matches.
func (c chatScreen) mentionView(maxW int) string {
	frag, users, ok := c.mentionCandidates()
	if !ok || len(users) == 0 {
		return ""
	}
	c.mention.clampSel(len(users))
	off := c.mention.off
	end := min(off+paletteMaxVisible, len(users))

	inner := maxW - 2 // room for the box border
	query := strings.ToLower(frag)

	fit := func(s string) string {
		if lipgloss.Width(s) > inner {
			return lipgloss.NewStyle().MaxWidth(inner).Render(s)
		}
		return s
	}

	nameCol := 0 // dynamic name column: longest visible name + gap
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
		// Highlight the typed fragment inside the member name.
		if len(name) >= len(query) && len(query) > 0 &&
			strings.EqualFold(name[:len(query)], query) {
			name = tuiPaletteMatchStyle.Render(name[:len(query)]) + name[len(query):]
		}
		line := fit(padVisible(name, nameCol) + tuiPaletteDescStyle.Render(userRoleTag(u.Role)))
		line = padVisible(line, inner) // full-width rows: chip reaches both edges
		if idx == c.mention.sel {
			line = tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle))
		}
		rows = append(rows, line)
	}
	// Overflow indicator, same wording as the "/" drawer: the painted
	// height always matches the paletteRows() budget.
	if below := len(users) - end; below > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d more", below)), inner)))
	} else if above := off; above > 0 {
		rows = append(rows, fit(padVisible(
			tuiPaletteHintStyle.Render(fmt.Sprintf("… +%d above", above)), inner)))
	}
	rows = append(rows, fit(tuiPaletteHintStyle.Render(padVisible(mentionFooterHints, inner))))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(rows, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

// handleMentionKeys intercepts keys while the "@" member dropdown is open
// (mirrors handlePaletteKeys): Up/Down move the highlight, Tab/Enter
// complete "@name " into the composer, Esc dismisses. handled=true means the
// caller must skip normal editing. With no matching candidates the dropdown
// paints nothing and only Esc stays meaningful — every other key keeps
// normal editing, so Enter still sends and Tab still cycles focus.
func (c *chatScreen) handleMentionKeys(msg tea.KeyMsg) (handled bool, action func() tea.Cmd) {
	if !c.mention.visible() {
		return false, nil
	}
	_, users, ok := c.mentionCandidates()
	if !ok || len(users) == 0 {
		if msg.Type == tea.KeyEsc {
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
	case tea.KeyTab, tea.KeyEnter:
		c.mention.clampSel(len(users))
		if c.mention.sel < len(users) {
			c.completeMention(users[c.mention.sel].Username)
		}
		return true, nil
	case tea.KeyEsc:
		c.mention.close()
		return true, nil
	}
	return false, nil
}

// completeMention replaces the live "@fragment" with "@name " — the
// trailing space terminates the token, matching the receipt parser — and
// closes the dropdown.
func (c *chatScreen) completeMention(name string) {
	value := c.input.Value()
	if at := strings.LastIndex(value, "@"); at >= 0 {
		c.input.SetValue(value[:at] + "@" + name + " ")
	}
	c.mention.close()
}
