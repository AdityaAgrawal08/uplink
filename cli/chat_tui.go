package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- styling ---------------------------------------------------------------

var (
	tuiHeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	tuiSystemStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	tuiMeStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	tuiNameStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	tuiErrStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	tuiTimeStyle   = lipgloss.NewStyle().Faint(true)
	tuiBorderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))

	tuiSectionTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("250"))

	tuiDimStyle = lipgloss.NewStyle().Faint(true)

	tuiRosterBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("240"))

	tuiHoverStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("213")) // pink — hover only, one row max

	tuiUnreadStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("16")). // near-black digits
			Background(lipgloss.Color("2"))   // green disc: glyph interior reads as the fill

	tuiComposerStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62")).
				Padding(0, 1)

	tuiScrollbarStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240"))
	tuiScrollbarThumbStyle = lipgloss.NewStyle().
					Foreground(lipgloss.Color("245")).
					Background(lipgloss.Color("240"))

	// WhatsApp-like bubble styles: own = green right, other = dark grey left.
	tuiOwnBubbleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("22")).
				Foreground(lipgloss.Color("15")).
				Padding(0, 1).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("22"))

	tuiOtherBubbleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("236")).
				Foreground(lipgloss.Color("15")).
				Padding(0, 1).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("236"))

	tuiBubbleTimeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("250")).
				Faint(true)
)

// ---- layout constants --------------------------------------------------------

// progressBar renders a text-based progress bar:
//
//	Uploading... [████████████████████] 100% (21.3 KB / 21.3 KB)
func progressBar(label string, done, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("%s …", label)
	}
	pct := int(100 * done / total)
	const barWidth = 20
	filled := barWidth * pct / 100
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	return fmt.Sprintf("%s [%s] %d%% (%s / %s)",
		label, bar, pct, humanSize(done), humanSize(total))
}

const (
	rosterTotalWidth = 24 // DEFAULT sidebar outer width (density scales it)
	rosterWidthInner = rosterTotalWidth - 2
	rosterMaxVisible = 16 // cap on user rows before "... +N more"
	composerRowsMax  = 3  // tall composer on roomy terminals
	headerHeight     = 1  // top banner line
	frameChrome      = 2  // outer app-frame border (top+bottom / left+right)
	transcriptBorder = 2  // rows consumed by the transcript box border
	minSidebarTermW  = 66 // below this width the sidebar collapses entirely
)

// sidebarWidthFor picks a comfortable reading column that grows with the
// terminal ("font size" adaptation for terminals happens via density).
func sidebarWidthFor(termW int) int {
	switch {
	case termW >= 130:
		return 30
	case termW >= 90:
		return 26
	default:
		return rosterTotalWidth
	}
}

// composerRowsFor gives the message box breathing room on tall screens and
// shrinks gracefully on small ones (0 => bare prompt, no border).
func composerRowsFor(termH int) int {
	switch {
	case termH >= 22:
		return composerRowsMax
	case termH >= 14:
		return 2
	case termH >= 10:
		return 1
	default:
		return 0
	}
}

// layout is the single source of truth for frame geometry. Both View() and the
// mouse hit-test derive their math from this struct so a click always maps to
// exactly what is on screen.
//
// Height invariant (the contract that keeps us inside the terminal):
//
//	headerHeight + (vpHeight + transcriptBorder) + inputChromeHeight + statusRows
//	    + paletteRows
//	    == termH   (exactly; never more)
type layout struct {
	vpWidth         int  // transcript viewport inner width
	vpHeight        int  // transcript viewport visible rows (inside its border)
	sidebarOn       bool // false on narrow terminals — panel collapses
	rosterX         int  // leftmost column of the sidebar
	rosterY0        int  // first terminal row inside the sidebar that holds a user
	rosterSlots     int  // how many roster rows fit under the current vpHeight
	statusRows      int  // extra rows consumed by the status line (0 or 1)
	paletteRows     int  // rows reserved for the "/" drawer incl. its spacer (0 = closed)
	showHeader      bool // staged degradation: hide banner on tiny heights
	boxedTranscript bool // staged degradation: drop border rows on tiny heights
	inputBoxed      bool // false => bare one-line prompt
	sidebarWidth    int  // density-scaled reading column (outer, incl border)
	composerRows    int  // writable rows inside the composer box (3/2/1/0)
	frameOn         bool // full-screen app frame (dropped only on tiny H)
}

// totalRows reports the exact number of terminal rows a frame will occupy.
func (l layout) totalRows() int {
	h := l.vpHeight + l.statusRows + l.paletteRows
	if l.boxedTranscript {
		h += transcriptBorder
	}
	if l.composerRows > 0 {
		h += l.composerRows + 2 // composer border
	} else {
		h++ // bare prompt line
	}
	if l.showHeader {
		h += headerHeight
	}
	if l.frameOn {
		h += frameChrome
	}
	return h
}

// computeLayout derives frame geometry purely from terminal size and whether
// the status line is visible. See computeLayoutWithPalette for the full
// contract; paletteRows defaults to 0 (drawer closed).
func computeLayout(termW, termH int, showStatus bool) layout {
	return computeLayoutWithPalette(termW, termH, showStatus, 0)
}

// computeLayoutWithPalette reserves paletteRows extra rows for the "/" drawer
// before handing the remainder to the viewport. Guarantees, in order:
//  1. totalRows() <= termH always (staged chrome degradation on tiny screens;
//     the drawer itself is sacrificed late — only after the frame is gone)
//  2. full-screen app frame whenever height allows (OpenCode-style shell)
//  3. sidebar collapses below minSidebarTermW / tiny content, else its width
//     scales with terminal width for comfortable reading
//  4. composer gets a tall writable box on roomy terminals, shrinking to a
//     bare prompt on absurd heights
//  5. viewport absorbs all remaining space (floors at zero rows)
//
// Pure function => trivially unit-testable.
func computeLayoutWithPalette(termW, termH int, showStatus bool, paletteRows int) layout {
	var l layout
	if termW <= 0 || termH <= 0 {
		return l
	}
	l.statusRows = 0
	if showStatus {
		l.statusRows = 1
	}
	l.paletteRows = paletteRows
	l.showHeader = true
	l.boxedTranscript = true
	l.frameOn = termH >= 12 // below this the shell cannot fit its own border
	l.composerRows = composerRowsFor(termH)
	l.sidebarWidth = sidebarWidthFor(termW)

	innerW := termW - frameChrome
	if !l.frameOn {
		innerW = termW
	}

	// --- width pass ---------------------------------------------------------
	l.sidebarOn = termW >= minSidebarTermW
	if l.sidebarOn {
		l.rosterX = innerW - l.sidebarWidth
		if l.frameOn {
			l.rosterX++ // shift past the left frame border
		}
		l.vpWidth = innerW - l.sidebarWidth - 1 - transcriptBorder // spacer + own border
	} else {
		l.rosterX = 0
		l.vpWidth = innerW - transcriptBorder
	}
	if l.vpWidth < 10 { // last-resort floor on absurdly narrow terms
		l.vpWidth = 10
	}

	// --- height pass: degrade until the frame provably fits -----------------
	shrink := func() {
		switch {
		case l.composerRows > 1:
			l.composerRows--
		case l.statusRows == 1:
			l.statusRows = 0
		case l.composerRows == 1:
			l.composerRows = 0 // bare prompt
		case l.boxedTranscript:
			l.boxedTranscript = false
			if l.sidebarOn { // nothing to sit beside once unframed
				l.sidebarOn = false
				l.rosterX = 0
				l.vpWidth = widthInsideFrame(termW, l.frameOn) - transcriptBorder
			}
		case l.frameOn:
			l.frameOn = false
			innerW = termW
			if l.sidebarOn { // recompute width pass without the frame inset
				l.rosterX = innerW - l.sidebarWidth
				l.vpWidth = innerW - l.sidebarWidth - 1 - transcriptBorder
			} else {
				l.vpWidth = innerW - transcriptBorder
			}
			if l.vpWidth < 10 {
				l.vpWidth = 10
			}
		case l.paletteRows > 0:
			l.paletteRows = 0 // absurdly tiny terminal: dissolve the drawer
		default:
			l.showHeader = false
		}
	}
	for l.totalRows() > termH {
		before := l.totalRows()
		shrink()
		if l.totalRows() == before {
			break // fully degraded; impossible beyond this point
		}
	}
	l.vpHeight = termH - l.totalRows() // chrome settled with vp=0; fill remainder
	if l.vpHeight < 0 {
		l.vpHeight = 0
	}

	// Roster adapts to the transcript height: title row eats one slot.
	l.rosterSlots = l.vpHeight - 1
	if l.rosterSlots > rosterMaxVisible {
		l.rosterSlots = rosterMaxVisible
	}
	if l.rosterSlots < 0 || !l.sidebarOn {
		l.rosterSlots = 0
	}
	if l.vpHeight < 3 && l.sidebarOn {
		l.sidebarOn = false // no room for border+title+even one user
		l.rosterX = 0
		l.vpWidth = widthInsideFrame(termW, l.frameOn) - transcriptBorder
		if l.vpWidth < 10 {
			l.vpWidth = 10
		}
	}

	// Sidebar stack above the first user row: [optional header] + sidebar-box
	// top border + "ONLINE" title (+1 if app frame is on).
	l.rosterY0 = 2
	if l.showHeader {
		l.rosterY0 += headerHeight
	}
	if l.frameOn {
		l.rosterY0++
	}
	return l
}

// widthInsideFrame is the usable content width before the transcript border:
// the full terminal when unframed, minus the shell's side borders otherwise.
func widthInsideFrame(termW int, frameOn bool) int {
	if frameOn {
		return termW - frameChrome
	}
	return termW
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// truncateStringPlain hard-cuts a string to n cells (runes), no styling.
func truncateStringPlain(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- messages --------------------------------------------------------------

type pollTickMsg struct{}
type beatTickMsg struct{}
type backlogMsg struct {
	msgs []chatMessage
	err  error
}
type pollDoneMsg struct {
	newMsgs []chatMessage
	users   []string // authoritative roster snapshot at poll time
	ended   bool
	err     error
}
type beatDoneMsg struct {
	users []string
	err   error
}

type wsConnectedMsg struct {
	ws *wsClient
}
type sendDoneMsg struct {
	text string
	to   string // empty for broadcasts
	seq  int
	code int
	err  error
}
type leaveDoneMsg struct{}
type convBacklogMsg struct {
	conv string
	msgs []chatMessage
	err  error
}

// ---- model -----------------------------------------------------------------

type chatScreen struct {
	client       *chatClient
	key          string
	me           string
	persist      bool
	width        int
	height       int
	history      []chatMessage        // every confirmed server message (deduped by seq)
	localLines   []localLine          // echoes & notes not backed by server docs
	fetchedConvs map[string]bool      // threads already deep-fetched this session
	hoverPeer    string               // sidebar row under the mouse ("" = none)
	unread       map[string]int       // peer -> unread DM count (cleared on open)
	lastDMAt     map[string]time.Time // peer -> newest incoming DM (recency sort)
	lines        []string             // DERIVED paint buffer: rebuildView() owns it
	rendered     map[int]bool         // server seqs already rendered
	pending      *pendingSend         // single in-flight send (nil = idle)
	outbox       []queuedLine         // queued sends waiting for the in-flight one
	users        []string
	vp           viewport.Model
	input        textinput.Model
	palette      paletteState    // "/" command drawer above the composer
	picker       pickerState     // file-browser mode of that drawer (/upload)
	uploadBuf    []string        // persistent upload buffer (survives picker close)
	uploadBufSet map[string]bool // set view of uploadBuf for O(1) lookups
	uploadQ      uploadState     // sequential session-file transfer queue
	dlQ          dlState         // sequential shared-file download queue
	filesSeen    map[string]bool // room files already announced to me
	lastFilesAt  string          // newest uploadedAt fed into /files?since=
	ws           *wsClient       // WebSocket connection (nil = using long-poll)
	status       string
	targetUser   string // private-chat peer; "" = general room
	beatFailures int
	pollFailures int
}

// pendingSend tracks the optimistic echo line for the in-flight send so the
// confirmation can swap it in place (or mark failure) without ambiguity.
type pendingSend struct {
	lineIdx int
	text    string
	conv    string // conversation the optimistic echo belongs to
	to      string // recipient ("": broadcast) - needed to reconstruct on settle
}

// localLineKind distinguishes plain text lines from styled file attachment cards.
type localLineKind int

const (
	lineText localLineKind = iota
	lineFileCard
)

// fileCardData holds metadata for rendering a file attachment card.
type fileCardData struct {
	filename  string
	username  string
	size      string // pre-formatted human size
	time      string // formatted timestamp "15:04"
	createdAt string // RFC3339 for chronological interleaving
}

// localLine is a UI-generated transcript row scoped to one conversation so
// mode switches never bleed it across views.
type localLine struct {
	conv     string
	text     string
	kind     localLineKind
	fileData *fileCardData // non-nil when kind == lineFileCard
}

// queuedLine remembers WHERE a typed line belonged when it was enqueued, so
// draining it later cannot fire it into whatever thread the user has since
// switched to.
type queuedLine struct {
	conv string
	text string
}

// activeConv is the conversation bucket currently painted on screen:
// "general" in the common room, the canonical pair key inside a thread.
func (c *chatScreen) activeConv() string {
	if c.targetUser == "" {
		return generalConv
	}
	return conversationKey(c.me, c.targetUser)
}

// composerPlaceholder doubles as the restore text whenever a drawer closes.
const composerPlaceholder = "Type a message…  ·  / commands  ·  tab picks a peer"

func newChatScreen(serverURL, key, me string) chatScreen {
	ti := textinput.New()
	ti.Placeholder = composerPlaceholder
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "❯ "
	ti.Width = 36
	vp := viewport.New(80, 20)
	return chatScreen{
		client:       newChatClient(serverURL, key, me),
		key:          key,
		me:           me,
		vp:           vp,
		input:        ti,
		rendered:     map[int]bool{},
		unread:       map[string]int{},
		lastDMAt:     map[string]time.Time{},
		fetchedConvs: map[string]bool{generalConv: true},
		filesSeen:    map[string]bool{},
		outbox:       nil,
	}
}

// orderedUsers produces the sidebar display order:
//
//	[me] ++ peers with a DM history, NEWEST incoming DM first,
//	     then never-messaged peers in their original roster order.
//
// Pure function so rendering and mouse hit-testing share one truth.
func orderedUsers(users []string, me string, lastDMAt map[string]time.Time) []string {
	out := make([]string, 0, len(users))
	var messaged, fresh []string
	for _, u := range users {
		if u == me {
			continue
		}
		if _, ok := lastDMAt[u]; ok {
			messaged = append(messaged, u)
		} else {
			fresh = append(fresh, u)
		}
	}
	sort.SliceStable(messaged, func(i, j int) bool {
		return lastDMAt[messaged[i]].After(lastDMAt[messaged[j]])
	})
	out = append(out, me)
	out = append(out, messaged...)
	out = append(out, fresh...)
	return out
}

// isOwnPresence reports whether a system presence line refers to me.
// Server format is strictly "<username> joined" / "<username> left", so an
// exact prefix+" joined/left" match avoids the bob/bobby false positive that
// a bare strings.Contains would produce.
func isOwnPresence(systemText, me string) bool {
	return systemText == me+" joined" || systemText == me+" left"
}

// shouldRender decides visibility BEFORE any styling. The rule is strictly
// conversational: a row paints only if it belongs to the ACTIVE bucket.
// General view therefore NEVER shows DM lines (they live exclusively in their
// own threads), and a thread view shows nothing from the room.
func (c *chatScreen) shouldRender(m chatMessage) bool {
	if m.ConvID == "" { // defensive for servers predating convId
		m.ConvID = generalConv
	}
	return m.ConvID == c.activeConv()
}

func (c *chatScreen) renderLine(m chatMessage) string {
	// Timestamp - used for system lines and as bubble timestamp
	tsPlain := "--:--"
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		tsPlain = t.Local().Format("15:04")
	}
	ts := tuiTimeStyle.Render("[" + tsPlain + "]")
	bubbleTs := tuiBubbleTimeStyle.Render(tsPlain)
	if m.Kind == "system" {
		if isOwnPresence(m.Text, c.me) {
			return "" // never announce my own join/leave to me
		}
		return ts + " " + tuiSystemStyle.Render("* "+m.Text)
	}
	isOwn := m.Username == c.me
	textRendered := renderMarkdown(m.Text)

	availWidth := c.vp.Width
	if availWidth <= 0 {
		availWidth = 60
	}
	maxBubbleW := int(float64(availWidth) * 0.62)
	if maxBubbleW < 22 {
		maxBubbleW = 22
	}
	if maxBubbleW > availWidth-2 {
		maxBubbleW = availWidth - 2
	}

	// Build bubble inner: for others show sender name on top line
	var innerPlain string
	if isOwn {
		innerPlain = textRendered
	} else {
		nameLine := tuiNameStyle.Render(m.Username)
		innerPlain = nameLine + "\n" + textRendered
	}
	innerWithTs := innerPlain + "  " + bubbleTs

	// Compact hug: needed = content width + padding/border
	needed := lipgloss.Width(innerPlain) + lipgloss.Width(tsPlain) + 6
	if needed < 14 {
		needed = 14
	}
	bubbleW := needed
	if bubbleW > maxBubbleW {
		bubbleW = maxBubbleW
	}
	bubbleInner := innerWithTs
	contentW := lipgloss.Width(innerPlain)
	if contentW > maxBubbleW-10 {
		bubbleInner = innerPlain + "\n" + strings.Repeat(" ", max(0, bubbleW-lipgloss.Width(tsPlain)-4)) + bubbleTs
	}

	var style lipgloss.Style
	if isOwn {
		style = tuiOwnBubbleStyle
	} else {
		style = tuiOtherBubbleStyle
	}
	bubble := style.Width(bubbleW).Render(bubbleInner)

	if isOwn {
		// Use lipgloss right-align so leading spaces survive viewport wrapping
		return lipgloss.NewStyle().Width(availWidth).Align(lipgloss.Right).Render(bubble)
	}
	return bubble
}

// addMessage records a confirmed server message and refreshes the view.
func (c *chatScreen) addMessage(m chatMessage) {
	if c.rendered[m.Seq] {
		return
	}
	c.rendered[m.Seq] = true
	if m.ConvID != "" && m.ConvID != generalConv && m.Username != c.me {
		if c.unread == nil {
			c.unread = map[string]int{}
		}
		if c.lastDMAt == nil {
			c.lastDMAt = map[string]time.Time{}
		}
		peer := peerOf(c.me, m.ConvID)
		if peer != "" {
			c.lastDMAt[peer] = time.Now() // recency bump on EVERY arrival
			if c.activeConv() != m.ConvID {
				c.unread[peer]++ // badge only when the thread is out of sight
			}
		}
	}
	c.history = append(c.history, m)
	c.rebuildView()
}

// peerOf extracts the OTHER participant from a canonical "a|b" pair key.
func peerOf(me, conv string) string {
	parts := strings.Split(conv, "|")
	for _, p := range parts {
		if p != me {
			return p
		}
	}
	return ""
}

func (c *chatScreen) appendLine(s string) {
	c.appendLocal(generalConv, s)
}

func (c *chatScreen) appendLocal(conv, text string) {
	c.localLines = append(c.localLines, localLine{conv: conv, text: text, kind: lineText})
	c.rebuildView()
}

// appendLocalFileCard adds a styled file attachment card to the transcript.
func (c *chatScreen) appendLocalFileCard(conv, filename, username, sizeStr, timestamp string) {
	// Derive RFC3339 for interleaving; caller passes local "15:04" so we
	// synthesize a full timestamp from now if not already RFC3339.
	rfc := ""
	if timestamp != "" {
		if _, err := time.Parse(time.RFC3339, timestamp); err == nil {
			rfc = timestamp
		} else if t, err := time.Parse("15:04", timestamp); err == nil {
			now := time.Now()
			rfc = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local).Format(time.RFC3339)
		} else {
			rfc = time.Now().Format(time.RFC3339)
		}
	} else {
		rfc = time.Now().Format(time.RFC3339)
	}
	c.localLines = append(c.localLines, localLine{
		conv: conv,
		kind: lineFileCard,
		fileData: &fileCardData{
			filename:  filename,
			username:  username,
			size:      sizeStr,
			time:      timestamp,
			createdAt: rfc,
		},
	})
	c.rebuildView()
}

// appendLocalFileCardWithRFC3339 is like appendLocalFileCard but accepts an
// explicit RFC3339 timestamp (used for server-sourced file announcements).
func (c *chatScreen) appendLocalFileCardWithRFC3339(conv, filename, username, sizeStr, rfc3339 string) {
	ts := ""
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		ts = t.Local().Format("15:04")
	}
	c.localLines = append(c.localLines, localLine{
		conv: conv,
		kind: lineFileCard,
		fileData: &fileCardData{
			filename:  filename,
			username:  username,
			size:      sizeStr,
			time:      ts,
			createdAt: rfc3339,
		},
	})
	c.rebuildView()
}

// rebuildView derives the painted transcript from raw history + local lines,
// applying the CURRENT visibility filter. Entering/leaving private mode just
// calls this — historical lines re-filter retroactively.
// File cards are interleaved chronologically among history messages so they
// behave like text messages; transient local lines (pending echo, progress)
// stay pinned at the bottom.
func (c *chatScreen) rebuildView() {
	c.lines = c.lines[:0]

	// Partition local lines: file cards (to interleave) vs transient lines (pinned at bottom).
	type cardEntry struct {
		ll localLine
		t  time.Time
	}
	var cards []cardEntry
	var pinned []localLine
	for _, ll := range c.localLines {
		if ll.conv != c.activeConv() {
			continue
		}
		if ll.kind == lineFileCard && ll.fileData != nil {
			tt := time.Now()
			if ll.fileData.createdAt != "" {
				if p, err := time.Parse(time.RFC3339, ll.fileData.createdAt); err == nil {
					tt = p
				}
			} else if ll.fileData.time != "" {
				if p, err := time.Parse("15:04", ll.fileData.time); err == nil {
					now := time.Now()
					tt = time.Date(now.Year(), now.Month(), now.Day(), p.Hour(), p.Minute(), 0, 0, time.Local)
				}
			}
			cards = append(cards, cardEntry{ll: ll, t: tt})
		} else {
			pinned = append(pinned, ll)
		}
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].t.Before(cards[j].t) })

	// Collect visible history messages sorted by Seq (already chronological).
	var visibleHistory []chatMessage
	for _, m := range c.history {
		if c.shouldRender(m) {
			visibleHistory = append(visibleHistory, m)
		}
	}

	// Merge history + file cards by timestamp.
	hi, ci := 0, 0
	for hi < len(visibleHistory) || ci < len(cards) {
		var histTime time.Time
		hasHist := hi < len(visibleHistory)
		if hasHist {
			if t, err := time.Parse(time.RFC3339, visibleHistory[hi].CreatedAt); err == nil {
				histTime = t
			} else {
				// Fallback: treat missing timestamp as very old so cards sort after;
				// but for our own messages CreatedAt is now set, so this rarely fires.
				histTime = time.Time{}
			}
		}
		hasCard := ci < len(cards)
		// If history timestamp missing, keep history order and render hist first.
		cardBeforeHist := hasCard && hasHist && !histTime.IsZero() && cards[ci].t.Before(histTime)
		if hasCard && (!hasHist || cardBeforeHist) {
			fd := cards[ci].ll.fileData
			card := fileAttachmentCard(fd.filename, fd.username, fd.size, fd.time, c.vp.Width)
			if fd.username == c.me {
				card = lipgloss.NewStyle().Width(c.vp.Width).Align(lipgloss.Right).Render(card)
			}
			c.lines = append(c.lines, card)
			ci++
		} else if hasHist {
			c.lines = append(c.lines, c.renderLine(visibleHistory[hi]))
			hi++
		} else {
			break
		}
	}
	// Append transient pinned lines (pending echo, progress bars, errors) at bottom.
	for _, ll := range pinned {
		c.lines = append(c.lines, ll.text)
	}
	if c.pending != nil && c.pending.conv == c.activeConv() && len(c.lines) > 0 {
		c.pending.lineIdx = len(c.lines) - 1 // pending echo is last local of its conv
	}
	c.refreshViewport()
}

// refreshViewport re-serializes the transcript, hard-wrapping every line to
// the current viewport width. lipgloss Width() wraps ANSI-aware, so styled
// lines fold instead of being clipped by the viewport on narrow terminals.
// It preserves the user's scroll position: only auto-scrolls if already at bottom.
func (c *chatScreen) refreshViewport() {
	w := c.vp.Width
	if w <= 0 {
		w = 40
	}
	atBottom := c.vp.AtBottom()
	st := lipgloss.NewStyle().Width(w)
	wrapped := make([]string, len(c.lines))
	for i, ln := range c.lines {
		wrapped[i] = st.Render(ln)
	}
	c.vp.SetContent(strings.Join(wrapped, "\n"))
	if atBottom {
		c.vp.GotoBottom()
	}
}

// scrollbarView renders a vertical scrollbar for the transcript viewport.
// Height h includes the border interior rows. Uses ScrollPercent() to position
// the thumb proportionally. Returns a single-column string of height h.
func (c chatScreen) scrollbarView(h int) string {
	if h <= 0 {
		return ""
	}
	total := c.vp.TotalLineCount()
	if total <= 0 {
		total = len(c.lines)
	}
	visible := c.vp.Height
	if visible <= 0 {
		visible = h
	}
	// No scrolling needed: draw empty track.
	if total <= visible {
		rows := make([]string, h)
		for i := range rows {
			rows[i] = tuiScrollbarStyle.Render("│")
		}
		return strings.Join(rows, "\n")
	}
	// Reserve top/bottom arrows.
	trackH := h
	hasArrows := h >= 3
	if hasArrows {
		trackH = h - 2
	}
	thumbH := trackH * visible / total
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > trackH {
		thumbH = trackH
	}
	pct := c.vp.ScrollPercent() // 0.0 - 1.0
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	thumbPos := int(float64(trackH-thumbH) * pct)
	if thumbPos < 0 {
		thumbPos = 0
	}
	if thumbPos+thumbH > trackH {
		thumbPos = trackH - thumbH
	}
	var rows []string
	if hasArrows {
		rows = append(rows, tuiScrollbarStyle.Render("▲"))
	}
	for i := 0; i < trackH; i++ {
		if i >= thumbPos && i < thumbPos+thumbH {
			rows = append(rows, tuiScrollbarThumbStyle.Render("█"))
		} else {
			rows = append(rows, tuiScrollbarStyle.Render("│"))
		}
	}
	if hasArrows {
		rows = append(rows, tuiScrollbarStyle.Render("▼"))
	}
	return strings.Join(rows, "\n")
}

func (c chatScreen) headerView() string {
	mode := ""
	if c.targetUser != "" {
		mode = fmt.Sprintf(" · private with %s · ESC = general", c.targetUser)
	}
	text := fmt.Sprintf(" uplink chat · key %s · you are %s · %d online%s ",
		c.key, c.me, len(c.users), mode)
	l := c.layoutFor()
	w := c.width
	if l.frameOn {
		w -= frameChrome // banner lives inside the app shell
	}
	// Width() wraps long banners into multiple lines — fatal for our exact
	// height contract. Overflowing text degrades to a hard-truncated plain
	// run instead; otherwise the banner fills the full terminal width.
	if w <= 0 || lipgloss.Width(text) > w {
		return truncateStringPlain(text, maxInt(w, 0))
	}
	return tuiHeaderStyle.Width(w).Render(text)
}

func (c chatScreen) statusView() string {
	if c.status == "" {
		return ""
	}
	return tuiErrStyle.Render(c.status)
}

// circledNum maps 1..50 onto Unicode circled digits (①…⑳ ㉑…㉟ ㊱…㊿).
// Enclosed glyphs read visually smaller than body text and their interior
// takes the BACKGROUND colour, so styling them black-on-green yields exactly
// a "green circle, dark number" chip without any font tricks.
func circledNum(n int) string {
	switch {
	case n >= 1 && n <= 20:
		return string(rune(0x2460 + n - 1))
	case n <= 35:
		return string(rune(0x3251 + n - 21))
	case n <= 50:
		return string(rune(0x32B1 + n - 36))
	default:
		return string(rune(0x32BF)) // ㊿ saturates
	}
}

// unreadBadge renders the pending-DM chip for a peer ("" when none).
func (c chatScreen) unreadBadge(peer string) string {
	n := c.unread[peer]
	if n <= 0 {
		return ""
	}
	return tuiUnreadStyle.Render(circledNum(n))
}

// rosterBody renders the bordered sidebar with EXACTLY slots content rows
// (title + users), so its height always matches the transcript column. The
// overflow indicator replaces the final slot when participants overflow.
// sidebarBody renders the right column: an ONLINE section with presence dots
// and a full-row highlight on the selected peer. The transcript itself is
// the single source of conversation context, so no thread list is shown.
// Height is deterministic: exactly `fill` content rows (+border in View).
func (c chatScreen) rosterBody(fill int) string {
	if fill < 1 {
		fill = 1 // always show the section header
	}
	slots := fill - 1 // title owns the first row
	inner := c.sidebarInnerWidth()
	trunc := func(t string) string {
		r := []rune(t)
		if len(r) > inner {
			return string(r[:maxInt(inner-1, 0)]) + "…"
		}
		return t
	}
	rows := make([]string, 0, slots+1)
	addPlain := func(text string) {
		if len(rows)-1 >= slots { // never exceed the slot budget
			return
		}
		rows = append(rows, " "+text)
	}

	online := orderedUsers(c.users, c.me, c.lastDMAt)
	title := fmt.Sprintf("ONLINE — %d", len(online))
	rows = append(rows, tuiSectionTitleStyle.Render(trunc(title)))
	bodySlots := maxInt(slots-1, 0)

	shown := 0
	for _, u := range online {
		if shown == bodySlots && len(online) > bodySlots {
			more := len(online) - shown
			addPlain(tuiDimStyle.Render(fmt.Sprintf("… +%d more", more)))
			shown++
			break
		}
		dot := "○"
		switch u {
		case c.me:
			dot = "●"
		case c.targetUser:
			dot = "●"
		}
		name := u
		if u == c.me {
			name += " (you)"
		}
		line := dot + " " + name

		if badge := c.unreadBadge(u); badge != "" {
			// Right-align the chip with a guaranteed gap from the name.
			bw := lipgloss.Width(badge)
			nameW := lipgloss.Width(line)
			gap := inner - nameW - bw - 1
			if gap < 2 {
				gap = 2 // minimum distance even on narrow columns
			}
			if nameW+gap+bw > inner {
				line = trunc(line[:maxInt(inner-bw-gap, 1)]) // hard clip name
			}
			line += strings.Repeat(" ", gap) + badge
		}

		if u == c.hoverPeer {
			line = tuiHoverStyle.Render(line) // pink on THIS row only
		}
		addPlain(line)
		shown++
	}

	for len(rows) < fill {
		rows = append(rows, "")
	}
	if len(rows) > fill {
		rows = rows[:fill]
	}
	return strings.Join(rows, "\n")
}

// sidebarInnerWidth is the writable width inside the sidebar border.
func (c chatScreen) sidebarInnerWidth() int {
	l := c.layoutFor()
	if !l.sidebarOn {
		return rosterWidthInner
	}
	return l.sidebarWidth - 2
}

// ---- async commands --------------------------------------------------------

func schedulePoll(backoff time.Duration) tea.Cmd {
	if backoff > 0 {
		return tea.Tick(backoff, func(time.Time) tea.Msg { return pollTickMsg{} })
	}
	return func() tea.Msg { return pollTickMsg{} }
}

func scheduleBeat() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg { return beatTickMsg{} })
}

func (c chatScreen) doPoll() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		newMsgs, ended, err := client.pollOnce()
		return pollDoneMsg{newMsgs: newMsgs, users: client.users, ended: ended, err: err}
	}
}

func (c chatScreen) doBeat() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		hb, err := client.beatOnce()
		return beatDoneMsg{users: hb.ActiveUsers, err: err}
	}
}

func (c chatScreen) doSend(text, to string) tea.Cmd {
	client := c.client
	ws := c.ws
	return func() tea.Msg {
		if ws != nil {
			if err := ws.send(text, to); err == nil {
				return sendDoneMsg{text: text, to: to, code: 200}
			}
			// WS send failed — fall back to HTTP
		}
		code, msg, err := client.sendMessage(text, to)
		return sendDoneMsg{text: text, to: msg.To, seq: msg.Seq, code: code, err: err}
	}
}

func (c chatScreen) doLeave() tea.Cmd {
	client := c.client
	ws := c.ws
	return func() tea.Msg {
		if ws != nil {
			ws.Close()
		}
		client.leave()
		return leaveDoneMsg{}
	}
}

func (c chatScreen) fetchBacklogCmd() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		msgs, err := client.fetchBacklog()
		return backlogMsg{msgs: msgs, err: err}
	}
}

// doConvFetch deep-fetches one conversation's latest page from the server.
func (c chatScreen) doConvFetch(conv string) tea.Cmd {
	client := c.client
	return func() tea.Msg {
		msgs, err := client.fetchConvBacklog(conv)
		return convBacklogMsg{conv: conv, msgs: msgs, err: err}
	}
}

// ---- tea.Model -------------------------------------------------------------

func (c chatScreen) Init() tea.Cmd {
	// Try WebSocket first; fall back to long-polling if unavailable.
	return tea.Batch(c.fetchBacklogCmd(), c.tryWS(), c.doBeat(), c.doFetchFiles(), scheduleBeat())
}

// tryWS attempts a WebSocket upgrade. On success it starts the readLoop
// and heartbeat goroutines and returns nil (no polling needed). On
// failure it returns the initial long-poll schedule.
func (c chatScreen) tryWS() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		ws := client.wsConnect()
		if ws == nil {
			return schedulePoll(0)() // fallback: start HTTP long-polling
		}
		// WebSocket connected — stash it and start background goroutines.
		// We can't mutate chatScreen from a Cmd goroutine, so we return
		// a special message that Update handles.
		go ws.readLoop(client)
		go ws.heartbeat(client)
		return wsConnectedMsg{ws: ws}
	}
}

func (c *chatScreen) handleNewMessage(m chatMessage) {
	c.addMessage(m)
}

// enterPrivate switches to a 1:1 thread. History older than the mixed
// backlog window is deep-fetched lazily the first time the thread opens.
// Returns nil or the backlog command - caller MUST schedule it.
func (c *chatScreen) enterPrivate(user string) tea.Cmd {
	c.targetUser = user
	c.palette.close()      // stale "/" query must not survive a mode switch
	delete(c.unread, user) // opening the thread clears its badge
	conv := conversationKey(c.me, user)
	c.rebuildView()
	if c.fetchedConvs == nil {
		c.fetchedConvs = map[string]bool{}
	}
	var fetchCmd tea.Cmd
	if !c.fetchedConvs[conv] {
		c.fetchedConvs[conv] = true // ask once regardless of outcome
		fetchCmd = c.doConvFetch(conv)
	}
	return fetchCmd
}

// exitPrivate returns to the common room; returns a system line or "".
func (c *chatScreen) exitPrivate() {
	if c.targetUser == "" {
		return
	}
	c.targetUser = ""
	c.palette.close()
	c.rebuildView()
}

// submitLine handles one committed input line. It returns the tea.Cmd that
// performs the network send (nil for local-only commands). CRITICAL: the
// caller MUST append this command — it is the ONLY thing that actually puts
// the message on the wire. Registry commands are executed here too, so a
// typed "/help" behaves exactly like one picked from the palette.
func (c *chatScreen) submitLine(text string) tea.Cmd {
	t := strings.ToLower(strings.TrimSpace(text))
	for _, cmd := range slashCommands {
		if t == cmd.Name {
			return c.runCommand(t)
		}
	}
	if c.pending != nil {
		c.outbox = append(c.outbox, queuedLine{conv: c.activeConv(), text: text})
		return nil
	}
	return c.dispatchSend(text)
}

// dispatchSend paints the optimistic echo, latches the in-flight slot and
// returns the wire command. Private view targets the message at the selected
// peer; common room sends broadcast.
func (c *chatScreen) dispatchSend(text string) tea.Cmd {
	return c.dispatchInConv(c.activeConv(), text)
}

// dispatchInConv paints the echo into a specific conversation bucket and
// targets the peer that conversation represents (empty conv => broadcast).
func (c *chatScreen) dispatchInConv(conv, text string) tea.Cmd {
	var peer string
	if conv != generalConv {
		parts := strings.Split(conv, "|")
		for _, u := range parts {
			if u != c.me {
				peer = u
			}
		}
	}
	availWidth := c.vp.Width
	if availWidth <= 0 {
		availWidth = 60
	}
	textRendered := renderMarkdown(text)
	tsPlain := time.Now().Format("15:04")
	bubbleTs := tuiBubbleTimeStyle.Render(tsPlain)
	maxBubbleW := int(float64(availWidth) * 0.62)
	if maxBubbleW < 22 {
		maxBubbleW = 22
	}
	if maxBubbleW > availWidth-2 {
		maxBubbleW = availWidth - 2
	}
	needed := lipgloss.Width(textRendered) + lipgloss.Width(tsPlain) + 6
	if needed < 14 {
		needed = 14
	}
	bubbleW := needed
	if bubbleW > maxBubbleW {
		bubbleW = maxBubbleW
	}
	innerWithTs := textRendered + "  " + bubbleTs
	if lipgloss.Width(textRendered) > maxBubbleW-10 {
		innerWithTs = textRendered + "\n" + strings.Repeat(" ", max(0, bubbleW-lipgloss.Width(tsPlain)-4)) + bubbleTs
	}
	bubble := tuiOwnBubbleStyle.Width(bubbleW).Render(innerWithTs)
	echo := lipgloss.NewStyle().Width(availWidth).Align(lipgloss.Right).Render(bubble)
	c.localLines = append(c.localLines, localLine{conv: conv, text: echo})
	c.pending = &pendingSend{text: text, conv: conv, to: peer}
	target := peer
	if target == "" && c.pending.conv == generalConv {
		target = ""
	}
	c.rebuildView()
	return c.doSend(text, target)
}

func (c chatScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.hoverPeer = "" // geometry changed; stale hover is meaningless
		l := c.layoutFor()
		// Reserve 1 column for scrollbar inside transcript.
		vpW := l.vpWidth
		if l.vpHeight > 0 && vpW > 10 {
			vpW--
		}
		c.vp.Width = vpW
		c.vp.Height = l.vpHeight
		// Keep the composer inside its column: textinput pads/clips to Width.
		if c.input.Width != l.vpWidth-4 {
			c.input.Width = maxInt(l.vpWidth-4, 8)
		}
		c.refreshViewport() // re-wrap transcript to the new width

	case backlogMsg:
		if msg.err != nil {
			c.status = "failed to load history: " + msg.err.Error()
			break
		}
		for _, m := range msg.msgs {
			c.addMessage(m)
		}

	case pollTickMsg:
		if c.ws == nil {
			cmds = append(cmds, c.doPoll())
		}

	case pollDoneMsg:
		if msg.ended {
			reason := "Session has ended"
			if msg.err != nil {
				reason = msg.err.Error()
			}
			c.appendLine(tuiSystemStyle.Render("* " + reason))
			cmds = append(cmds, c.doLeave(), tea.Quit)
			return c, tea.Batch(cmds...)
		}
		if msg.err != nil {
			c.pollFailures++
			c.status = fmt.Sprintf("reconnecting… (%d)", c.pollFailures)
			cmds = append(cmds, schedulePoll(time.Duration(400*c.pollFailures)*time.Millisecond))
			return c, tea.Batch(cmds...)
		}
		if c.pollFailures > 0 {
			c.pollFailures = 0
			c.status = ""
		}
		if msg.users != nil {
			c.users = msg.users // join/leave freshness without waiting for heartbeat
		}
		for _, m := range msg.newMsgs {
			c.handleNewMessage(m)
		}
		cmds = append(cmds, schedulePoll(0))

	case beatTickMsg:
		cmds = append(cmds, c.doBeat())

	case beatDoneMsg:
		if msg.err != nil {
			c.beatFailures++
			if c.beatFailures >= 3 {
				c.status = "connection lost… retrying"
			}
		} else {
			c.beatFailures = 0
			if c.status == "connection lost… retrying" {
				c.status = ""
			}
			// Authoritative snapshot — INCLUDING shrinking to empty.
			c.users = msg.users
			live := map[string]bool{c.me: true}
			for _, u := range msg.users {
				live[u] = true
			}
			for peer := range c.unread {
				if !live[peer] {
					delete(c.unread, peer)
				}
			}
			for peer := range c.lastDMAt {
				if !live[peer] {
					delete(c.lastDMAt, peer)
				}
			}
			if c.hoverPeer != "" && !live[c.hoverPeer] {
				c.hoverPeer = ""
			}
		}
		cmds = append(cmds, scheduleBeat(), c.doFetchFiles()) // roster + room files

	case sendDoneMsg:
		if nc := c.settleSend(msg); nc != nil {
			cmds = append(cmds, nc)
		}

	case wsConnectedMsg:
		c.ws = msg.ws
		c.status = "connected (websocket)"

	case convBacklogMsg:
		if msg.err == nil {
			for _, m := range msg.msgs {
				c.addMessage(m) // seq-deduped; pre-window history lands here
			}
		}

	case uploadProgressMsg:
		if c.uploadQ.active && msg.total > 0 {
			c.paintUploadLine(tuiUploadRunStyle.Render(
				progressBar("Uploading…", msg.done, msg.total)), c.uploadQ.conv)
		}
		cmds = append(cmds, drainUploadProgressCmd(c.uploadQ.progCh))

	case uploadDrainMsg:
		// channel closed; the done msg lands separately

	case uploadDoneMsg:
		if nc := c.settleUploadDone(msg); nc != nil {
			cmds = append(cmds, nc)
		}
		cmds = append(cmds, c.doFetchFiles()) // instant self-feedback

	case filesFetchedMsg:
		if msg.err == nil {
			c.applyRoomFiles(msg.files)
		}

	case filesListMsg:
		if msg.err != nil {
			if c.picker.isActive() && c.picker.mode == modeFiles {
				c.picker.loading = false
				c.picker.notice = truncateStringPlain(msg.err.Error(), 40)
			}
			break
		}
		c.applyFilesList(msg.files)

	case dlProgressMsg:
		if c.dlQ.active && msg.total > 0 {
			c.paintDlLine(tuiDownloadRunStyle.Render(
				progressBar("Downloading…", msg.done, msg.total)), c.dlQ.conv)
		}
		cmds = append(cmds, drainDlProgressCmd(c.dlQ.progCh))

	case dlDrainMsg:
		// channel closed; the done msg lands separately

	case dlDoneMsg:
		if nc := c.settleDownloadDone(msg); nc != nil {
			cmds = append(cmds, nc)
		}

	case leaveDoneMsg:
		return c, tea.Quit

	case tea.MouseMsg:
		if cmd := c.handleMouse(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			cmds = append(cmds, c.doLeave(), tea.Quit)
			return c, tea.Batch(cmds...)
		}
		// The file browser owns ALL keys while open (it sits where the "/"
		// drawer paints, one mode at a time).
		if c.picker.isActive() {
			if handled, action := c.handlePickerKeys(msg); handled {
				if action != nil {
					cmds = append(cmds, action())
				}
				return c, tea.Batch(cmds...)
			}
			break // unknown keys do nothing in browser mode
		}
		// Esc during a transfer cancels it before anything else sees the key.
		if msg.Type == tea.KeyEsc && (c.uploadQ.isActive() || c.dlQ.isActive()) {
			if c.uploadQ.isActive() {
				c.cancelUploads()
			}
			if c.dlQ.isActive() {
				c.cancelDownloads()
			}
			break
		}
		// "/" command drawer eats navigation + selection keys while open.
		// Early return keeps those keys away from the viewport so moving the
		// highlight never scrolls the transcript underneath.
		if handled, action := c.handlePaletteKeys(msg); handled {
			if action != nil {
				cmds = append(cmds, action())
			}
			return c, tea.Batch(cmds...)
		}
		if msg.Type == tea.KeyEsc {
			// Esc ONLY leaves private view. It must NEVER quit the app.
			c.exitPrivate()
			break
		}
		// Tab / Shift+Tab cycle the roster highlight — the KEYBOARD way to
		// reach a peer (mouse-less terminals otherwise have no path to DMs).
		if msg.Type == tea.KeyTab || msg.Type == tea.KeyShiftTab {
			c.cycleRosterFocus(msg.Type == tea.KeyShiftTab)
			break
		}
		if msg.Type == tea.KeyEnter {
			text := strings.TrimSpace(c.input.Value())
			c.input.SetValue("")
			if text == "" {
				// Empty Enter on a highlighted roster row OPENS that thread.
				if c.hoverPeer != "" && c.hoverPeer != c.me && c.hoverPeer != c.targetUser {
					cmd := c.enterPrivate(c.hoverPeer)
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
				break
			}
			if sc := c.submitLine(text); sc != nil {
				cmds = append(cmds, sc)
			}
			var ic tea.Cmd
			c.input, ic = c.input.Update(msg)
			cmds = append(cmds, ic)
			break
		}
		var ic tea.Cmd
		c.input, ic = c.input.Update(msg)
		cmds = append(cmds, ic)
		ensurePaletteOpen(&c) // plain edits may open/close the "/" drawer
	}

	// Viewport keeps its own scroll handling for every message.
	var vpc tea.Cmd
	c.vp, vpc = c.vp.Update(msg)
	cmds = append(cmds, vpc)

	return c, tea.Batch(cmds...)
}

// settleSend resolves the completed send against the transcript.
//
// Ownership model: a CONFIRMED message becomes part of history (the poll
// copy is suppressed via rendered-dedupe), so its optimistic echo row is
// REMOVED — never rewritten — which guarantees exactly one painted row in
// exactly one conversation. Failures keep the echo row in place, annotated.
// Either way the next queued line is promoted, preserving its origin conv.
func (c *chatScreen) settleSend(msg sendDoneMsg) tea.Cmd {
	pc := generalConv
	if c.pending != nil {
		pc = c.pending.conv
	}

	lastOfPending := -1
	for i := range c.localLines {
		if c.localLines[i].conv == pc {
			lastOfPending = i
		}
	}

	switch {
	case msg.code == 429:
		if lastOfPending >= 0 {
			c.localLines[lastOfPending] = localLine{conv: pc, text: tuiErrStyle.Render("✗ slow down — try again")}
		}
	case msg.code == 410:
		if lastOfPending >= 0 {
			c.localLines[lastOfPending] = localLine{conv: pc, text: tuiSystemStyle.Render("* Session has ended")}
		}
	case msg.err != nil:
		if lastOfPending >= 0 {
			c.localLines[lastOfPending] = localLine{conv: pc, text: tuiErrStyle.Render("✗ send failed: " + msg.err.Error())}
		} else {
			c.appendLocal(generalConv, tuiErrStyle.Render("✗ send failed: "+msg.err.Error()))
		}
	default:
		to := ""
		if c.pending != nil {
			to = c.pending.to
		} else if msg.to != "" {
			to = msg.to
		}
		conv := generalConv
		if to != "" {
			conv = conversationKey(c.me, to)
		}
		m := chatMessage{
			Seq: msg.seq, Username: c.me, Kind: "chat",
			Text: msg.text, To: to, ConvID: conv,
			CreatedAt: time.Now().Format(time.RFC3339),
		}
		c.rendered[m.Seq] = true
		known := false
		for _, h := range c.history {
			if h.Seq == m.Seq {
				known = true
				break
			}
		}
		if !known {
			c.history = append(c.history, m)
		}
		if lastOfPending >= 0 {
			c.localLines = append(c.localLines[:lastOfPending], c.localLines[lastOfPending+1:]...)
		}
	}

	c.pending = nil
	c.rebuildView()

	if msg.code == 410 {
		return nil // room ended; nothing further to promote
	}
	if n := len(c.outbox); n > 0 {
		next := c.outbox[0]
		c.outbox = c.outbox[1:]
		return c.dispatchInConv(next.conv, next.text)
	}
	return nil
}

// cycleRosterFocus moves the pink highlight through the sidebar roster
// (skipping me), wrapping at both ends. The highlighted peer is what empty
// Enter opens — the keyboard twin of clicking a row.
func (c *chatScreen) cycleRosterFocus(reverse bool) {
	online := orderedUsers(c.users, c.me, c.lastDMAt)
	if len(online) <= 1 { // only me in the room
		return
	}
	// Current position within the display order.
	cur := -1
	for i, u := range online {
		if u == c.hoverPeer {
			cur = i
			break
		}
	}
	step := 1
	if reverse {
		step = -1
	}
	for i := 0; i < len(online); i++ {
		cur += step
		if cur < 0 {
			cur = len(online) - 1
		}
		if cur >= len(online) {
			cur = 0
		}
		if online[cur] != c.me {
			c.hoverPeer = online[cur]
			return
		}
	}
}

// handleMouse translates a click into a sidebar selection using the SAME
// geometry View() will paint. Returns a tea.Cmd (send-free) or nil.
// peerAtY maps a terminal Y coordinate onto the sidebar's DISPLAY-ordered
// user list ("" when the point is outside any user row).
func (c chatScreen) peerAtY(y int, l layout) string {
	if c.width == 0 || c.height == 0 || !l.sidebarOn {
		return ""
	}
	row := y - l.rosterY0
	if row < 0 || row >= l.rosterSlots {
		return ""
	}
	online := orderedUsers(c.users, c.me, c.lastDMAt)
	if row >= len(online) {
		return ""
	}
	return online[row]
}

// handleMouse routes hover motion and clicks. Hovering paints exactly one
// pink row; clicking opens that peer's thread.
func (c *chatScreen) handleMouse(msg tea.MouseMsg) tea.Cmd {
	l := c.layoutFor()

	switch msg.Type {
	case tea.MouseMotion:
		c.hoverPeer = "" // default: outside every row
		if msg.X >= 0 && msg.X < c.width && msg.Y >= 0 && msg.Y < c.height {
			inColumn := msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth
			if inColumn && l.sidebarOn {
				c.hoverPeer = c.peerAtY(msg.Y, l)
			}
		}
		return nil

	case tea.MouseLeft:
		if c.width == 0 || c.height == 0 {
			return nil
		}
		// Reject coordinates outside the painted terminal area entirely.
		if msg.X < 0 || msg.X >= c.width || msg.Y < 0 || msg.Y >= c.height {
			return nil
		}
		if !l.sidebarOn {
			return nil // sidebar collapsed on narrow terminals
		}
		inColumn := msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth
		if !inColumn {
			return nil // clicks outside the sidebar never select
		}
		u := c.peerAtY(msg.Y, l)
		switch {
		case u == "", u == c.me:
			return nil // no row / clicking yourself is a no-op
		case u == c.targetUser:
			return nil // already chatting privately with them
		}
		c.enterPrivate(u)
	}
	return nil
}

func (c chatScreen) View() string {
	if c.width == 0 || c.height == 0 {
		return "connecting…"
	}
	l := c.layoutFor()

	// Keep viewport dims in lockstep with the painted layout.
	// Reserve 1 column for the scrollbar inside the transcript border.
	vp := c.vp
	scrollW := 1
	innerW := l.vpWidth
	if l.vpHeight > 0 && innerW > 10 {
		vp.Width = innerW - scrollW
	} else {
		vp.Width = innerW
		scrollW = 0
	}
	vp.Height = l.vpHeight

	var body string
	switch {
	case l.vpHeight == 0:
		// viewport.View() emits one padded blank row even at Height 0;
		// omitting the block keeps the exact-row contract intact.
		body = ""
	case l.boxedTranscript:
		if scrollW > 0 {
			bar := c.scrollbarView(l.vpHeight)
			// Viewport content + scrollbar joined, then bordered.
			inner := lipgloss.JoinHorizontal(lipgloss.Top, vp.View(), bar)
			body = tuiBorderStyle.Render(inner)
		} else {
			body = tuiBorderStyle.Render(vp.View()) // exactly vpHeight+2 rows
		}
	default:
		if scrollW > 0 {
			bar := c.scrollbarView(l.vpHeight)
			body = lipgloss.JoinHorizontal(lipgloss.Top, vp.View(), bar)
		} else {
			body = vp.View() // degraded: border dropped on tiny terminals
		}
	}

	if l.sidebarOn && body != "" {
		// Sidebar height forced to match the transcript column exactly; its
		// content truncates to l.rosterSlots so it can never inflate the row.
		sidebar := tuiRosterBoxStyle.
			Width(c.sidebarInnerWidth()).
			Height(l.vpHeight). // interior rows; border completes the column
			MaxHeight(l.vpHeight).
			Render(c.rosterBody(l.vpHeight))
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, " ", sidebar)
	}

	rows := make([]string, 0, 5)
	if l.showHeader {
		rows = append(rows, c.headerView())
	}
	if body != "" {
		rows = append(rows, body)
	}
	var input string
	switch {
	case l.composerRows > 0:
		// Significant writing area: accent-bordered box with inner padding.
		input = tuiComposerStyle.
			Width(l.vpWidth). // border completes alignment with transcript
			Height(l.composerRows).
			Render(c.input.View())
	case l.inputBoxed:
		input = tuiBorderStyle.Render(c.input.View())
	default:
		input = "❯ " + c.input.View()
	}
	// OpenCode-style pop-out: with a leading "/" the command drawer emerges
	// upward out of the composer; in /upload|/download mode the same slot
	// paints a browser instead. A blank spacer row sells the "lifted off the
	// input" look. When the layout budget DISSOLVED the drawer (absurdly tiny
	// terminals), paletteRows==0 wins over visibility — never paint unbudgeted.
	if l.paletteRows > 0 {
		if pal := c.drawerView(maxInt(l.vpWidth+2, 0)); pal != "" {
			rows = append(rows, "", pal)
		}
	}
	rows = append(rows, input)
	if l.statusRows == 1 {
		rows = append(rows, c.statusView())
	}
	out := strings.Join(rows, "\n")
	if l.frameOn {
		out = tuiBorderStyle.Render(out) // full-screen app shell
	}
	return out
}

// runChatTUI is the default interactive experience (alt-screen + mouse).
func runChatTUI(serverURL, key, me string, persist bool) {
	scr := newChatScreen(serverURL, key, me)
	scr.persist = persist
	p := tea.NewProgram(scr, tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	if persist {
		_ = saveHistory(key, scr.history)
	}
	scr.client.leave()
	fmt.Printf("\nYou left session %s.\n", key)
}
