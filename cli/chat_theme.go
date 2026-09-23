package main

// chat_theme.go — "Uplink" visual theme for the chat TUI.
//
// PURE PRESENTATION LAYER. Nothing here touches the network, the engine, or
// any backend state: every helper below only *reads* the already-loaded
// model (history, roster, media frames) and returns styled strings plus a few
// deterministic hit-test geometries that View() and handleMouse() share.
//
// The reference look: deep-navy app, left chat list with search + avatars,
// center room header + dark message bubbles (blue for own), composer with
// clip button, join/leave-free transcript.

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
// resize visibly rebalances chrome vs content. Thresholds are stepped, but
// the underlying widths (sidebar, bubbles, tiles) scale fluidly between
// them — nothing renders identically across sizes except by coincidence.

// compactTranscript hides sender avatars when the transcript is too narrow
// for chips + bubbles to coexist.
func compactTranscript(termW int) bool { return termW > 0 && termW < 80 }

// compactItems collapses sidebar chats to one row when height is scarce.
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

// ---- palette --------------------------------------------------------------

const (
	thBg        = "#070b14" // app background (frame fill)
	thPanel     = "#0b1220" // sidebar / cards
	thPanelEdge = "#1e293b" // borders
	thText      = "#e5eaf3" // primary text
	thDim       = "#8b98b3" // secondary text
	thFaint     = "#475569" // timestamps, hints
	thAccent    = "#22d3ee" // cyan highlights
	thBlue      = "#2563eb" // own bubbles, Send button, unread dot
	thOwnBg     = "#1d4ed8" // own message bubble
	thOtherBg   = "#131b2e" // others' message bubble
	thSelBg     = "#16233d" // selected chat row
	thGreen     = "#22c55e" // live dot, lock
	thAmber     = "#fbbf24" // reactions / warnings
)

var (
	thTopbarLogoStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(thAccent))
	thTopbarDimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))
	thTopbarTimeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thText))
	thSignalStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(thAccent))
	thLockStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color(thGreen))

	thSearchStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thFaint))
	thSearchBox   = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(thPanelEdge)).
			Foreground(lipgloss.Color(thFaint))

	thChatNameStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thText))
	thChatTimeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thFaint))
	thPreviewStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))
	thUnreadNewStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thGreen))
	thUnreadDotStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thBlue))

	thSelRowStyle   = lipgloss.NewStyle().Background(lipgloss.Color(thSelBg))
	thSelBarStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(thAccent)).Background(lipgloss.Color(thSelBg))
	thHoverRowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#f0abfc"))

	thRoomNameStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(thText))
	thRoomSubStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))

	thMsgNameStyle = lipgloss.NewStyle()
	thMsgTimeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thFaint))

	thOtherBubbleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color(thOtherBg)).
				Foreground(lipgloss.Color(thText)).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color(thPanelEdge)).
				Padding(0, 1)

	thOwnBubbleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color(thOwnBg)).
				Foreground(lipgloss.Color("#ffffff")).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color(thOwnBg)).
				Padding(0, 1)

	thBubbleTimeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#c3cede"))

	thSendBtnStyle = lipgloss.NewStyle().
			Background(lipgloss.Color(thBlue)).
			Foreground(lipgloss.Color("#ffffff")).
			Bold(true)

	thTabActiveStyle = lipgloss.NewStyle().
				Background(lipgloss.Color(thBlue)).
				Foreground(lipgloss.Color("#ffffff")).
				Bold(true)
	thTabInactiveStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#16233d")).
				Foreground(lipgloss.Color(thDim))
	thTabStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(thDim))

	thClipStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))

	thFileCardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(thPanelEdge)).
			Background(lipgloss.Color(thPanel)).
			Padding(0, 1)

	thFileNameStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(thText))
	thFileMetaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))
	thFileDlStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(thAccent))

	thCamLiveStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(thGreen))
	thCamMetaStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(thFaint))
	thSystemBarStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thGreen))
	thSystemNameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thGreen))
)

// avatarPalette assigns each user a stable, distinct avatar colour.
var avatarPalette = []string{
	"#14b8a6", "#22c55e", "#f472b6", "#f59e0b", "#8b5cf6",
	"#0ea5e9", "#ef4444", "#84cc16", "#e879f9", "#fb7185",
}

// avatarColorFor hashes a username onto the avatar palette (stable per name).
func avatarColorFor(name string) lipgloss.Color {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(name)))
	return lipgloss.Color(avatarPalette[int(h.Sum32())%len(avatarPalette)])
}

// avatarCell renders the 1-row coloured initial chip, e.g. " A ".
func avatarCell(name string) string {
	initial := "•"
	if r := []rune(strings.ToUpper(name)); len(r) > 0 {
		initial = string(r[0])
	}
	return lipgloss.NewStyle().
		Background(avatarColorFor(name)).
		Foreground(lipgloss.Color("#0b1220")).
		Bold(true).
		Padding(0, 1).
		Render(initial)
}

// roomAvatarCell is the group glyph for the General room.
func roomAvatarCell() string {
	return lipgloss.NewStyle().
		Background(lipgloss.Color(thBlue)).
		Foreground(lipgloss.Color("#ffffff")).
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
	inCall  bool // live video/audio on this conversation
}

// itemRowsPerChat is the comfortable row height of one chat item: name +
// preview. Selection is a side bar + tint inside the same rows.
const itemRowsPerChat = 2

// chatItems builds the sidebar order: General room first, then DM peers in
// display (recency) order. Read-only: no model mutation.
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
	return items
}

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
// wide, essentials in the middle, logo + time only when cramped.
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

// ---- room header ---------------------------------------------------------------

// roomHeaderView paints the 2-row conversation heading for the center column:
// avatar + name, then membership/type context. outerW is the transcript box
// outer width so the heading aligns with the box below it.
func (c *chatScreen) roomHeaderView(outerW int) string {
	var av, name, sub string
	if c.targetUser == "" {
		av = roomAvatarCell()
		name = "General"
		sub = "Public Room"
		if c.key != "" {
			sub += "  ·  key " + c.key
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
	// content is hard-truncated to fit instead; the style only pads.
	name = truncateStringPlain(name, maxInt(outerW-lipgloss.Width(av)-4, 1))
	line1 := av + "  " + thRoomNameStyle.Render(name)
	line2 := "     " + thRoomSubStyle.Render(truncateStringPlain(sub, maxInt(outerW-6, 0)))
	st := lipgloss.NewStyle().Width(maxInt(outerW, 0))
	return st.Render(line1) + "\n" + st.Render(line2)
}

// roomTabsLine appends the right-aligned Chat/Files tabs + menu to the room
// header's name row. Geometry mirrors roomTabsGeoms (hit-testing): the Chat
// chip is 6 cells, Files 7, ⋮ 3, single-space separated.
func roomTabsLine(outerW int, line1 string) string {
	chat := thTabActiveStyle.Render(" Chat ")
	files := thTabInactiveStyle.Render(" Files ")
	more := thTabStyle.Render(" ⋮ ")
	tabs := chat + " " + files + " " + more
	gap := outerW - lipgloss.Width(stripForWidth(line1)) - lipgloss.Width(tabs)
	if gap < 1 || outerW < 40 {
		return line1 // cramped: no tabs painted, none hit-testable either
	}
	return line1 + strings.Repeat(" ", gap) + tabs
}

// roomHeaderCompact paints the 1-row room heading for short/narrow
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
	} else {
		av = avatarCell(c.targetUser)
		name = c.targetUser
		sub = "Private · ESC"
		if c.roomUnread > 0 {
			sub += fmt.Sprintf(" · %d new", c.roomUnread)
		}
	}
	line := av + "  " + thRoomNameStyle.Render(name) + "  " + thRoomSubStyle.Render(sub)
	st := lipgloss.NewStyle().Width(maxInt(outerW, 0))
	return st.Render(truncateByWidth(line, maxInt(outerW, 0)))
}

// ---- composer -------------------------------------------------------------------

// truncateByWidth hard-cuts a string to w CELLS (width-aware: wide runes
// count double). Width-truncation keeps single-row views exact where
// rune-truncation would overflow on emoji/double-width glyphs.
func truncateByWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "")
}

// composerTopRows counts the terminal rows above the composer box, mirroring
// View()'s assembly order (header, body, strip, hints, drawer) so
// hit-testing stays pixel-truthful even with the drawer open.
func composerTopRows(l layout) int {
	top := 0
	if l.showHeader {
		top += headerHeight
	}
	bodyRows := 0
	if l.vpHeight > 0 {
		bodyRows = l.vpHeight
		if l.boxedTranscript {
			bodyRows += transcriptBorder
		}
	}
	bodyRows += l.headRows
	if bodyRows > 0 {
		top += bodyRows
	}
	top += l.hintRows
	if l.paletteRows > 0 {
		top += l.paletteRows + 1 // drawer panel + its spacer row
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
func composerGeoms(l layout, termW, termH int) (clipX, y0, y1 int) {
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
	_ = termW
	_ = termH
	return
}

// roomTabsGeoms maps the Chat/Files/⋮ tab hit rects in terminal coords.
// Mirrors roomTabsLine exactly: right-aligned tabs, none when cramped.
func roomTabsGeoms(c *chatScreen, l layout) (chat, files, more tabRect, ok bool) {
	if l.headRows < 2 || c.width == 0 || c.height == 0 {
		return tabRect{}, tabRect{}, tabRect{}, false
	}
	outerW := l.vpWidth + 2
	if outerW < 40 {
		return tabRect{}, tabRect{}, tabRect{}, false
	}
	frameOff := 0
	if l.frameOn {
		frameOff = 1
	}
	headOff := 0
	if l.showHeader {
		headOff = headerHeight
	}
	y := frameOff + headOff // first room-header row
	tx0 := transcriptX0(l)
	x1 := tx0 + outerW
	more = tabRect{x0: x1 - 3, x1: x1, y: y}
	files = tabRect{x0: x1 - 3 - 1 - 7, x1: x1 - 3 - 1, y: y}
	chat = tabRect{x0: x1 - 3 - 1 - 7 - 1 - 6, x1: x1 - 3 - 1 - 7 - 1, y: y}
	return chat, files, more, true
}

// tabRect is one clickable tab (x1 exclusive, single row y).
type tabRect struct {
	x0, x1, y int
}

// hit reports whether a terminal point lands in the tab.
func (t tabRect) hit(x, y int) bool {
	return y == t.y && x >= t.x0 && x < t.x1
}

// composerIndent is the left indent of the composer row (blank above the
// sidebar column), shared by View() and geometry.
func composerIndent(l layout) int {
	if l.sidebarOn {
		return l.sidebarWidth + 1
	}
	return 0
}

// interleave splices sep between items for JoinHorizontal calls.
func interleave(items []string, sep string) []string {
	out := make([]string, 0, len(items)*2-1)
	for i, s := range items {
		if i > 0 {
			out = append(out, sep)
		}
		out = append(out, s)
	}
	return out
}

// renderSystemCard paints a transcript system line the reference way: green
// "System" sender, then a rounded card whose text rows carry a green left
// bar. Width hugs content (capped); every row is exactly cardW+2 cells.
func renderSystemCard(name, tsPlain, text string, availWidth int) string {
	sender := thSystemNameStyle.Render(name) + "  " + thMsgTimeStyle.Render(tsPlain)
	maxW := availWidth - 2
	if maxW < 10 {
		maxW = 10
	}
	if maxW > 52 {
		maxW = 52
	}
	// Wrap plain first (ANSI-safe), then dress each row with the bar.
	words := strings.Fields(text)
	var wrapped []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
		} else if len([]rune(cur))+1+len([]rune(w)) <= maxW-4 {
			cur += " " + w
		} else {
			wrapped = append(wrapped, cur)
			cur = w
		}
	}
	if cur != "" || len(wrapped) == 0 {
		wrapped = append(wrapped, cur)
	}
	bar := thSystemBarStyle.Render("▌")
	rows := make([]string, 0, len(wrapped))
	widest := 0
	for _, ln := range wrapped {
		if w := len([]rune(ln)); w > widest {
			widest = w
		}
	}
	for _, ln := range wrapped {
		rows = append(rows, bar+" "+tuiSystemStyle.Render(ln+strings.Repeat(" ", widest-len([]rune(ln)))))
	}
	cardW := widest + 4 // bar + pads
	if cardW > maxW {
		cardW = maxW
	}
	card := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(thPanelEdge)).
		Background(lipgloss.Color(thPanel)).
		Width(cardW).
		Render(strings.Join(rows, "\n"))
	return sender + "\n" + card
}

// keyHintsView paints the 1-row composer footer. Every hint names a binding
// that actually exists: Ctrl+K drawer, Ctrl+L clear, ↑↓ scroll, Enter send.
// Narrow transcripts get the abbreviated variant (never mid-word).
func keyHintsView(outerW int) string {
	hints := "Ctrl+k commands  •  Ctrl+l clear  •  ↑↓ navigate  •  Enter send"
	if outerW < 58 {
		hints = "Ctrl+k  •  Ctrl+l  •  ↑↓  •  Enter"
	}
	return thCamMetaStyle.Render(truncateByWidth(hints, maxInt(outerW, 0)))
}
