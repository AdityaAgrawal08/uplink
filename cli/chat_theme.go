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
// clip + Send button, bottom "Live Cameras" strip.

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

// tileMinFor keeps tiles readable on wide screens but lets them shrink (with
// horizontal scroll) instead of vanishing on narrow ones.
func tileMinFor(outerW int) int {
	if outerW < 90 {
		return 16
	}
	return 22
}

// tilePeerTarget grows the camera strip with width: total tiles including
// self, so more peers stay visible on wide terminals.
func tilePeerTarget(termW int) int {
	switch {
	case termW <= 0:
		return 3 // unknown size: legacy default until measured
	case termW < 70:
		return 2
	case termW < 120:
		return 3
	case termW < 170:
		return 4
	default:
		return 6
	}
}

// tileFeedCap bounds total tiles (self + publishers + peers).
func tileFeedCap(termW int) int {
	if termW >= 150 {
		return 6
	}
	return 4
}

// sendBtnWidthFor shrinks the Send button to an icon on narrow transcripts.
func sendBtnWidthFor(transcriptOuter int) int {
	if transcriptOuter < 60 {
		return 6
	}
	return sendBtnWidth
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

	thClipStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))

	thFileCardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(thPanelEdge)).
			Background(lipgloss.Color(thPanel)).
			Padding(0, 1)

	thFileNameStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(thText))
	thFileMetaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thDim))
	thFileDlStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(thAccent))

	thCamTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thAccent))
	thCamNameStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thText))
	thCamLiveStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thGreen))
	thCamMetaStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(thFaint))
	thTileStyle     = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(thPanelEdge)).
			Background(lipgloss.Color("#05080f"))
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
}

// itemRowsPerChat is the fixed row height of one chat item (name + preview).
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
	items = append(items, chatItem{
		peer: "", name: "General",
		preview: preview, timeStr: ts,
		unread: c.roomUnread, active: c.targetUser == "",
		isRoom: true, live: true,
	})
	for _, u := range online {
		if u == c.me {
			continue
		}
		conv := conversationKey(c.me, u)
		pv, tm := c.convPreview(conv)
		items = append(items, chatItem{
			peer: u, name: u,
			preview: pv, timeStr: tm,
			unread: c.unread[u], active: c.targetUser == u,
			isRoom: false, live: live[u],
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
	return bestText, bestTime
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

// topBarView paints the app banner: logo + promises left, signal/lock/clock
// right. Pure function of width + wall clock. Density collapses in steps:
// full promises + date on wide, essentials in the middle, logo + time only
// when cramped.
func topBarView(w int) string {
	now := time.Now()
	var left, right string
	switch {
	case w <= 0 || w >= 110:
		left = thTopbarLogoStyle.Render("◆ UPLINK") + " " +
			thTopbarDimStyle.Render("secure  •  ephemeral  •  p2p")
		right = thSignalStyle.Render("▂▄▆") + "  " +
			thLockStyle.Render("🔒") + "  " +
			thTopbarDimStyle.Render(now.Format("Mon, 02 Jan 2006")) + "  " +
			thTopbarTimeStyle.Render(now.Format("15:04"))
	case w >= 75:
		left = thTopbarLogoStyle.Render("◆ UPLINK") + " " +
			thTopbarDimStyle.Render("secure  •  ephemeral  •  p2p")
		right = thSignalStyle.Render("▂▄▆") + "  " +
			thLockStyle.Render("🔒") + "  " +
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
		n := len(c.users)
		if n == 0 {
			n = 1
		}
		sub = fmt.Sprintf("%d members  |  Public Room", n)
		if c.key != "" {
			sub += "  ·  key " + c.key
		}
		if shortVersion(version) != "" {
			sub += "  ·  v" + normVersion(shortVersion(version))
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

// roomHeaderCompact paints the 1-row room heading for short/narrow
// terminals: avatar + name + context on a single line.
func (c *chatScreen) roomHeaderCompact(outerW int) string {
	var av, name, sub string
	if c.targetUser == "" {
		av = roomAvatarCell()
		name = "General"
		n := len(c.users)
		if n == 0 {
			n = 1
		}
		sub = fmt.Sprintf("%d · Public", n)
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

// sendBtnWidth is the fixed outer width of the Send button.
const sendBtnWidth = 10

// truncateByWidth hard-cuts a string to w CELLS (width-aware: wide runes
// count double). Width-truncation keeps single-row views exact where
// rune-truncation would overflow on emoji/double-width glyphs.
func truncateByWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "")
}

// sendButtonView renders the blue Send button at exactly sendW wide
// and boxH rows tall (matches the composer box height; the style is
// borderless so every row is button face). Narrow transcripts get an icon.
func sendButtonView(boxH, sendW int) string {
	inner := maxInt(boxH, 1)
	label := "Send"
	if sendW < sendBtnWidth {
		label = "➤"
	}
	rows := make([]string, 0, inner)
	mid := inner / 2
	for i := 0; i < inner; i++ {
		if i == mid {
			rows = append(rows, thSendBtnStyle.Width(sendW-2).Align(lipgloss.Center).Render(label))
		} else {
			rows = append(rows, thSendBtnStyle.Width(sendW-2).Render(" "))
		}
	}
	return thSendBtnStyle.Width(sendW).Render(strings.Join(rows, "\n"))
}

// composerTopRows counts the terminal rows above the composer box, mirroring
// View()'s assembly order (header, body, strip, drawer) so hit-testing stays
// pixel-truthful even with the drawer open.
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
	top += l.camRows
	if l.paletteRows > 0 {
		top += l.paletteRows + 1 // drawer panel + its spacer row
	}
	if l.frameOn {
		top++ // frame top edge
	}
	return top
}

// composerGeoms derives the composer hit-test geometry deterministically from
// the layout so View() and handleMouse() agree. Returns the Send button
// x-range [sendX0,sendX1), its y-range [y0,y1), and the clip glyph column.
// sendX0 < 0 when no button is painted (bare prompt mode).
func composerGeoms(l layout, termW, termH int) (sendX0, sendX1, y0, y1, clipX int) {
	sendX0, sendX1, clipX = -1, -1, -1
	if l.composerRows <= 0 {
		return
	}
	frameOff := 0
	if l.frameOn {
		frameOff = 1
	}
	txX0 := frameOff + composerIndent(l)
	transcriptOuter := l.vpWidth + 2
	boxH := l.composerRows + 2
	y0 = composerTopRows(l)
	y1 = y0 + boxH
	sendW := sendBtnWidthFor(transcriptOuter)
	inputOuter := transcriptOuter - sendW - 1
	sendX0 = txX0 + inputOuter + 1
	sendX1 = sendX0 + sendW
	if inputOuter >= 26 {
		clipX = txX0 + inputOuter - 2
	}
	_ = termW
	_ = termH
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

// ---- live cameras strip ------------------------------------------------------------

// camFeed is one tile in the Live Cameras strip.
type camFeed struct {
	name      string
	me        bool
	lines     []string // newest frame rows (may be empty → placeholder)
	live      bool     // a real frame is showing
	audioOnly bool     // ♪ contributor without video
}

// camStripRows is the fallback strip height (title + tile box) before the
// layout pass measures the terminal. Tiles stay tall enough for the ASCII
// blur to read: header + pic + footer + border. The live height is fully
// adaptive (see layoutFor): taller terminals earn taller pictures.
const camStripRows = 12

// camPicRows is the picture height inside every tile.
const camPicRows = camStripRows - 1 - 2 - 2

// camStripContentW is the full content width available to the strip.
func camStripContentW(termW int, frameOn bool) int {
	if frameOn {
		return termW - frameChrome
	}
	return termW
}

// camTileInner derives the tile interior width shared by the painter and
// the render-size handshake below (single source of truth: frames render
// at exactly the width their tile paints).
func camTileInner(contentW, nFeeds int) int {
	if nFeeds <= 0 {
		nFeeds = 1
	}
	tileW := (contentW - (nFeeds - 1)) / nFeeds
	return maxInt(tileW-2, 8)
}

// camFeeds builds the tiles: self first, then video publishers, audio-only
// contributors, then recent peers (camera-off placeholders) up to 4 tiles.
func (c *chatScreen) camFeeds() []camFeed {
	feeds := []camFeed{{name: c.me + " (You)", me: true, lines: c.selfLines, live: len(c.selfLines) > 0}}
	seen := map[string]bool{c.me: true}
	if c.call != nil {
		for _, p := range c.call.VideoPublishers() {
			if seen[p] {
				continue
			}
			seen[p] = true
			// The engine renders a single shared remote feed; it belongs to
			// the first remote publisher tile, others idle until focused.
			lines := []string{}
			if len(feeds) == 1 {
				lines = c.videoLines
			}
			feeds = append(feeds, camFeed{name: p, lines: lines, live: len(lines) > 0})
		}
		for _, p := range c.call.AudioPublishers() {
			if seen[p] {
				continue
			}
			seen[p] = true
			feeds = append(feeds, camFeed{name: p, audioOnly: true})
		}
	}
	// Fill peer tiles up to the width-derived target so the strip mirrors
	// the room: more peers stay visible on wide terminals.
	peerTarget := tilePeerTarget(c.width)
	for _, u := range orderedUsers(c.users, c.me, c.lastDMAt) {
		if len(feeds) >= peerTarget || len(c.users) == 0 {
			break
		}
		if u == c.me || seen[u] {
			continue
		}
		seen[u] = true
		feeds = append(feeds, camFeed{name: u})
	}
	// A remote feed with no listed publisher yet (transient roster) still
	// pins to the first remote tile — live pictures never hide behind
	// placeholders. Mirrors the old pane, which preferred videoLines.
	if len(c.videoLines) > 0 {
		claimed := false
		for _, f := range feeds {
			if !f.me && len(f.lines) > 0 {
				claimed = true
				break
			}
		}
		if !claimed {
			for i := range feeds {
				if !feeds[i].me && !feeds[i].audioOnly {
					feeds[i].lines = c.videoLines
					feeds[i].live = true
					break
				}
			}
		}
	}
	if cap := tileFeedCap(c.width); len(feeds) > cap {
		feeds = feeds[:cap]
	}
	return feeds
}

// tilePlaceholder paints a dark "camera off" tile with a big initial.
func tilePlaceholder(name string, w, h int) string {
	if w < 6 {
		w = 6
	}
	if h < 3 {
		h = 3
	}
	initial := "○"
	if r := []rune(strings.ToUpper(name)); len(r) > 0 {
		initial = string(r[0])
	}
	big := lipgloss.NewStyle().
		Background(avatarColorFor(name)).
		Foreground(lipgloss.Color("#0b1220")).
		Bold(true).
		Padding(0, 2).
		Render(initial)
	off := thCamMetaStyle.Render("camera off")
	rows := make([]string, 0, h)
	mid := h / 2
	for i := 0; i < h; i++ {
		var row string
		switch i {
		case mid - 1:
			row = big
		case mid + 1:
			row = off
		default:
			row = ""
		}
		rows = append(rows, lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Render(row))
	}
	return strings.Join(rows, "\n")
}

// renderCamTile paints one camera tile of exactly tileW (outer, incl border)
// and camStripRows-1 rows (outer, incl border).
//
// Live frames paint VERBATIM: the engine renders them at exactly this tile's
// geometry (see the SetVideoSize handshake in syncViewport) with full
// truecolor braille. They are truncated by VISIBLE width only (SGR intact)
// and never re-wrapped — stripping or re-wrapping is what turns the blur
// image into monochrome dots.
func renderCamTile(f camFeed, tileW int, pictureRows ...int) string {
	inner := maxInt(tileW-2, 8)
	head := thCamLiveStyle.Render("●") + " " + thCamNameStyle.Render(truncateByWidth(f.name, maxInt(inner-4, 1)))
	picH := camPicRows
	if len(pictureRows) > 0 {
		picH = pictureRows[0]
	}
	if picH < 2 {
		picH = 2
	}
	var pic string
	switch {
	case f.audioOnly:
		wave := thCamMetaStyle.Render("♪ audio only")
		rows := make([]string, 0, picH)
		for i := 0; i < picH; i++ {
			if i == picH/2 {
				rows = append(rows, lipgloss.NewStyle().Width(inner).Align(lipgloss.Center).Render(wave))
			} else {
				rows = append(rows, "")
			}
		}
		pic = strings.Join(rows, "\n")
	case len(f.lines) > 0:
		take := append([]string(nil), f.lines...)
		if len(take) > picH {
			take = take[len(take)-picH:]
		}
		fit := make([]string, 0, picH)
		for i := 0; i < picH-len(take); i++ {
			fit = append(fit, strings.Repeat(" ", inner))
		}
		for _, ln := range take {
			fit = append(fit, truncateVisible(ln, inner))
		}
		pic = strings.Join(fit, "\n")
	default:
		pic = tilePlaceholder(f.name, inner, picH)
	}
	foot := thCamMetaStyle.Render("720p • 30 FPS") +
		strings.Repeat(" ", maxInt(inner-lipgloss.Width("720p • 30 FPS")-lipgloss.Width("⛶"), 1)) +
		thCamMetaStyle.Render("⛶")
	body := lipgloss.NewStyle().Width(inner).Render(head + "\n" + pic + "\n" + foot)
	return thTileStyle.Width(inner).Render(body)
}

// camerasStripView paints the full-width bottom strip: title + tiles.
// outerW is the full content width (inside the app frame).
func (c *chatScreen) camerasStripView(outerW int) string {
	feeds := c.camFeeds()
	title := thCamTitleStyle.Render("🎥 Live Cameras ("+fmt.Sprint(len(feeds))+")") + " " +
		thCamLiveStyle.Render("●")
	gap := 1
	inner := max(tileMinFor(outerW), camTileInner(outerW, len(feeds)))
	tileW := inner + 2
	picRows := camPicRows
	if c.width > 0 && c.height > 0 {
		picRows = max(3, c.layoutFor().camRows-5)
	}
	// Horizontal join (NOT strings.Join: tiles are multi-line blocks).
	strs := make([]string, 0, len(feeds)*2-1)
	for i, f := range feeds {
		if i > 0 {
			strs = append(strs, strings.Repeat(" ", gap))
		}
		strs = append(strs, renderCamTile(f, tileW, picRows))
	}
	content := lipgloss.JoinHorizontal(lipgloss.Top, strs...)
	maxOffset := max(0, lipgloss.Width(content)-outerW)
	offset := min(c.cameraOffset, maxOffset)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = ansi.Cut(line, offset, offset+outerW)
	}
	if maxOffset > 0 {
		title += thCamMetaStyle.Render("  scroll to browse")
	}
	return truncateByWidth(title, outerW) + "\n" + strings.Join(lines, "\n")
}
