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
)

// ---- layout constants --------------------------------------------------------

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
	palette      paletteState // "/" command drawer above the composer
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

// localLine is a UI-generated transcript row scoped to one conversation so
// mode switches never bleed it across views.
type localLine struct {
	conv string
	text string
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

func newChatScreen(serverURL, key, me string) chatScreen {
	ti := textinput.New()
	ti.Placeholder = "Type a message…  ·  / for commands"
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
	ts := tuiTimeStyle.Render("[--:--]")
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		ts = tuiTimeStyle.Render("[" + t.Local().Format("15:04") + "]")
	}
	if m.Kind == "system" {
		if isOwnPresence(m.Text, c.me) {
			return "" // never announce my own join/leave to me
		}
		return ts + " " + tuiSystemStyle.Render("* "+m.Text)
	}
	name := tuiNameStyle.Render(m.Username)
	if m.Username == c.me {
		name = tuiMeStyle.Render(name + " (you)")
	}
	line := ts + " " + name + ": " + m.Text
	if m.ConvID != generalConv && m.Username == c.me && c.targetUser != "" {
		line += tuiTimeStyle.Render("  → " + c.targetUser)
	}
	return line
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
	c.localLines = append(c.localLines, localLine{conv: conv, text: text})
	c.rebuildView()
}

// rebuildView derives the painted transcript from raw history + local lines,
// applying the CURRENT visibility filter. Entering/leaving private mode just
// calls this — historical lines re-filter retroactively.
func (c *chatScreen) rebuildView() {
	c.lines = c.lines[:0]
	for _, m := range c.history {
		if !c.shouldRender(m) {
			continue
		}
		c.lines = append(c.lines, c.renderLine(m))
	}
	for _, ll := range c.localLines {
		if ll.conv == c.activeConv() {
			c.lines = append(c.lines, ll.text)
		}
	}
	if c.pending != nil && c.pending.conv == c.activeConv() && len(c.lines) > 0 {
		c.pending.lineIdx = len(c.lines) - 1 // pending echo is last local of its conv
	}
	c.refreshViewport()
}

// refreshViewport re-serializes the transcript, hard-wrapping every line to
// the current viewport width. lipgloss Width() wraps ANSI-aware, so styled
// lines fold instead of being clipped by the viewport on narrow terminals.
func (c *chatScreen) refreshViewport() {
	w := c.vp.Width
	if w <= 0 {
		w = 40
	}
	st := lipgloss.NewStyle().Width(w)
	wrapped := make([]string, len(c.lines))
	for i, ln := range c.lines {
		wrapped[i] = st.Render(ln)
	}
	c.vp.SetContent(strings.Join(wrapped, "\n"))
	c.vp.GotoBottom()
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
	return func() tea.Msg {
		code, msg, err := client.sendMessage(text, to)
		return sendDoneMsg{text: text, to: msg.To, seq: msg.Seq, code: code, err: err}
	}
}

func (c chatScreen) doLeave() tea.Cmd {
	client := c.client
	return func() tea.Msg {
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
	return tea.Batch(c.fetchBacklogCmd(), c.doBeat(), schedulePoll(0), scheduleBeat())
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
	echo := tuiMeStyle.Render("[you →] " + text)
	if c.targetUser != "" {
		echo = tuiMeStyle.Render("[you → " + c.targetUser + "] " + text)
	}
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
		c.vp.Width = l.vpWidth
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
		cmds = append(cmds, c.doPoll())

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
		cmds = append(cmds, scheduleBeat())

	case sendDoneMsg:
		if nc := c.settleSend(msg); nc != nil {
			cmds = append(cmds, nc)
		}

	case convBacklogMsg:
		if msg.err == nil {
			for _, m := range msg.msgs {
				c.addMessage(m) // seq-deduped; pre-window history lands here
			}
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
		if msg.Type == tea.KeyEnter {
			text := strings.TrimSpace(c.input.Value())
			c.input.SetValue("")
			if text == "" {
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
	vp := c.vp
	vp.Width = l.vpWidth
	vp.Height = l.vpHeight

	var body string
	switch {
	case l.vpHeight == 0:
		// viewport.View() emits one padded blank row even at Height 0;
		// omitting the block keeps the exact-row contract intact.
		body = ""
	case l.boxedTranscript:
		body = tuiBorderStyle.Render(vp.View()) // exactly vpHeight+2 rows
	default:
		body = vp.View() // degraded: border dropped on tiny terminals
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
	// upward out of the composer. A blank spacer row sells the "lifted off
	// the input" look; it is transient (only while the query starts with "/").
	if pal := c.paletteView(maxInt(l.vpWidth+2, 0)); pal != "" {
		rows = append(rows, "", pal)
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
func runChatTUI(serverURL, key, me string) {
	scr := newChatScreen(serverURL, key, me)
	p := tea.NewProgram(scr, tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	scr.client.leave()
	fmt.Printf("\nYou left session %s.\n", key)
}
