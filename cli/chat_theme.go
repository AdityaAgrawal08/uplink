package main

// chat_theme.go — the visual language of the Uplink chat shell.
//
// PURE PRESENTATION LAYER. Nothing here touches the network, the engine, or
// any backend state: every helper below only *reads* the already-loaded
// model (history, roster, media frames) and returns styled strings plus the
// few deterministic hit-test geometries that View() and handleMouse() share.
//
// The reference look: an edge-to-edge chat application. A tinted left rail
// holds the conversation list, the main column opens with a tinted header
// line, the transcript reads as grouped conversation, and the composer is
// the one persistently bordered surface in the whole shell.

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ---- adaptive density ---------------------------------------------------------
//
// The terminal owns the true font size; the app answers with DENSITY: every
// chrome choice below is a pure function of the live terminal size, so each
// resize visibly rebalances chrome vs content.

// compactTranscript hides the roomier transcript ornaments when the
// transcript is too narrow for them to coexist with the text.
func compactTranscript(termW int) bool { return termW > 0 && termW < 80 }

// compactItems collapses sidebar conversations to a single row when height
// is scarce.
func compactItems(termW, termH int) bool {
	if termH <= 0 {
		return false // unknown size: comfortable until measured
	}
	return termH < 28
}

// bubbleRatioFor scales message bubbles fluidly: near-full-width on narrow
// terminals, tighter columns when space abounds.
func bubbleRatioFor(availW int) float64 {
	switch {
	case availW < 60:
		return 0.9
	case availW < 100:
		return 0.7
	case availW < 150:
		return 0.62
	default:
		return 0.5
	}
}

// wheelStepFor scales wheel/drag steps to the pane height so scrolling feels
// identical on short and tall terminals (was a fixed 3 lines everywhere).
func wheelStepFor(paneH int) int {
	s := paneH / 5
	if s < 1 {
		s = 1
	}
	if s > 6 {
		s = 6
	}
	return s
}

// ---- palette ------------------------------------------------------------------
//
// EVERY colour is an AdaptiveColor with both Light and Dark set: the same
// design has to read on a white-background terminal and on a black one.
// Greys are ANSI-256 indices (they survive a 256-colour profile without
// being quantised into a muddy hue); accents are hex so they keep their
// character on truecolor terminals.

var (
	// surfaces
	colPanel  = lipgloss.AdaptiveColor{Light: "#e9edf5", Dark: "#0f1524"}
	colPanel2 = lipgloss.AdaptiveColor{Light: "#e0e6f1", Dark: "#121a2b"}
	colEdge   = lipgloss.AdaptiveColor{Light: "250", Dark: "236"}

	// text
	colText  = lipgloss.AdaptiveColor{Light: "#101728", Dark: "#e7ecf7"}
	colDim   = lipgloss.AdaptiveColor{Light: "242", Dark: "246"}
	colFaint = lipgloss.AdaptiveColor{Light: "246", Dark: "240"}

	// accents
	colAccent = lipgloss.AdaptiveColor{Light: "#0b62c9", Dark: "#4cc9f0"}
	colOwn    = lipgloss.AdaptiveColor{Light: "#0b62c9", Dark: "#1d4ed8"}
	colOwnFg  = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#f2f7ff"}
	colSel    = lipgloss.AdaptiveColor{Light: "#d7e6ff", Dark: "#152741"}
	colHover  = lipgloss.AdaptiveColor{Light: "#9d174d", Dark: "#f0abfc"}
	colGreen  = lipgloss.AdaptiveColor{Light: "#0f7a52", Dark: "#34d399"}
	colAmber  = lipgloss.AdaptiveColor{Light: "#8a5a00", Dark: "#fbbf24"}
	colRed    = lipgloss.AdaptiveColor{Light: "#b4232a", Dark: "#f87171"}

	// unread badge (ANSI indices: white on blue at every colour profile)
	colBadgeBg = lipgloss.AdaptiveColor{Light: "27", Dark: "27"}
	colBadgeFg = lipgloss.AdaptiveColor{Light: "15", Dark: "15"}

	// message blocks
	colOtherBg = lipgloss.AdaptiveColor{Light: "#eef2f9", Dark: "#111a2b"}
	colOwnBg   = lipgloss.AdaptiveColor{Light: "#dcebff", Dark: "#122444"}

	// markdown
	colCodeFg = lipgloss.AdaptiveColor{Light: "#9a3412", Dark: "#f0abfc"}
	colCodeBg = lipgloss.AdaptiveColor{Light: "#e8ecf4", Dark: "#161d2e"}
	colLangFg = lipgloss.AdaptiveColor{Light: "#64748b", Dark: "#7b8aa5"}
	colLinkFg = lipgloss.AdaptiveColor{Light: "#0b62c9", Dark: "#4cc9f0"}
)

// ---- shared styles -------------------------------------------------------------

var (
	// top bar
	thTopbarLogoStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#0b62c9", Dark: "#4cc9f0"})
	thTopbarDimStyle  = lipgloss.NewStyle().Foreground(colDim)
	thTopbarTimeStyle = lipgloss.NewStyle().Foreground(colText)
	thSignalStyle     = lipgloss.NewStyle().Foreground(colAccent)
	thLockStyle       = lipgloss.NewStyle().Foreground(colGreen)

	// sidebar
	thSearchStyle        = lipgloss.NewStyle().Foreground(colFaint)
	thSidebarStyle       = lipgloss.NewStyle().Background(colPanel)
	thSidebarHeaderStyle = lipgloss.NewStyle().Background(colPanel2)

	thChatNameStyle = lipgloss.NewStyle().Foreground(colText)
	thChatActive    = lipgloss.NewStyle().Bold(true).Foreground(colText)
	thChatTimeStyle = lipgloss.NewStyle().Foreground(colFaint)
	thPreviewStyle  = lipgloss.NewStyle().Foreground(colDim)
	thPreviewUnread = lipgloss.NewStyle().Foreground(colText)

	thSelBarStyle   = lipgloss.NewStyle().Foreground(colAccent).Background(colSel)
	thHoverRowStyle = lipgloss.NewStyle().Foreground(colHover)
	thPresenceStyle = lipgloss.NewStyle().Foreground(colGreen)
	thPresenceOff   = lipgloss.NewStyle().Foreground(colFaint)

	// chat header
	thRoomNameStyle = lipgloss.NewStyle().Bold(true).Foreground(colText)
	thRoomSubStyle  = lipgloss.NewStyle().Foreground(colDim)
	thRoomHeadStyle = lipgloss.NewStyle().Background(colPanel)

	// transcript
	thMsgTimeStyle = lipgloss.NewStyle().Foreground(colFaint)

	thOtherBubbleStyle = lipgloss.NewStyle().
				Background(colOtherBg).
				Foreground(colText).
				Padding(0, 1)

	thOwnBubbleStyle = lipgloss.NewStyle().
				Background(colOwnBg).
				Foreground(colText).
				Padding(0, 1)

	thSystemLineStyle = lipgloss.NewStyle().Foreground(colFaint).Italic(true).Faint(true)

	// composer + hints
	thComposerBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colEdge).
				Foreground(colText)

	thComposerFocusStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colAccent).
				Foreground(colText)

	thFocusBar  = lipgloss.NewStyle().Foreground(colAccent)
	thClipStyle = lipgloss.NewStyle().Foreground(colDim)
	thHintStyle = lipgloss.NewStyle().Foreground(colFaint)

	// transient notices
	thStatusErrStyle  = lipgloss.NewStyle().Foreground(colRed)
	thStatusWarnStyle = lipgloss.NewStyle().Foreground(colAmber)
	thStatusInfoStyle = lipgloss.NewStyle().Foreground(colDim)

	// file cards
	thFileCardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colEdge).
			Background(colPanel).
			Padding(0, 1)

	thFileNameStyle = lipgloss.NewStyle().Bold(true).Foreground(colText)
	thFileMetaStyle = lipgloss.NewStyle().Foreground(colDim)
	thFileDlStyle   = lipgloss.NewStyle().Foreground(colAccent)
)

// avatarPalettes assign each user a stable, distinct identity hue: the
// vivid set reads on dark terminals, the deeper set keeps the same hue
// family while staying legible on white. The hash picks the slot, so a
// user's colour never moves between themes — only its lightness adapts.
var (
	avatarPaletteDark = []string{
		"#14b8a6", "#22c55e", "#f472b6", "#f59e0b", "#8b5cf6",
		"#0ea5e9", "#ef4444", "#84cc16", "#e879f9", "#fb7185",
	}
	avatarPaletteLight = []string{
		"#0f766e", "#15803d", "#be185d", "#a16207", "#6d28d9",
		"#0369a1", "#b91c1c", "#4d7c0f", "#a21caf", "#be123c",
	}
)

// avatarColorFor hashes a username onto the avatar palette (stable per name)
// and returns the adaptive pair.
func avatarColorFor(name string) lipgloss.TerminalColor {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(name)))
	i := int(h.Sum32() % uint32(len(avatarPaletteDark)))
	return lipgloss.AdaptiveColor{Light: avatarPaletteLight[i], Dark: avatarPaletteDark[i]}
}

// avatarGlyphFg contrasts with the avatar hue on either theme: dark ink on
// the light palette's deep hues, near-white on the dark palette's neons.
var avatarGlyphFg = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#0b1220"}

// avatarCell renders the 1-row coloured initial chip, e.g. " A ".
func avatarCell(name string) string {
	initial := "•"
	if r := []rune(strings.ToUpper(name)); len(r) > 0 {
		initial = string(r[0])
	}
	return lipgloss.NewStyle().
		Background(avatarColorFor(name)).
		Foreground(avatarGlyphFg).
		Bold(true).
		Padding(0, 1).
		Render(initial)
}

// roomAvatarCell is the group glyph for the General room.
func roomAvatarCell() string {
	return lipgloss.NewStyle().
		Background(colOwn).
		Foreground(colOwnFg).
		Bold(true).
		Padding(0, 1).
		Render("◉")
}

// ---- chat list model ---------------------------------------------------------

// chatItem is one sidebar row set: the General room or a DM thread.
type chatItem struct {
	peer    string // "" = General room, else the peer username
	name    string
	preview string
	timeStr string
	unread  int
	active  bool
	isRoom  bool
	live    bool // peer online (room always true)
	inCall  bool // live voice on this conversation
}

// itemRowsPerChat is the row height of one chat item: the identity line
// (presence + name + time + badge) over the preview line. Selection is an
// accent bar + tint inside the same rows — no extra chrome.
const itemRowsPerChat = 2

// chatItems builds the sidebar order: General room first, then DM peers in
// display (recency) order, narrowed by the rail's inline filter. Read-only:
// no model mutation.
func (c *chatScreen) chatItems() []chatItem {
	online := orderedUsers(c.users, c.me, c.lastDMAt)
	live := map[string]bool{}
	for _, u := range c.users {
		live[u] = true
	}
	items := make([]chatItem, 0, len(online)+1)
	preview, ts := c.convPreview(generalConv)
	inCall, inCallElapsed := false, ""
	if c.callParties() > 0 {
		preview = "● Live now"
		ts = time.Now().Local().Format("15:04")
		inCall = true
	}
	items = append(items, chatItem{
		peer: "", name: "General",
		preview: preview, timeStr: ts,
		unread: c.roomUnread, active: c.targetUser == "",
		isRoom: true, live: true, inCall: inCall,
	})
	for _, u := range online {
		if u == c.me {
			continue
		}
		conv := conversationKey(c.me, u)
		pv, tm := c.convPreview(conv)
		peerCall := c.peerInCall(u)
		if peerCall {
			if inCallElapsed == "" {
				inCallElapsed = c.callElapsed()
			}
			if inCallElapsed == "--:--:--" {
				pv = "In call now"
			} else {
				pv = "Voice call • " + inCallElapsed[len("00:"):]
			}
			tm = time.Now().Local().Format("15:04")
		}
		items = append(items, chatItem{
			peer: u, name: u,
			preview: pv, timeStr: tm,
			unread: c.unread[u], active: c.targetUser == u,
			isRoom: false, live: live[u], inCall: peerCall,
		})
	}
	// Inline filter: every list consumer (paint, hit-test, keyboard cursor,
	// scroll clamp) goes through chatItems, so a filtered list can never
	// disagree with itself.
	if c.sideFilter != "" {
		q := strings.ToLower(c.sideFilter)
		kept := items[:0]
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.name), q) ||
				strings.Contains(strings.ToLower(it.preview), q) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	return items
}

// onlineCount is the member count the chat header advertises.
func (c *chatScreen) onlineCount() int { return len(c.users) }

// convPreview returns the newest text + time for a conversation bucket.
// Own messages are prefixed with "You: ". Empty when nothing arrived yet.
func (c *chatScreen) convPreview(conv string) (string, string) {
	bestText, bestTime := "", ""
	var bestAt time.Time
	consider := func(text, tsRaw string) {
		var at time.Time
		ts := "--:--"
		if p, err := time.Parse(time.RFC3339, tsRaw); err == nil {
			at = p
			ts = p.Local().Format("15:04")
		} else if p, err := time.Parse("15:04", tsRaw); err == nil {
			now := time.Now()
			at = time.Date(now.Year(), now.Month(), now.Day(), p.Hour(), p.Minute(), 0, 0, time.Local)
			ts = p.Format("15:04")
		} else if tsRaw != "" {
			ts = tsRaw
		}
		if bestText == "" || !at.IsZero() && at.After(bestAt) {
			bestText, bestTime, bestAt = text, ts, at
		}
	}
	for _, m := range c.history {
		cid := m.ConvID
		if cid == "" {
			cid = generalConv
		}
		if cid != conv || m.Kind == "system" {
			continue
		}
		text := m.Text
		if m.Username == c.me {
			text = "You: " + text
		} else if !strings.HasPrefix(conv, "general") && conv != generalConv {
			text = m.Username + ": " + text
		}
		consider(text, m.CreatedAt)
	}
	for _, ll := range c.localLines {
		if ll.conv != conv || ll.kind != lineFileCard || ll.fileData == nil {
			continue
		}
		who := ll.fileData.username
		if who == c.me {
			who = "You"
		}
		consider(who+" shared a file", ll.fileData.createdAt)
	}
	if bestText == "" {
		return "No messages yet", ""
	}
	// Previews paint raw (no markdown pass): strip control bytes here so a
	// peer message can never inject terminal escapes via the sidebar.
	return sanitizeDisplay(bestText), bestTime
}

// itemIndexFor returns the chatItems index for a peer (0 = General).
func (c *chatScreen) itemIndexFor(peer string) int {
	for i, it := range c.chatItems() {
		if it.peer == peer {
			return i
		}
	}
	return -1
}

// ---- top bar -----------------------------------------------------------------

// topBarView paints the app banner: logo left, signal/lock/clock right.
// Pure function of width + wall clock. Density collapses in steps: date on
// wide, essentials in the middle, logo + time only when cramped. The
// encryption promise lives HERE and nowhere else — one persistent signal.
func topBarView(w int) string {
	now := time.Now()
	enc := thLockStyle.Render("🔒 End-to-End Encrypted")
	var left, right string
	switch {
	case w <= 0 || w >= 110:
		left = thTopbarLogoStyle.Render("◆ UPLINK")
		right = thSignalStyle.Render("▂▄▆") + "  " + enc + "  " +
			thTopbarDimStyle.Render(now.Format("Mon, 02 Jan 2006")) + "  " +
			thTopbarTimeStyle.Render(now.Format("15:04"))
	case w >= 85:
		left = thTopbarLogoStyle.Render("◆ UPLINK")
		right = thSignalStyle.Render("▂▄▆") + "  " + enc + "  " +
			thTopbarTimeStyle.Render(now.Format("15:04"))
	case w >= 55:
		left = thTopbarLogoStyle.Render("◆ UPLINK")
		right = thSignalStyle.Render("▂▄▆") + "  " +
			thLockStyle.Render("🔒") + "  " +
			thTopbarTimeStyle.Render(now.Format("15:04"))
	default:
		left = thTopbarLogoStyle.Render("◆ UPLINK")
		right = thTopbarTimeStyle.Render(now.Format("15:04"))
	}
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if w <= 0 {
		return left + "  " + right
	}
	if lw+rw+2 > w {
		return truncateByWidth(left+"  "+right, w)
	}
	return left + strings.Repeat(" ", w-lw-rw) + right
}

// ---- chat header ---------------------------------------------------------------

// roomHeaderView paints the 2-row conversation heading for the main column:
// avatar + name over a single context line (kind, key, membership). outerW
// is the main column width so the heading aligns with everything below it.
func (c *chatScreen) roomHeaderView(outerW int) string {
	var av, name, sub string
	if c.targetUser == "" {
		av = roomAvatarCell()
		name = "General"
		sub = "Public Room"
		if c.key != "" {
			sub += "  ·  key " + c.key
		}
		if n := c.onlineCount(); n > 0 {
			sub += fmt.Sprintf("  ·  %d online", n)
		}
	} else {
		av = avatarCell(c.targetUser)
		name = c.targetUser
		sub = fmt.Sprintf("Private with %s  ·  ESC = general", c.targetUser)
		if c.roomUnread > 0 {
			sub += fmt.Sprintf("  ·  %d new in room", c.roomUnread)
		}
	}
	// Width() would wrap overlong rows and break the exact-row contract, so
	// content is hard-truncated to fit instead; the tint pads the row.
	name = truncateStringPlain(name, maxInt(outerW-lipgloss.Width(av)-4, 1))
	line1 := av + "  " + thRoomNameStyle.Render(name)
	line2 := "     " + thRoomSubStyle.Render(truncateStringPlain(sub, maxInt(outerW-6, 0)))
	return tintFit(thRoomHeadStyle, line1, outerW) + "\n" + tintFit(thRoomHeadStyle, line2, outerW)
}

// roomHeaderCompact paints the 1-row conversation heading for short/narrow
// terminals: avatar + name + context on a single line.
func (c *chatScreen) roomHeaderCompact(outerW int) string {
	var av, name, sub string
	if c.targetUser == "" {
		av = roomAvatarCell()
		name = "General"
		sub = "Public"
		if c.key != "" {
			sub += " · " + c.key
		}
		if n := c.onlineCount(); n > 0 {
			sub += fmt.Sprintf(" · %d online", n)
		}
	} else {
		av = avatarCell(c.targetUser)
		name = c.targetUser
		sub = "Private · ESC"
		if c.roomUnread > 0 {
			sub += fmt.Sprintf(" · %d new", c.roomUnread)
		}
	}
	line := av + "  " + thRoomNameStyle.Render(name) + "  " + thRoomSubStyle.Render(sub)
	return tintFit(thRoomHeadStyle, line, outerW)
}

// ---- helpers -------------------------------------------------------------------

// ---- helpers -------------------------------------------------------------------

// truncateByWidth hard-cuts a string to w CELLS (width-aware: wide runes
// count double). Width-truncation keeps single-row views exact where
// rune-truncation would overflow on emoji/double-width glyphs.
func truncateByWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "")
}

// fitRow pins a (possibly styled) string to exactly w CELLS: truncate with
// the ANSI/width-aware cutter (never rune counting — a single emoji is two
// cells and would otherwise push the row a column past the terminal), then
// pad.
func fitRow(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return padVisible(truncateByWidth(s, w), w)
}

// styleSeq returns the SGR opener a style emits, or "" when it emits none
// (an unstyled Render returns its input untouched, so there is nothing to
// re-assert).
func styleSeq(st lipgloss.Style) string {
	probe := st.Render("x")
	i := strings.Index(probe, "x")
	if i <= 0 || probe[0] != 0x1b {
		return ""
	}
	if j := strings.IndexByte(probe[:i], 'm'); j > 0 {
		return probe[:j+1]
	}
	return ""
}

// retint re-asserts an OUTER style after every inner reset inside CONTENT.
// lipgloss wraps each styled fragment in its own terminator, so a row tint
// or bubble background would otherwise fall off after the first bold word,
// inline code chip, avatar or badge. The rewrite does a full reset first
// (so no stale foreground leaks) and then re-asserts the outer style —
// each fragment keeps its own attributes exactly where they were written.
func retint(s string, outer lipgloss.Style) string {
	seq := styleSeq(outer)
	if seq == "" {
		return s
	}
	return strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+seq)
}

// tintFit pins a styled row to exactly w cells AND keeps an outer tint
// applied across the whole row, padding included. Every tinted row in the
// shell goes through this one helper.
func tintFit(outer lipgloss.Style, s string, w int) string {
	return outer.Render(retint(fitRow(s, w), outer))
}

// composerTopRows counts the terminal rows above the composer box, mirroring
// View()'s assembly order (header, body, drawer) so hit-testing stays
// pixel-truthful even with the drawer open.
func composerTopRows(l layout) int {
	top := 0
	if l.showHeader {
		top += headerHeight
	}
	bodyRows := 0
	if l.vpHeight > 0 {
		bodyRows = l.vpHeight
	}
	bodyRows += l.headRows
	if bodyRows > 0 {
		top += bodyRows
	}
	if l.paletteRows > 0 {
		top += l.paletteRows // drawer panel + its spacer row
	}
	if l.frameOn {
		top++ // frame top edge
	}
	return top
}

// composerGeoms derives the composer hit-test geometry deterministically from
// the layout so View() and handleMouse() agree. Returns the clip glyph
// column and its single-row y-range [y0,y1) — the clip sits on the composer
// field row (middle of the box). clipX < 0 when no clip is painted (bare
// prompt mode, or too narrow to fit it).
func composerGeoms(l layout) (clipX, y0, y1 int) {
	clipX = -1
	if l.composerRows <= 0 {
		return
	}
	frameOff := 0
	if l.frameOn {
		frameOff = 1
	}
	txX0 := frameOff + composerIndent(l)
	transcriptOuter := l.vpWidth + 2
	inputOuter := transcriptOuter // message box spans the full column width
	boxH := l.composerRows + 2
	y0 = composerTopRows(l) + boxH/2 // the field row (middle of the box)
	y1 = y0 + 1
	if inputOuter >= 26 {
		clipX = txX0 + inputOuter - 2
	}
	return
}

// composerIndent is the left indent of the composer row (blank above the
// sidebar column), shared by View() and geometry.
func composerIndent(l layout) int {
	if l.sidebarOn {
		return l.sidebarWidth + 1
	}
	return 0
}

// renderSystemLine paints a transcript system event the quiet way: one
// centred, faint, italic row set apart from chat — never mistakable for a
// message, never given a box of its own.
func renderSystemLine(text, tsPlain string, availWidth int) string {
	if availWidth <= 0 {
		availWidth = 60
	}
	body := sanitizeDisplay(text)
	label := "· " + body
	if tsPlain != "" && tsPlain != "--:--" {
		label = "· " + tsPlain + " · " + body
	}
	return thSystemLineStyle.
		Width(availWidth).
		Align(lipgloss.Center).
		Render(truncateByWidth(label, availWidth))
}

// keyHintsView paints the 1-row footer under the composer. Every hint names
// a binding that actually exists, and the SET changes with focus, so the
// footer always teaches the keys that work right now.
func keyHintsView(outerW int, f focusPane) string {
	var wide, narrow string
	switch f {
	case focusSidebar:
		wide = "↑↓ pick  ·  type filters  ·  enter opens  ·  esc clears  ·  tab next"
		narrow = "↑↓  ·  type  ·  enter  ·  esc"
	case focusTranscript:
		wide = "↑↓ scroll  ·  pgup/pgdn page  ·  home/end jump  ·  tab next"
		narrow = "↑↓  ·  pgup/pgdn  ·  tab"
	default:
		wide = "Ctrl+k commands  •  Ctrl+l clear  •  ↑↓ navigate  •  Enter send"
		narrow = "Ctrl+k  •  Ctrl+l  •  ↑↓  •  Enter"
	}
	hints := narrow
	if outerW >= lipgloss.Width(wide)+2 {
		hints = wide
	}
	return thHintStyle.Render(truncateByWidth(hints, maxInt(outerW, 0)))
}
