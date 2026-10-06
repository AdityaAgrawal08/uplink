package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	bkeys "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ---- styling ---------------------------------------------------------------
//
// Shared primitives only: the design language itself lives in chat_theme.go.
// Every colour below is adaptive so the shell reads on light and dark
// terminals alike.

var (
	tuiSystemStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	tuiNameStyle   = lipgloss.NewStyle().Foreground(colAccent)
	tuiMeStyle     = lipgloss.NewStyle().Foreground(colAmber)
	tuiErrStyle    = lipgloss.NewStyle().Foreground(colRed)
	tuiDimStyle    = lipgloss.NewStyle().Foreground(colDim)

	tuiUnreadStyle = lipgloss.NewStyle().
			Foreground(colBadgeFg).
			Background(colBadgeBg)

	tuiScrollbarStyle = lipgloss.NewStyle().
				Foreground(colEdge)
	tuiScrollbarThumbStyle = lipgloss.NewStyle().
				Foreground(colAccent).
				Background(colEdge)
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
	rosterTotalWidth = 24 // DEFAULT sidebar column width (density scales it)
	rosterWidthInner = rosterTotalWidth - 2
	rosterMaxVisible = 16 // cap on chat rows before the list scrolls
	headerHeight     = 1  // top banner line
	frameChrome      = 2  // reserved shell margin (the chat shell is edge-to-edge)
	transcriptBorder = 2  // columns reserved in the main column: focus gutter + scroll rail
	minSidebarTermW  = 66 // below this width the sidebar collapses entirely
)

// sidebarWidthFor picks a reading column that grows FLUIDLY with the
// terminal: ~1 extra cell per 10 terminal columns, clamped to a usable
// chat list (narrow enough to leave the transcript room, wide enough for
// presence + name + time + preview). Every resize step visibly rebalances.
func sidebarWidthFor(termW int) int {
	w := 22 + (termW-minSidebarTermW)/10
	if w < 22 {
		w = 22
	}
	if w > 38 {
		w = 38
	}
	return w
}

// composerRowsFor keeps the message box to a single line whenever it is
// boxed (bare prompt when every row counts). The reference input is one
// line; long text scrolls inside the field, rows belong to the transcript.
func composerRowsFor(termH int) int {
	if termH >= 12 {
		return 1
	}
	return 0
}

// layout is the single source of truth for frame geometry. Both View() and the
// mouse hit-test derive their math from this struct so a click always maps to
// exactly what is on screen.
//
// Height invariant (the contract that keeps us inside the terminal):
//
//	headerHeight + headRows + vpHeight + paletteRows + composerRows(+border)
//	    + hintRows + statusRows
//	    == termH   (exactly; never more)
type layout struct {
	vpWidth      int  // transcript content width (inside the gutter + rail)
	vpHeight     int  // transcript visible rows
	sidebarOn    bool // false on narrow terminals — the rail collapses
	rosterX      int  // leftmost column of the sidebar (LEFT column)
	rosterY0     int  // first terminal row of the sidebar conversation list
	headRows     int  // chat-header rows above the transcript (0/1/2 by space)
	hintRows     int  // composer key-hints row under the composer (0 when collapsed)
	statusRows   int  // extra rows consumed by the status line (0 or 1)
	paletteRows  int  // rows reserved for the "/" drawer incl. its spacer (0 = closed)
	quoteRows    int  // rows the pinned reply citation consumes above the composer (0/1)
	showHeader   bool // staged degradation: hide banner on tiny heights
	sidebarWidth int  // density-scaled conversation column
	composerRows int  // writable rows inside the composer box (1/0)
	frameOn      bool // reserved shell margin; the chat shell is edge-to-edge
}

// totalRows reports the exact number of terminal rows a frame will occupy.
func (l layout) totalRows() int {
	h := l.vpHeight + l.statusRows + l.paletteRows + l.headRows + l.hintRows + l.quoteRows
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
//     the drawer itself is sacrificed last)
//  2. sidebar collapses below minSidebarTermW / tiny content, else its width
//     scales with terminal width for comfortable reading
//  3. composer keeps a writable box on roomy terminals, shrinking to a
//     bare prompt on absurd heights
//  4. viewport absorbs all remaining space (floors at zero rows)
//
// Pure function => trivially unit-testable.
func computeLayoutWithPalette(termW, termH int, showStatus bool, paletteRows int) layout {
	return computeLayoutMedia(termW, termH, showStatus, paletteRows, 0)
}

// computeLayoutMedia is THE pure geometry pass: terminal size + status +
// drawer budget + pinned-quote budget in, every pane's rectangle out.
func computeLayoutMedia(termW, termH int, showStatus bool, paletteRows, quoteRows int) layout {
	var l layout
	if termW <= 0 || termH <= 0 {
		return l
	}
	l.statusRows = 0
	if showStatus {
		l.statusRows = 1
	}
	l.paletteRows = paletteRows
	l.quoteRows = quoteRows
	l.showHeader = true
	// The chat shell is EDGE-TO-EDGE: no outer frame, no margin — every row
	// and column belongs to content. frameOn/frameChrome stay in the layout
	// contract (the invariant sweep reads them) but the shell never claims
	// the reserved margin.
	l.frameOn = false
	l.composerRows = composerRowsFor(termH)
	l.sidebarWidth = sidebarWidthFor(termW)

	innerW := widthInsideFrame(termW, l.frameOn)

	// --- width pass ---------------------------------------------------------
	// The sidebar is the LEFT column (conversation list); the transcript
	// paints to its right. rosterX is therefore the left inset, not the
	// right edge.
	l.sidebarOn = termW >= minSidebarTermW
	if l.sidebarOn {
		l.rosterX = 0
		if l.frameOn {
			l.rosterX++ // shift past the left frame margin
		}
		l.vpWidth = innerW - l.sidebarWidth - 1 - transcriptBorder
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
		case l.paletteRows > 0:
			l.paletteRows = 0 // absurdly tiny terminal: dissolve the drawer
		case l.quoteRows > 0:
			l.quoteRows = 0 // the pinned quote card goes before the banner
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

	if l.vpHeight < 3 && l.sidebarOn {
		l.sidebarOn = false // no room for header + even one conversation row
		l.rosterX = 0
		l.vpWidth = widthInsideFrame(termW, l.frameOn) - transcriptBorder
		if l.vpWidth < 10 {
			l.vpWidth = 10
		}
	}

	// Sidebar list starts below the app banner and its own header block:
	// [banner] + [filter row + hairline].
	l.rosterY0 = 0
	if l.showHeader {
		l.rosterY0 += headerHeight
	}
	if l.sidebarOn {
		l.rosterY0 += searchHeightFor(0, l.sidebarWidth)
	}
	return l
}

// widthInsideFrame is the usable content width before the main column's
// gutter + rail: the full terminal for the edge-to-edge chat shell, minus
// the reserved shell margin should one ever be turned on.
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

// tryEnqueue inserts into the UI event queue without blocking: true when
// the main loop will see the message. Chat acks ride on acceptance — an
// acked-but-dropped message would graduate the sender's backstop and
// vanish forever.
func tryEnqueue(ch chan tea.Msg, m tea.Msg) bool {
	select {
	case ch <- m:
		return true
	default:
		return false
	}
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

// The engine pushes network events from its own goroutines; they arrive here
// through netCh (see drainNetCmd) because bubbletea Update must stay on the
// main loop. Buffer is generous; a dropped paint is only a missed row when
// the sender still retries — so chat acks ride on successful enqueue (see
// onChat), never before it. P2P is otherwise reliably delivered.
type netChatMsg struct {
	key  string // session code that produced this event ("" = pre-groups/legacy: treat as active)
	chat engineChat
}
type netFileMsg struct {
	key  string
	file engineFile
}
type netFileErrMsg struct {
	key                 string
	msgId, from, reason string
}
type netReadyMsg struct {
	key, user, code string
}
type netLostMsg struct {
	key, user string
}
type netErrMsg struct {
	key string
	err error
}
type rosterTickMsg struct{}

// netRosterMsg arrives when the engine's beat learns membership moved
// (join/leave): refresh the sidebar now instead of waiting for the tick.
type netRosterMsg struct{}

// ─── quote-reply state ─────────────────────────────────────────────────────
//
// Three cooperating surfaces: the /reply selection pointer (keyboard), the
// floating right-click menu (mouse + keyboard), and the pinned composer
// quote card (the one that travels on the wire). All are transient UI
// state; only the chat frame's optional JSON fields are protocol.

// replyPickState is the /reply selection mode. target is the pointed
// message; anim is the painted pointer frame (pulse/slide-in reveal mirror
// of the reaction dropdown); gen fences ticks from a superseded session;
// row is the pointer's first viewport content row (refreshViewport).
type replyPickState struct {
	target string
	anim   int
	gen    int
	row    int
}

// replyPickAnimFrames is the number of painted stages in the pointer's
// slide-in reveal: 0 is the "still arriving" state, the last frame is the
// settled "<" beside the message. replyPickAnimStep spaces the tea.Tick
// frames ~50ms apart, mirroring the reaction dropdown's reveal.
const (
	replyPickAnimFrames = 4
	replyPickAnimStep   = 50 * time.Millisecond
)

// replyPickAnimMsg advances the pointer to painted stage frame. gen fences
// out ticks from a superseded open (dismiss, reopen, a second target).
type replyPickAnimMsg struct{ gen, frame int }

// scheduleReplyPickAnim arms the next pointer reveal stage for generation gen.
func scheduleReplyPickAnim(gen, frame int) tea.Cmd {
	return tea.Tick(replyPickAnimStep, func(time.Time) tea.Msg {
		return replyPickAnimMsg{gen: gen, frame: frame}
	})
}

// replyMenuState is the floating right-click menu. items is the scoped
// item list ("Reply" always; "Reply-Privately" on others' messages), sel
// the highlighted row, x/y the overlay origin in terminal coordinates and
// w/h its size. Both geometry and items are fixed at open time so paint,
// keyboard selection and click hit-testing can never drift.
type replyMenuState struct {
	msgId string
	items []string
	sel   int
	x, y  int
	w, h  int
}

// quoteJumpState is an in-flight quote-jump highlight: the quoted message's
// whole row paints blue until left ticks expire. gen fences stale expiry
// ticks so a superseded jump can never shorten a newer one.
type quoteJumpState struct {
	msgId string
	gen   int
	left  int
}

// quoteJumpTicks is the highlight lifetime in seconds; quoteJumpStep spaces
// the expiry ticks.
const (
	quoteJumpTicks = 3
	quoteJumpStep  = time.Second
)

// quoteJumpTickMsg expires one highlight second (or clears the highlight on
// the final tick).
type quoteJumpTickMsg struct{ gen int }

// scheduleQuoteJumpTick arms the next highlight expiry second for generation gen.
func scheduleQuoteJumpTick(gen int) tea.Cmd {
	return tea.Tick(quoteJumpStep, func(time.Time) tea.Msg {
		return quoteJumpTickMsg{gen: gen}
	})
}

// kickDoneMsg arrives when a /kick request completes.
type kickDoneMsg struct {
	target string
	roster []rosterMember
	epoch  int64
	err    error
}

// roleDoneMsg arrives when a /admin or /unadmin request completes.
type roleDoneMsg struct {
	target string
	admin  bool // true = grant, false = revoke
	roster []rosterMember
	epoch  int64
	err    error
}

// syncRosterFromEngine refreshes the sidebar from the engine's heartbeat
// roster: membership, call publish scope, unread/hover pruning, and an
// immediate repaint on ANY change. Shared by the 2s render tick and the
// engine's roster-changed push.
//
// The sidebar ALWAYS renders the HOME session's membership — General,
// every main-room user, then every joined group — in every conversation
// view. While a group view is open c.eng is the GROUP's engine, whose peer
// set is a different room's membership: feeding it to the sidebar would
// hide main-room users (and prune their unread/recency) behind the group
// view, so the roster derives from c.homeEng whenever one exists.
func (c *chatScreen) syncRosterFromEngine() {
	if c.eng == nil {
		return // bare/test screens carry no engine
	}
	eng := c.eng
	if c.homeEng != nil {
		eng = c.homeEng
	}
	roster := eng.peers()
	users := onlineNames(roster, c.me)
	changed := len(users) != len(c.users)
	if !changed {
		for i := range users {
			if users[i] != c.users[i] {
				changed = true
				break
			}
		}
	}
	c.users = users
	live := map[string]bool{c.me: true}
	for _, u := range users {
		live[u] = true
	}
	// Publish catch-up: late joiners get our announce; leavers get
	// pruned (streams with an emptied scope stop themselves).
	if c.call != nil {
		room := users
		if c.targetUser != "" {
			room = []string{c.targetUser}
		}
		c.call.PublishTo(room)
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
	// Group hover targets survive roster syncs (groups are not in the
	// member roster). The live roster above is always the HOME room's
	// membership, so main-room hover pruning stays correct in every view;
	// a group view only skips it because group rows are not home members.
	if c.activeGroup == "" && c.hoverPeer != "" && groupPeerCode(c.hoverPeer) == "" && !live[c.hoverPeer] {
		c.hoverPeer = ""
	}
	if c.hoverPeer != "" && groupPeerCode(c.hoverPeer) != "" {
		// A tombstone row is still a real (explainable) row; only a peer
		// that vanished from both the live set and the tombstones drops
		// the cursor.
		code := groupPeerCode(c.hoverPeer)
		if c.groups[code] == nil {
			if _, dead := c.tombstones[code]; !dead {
				c.hoverPeer = ""
			}
		}
	}
	if changed {
		c.rebuildView() // repaint the sidebar NOW, not on the next message
	}
}

// unconfirmedAfter is how long an own message may sit without a delivery
// ack before the status line warns. Churn windows (re-handshake + 3 inbox
// retries) legitimately take ~10-15s; past 30s something is wrong enough
// to say so out loud instead of showing false confidence.
const unconfirmedAfter = 30 * time.Second

// receiptExpiry forgets an unacked own message (dimming lifts). Past this
// point the engine's retries are over; a still-missing ack is far likelier
// a lost ack frame than a lost message, so nagging forever would cry wolf.
const receiptExpiry = 5 * time.Minute

// netDeliveredMsg arrives when the peer acked one of our messages.
type netDeliveredMsg struct {
	key   string
	msgId string
}

// netReactionMsg carries one inbound reaction nudge. It never mutates local
// counts directly: the nudge schedules an immediate GET /reactions, keeping
// the server the single source of truth (and correctly handling replace and
// remove, which a delta-only frame cannot express without per-sender state).
type netReactionMsg struct {
	key      string
	reaction engineReaction
}

// reactionsFetchedMsg carries one GET /reactions result: the ids that were
// asked for plus the server's summaries. The requested scope is replaced
// wholesale, so messages whose reactions vanished are cleared too.
type reactionsFetchedMsg struct {
	ids       []string
	summaries []reactionSummary
	err       error
}

// reactionDoneMsg resolves one POST /reactions. On failure the optimistic
// toggle is rolled back to its captured prior state.
type reactionDoneMsg struct {
	msgId, emoji, prev string
	err                error
}

// Media UI messages: publish status lines + VU level.
type mediaInfoMsg struct{ info string }
type callLevelMsg struct{ level float64 }

// netIdleMsg keeps the drain pump alive: drainNetCmd always leads to either
// a network event or one of these, and both handlers re-arm the pump, so
// exactly one pump goroutine exists at all times.
type netIdleMsg struct{}

type sendDoneMsg struct {
	text  string
	to    string // empty for broadcasts
	seq   int    // local display sequence (server keeps no transcript)
	msgId string // E2E message identity (ACKs/dedup)
	code  int
	err   error
	quote chatQuote // citation this send carried (all-empty = plain)
	conv  string    // conversation bucket the send was dispatched into
}
type leaveDoneMsg struct{}
type groupLeftMsg struct{ code string }

// groupRestoreDoneMsg resolves one startup re-join attempt for a group row
// painted optimistically from the local store. nil error = the session is
// live again (the engine starts); a terminal failure drops the row and
// prunes the store.
type groupRestoreDoneMsg struct {
	code string
	err  error
}

// ---- model -----------------------------------------------------------------

type chatScreen struct {
	sig        *signalClient
	eng        *engine
	key        string
	me         string
	width      int
	height     int
	history    []chatMessage        // live transcript (deduped by local seq)
	localLines []localLine          // echoes & notes
	netCh      chan tea.Msg         // engine -> Update bridge (non-blocking)
	nextSeq    int                  // local display sequence counter
	hoverPeer  string               // conversation row under the mouse / keyboard cursor ("" = none)
	focus      focusPane            // which pane owns the keyboard (zero = composer)
	sideFilter string               // inline rail filter ("" = show everything)
	unread     map[string]int       // peer -> unread DM count (cleared on open)
	lastDMAt   map[string]time.Time // peer -> newest incoming DM (recency sort)
	lines      []string             // DERIVED paint buffer: rebuildView() owns it
	rendered   map[int]bool         // local seqs already rendered
	// seenMsg dedups inbound chat/file frames by msgId (consumer-side
	// exactly-once). Every received copy is still acked — only the first
	// paints. Bounded via seenSet; lazily created so every constructor
	// (including tests) is safe without explicit init.
	seenMsg *seenSet
	// Render caches (perf): renderCache memoizes per-message bubbles,
	// tsCache memoizes parsed timestamps, wrapCache memoizes width-wrapped
	// lines by content. All three are keyed independent of position and are
	// dropped wholesale whenever vp.Width changes (the only input besides
	// message content). Without them every new message re-renders and
	// re-measures the entire transcript (grapheme segmentation dominates
	// profiles) — O(n) per message, ~90ms at 5000 lines.
	renderCache map[int]string
	hdrCache    map[int]string // sender-group headers (see renderedSender)
	tsCache     map[int]time.Time
	wrapCache   map[string]string
	cacheWidth  int
	cacheAdapt  int          // density generation for the render caches (see cacheForWidth)
	pending     *pendingSend // single in-flight send (nil = idle)
	outbox      []queuedLine // queued sends waiting for the in-flight one
	// unackedUI tracks own confirmed sends awaiting a delivery ack
	// (msgId -> send time). Own bubbles render dimmed until the ack lands;
	// entries older than unconfirmedAfter raise the status warning below.
	// The engine owns retry/expiry — this map is display state only.
	unackedUI map[string]time.Time
	// Message reactions (cosmetic, server-truth). reactionCounts holds
	// per-message emoji tallies, myReactions the one emoji I picked per
	// message ("" = none). Both are keyed by MsgId alone — ids are globally
	// unique, so general and DM spaces can never collide — and are replaced
	// wholesale from GET /reactions every 2s; local toggles are optimistic
	// until the next fetch. pendingReactionMsgId is the message whose
	// anchored picker row is open directly above it ("" = closed).
	reactionCounts       map[string]map[string]int
	myReactions          map[string]string
	pendingReactionMsgId string
	// reactionDetails is the optional per-reactor breakdown from the same
	// fetch (server field may be absent: fall back to counts+mine). detailMsgId
	// plus detailEmoji identify the open reactor dropdown card — an extra
	// block directly BELOW its message. Only one aux row is ever open: the
	// picker and the dropdown are mutually exclusive.
	reactionDetails map[string][]reactionDetail
	detailMsgId     string
	detailEmoji     string
	// detailAnim is the painted stage of the open dropdown's reveal (0 = bare
	// borders .. reactionDetailAnimFrames-1 = fully expanded); detailAnimGen
	// fences out reveal ticks from a superseded open (dismiss, reopen, a
	// second chip). Both are ephemeral paint state, never server truth.
	detailAnim    int
	detailAnimGen int
	// animations enables transient motion (the dropdown's staged reveal).
	// Plain/CI runs construct with it off (UPLINK_CHAT_PLAIN=1): the card
	// paints fully expanded, identical to the last frame, with no timers.
	animations bool
	// The anchored reaction picker is an extra transcript row directly above
	// its target message, so both indexes live with the paint: reactionLineIdx
	// is its entry in lines (rebuildView), reactionRow its first viewport
	// content row (refreshViewport). Together they keep the hit-test glued to
	// the picker after any rebuild or scroll. -1 = not painted. The detail bar
	// gets the same treatment (detailLineIdx/detailRow/detailRowH) since it is
	// a multi-row control block, never a message hit.
	reactionLineIdx int
	reactionRow     int
	detailLineIdx   int
	detailN         int // lines[] entries the open detail bar occupies
	detailRow       int // first viewport content row (-1 = not painted)
	detailRowH      int // content rows the detail block spans
	// lineMsg is parallel to lines: the MsgId each transcript entry belongs
	// to ("" for cards/system/pinned rows). refreshViewport expands it into
	// rowMsg (viewport content row -> MsgId) so a click maps to a message.
	lineMsg []string
	rowMsg  []string
	// lineCard is parallel to lines: the row offset INSIDE the entry where
	// its quote-reply citation card starts (-1 = no card). The card is the
	// line right after the group header when one paints, else the entry's
	// first line. refreshViewport expands it into rowCard (content row ->
	// card row), so a click on the card jumps to the quoted message instead
	// of opening the reaction picker.
	lineCard []int
	rowCard  []bool
	// roomUnread counts room broadcasts that arrived while a DM thread is
	// in view (broadcasts otherwise paint nowhere and badge nothing — a
	// message can sit in history looking "missing"). Cleared on return to
	// the room. DM unreads keep using the per-peer map.
	roomUnread int
	users      []string
	// composerQuote is the pinned WhatsApp-style reply citation above the
	// composer (nil = none). The X at the card's right edge clears it; the
	// next send attaches it to the chat frame. Reply-Privately carries it
	// into the DM composer untouched.
	composerQuote *chatQuote
	// replyPick is the /reply selection mode: an animated "<" pointer
	// beside the targeted message (nil = closed). Enter on a message opens
	// the floating reply menu; Esc/typing/sends/mode switches exit.
	// replyPick.anim is the painted pointer frame (see replyPickAnimFrames);
	// replyPick.gen fences ticks from a superseded session. replyPick.row
	// is the pointer's first viewport content row (refreshViewport).
	replyPick *replyPickState
	// replyPickLineIdx is the pointer's entry in lines (rebuildView), -1
	// when no pointer paints.
	replyPickLineIdx int
	// replyMenu is the floating right-click menu (nil = closed): a small
	// boxed panel near the click offering Reply / Reply-Privately. Esc,
	// click-elsewhere, and typing dismiss it.
	replyMenu *replyMenuState
	// quoteJump is an in-flight quote-jump highlight: the quoted message's
	// ENTIRE row paints blue until quoteJumpTicks ticks expire (nil =
	// nothing highlighted). gen fences stale expiry ticks.
	quoteJump *quoteJumpState
	// replyPickCounter / quoteJumpCounter are monotonic generation
	// counters: they make tick fencing survive state teardown and reopen
	// (a fresh session can never collide with a stale timer's gen).
	replyPickCounter int
	quoteJumpCounter int
	// call owns the media lifecycle (publish/subscribe; nil-safe).
	call      *mediaManager
	callLevel float64        // mic loudness for the status meter
	callStart time.Time      // latched while a call is live (timer source)
	rosterVp  viewport.Model // scrollable users list (wheel + scrollbar)
	vp        viewport.Model
	drag      barDrag // scrollbar drag state (any of the three panes)
	input     textinput.Model
	palette   paletteState // "/" command drawer above the composer
	mention   paletteState // "@" member dropdown (general room only; the palette's mirror)
	picker    pickerState  // file-browser mode of that drawer (/upload)
	// frec is the picker/palette usage history behind the ranking's
	// frecency tiebreak (name-keyed; nil = no history yet).
	frec map[string]frecEntry
	// drawerHoverLock is the mouse-parity hold after a filter change: the
	// next motion event is ignored so a stale hover cannot fight the fresh
	// ranking (inputMode tracking lives in mouseActive).
	drawerHoverLock int
	// mouseActive tracks the last input class (true = mouse): hover
	// follows rows only while the mouse is actually in play.
	mouseActive  bool
	uploadBuf    []string        // persistent upload buffer (survives picker close)
	uploadBufSet map[string]bool // set view of uploadBuf for O(1) lookups
	uploadQ      uploadState     // sequential session-file transfer queue
	received     []receivedFile  // files arrived this session (for /download)
	status       string
	targetUser   string // private-chat peer; "" = general room
	// ── groups ────────────────────────────────────────────────────────────
	// A group is a named session (code) joined via invite or created with
	// /new-group. Every joined group keeps its own signal client + engine
	// running in the background so sidebar unread/previews stay fresh; the
	// ACTIVE session's client/engine live in sig/eng (the constructor's are
	// the home room, preserved in homeSig/homeEng so switching back is a
	// pointer swap). Group messages travel with key = group code and land
	// in history under the "group:<code>" conversation bucket, so the whole
	// transcript/reaction/reply pipeline works unchanged.
	groups         map[string]*groupSession // code -> joined group
	activeGroup    string                   // open group code ("" = home room)
	lastGroupAt    map[string]time.Time     // code -> newest inbound message (recency sort)
	// tombstones are session-only placeholder rows for groups this client is
	// no longer a member of (left/kicked/dissolved). Never persisted.
	tombstones map[string]groupTombstone
	// groupsPath is the local membership store (~/.uplink/groups.json; env
	// override UPLINK_GROUPS_FILE). Only runChatTUI wires it — bare/test
	// screens leave it empty so no test ever touches a developer's HOME.
	groupsPath    string
	homeKey       string // the constructor's session code (the main room)
	homeSig        *signalClient            // home session client (survives group switches)
	homeEng        *engine                  // home session engine (kept running in background)
	id             *identityKey             // device identity (group engines reuse it)
	pendingInvites int                      // badge: invite rows the server currently holds
	seenInvites    map[string]bool          // invite codes already beeep'd (one ping per invite)
	leftSent       *atomic.Bool             // per-screen leave guard (pointer: screen is copied by value)
	drainTimer     *time.Timer              // reused pump timer (no time.After alloc per cycle)
}

// focusPane names the one component that owns keyboard focus. Exactly one
// pane is focused at a time and exactly one visual indicator marks it: an
// accent bar on the conversation rail, an accent gutter on the transcript,
// or the composer's accent border. There is no focus manager in bubbles v1,
// so this enum + key.Matches IS the routing table.
type focusPane int

const (
	// focusComposer is the resting focus: the app opens ready to type.
	focusComposer focusPane = iota
	focusSidebar
	focusTranscript
)

// next/prev walk the ring sidebar → transcript → composer → sidebar.
func (f focusPane) next() focusPane {
	switch f {
	case focusComposer:
		return focusSidebar
	case focusSidebar:
		return focusTranscript
	default:
		return focusComposer
	}
}

func (f focusPane) prev() focusPane {
	switch f {
	case focusComposer:
		return focusTranscript
	case focusTranscript:
		return focusSidebar
	default:
		return focusComposer
	}
}

// scrollSection identifies one independently scrollable pane.
type scrollSection int

const (
	secChat scrollSection = iota
	secRoster
)

// barGeom is one scrollbar's track geometry in terminal coordinates
// (paint-verified: see TestScrollbarDragGeometry). x is the scrollbar
// column, trackY0 the first track row (below the up-arrow), trackH the
// draggable track length.
type barGeom struct {
	x, trackY0, trackH int
	thumbTop, thumbH   int
}

// barDrag is an in-progress scrollbar drag.
type barDrag struct {
	active  bool
	sec     scrollSection
	grabOff int // msg.Y - (trackY0 + thumbTop) at grab time
}

// pendingSend tracks the optimistic echo line for the in-flight send so the
// confirmation can swap it in place (or mark failure) without ambiguity.
type pendingSend struct {
	lineIdx int
	// localIdx pins the optimistic echo row in localLines (set once at
	// dispatch). Settle removes/annotates exactly this row instead of
	// scanning for "last of conv", which breaks when other lines land
	// while a send is in flight.
	localIdx int
	text     string
	conv     string // conversation the optimistic echo belongs to
	to       string // recipient ("": broadcast) - needed to reconstruct on settle
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
// the open group's bucket, "general" in the common room, the canonical
// pair key inside a thread.
func (c *chatScreen) activeConv() string {
	if c.activeGroup != "" {
		return groupConv(c.activeGroup)
	}
	if c.targetUser == "" {
		return generalConv
	}
	return conversationKey(c.me, c.targetUser)
}

// composerPlaceholder doubles as the restore text whenever a drawer closes.
const composerPlaceholder = "Type a message…  ·  / commands  ·  tab picks a peer"

// receivedFile is one file that arrived this session (P2P or fallback).
// There is no server-side file index anymore; /download lists these.
type receivedFile struct {
	filename string
	from     string
	size     int64
	path     string
	at       time.Time
}

// maxReceivedFiles bounds the /download drawer source in marathon sessions
// (oldest fall off; the files themselves stay saved on disk).
const maxReceivedFiles = 200

// maxHistory bounds the in-memory transcript for the same reason (oldest
// scrollback falls off first).
const maxHistory = 5000

// maxLocalLines bounds transient echo/note/card rows likewise. Echoes and
// progress rows are removed in place; error/help rows would otherwise grow
// without limit. Dropped file cards stay reachable in the files drawer.
const maxLocalLines = 500

// maxOutbox bounds queued unsent lines while a send is in flight.
const maxOutbox = 100

func newChatScreen(serverURL, key, me string, id *identityKey, password string) chatScreen {
	// fresh screen, fresh leave guard (no global reset needed)
	ti := textinput.New()
	ti.Placeholder = composerPlaceholder
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "❯ "
	ti.Width = 36
	vp := viewport.New(80, 20)
	// The composer owns text; the transcript owns arrows and paging. Left
	// with the library defaults, typing "j"/"k"/"f"/space would scroll the
	// transcript while the user is writing — so the viewport's vi/letter
	// bindings are dropped and only the dedicated scroll keys remain.
	vp.KeyMap = viewport.KeyMap{
		Up:       bkeys.NewBinding(bkeys.WithKeys("up")),
		Down:     bkeys.NewBinding(bkeys.WithKeys("down")),
		PageUp:   bkeys.NewBinding(bkeys.WithKeys("pgup")),
		PageDown: bkeys.NewBinding(bkeys.WithKeys("pgdown")),
	}
	netCh := make(chan tea.Msg, 256)
	sig := &signalClient{serverURL: serverURL, key: key, me: me, id: id}
	// Engine callbacks only ever push into netCh (never touch the screen:
	// they run on network goroutines). The drain command below feeds them
	// into Update on the main loop. Every event is tagged with the session
	// code that produced it so Update can route background groups.
	push := func(m tea.Msg) {
		// Control/notice messages (ready/lost/error/file) must not be
		// silently dropped under burst while chat backpressures: retry
		// once synchronously before falling back to drop.
		if _, ok := m.(netChatMsg); !ok {
			if tryEnqueue(netCh, m) {
				return
			}
			select {
			case netCh <- m:
			default:
				// Still full: drop with a diagnostic on the next drain.
			}
			return
		}
		tryEnqueue(netCh, m)
	}
	var eng *engine
	callMgr := newMediaManager(me, id,
		func(to, noteType, payload string) error { return sig.signalSend(to, noteType, payload) },
		nil, // roster bound below once eng exists
		mediaUICallbacks{
			onInfo:  func(info string) { push(mediaInfoMsg{info: info}) },
			onLevel: func(level float64) { push(callLevelMsg{level: level}) },
		})
	eng = newEngine(me, id, sig, engineCallbacks{
		onChat: func(c engineChat) {
			// Ack only what the queue accepted: a dropped slot stays
			// unacked so the sender's retry redelivers it (paint dedups
			// via seenMsg — at-least-once in, exactly-once shown).
			if tryEnqueue(netCh, netChatMsg{key: key, chat: c}) {
				_ = eng.sendAck(c.From, c.MsgId)
			}
		},
		onFile: func(f engineFile) { push(netFileMsg{key: key, file: f}) },
		onFileErr: func(msgId, from, reason string) {
			push(netFileErrMsg{key: key, msgId: msgId, from: from, reason: reason})
		},
		onPeerReady:  func(user, code string) { push(netReadyMsg{key: key, user: user, code: code}) },
		onPeerLost:   func(user string) { push(netLostMsg{key: key, user: user}) },
		onRoster:     func() { push(netRosterMsg{}) },
		onDelivered:  func(msgId string) { push(netDeliveredMsg{key: key, msgId: msgId}) },
		onReaction:   func(r engineReaction) { push(netReactionMsg{key: key, reaction: r}) },
		onError:      func(err error) { push(netErrMsg{key: key, err: err}) },
		onSignalNote: func(n signalNote) { callMgr.onSignalNote(n) },
	})
	eng.joinPassword = password // enables engine self-rejoin after prune
	callMgr.SetRoster(func() map[string][]byte { return rosterMap(eng.peers()) })
	return chatScreen{
		sig:             sig,
		eng:             eng,
		call:            callMgr,
		key:             key,
		me:              me,
		vp:              vp,
		input:           ti,
		netCh:           netCh,
		leftSent:        &atomic.Bool{},
		rendered:        map[int]bool{},
		renderCache:     map[int]string{},
		tsCache:         map[int]time.Time{},
		wrapCache:       map[string]string{},
		unread:          map[string]int{},
		lastDMAt:        map[string]time.Time{},
		outbox:          nil,
		reactionCounts:  map[string]map[string]int{},
		myReactions:     map[string]string{},
		reactionDetails: map[string][]reactionDetail{},
		animations:      chatAnimationsEnabled(),
		// Group state: the constructor's session IS the home room.
		homeKey: key,
		homeSig: sig,
		homeEng: eng,
		id:      id,
		groups:  map[string]*groupSession{},
		// No aux row painted yet; -1 keeps stale indexes from ever matching.
		reactionLineIdx: -1,
		reactionRow:     -1,
		detailLineIdx:   -1,
		detailRow:       -1,
	}
}

// newSessionEngine builds an engine for an extra session (a joined group):
// same shared UI queue, every event tagged with the session code so Update
// routes background traffic. No media: calls are home-session-only.
func (c *chatScreen) newSessionEngine(sig *signalClient) *engine {
	key := sig.key
	// Capture the shared queue ONCE: these callbacks outlive the model value
	// this constructor was called on (the beat loop holds them while Update
	// reassigns the root model), so reading c.netCh per call would race the
	// owner. The channel value never changes after construction.
	netCh := c.netCh
	push := func(m tea.Msg) {
		if _, ok := m.(netChatMsg); !ok {
			if tryEnqueue(netCh, m) {
				return
			}
			select {
			case netCh <- m:
			default:
			}
			return
		}
		tryEnqueue(netCh, m)
	}
	var eng *engine
	eng = newEngine(c.me, c.id, sig, engineCallbacks{
		onChat: func(ch engineChat) {
			if tryEnqueue(netCh, netChatMsg{key: key, chat: ch}) {
				_ = eng.sendAck(ch.From, ch.MsgId)
			}
		},
		onFile: func(f engineFile) { push(netFileMsg{key: key, file: f}) },
		onFileErr: func(msgId, from, reason string) {
			push(netFileErrMsg{key: key, msgId: msgId, from: from, reason: reason})
		},
		onPeerReady:  func(user, code string) { push(netReadyMsg{key: key, user: user, code: code}) },
		onPeerLost:   func(user string) { push(netLostMsg{key: key, user: user}) },
		onRoster:     func() { push(netRosterMsg{}) },
		onDelivered:  func(msgId string) { push(netDeliveredMsg{key: key, msgId: msgId}) },
		onReaction:   func(r engineReaction) { push(netReactionMsg{key: key, reaction: r}) },
		onError:      func(err error) { push(netErrMsg{key: key, err: err}) },
		onSignalNote: nil, // groups have no media surface
	})
	return eng
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

// convFor resolves the conversation bucket for a message or file traveling
// from->to (empty to = room broadcast). Canonical pair keys make direction
// irrelevant: alice->bob and bob->alice land in the same thread.
func convFor(from, to string) string {
	if to == "" {
		return generalConv
	}
	return conversationKey(from, to)
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

// reactableMsgId is the MsgId a transcript row exposes for reactions:
// system events have no identity, everything else keeps its E2E id.
func reactableMsgId(m chatMessage) string {
	if m.Kind == "system" {
		return ""
	}
	return m.MsgId
}

// chatTimeLabel formats a message timestamp for the transcript ("" when the
// message carries no parseable time).
func chatTimeLabel(created string) string {
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		return t.Local().Format("15:04")
	}
	return "--:--"
}

// transcriptW is the width every transcript block is measured against: the
// live viewport width, with the same fallback the wrap pass uses so a block
// built before the first resize can never disagree with it.
func (c *chatScreen) transcriptW() int {
	if w := c.vp.Width; w > 0 {
		return w
	}
	return 60
}

// senderLabel paints the group header above a run of messages from one
// sender: name in the sender's hue plus a subdued timestamp. Own messages
// put the time first and right-align the whole line so the two sides of the
// conversation read as two sides.
func (c *chatScreen) senderLabel(name, tsPlain string, isOwn bool) string {
	return c.senderLabelStyled(name, tsPlain, isOwn, nil)
}

// senderLabelStyled is senderLabel with an optional full-row background
// (the quote-jump highlight tints the whole row blue).
func (c *chatScreen) senderLabelStyled(name, tsPlain string, isOwn bool, bg lipgloss.TerminalColor) string {
	w := c.transcriptW()
	display := name
	var nameStyle lipgloss.Style
	if isOwn {
		display = "You"
		nameStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	} else {
		nameStyle = lipgloss.NewStyle().Bold(true).Foreground(avatarColorFor(name))
	}
	if bg != nil {
		nameStyle = nameStyle.Background(bg)
	}
	var line string
	if isOwn {
		line = thMsgTimeStyle.Render(tsPlain) + "  " + nameStyle.Render(display)
	} else {
		line = nameStyle.Render(display) + "  " + thMsgTimeStyle.Render(tsPlain)
	}
	if isOwn {
		st := lipgloss.NewStyle().Width(w).Align(lipgloss.Right)
		if bg != nil {
			st = st.Background(bg)
		}
		return st.Render(line)
	}
	if bg != nil {
		return lipgloss.NewStyle().Background(bg).Width(w).Render(truncateByWidth(line, w))
	}
	return truncateByWidth(line, w)
}

// renderSender is the chatMessage-flavoured wrapper over senderLabel.
func (c *chatScreen) renderSender(m chatMessage) string {
	return c.senderLabel(m.Username, chatTimeLabel(m.CreatedAt), m.Username == c.me)
}

// chatBubble renders one chat message block: a tinted bubble that hugs its
// text, right-aligned when it is ours so the two sides of the conversation
// read as two sides. dim marks a send still waiting for its delivery ack,
// so "sent" is never confused with "received". Every valid "@username"
// token in the text paints as the mention chip — own messages, inbound
// general-room messages, and DM threads alike (DMs highlight for display
// only; receipts stay general-room-only, gated upstream).
func chatBubble(text string, isOwn, dim bool, availWidth int) string {
	return chatBubbleStyled(text, isOwn, dim, availWidth, nil)
}

// chatBubbleStyled is chatBubble with an optional bubble background
// override (the quote-jump highlight tints the whole bubble blue).
func chatBubbleStyled(text string, isOwn, dim bool, availWidth int, bg lipgloss.TerminalColor) string {
	textRendered := renderMarkdown(text)
	textRendered = mentionHighlighted(textRendered)
	// Fluid bubbles: near-full-width on narrow transcripts, tighter
	// columns when space abounds. Pure function of width, so the width
	// cache key stays sufficient.
	maxBubbleW := int(float64(availWidth) * bubbleRatioFor(availWidth))
	if maxBubbleW < 14 {
		maxBubbleW = 14
	}
	if maxBubbleW > availWidth-2 {
		maxBubbleW = availWidth - 2
	}
	needed := lipgloss.Width(textRendered) + 4
	if needed < 12 {
		needed = 12
	}
	bubbleW := needed
	if bubbleW > maxBubbleW {
		bubbleW = maxBubbleW
	}
	style := thOtherBubbleStyle
	if isOwn {
		style = thOwnBubbleStyle
		if dim {
			style = style.Faint(true)
		}
	}
	if bg != nil {
		style = style.Background(bg)
	}
	// Re-assert the bubble's colours after every reset the markdown emits,
	// then let the bubble itself do the wrapping.
	bubble := style.Width(bubbleW).Render(retint(textRendered, style))
	if isOwn {
		return lipgloss.NewStyle().Width(availWidth).Align(lipgloss.Right).Render(bubble)
	}
	return truncateByWidth(bubble, availWidth)
}

// renderBody paints just the message itself: a tinted block for chat, one
// centred faint row for a system event.
func (c *chatScreen) renderBody(m chatMessage) string {
	availWidth := c.transcriptW()
	if m.Kind == "system" {
		if isOwnPresence(m.Text, c.me) {
			return "" // never announce my own join/leave to me
		}
		return renderSystemLine(m.Text, chatTimeLabel(m.CreatedAt), availWidth)
	}
	dim := false
	if _, ok := c.unackedUI[m.MsgId]; ok && m.MsgId != "" {
		dim = true
	}
	// Every valid @username token chips — in the room, in own messages, and
	// in DMs alike (mentionHighlighted runs inside chatBubble; receipts are
	// gated general-room-only upstream, so DM highlighting is display-only).
	body := chatBubble(m.Text, m.Username == c.me, dim, availWidth)
	if card := c.quoteCardBlock(m.quote(), m.Username == c.me, availWidth); card != "" {
		body = alignBlock(card, m.Username == c.me, availWidth) + "\n" + body
	}
	if badge := c.reactionBadge(m); badge != "" {
		body += "\n" + badge
	}
	return body
}

// alignBlock pins a transcript block to the bubble's side of the column —
// right for own messages, flush left for peers — clipped to the transcript
// width so it can never wrap into a second row.
func alignBlock(block string, isOwn bool, availWidth int) string {
	if isOwn {
		return lipgloss.NewStyle().Width(availWidth).Align(lipgloss.Right).Render(block)
	}
	return truncateByWidth(block, availWidth)
}

// quoteCardBlock renders the WhatsApp-style citation card that tops a
// message carrying a quote: an accent bar, the quoted author, and the
// excerpt, painted on the bubble's own background so it reads as part of
// the bubble. One row, always (the excerpt is capped and clipped). Empty
// when there is no citation.
func (c *chatScreen) quoteCardBlock(q chatQuote, isOwn bool, availWidth int) string {
	if q.ReplyTo == "" || q.ReplyAuthor == "" {
		return ""
	}
	bg := colOtherBg
	if isOwn {
		bg = colOwnBg
	}
	bar := lipgloss.NewStyle().Foreground(colAccent).Render("▎")
	name := lipgloss.NewStyle().Bold(true).Foreground(avatarColorFor(q.ReplyAuthor)).Render(q.ReplyAuthor)
	excerpt := lipgloss.NewStyle().Foreground(colDim).Render(sanitizeDisplay(q.ReplyExcerpt))
	inner := truncateByWidth(bar+" "+name+": "+excerpt, maxInt(availWidth-2, 1))
	return lipgloss.NewStyle().Background(bg).Foreground(colText).Padding(0, 1).Render(inner)
}

// jumpBg is the background every row of a quote-jumped message paints with
// for the 3s highlight.
func jumpBg() lipgloss.TerminalColor { return colJumpBg }

// renderSenderJump paints the group header of a quote-jumped message with
// the full-row blue highlight.
func (c *chatScreen) renderSenderJump(m chatMessage) string {
	return c.senderLabelStyled(m.Username, chatTimeLabel(m.CreatedAt), m.Username == c.me, jumpBg())
}

// renderBodyJump paints a quote-jumped message's body with the full-row
// blue highlight: bubble and citation card both tint blue. System rows are
// never jump targets and keep their quiet look.
func (c *chatScreen) renderBodyJump(m chatMessage) string {
	availWidth := c.transcriptW()
	if m.Kind == "system" {
		return c.renderBody(m)
	}
	dim := false
	if _, ok := c.unackedUI[m.MsgId]; ok && m.MsgId != "" {
		dim = true
	}
	body := chatBubbleStyled(m.Text, m.Username == c.me, dim, availWidth, jumpBg())
	if card := c.quoteCardBlockJump(m.quote(), m.Username == c.me, availWidth); card != "" {
		body = alignBlock(card, m.Username == c.me, availWidth) + "\n" + body
	}
	if badge := c.reactionBadge(m); badge != "" {
		body += "\n" + badge
	}
	return body
}

// quoteCardBlockJump is quoteCardBlock on the blue highlight background.
func (c *chatScreen) quoteCardBlockJump(q chatQuote, isOwn bool, availWidth int) string {
	if q.ReplyTo == "" || q.ReplyAuthor == "" {
		return ""
	}
	bar := lipgloss.NewStyle().Foreground(colAccent).Render("▎")
	name := lipgloss.NewStyle().Bold(true).Foreground(avatarColorFor(q.ReplyAuthor)).Render(q.ReplyAuthor)
	excerpt := lipgloss.NewStyle().Foreground(colDim).Render(sanitizeDisplay(q.ReplyExcerpt))
	inner := truncateByWidth(bar+" "+name+": "+excerpt, maxInt(availWidth-2, 1))
	return lipgloss.NewStyle().Background(jumpBg()).Foreground(colText).Padding(0, 1).Render(inner)
}

// reactionChip renders one reaction pill through the shared chip geometry:
// one cell of horizontal padding — the same sizing the badge has always used —
// and ZERO vertical padding, so a backgrounded chip is always exactly one row
// tall (no double-height background when rows are aligned or clipped). The
// transcript badge and the anchored picker both build every chip through this
// helper, keeping their sizes in lockstep.
func reactionChip(content string, st lipgloss.Style) string {
	return st.Padding(0, 1).Render(content)
}

// reactionBadgeChip is one painted badge pill plus its span inside the joined
// badge line: start is the chip's left-pad cell, inner the content cells a
// click may land on. Paint and hit-test both derive from reactionBadgeCore,
// so the chips and their click targets can never drift.
type reactionBadgeChip struct {
	emoji string
	start int
	inner int
}

// reactionBadgeCore builds the badge's joined chip text (`👍2❤️1`, allowlist
// order) plus each chip's hit span, clipped to the transcript width so the
// row can never wrap into a second (backgrounded) row.
func (c chatScreen) reactionBadgeCore(m chatMessage) (string, []reactionBadgeChip) {
	if m.MsgId == "" {
		return "", nil
	}
	counts := c.reactionCounts[m.MsgId]
	if len(counts) == 0 {
		return "", nil
	}
	var chips []string
	var hits []reactionBadgeChip
	pos := 0
	for _, e := range reactionEmojis {
		n := counts[e]
		if n <= 0 {
			continue
		}
		content := fmt.Sprintf("%s%d", e, n)
		chip := reactionChip(content, tuiUnreadStyle)
		chips = append(chips, chip)
		hits = append(hits, reactionBadgeChip{emoji: e, start: pos, inner: lipgloss.Width(content)})
		pos += lipgloss.Width(chip)
	}
	if len(chips) == 0 {
		return "", nil
	}
	line := strings.Join(chips, "")
	if w := c.transcriptW(); lipgloss.Width(line) > w {
		line = truncateByWidth(line, w)
	}
	return line, hits
}

// reactionBadge renders the chips under a bubble (`👍2 ❤️1`), in allowlist
// order, aligned to the same side as the bubble. Counts live in the map, so
// evictRenderCache must run whenever they move.
func (c *chatScreen) reactionBadge(m chatMessage) string {
	line, _ := c.reactionBadgeCore(m)
	if line == "" {
		return ""
	}
	if m.Username == c.me {
		return lipgloss.NewStyle().Width(c.transcriptW()).Align(lipgloss.Right).Render(line)
	}
	return line
}

// reactionBadgeEmojiAt maps a transcript click onto the badge chip painted at
// that point (`👍2` -> 👍). The badge is the entry's LAST painted row (it fits
// exactly one row by construction), so a click on a bubble row misses and
// keeps its old meaning (open the picker).
func (c chatScreen) reactionBadgeEmojiAt(msgId string, x, y int, l layout) (string, bool) {
	top := transcriptTopRow(l)
	if msgId == "" || l.vpHeight <= 0 || y < top || y >= top+l.vpHeight {
		return "", false
	}
	row := y - top + c.vp.YOffset
	if row < 0 || row >= len(c.rowMsg) || c.rowMsg[row] != msgId {
		return "", false
	}
	if c.pendingReactionMsgId != "" && row == c.reactionRow {
		return "", false // picker row is a control, never a message hit
	}
	if row+1 < len(c.rowMsg) && c.rowMsg[row+1] == msgId {
		return "", false // inside the entry (header/bubble), not its badge row
	}
	m, ok := c.msgById(msgId)
	if !ok {
		return "", false
	}
	line, hits := c.reactionBadgeCore(m)
	if line == "" {
		return "", false
	}
	off := x - transcriptX0(l)
	if m.Username == c.me {
		off -= maxInt(c.transcriptW()-lipgloss.Width(line), 0)
	}
	if off < 0 {
		return "", false
	}
	for _, h := range hits {
		if off >= h.start+1 && off < h.start+1+h.inner { // +1: skip the left pad
			return h.emoji, true
		}
	}
	return "", false
}

// renderEntry composes one transcript entry: the group header (when the
// caller says this message starts a run) over the message body.
func (c *chatScreen) renderEntry(m chatMessage, withHeader bool) string {
	body := c.renderedBody(m)
	if body == "" {
		return ""
	}
	if !withHeader || m.Kind == "system" {
		return body
	}
	return c.renderedSender(m) + "\n" + body
}

// renderEntryJump composes a quote-jumped message's entry with the whole
// row painted blue (header, bubble, citation card).
func (c *chatScreen) renderEntryJump(m chatMessage, withHeader bool) string {
	body := c.renderBodyJump(m)
	if body == "" {
		return ""
	}
	if !withHeader || m.Kind == "system" {
		return body
	}
	return c.renderSenderJump(m) + "\n" + body
}

// renderLine renders a message as a complete standalone entry (header +
// body). rebuildView uses the finer-grained pieces for grouping; this is the
// single-message view.
func (c *chatScreen) renderLine(m chatMessage) string {
	return c.renderEntry(m, true)
}

// addMessage records a confirmed server message and refreshes the view.
func (c *chatScreen) addMessage(m chatMessage) {
	if c.rendered[m.Seq] {
		return
	}
	c.rendered[m.Seq] = true
	// Group buckets: unread lives on the group session (badged on its
	// sidebar row), recency drives the groups' display order. Background
	// group traffic (engine running while another conversation is active)
	// lands here too — the transcript filters it out until the group opens.
	if gc := groupCodeOf(m.ConvID); gc != "" {
		if g := c.groups[gc]; g != nil {
			if c.activeGroup != gc {
				g.unread++
			}
			if c.lastGroupAt == nil {
				c.lastGroupAt = map[string]time.Time{}
			}
			c.lastGroupAt[gc] = time.Now() // recency bump on EVERY arrival
		}
	} else if m.ConvID != "" && m.ConvID != generalConv && m.Username != c.me {
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
	// Room broadcasts arriving while a DM thread is in view get no paint
	// and no badge by the rules above — count them so the header can say
	// "N new in room" instead of looking like loss. (Empty ConvID is a
	// legacy room message, same as shouldRender treats it.)
	if (m.ConvID == generalConv || m.ConvID == "") && m.Username != c.me && c.activeConv() != generalConv {
		c.roomUnread++
	}
	c.history = append(c.history, m)
	// Bound the transcript: marathon sessions must not grow it without
	// limit. Oldest scrollback falls off first (rendered flags for dropped
	// seqs go with them); rendering is unaffected.
	if len(c.history) > maxHistory {
		for _, dropped := range c.history[:len(c.history)-maxHistory] {
			delete(c.rendered, dropped.Seq)
			delete(c.renderCache, dropped.Seq)
			delete(c.tsCache, dropped.Seq)
			delete(c.reactionCounts, dropped.MsgId)
			delete(c.myReactions, dropped.MsgId)
			delete(c.reactionDetails, dropped.MsgId)
			if c.pendingReactionMsgId == dropped.MsgId {
				// Its anchor row fell out of history: no picker row exists
				// to paint and no target remains.
				c.pendingReactionMsgId = ""
			}
			if c.detailMsgId == dropped.MsgId {
				// Same for the reactor detail bar hanging under it.
				c.detailMsgId, c.detailEmoji = "", ""
			}
		}
		c.history = append([]chatMessage(nil), c.history[len(c.history)-maxHistory:]...)
	}
	c.rebuildView()
}

// historyHasSeq reports whether a display seq is already taken.
func historyHasSeq(history []chatMessage, seq int) bool {
	for _, h := range history {
		if h.Seq == seq {
			return true
		}
	}
	return false
}

// allocSeq hands out the next display sequence number. ALL consumers
// (own-send reservation, inbound assignment, settle reallocation) go
// through here: taking c.nextSeq raw reuses the last settled number and
// silently drops the message via the rendered[] guard.
func (c *chatScreen) allocSeq() int {
	c.nextSeq++
	return c.nextSeq
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

func (c *chatScreen) appendLocal(conv, text string) {
	c.pushLocalLine(localLine{conv: conv, text: text, kind: lineText})
	c.rebuildView()
}

// pushLocalLine appends a transcript row, enforcing the cap. Dropping head
// rows shifts stored indices (in-flight echo, upload progress): adjust them
// so later lookups still hit their row, invalidating ones that fell off.
func (c *chatScreen) pushLocalLine(ll localLine) {
	c.localLines = append(c.localLines, ll)
	if overflow := len(c.localLines) - maxLocalLines; overflow > 0 {
		c.localLines = append([]localLine(nil), c.localLines[overflow:]...)
		if c.pending != nil {
			c.pending.localIdx -= overflow
			if c.pending.localIdx < 0 {
				c.pending.localIdx = -1
			}
		}
		if c.uploadQ.lineIdx >= 0 {
			c.uploadQ.lineIdx -= overflow
			if c.uploadQ.lineIdx < 0 {
				c.uploadQ.lineIdx = -1
			}
		}
	}
}

// rebuildView derives the painted transcript from raw history + local lines,
// applying the CURRENT visibility filter. Entering/leaving private mode just
// calls this — historical lines re-filter retroactively.
// File cards are interleaved chronologically among history messages so they
// behave like text messages; transient local lines (pending echo, progress)
// stay pinned at the bottom.
// cacheForWidth drops all render caches when the viewport width changed.
// Rendered output depends on (message content, width, density): bubbles
// scale fluidly with width while avatar density follows the terminal, so
// both join the cache key. Caches stay valid across rebuilds at a stable
// size no matter how history grows.
func (c *chatScreen) cacheForWidth(w int) {
	if c.renderCache == nil {
		c.renderCache = map[int]string{}
	}
	if c.hdrCache == nil {
		c.hdrCache = map[int]string{}
	}
	if c.tsCache == nil {
		c.tsCache = map[int]time.Time{}
	}
	if c.wrapCache == nil {
		c.wrapCache = map[string]string{}
	}
	adaptSig := 0
	if compactTranscript(c.width) {
		adaptSig = 1
	}
	if c.cacheWidth != w || c.cacheAdapt != adaptSig {
		c.renderCache = map[int]string{}
		c.hdrCache = map[int]string{}
		c.tsCache = map[int]time.Time{}
		c.wrapCache = map[string]string{}
		c.cacheWidth = w
		c.cacheAdapt = adaptSig
	}
}

// evictRenderCache drops the memoized bubble for one msgId (dim state
// changed), so the next rebuild repaints it instead of reusing stale art.
func (c *chatScreen) evictRenderCache(msgId string) {
	if msgId == "" || len(c.renderCache) == 0 {
		return
	}
	for _, h := range c.history {
		if h.MsgId == msgId {
			delete(c.renderCache, h.Seq)
		}
	}
}

// ---- reactions ---------------------------------------------------------------

// maxReactionPollIDs caps one GET /reactions narrowing list (the server caps
// it at the same number). Newest messages win: they are the ones on screen.
const maxReactionPollIDs = 50

// activeReactionMsgIds lists the active conversation's renderable MsgIds,
// newest first, bounded for the poll query. Reactions are room-scoped on the
// server, but a client only ever asks about what it currently shows.
func (c chatScreen) activeReactionMsgIds() []string {
	var ids []string
	for i := len(c.history) - 1; i >= 0 && len(ids) < maxReactionPollIDs; i-- {
		m := c.history[i]
		if reactableMsgId(m) == "" || !c.shouldRender(m) {
			continue
		}
		ids = append(ids, m.MsgId)
	}
	return ids
}

// applyLocalReaction mirrors the server's toggle semantics optimistically:
// the same emoji removes my reaction, a different one replaces it.
func (c *chatScreen) applyLocalReaction(msgId, emoji string) {
	if c.myReactions == nil {
		c.myReactions = map[string]string{}
	}
	if c.reactionCounts == nil {
		c.reactionCounts = map[string]map[string]int{}
	}
	counts := c.reactionCounts[msgId]
	cur := c.myReactions[msgId]
	if cur == emoji { // same emoji toggles off
		delete(c.myReactions, msgId)
		if counts != nil {
			c.decReaction(counts, emoji)
			if len(counts) == 0 {
				delete(c.reactionCounts, msgId)
			}
		}
		return
	}
	if counts == nil {
		counts = map[string]int{}
		c.reactionCounts[msgId] = counts
	}
	if cur != "" {
		c.decReaction(counts, cur)
	}
	counts[emoji]++
	c.myReactions[msgId] = emoji
}

// decReaction decrements one emoji tally, dropping the key at zero.
func (c *chatScreen) decReaction(counts map[string]int, emoji string) {
	if counts[emoji] <= 1 {
		delete(counts, emoji)
		return
	}
	counts[emoji]--
}

// undoReaction rolls back one optimistic toggle after a failed POST,
// provided the user has not moved the reaction elsewhere meanwhile.
func (c *chatScreen) undoReaction(msgId, emoji, prev string) {
	if c.myReactions[msgId] != emoji {
		return
	}
	c.applyLocalReaction(msgId, emoji) // same emoji removes the optimistic pick
	if prev != "" && reactionEmojiAllowed(prev) {
		c.applyLocalReaction(msgId, prev)
	}
}

// reactionCountsEqual compares two emoji tallies.
func reactionCountsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// reactionDetailsEqual compares two breakdowns as sets: ordering of emojis or
// reactors never forces a repaint. A missing breakdown (nil) equals an empty
// one so an old server's poll is a no-op.
func reactionDetailsEqual(a, b []reactionDetail) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(details []reactionDetail) map[string]string {
		out := make(map[string]string, len(details))
		for _, d := range details {
			names := append([]string(nil), d.Usernames...)
			sort.Strings(names)
			out[d.Emoji] = strings.Join(names, "\x00")
		}
		return out
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for emoji, names := range ka {
		if kb[emoji] != names {
			return false
		}
	}
	return true
}

// applyFetchedReactions replaces the requested scope with the server's
// aggregate (the truth): ids absent from the response clear to zero. The
// optional per-reactor breakdown is stored alongside; a summary without it
// clears any stale breakdown and leaves the detail bar on its fallback.
// Returns true when anything changed, so unchanged polls never repaint.
func (c *chatScreen) applyFetchedReactions(ids []string, summaries []reactionSummary) bool {
	byID := make(map[string]reactionSummary, len(summaries))
	for _, s := range summaries {
		byID[s.MsgId] = s
	}
	var changed []string
	for _, id := range ids {
		s := byID[id]
		var newCounts map[string]int
		if len(s.Counts) > 0 {
			newCounts = s.Counts
		}
		newMine := ""
		if len(s.Mine) > 0 {
			newMine = s.Mine[0] // server keeps one reaction per user
		}
		var newDetails []reactionDetail
		if len(s.Details) > 0 {
			newDetails = s.Details
		}
		if c.myReactions[id] == newMine && reactionCountsEqual(c.reactionCounts[id], newCounts) &&
			reactionDetailsEqual(c.reactionDetails[id], newDetails) {
			continue
		}
		if newCounts == nil {
			delete(c.reactionCounts, id)
		} else {
			if c.reactionCounts == nil {
				c.reactionCounts = map[string]map[string]int{}
			}
			c.reactionCounts[id] = newCounts
		}
		if newMine == "" {
			delete(c.myReactions, id) // nil map delete is a no-op
		} else {
			if c.myReactions == nil {
				c.myReactions = map[string]string{}
			}
			c.myReactions[id] = newMine
		}
		if newDetails == nil {
			delete(c.reactionDetails, id) // old server / no breakdown: fallback
		} else {
			if c.reactionDetails == nil {
				c.reactionDetails = map[string][]reactionDetail{}
			}
			c.reactionDetails[id] = newDetails
		}
		changed = append(changed, id)
	}
	for _, id := range changed {
		c.evictRenderCache(id)
	}
	return len(changed) > 0
}

// closeReactionAux dismisses whichever anchored reaction row is open — the
// picker above its message and the reactor dropdown below it are mutually
// exclusive, so one call maintains the single-aux-row discipline. Both are
// part of the transcript content, so clearing the ids must re-derive the
// paint buffer or rows would linger on screen.
func (c *chatScreen) closeReactionAux() {
	if c.pendingReactionMsgId == "" && c.detailMsgId == "" {
		return
	}
	c.pendingReactionMsgId = ""
	c.detailMsgId, c.detailEmoji = "", ""
	c.detailAnim = 0
	c.rebuildView()
}

// closeReactionDetail dismisses only the reactor dropdown (opening the
// picker uses this before anchoring above the same message).
func (c *chatScreen) closeReactionDetail() {
	if c.detailMsgId == "" && c.detailEmoji == "" {
		return
	}
	c.detailMsgId, c.detailEmoji = "", ""
	c.detailAnim = 0
	c.rebuildView()
}

// openReactionDetail opens the reactor dropdown below msgId for one emoji and
// returns the open reveal's first tick (nil when motion is off or there is
// nothing to show). Opening it dismisses the picker (single aux row); an
// emoji with no remaining reactors stays closed.
func (c *chatScreen) openReactionDetail(msgId, emoji string) tea.Cmd {
	m, ok := c.msgById(msgId)
	if !ok || emoji == "" || len(c.reactionDetailRows(m, emoji)) == 0 {
		c.closeReactionDetail()
		return nil
	}
	c.pendingReactionMsgId = ""
	c.detailMsgId, c.detailEmoji = msgId, emoji
	c.detailAnimGen++ // supersede every reveal still in flight
	c.detailAnim = 0
	if !c.animations {
		c.detailAnim = reactionDetailAnimFrames - 1 // plain run: no timers, final paint
		c.rebuildView()
		return nil
	}
	c.rebuildView()
	return scheduleReactionDetailAnim(c.detailAnimGen, 1)
}

// toggleReactionDetail opens the dropdown under a message, or closes it when
// the same chip is reselected. Returning the reveal command lets the mouse
// path start the animation without knowing the frame machinery.
func (c *chatScreen) toggleReactionDetail(msgId, emoji string) tea.Cmd {
	if c.detailMsgId == msgId && c.detailEmoji == emoji {
		c.closeReactionDetail()
		return nil
	}
	return c.openReactionDetail(msgId, emoji)
}

// dismissAuxOnScroll closes the picker and/or detail bar whenever the
// transcript actually moved (wheel, drag, keyboard pages): both are anchored
// to their message, so a change of viewport offset would leave them trailing.
func (c *chatScreen) dismissAuxOnScroll(beforeOffset int) {
	if c.vp.YOffset == beforeOffset {
		return
	}
	c.closeReactionAux()
}

// toggleReaction applies the optimistic toggle, evicts the stale bubble and
// returns the command that persists it (server POST + best-effort peer nudge).
// One pick closes the anchored picker, WhatsApp-style; it never opens the
// reactor detail — inspection is an explicit badge-chip click.
func (c *chatScreen) toggleReaction(msgId, emoji string) tea.Cmd {
	if msgId == "" || !reactionEmojiAllowed(emoji) {
		return nil
	}
	prev := c.myReactions[msgId]
	c.applyLocalReaction(msgId, emoji)
	c.evictRenderCache(msgId)
	c.pendingReactionMsgId = "" // one pick: the anchored picker closes
	c.detailMsgId, c.detailEmoji = "", ""
	c.rebuildView()
	return c.doReact(msgId, emoji, prev, c.toForMsgId(msgId))
}

// doReact persists one reaction: POST /reactions is authoritative, the P2P
// frame only nudges peers to fetch earlier than their next 2s poll.
func (c chatScreen) doReact(msgId, emoji, prev, to string) tea.Cmd {
	if c.sig == nil {
		return nil
	}
	return func() tea.Msg {
		if err := c.sig.react(msgId, emoji); err != nil {
			return reactionDoneMsg{msgId: msgId, emoji: emoji, prev: prev, err: err}
		}
		if c.eng != nil {
			_ = c.eng.sendReaction(to, msgId, emoji)
		}
		return reactionDoneMsg{msgId: msgId, emoji: emoji, prev: prev}
	}
}

// fetchReactionsCmd builds the GET /reactions poll for the active
// conversation's newest messages (nil when there is nothing to ask about).
func (c chatScreen) fetchReactionsCmd() tea.Cmd {
	if c.sig == nil {
		return nil
	}
	ids := c.activeReactionMsgIds()
	if len(ids) == 0 {
		return nil
	}
	return func() tea.Msg {
		summaries, err := c.sig.reactions(ids)
		return reactionsFetchedMsg{ids: ids, summaries: summaries, err: err}
	}
}

// toForMsgId resolves the P2P destination for a reaction nudge: the DM peer
// for a thread message, "" (broadcast) for the room.
func (c chatScreen) toForMsgId(msgId string) string {
	for _, m := range c.history {
		if m.MsgId != msgId {
			continue
		}
		if m.ConvID == "" || m.ConvID == generalConv {
			return ""
		}
		// Groups broadcast inside their own session: no per-peer nudge
		// target, exactly like the common room.
		if groupCodeOf(m.ConvID) != "" {
			return ""
		}
		return peerOf(c.me, m.ConvID)
	}
	return ""
}

// newestReactableMsgId is the keyboard fallback target: the newest message
// with an identity in the active conversation.
func (c chatScreen) newestReactableMsgId() string {
	for i := len(c.history) - 1; i >= 0; i-- {
		if id := reactableMsgId(c.history[i]); id != "" && c.shouldRender(c.history[i]) {
			return id
		}
	}
	return ""
}

// renderedBody returns the cached message block for a history message,
// rendering and memoizing on miss.
func (c *chatScreen) renderedBody(m chatMessage) string {
	if s, ok := c.renderCache[m.Seq]; ok {
		return s
	}
	if c.renderCache == nil {
		c.renderCache = map[int]string{}
	}
	s := c.renderBody(m)
	c.renderCache[m.Seq] = s
	return s
}

// renderedSender returns the cached group header for a history message.
func (c *chatScreen) renderedSender(m chatMessage) string {
	if s, ok := c.hdrCache[m.Seq]; ok {
		return s
	}
	if c.hdrCache == nil {
		c.hdrCache = map[int]string{}
	}
	s := c.renderSender(m)
	c.hdrCache[m.Seq] = s
	return s
}

// renderedLine returns the cached message BLOCK for a history message
// (body only — the sender-group header is composed by rebuildView, which
// knows whether this message starts a run).
func (c *chatScreen) renderedLine(m chatMessage) string {
	return c.renderedBody(m)
}

// renderedEntry is renderedLine plus the group header when the caller says
// this message starts a run.
func (c *chatScreen) renderedEntry(m chatMessage, withHeader bool) string {
	body := c.renderedBody(m)
	if body == "" {
		return ""
	}
	if !withHeader || m.Kind == "system" {
		return body
	}
	return c.renderedSender(m) + "\n" + body
}

// messageTime returns the cached parsed timestamp for a history message.
func (c *chatScreen) messageTime(m chatMessage) time.Time {
	if t, ok := c.tsCache[m.Seq]; ok {
		return t
	}
	var t time.Time
	if parsed, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		t = parsed
	}
	// Fallback matches the old inline behavior: missing timestamps sort as
	// very old (cards after). Cached either way — parse once per message.
	c.tsCache[m.Seq] = t
	return t
}

func (c *chatScreen) rebuildView() {
	c.lines = c.lines[:0]
	c.lineMsg = c.lineMsg[:0]
	c.lineCard = c.lineCard[:0]
	c.reactionLineIdx = -1 // no anchored picker until one is seen below
	c.detailLineIdx = -1   // no reactor detail bar until one is seen below
	c.detailN = 0
	c.replyPickLineIdx = -1 // no reply pointer until one is seen below
	c.cacheForWidth(c.vp.Width)

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

	// Merge history + file cards by timestamp (parsed timestamps cached).
	//
	// Sender grouping: a sender/kind header is painted only when the run
	// changes — new sender, new kind, or a gap of groupAfter — so a
	// conversation reads as conversation instead of a stack of labels.
	const groupAfter = 5 * time.Minute
	var lastUser, lastKind string
	var lastAt time.Time
	firstEntry := true
	startGroup := func(user, kind string, at time.Time) bool {
		if firstEntry {
			firstEntry = false
			lastUser, lastKind, lastAt = user, kind, at
			return true
		}
		newRun := user != lastUser || kind != lastKind
		if !newRun && !at.IsZero() && !lastAt.IsZero() && at.Sub(lastAt) > groupAfter {
			newRun = true
		}
		lastUser, lastKind = user, kind
		if !at.IsZero() {
			lastAt = at
		}
		return newRun
	}
	push := func(entry, msgId string, cardOff int) {
		if entry == "" {
			return // suppressed (own presence line): leave no blank row
		}
		c.lines = append(c.lines, entry)
		c.lineMsg = append(c.lineMsg, msgId)
		c.lineCard = append(c.lineCard, cardOff)
	}

	hi, ci := 0, 0
	for hi < len(visibleHistory) || ci < len(cards) {
		var histTime time.Time
		hasHist := hi < len(visibleHistory)
		if hasHist {
			histTime = c.messageTime(visibleHistory[hi])
		}
		hasCard := ci < len(cards)
		// If history timestamp missing, keep history order and render hist first.
		cardBeforeHist := hasCard && hasHist && !histTime.IsZero() && cards[ci].t.Before(histTime)
		if hasCard && (!hasHist || cardBeforeHist) {
			fd := cards[ci].ll.fileData
			who := fd.username
			if who == "" {
				who = "File"
			}
			header := ""
			if startGroup(who, "file", cards[ci].t) {
				header = c.senderLabel(who, fd.time, who == c.me) + "\n"
			}
			card := fileAttachmentCard(fd.filename, fd.username, fd.size, fd.time, c.vp.Width)
			if fd.username == c.me {
				card = lipgloss.NewStyle().Width(maxInt(c.transcriptW(), 1)).Align(lipgloss.Right).Render(card)
			}
			push(header+card, "", -1)
			ci++
		} else if hasHist {
			m := visibleHistory[hi]
			withHeader := startGroup(m.Username, m.Kind, c.messageTime(m))
			entry := c.renderedEntry(m, withHeader)
			// A quote-jumped message's WHOLE row paints blue: re-render the
			// entry with the highlight backgrounds instead of the cached art.
			if entry != "" && c.quoteJump != nil && m.MsgId == c.quoteJump.msgId {
				entry = c.renderEntryJump(m, withHeader)
			}
			// The /reply pointer is an extra transcript row directly ABOVE
			// its target message (same anchored pattern as the reaction
			// picker): it scrolls with the message. refreshViewport turns
			// replyPickLineIdx into the viewport-relative replyPick.row.
			if entry != "" && c.replyPick != nil && m.MsgId == c.replyPick.target {
				c.replyPickLineIdx = len(c.lines)
				push(c.replyPointerPaint(m), "", -1)
			}
			// The reaction picker is an extra transcript row directly ABOVE
			// its message's entry (WhatsApp-style anchor), never a drawer row:
			// it scrolls with the message it targets. refreshViewport turns
			// reactionLineIdx into the viewport-relative reactionRow the
			// hit-test reads.
			if entry != "" && m.MsgId != "" && m.MsgId == c.pendingReactionMsgId {
				c.reactionLineIdx = len(c.lines)
				push(c.reactionPickerView(m), "", -1)
			}
			cardOff := -1
			if entry != "" && m.repliedTo() {
				cardOff = 0
				if withHeader && m.Kind != "system" {
					cardOff = 1 // the card sits right under the group header
				}
			}
			push(entry, reactableMsgId(m), cardOff)
			// The reactor dropdown hangs directly BELOW its message: a
			// rounded card (header + divider + reactor rows) whose reveal
			// frame is c.detailAnim, painted as control lines that never
			// map to a message hit.
			if entry != "" && m.MsgId != "" && m.MsgId == c.detailMsgId && c.detailEmoji != "" {
				c.detailLineIdx = len(c.lines)
				for _, row := range c.reactionDetailPaint(m, c.detailEmoji, c.detailAnim) {
					push(row, "", -1)
				}
				c.detailN = len(c.lines) - c.detailLineIdx
			}
			hi++
		} else {
			break
		}
	}
	// Append transient pinned lines (pending echo, progress bars, errors) at bottom.
	for _, ll := range pinned {
		push(ll.text, "", -1)
	}
	c.syncRosterVp() // users list content lives with the screen, not the copy
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
	// Validate caches against the RAW width (same key rebuildView uses);
	// the mapped fallback below is style-only. Mismatched keys here once
	// caused every rebuild to clear the caches — zero benefit.
	c.cacheForWidth(c.vp.Width)
	w := c.vp.Width
	if w <= 0 {
		w = 40
	}
	atBottom := c.vp.AtBottom()
	if len(c.lines) == 0 {
		// Empty state: a centred, helpful hint — never a blank pane. It is
		// painted straight into the viewport (never into c.lines) so the
		// transcript's row model stays message-for-message.
		c.rowMsg = c.rowMsg[:0]
		c.rowCard = c.rowCard[:0]
		c.reactionRow = -1
		c.detailRow = -1
		c.detailRowH = 0
		if c.replyPick != nil {
			c.replyPick.row = -1
		}
		c.vp.SetContent(c.emptyStateView(w))
		if atBottom {
			c.vp.GotoBottom()
		}
		return
	}
	st := lipgloss.NewStyle().Width(w)
	wrapped := make([]string, len(c.lines))
	for i, ln := range c.lines {
		// Unchanged lines re-wrap identically: memoize by content. Only new
		// or edited rows pay the grapheme-segmentation cost per rebuild.
		if prev, ok := c.wrapCache[ln]; ok {
			wrapped[i] = prev
			continue
		}
		r := st.Render(ln)
		// Bound the cache: content-keyed entries outlive their rows, so
		// cap at ~2x history. Oldest eviction is approximate but safe
		// (a miss just re-renders).
		if len(c.wrapCache) > 2*(len(c.history)+len(c.localLines)+64) {
			clear(c.wrapCache)
		}
		c.wrapCache[ln] = r
		wrapped[i] = r
	}
	// Row index: viewport content row -> owning MsgId. Every line is
	// pre-wrapped to `w`, so a wrapped block occupies exactly its newline
	// count + 1 screen rows (the viewport never re-wraps). The anchored
	// picker line is indexed in the same pass: reactionRow is the content row
	// its single line paints on, so the hit-test follows any scroll.
	c.rowMsg = c.rowMsg[:0]
	c.rowCard = c.rowCard[:0]
	c.reactionRow = -1
	c.detailRow = -1
	c.detailRowH = 0
	if c.replyPick != nil {
		c.replyPick.row = -1
	}
	row := 0
	for i, wl := range wrapped {
		if i == c.reactionLineIdx {
			c.reactionRow = row
		}
		if i == c.detailLineIdx {
			c.detailRow = row
		}
		if i == c.replyPickLineIdx && c.replyPick != nil {
			c.replyPick.row = row
		}
		id := ""
		if i < len(c.lineMsg) {
			id = c.lineMsg[i]
		}
		cardOff := -1
		if i < len(c.lineCard) {
			cardOff = c.lineCard[i]
		}
		n := strings.Count(wl, "\n") + 1
		if c.detailLineIdx >= 0 && i >= c.detailLineIdx && i < c.detailLineIdx+c.detailN {
			c.detailRowH += n
		}
		for j := 0; j < n; j++ {
			c.rowMsg = append(c.rowMsg, id)
			c.rowCard = append(c.rowCard, cardOff == j)
		}
		row += n
	}
	c.vp.SetContent(strings.Join(wrapped, "\n"))
	if atBottom {
		c.vp.GotoBottom()
	}
}

// emptyStateView paints the transcript's resting state: a centred, helpful
// hint occupying exactly the viewport box. Never a blank pane, never a
// debug dump — the pane always answers "where am I and what do I do".
func (c *chatScreen) emptyStateView(w int) string {
	if w <= 0 {
		w = 40
	}
	title := "No messages yet"
	hint := "Be the first to say something — press Enter to send."
	switch {
	case c.sideFilter != "":
		title = "Nothing matches “" + c.sideFilter + "”"
		hint = "Press Esc to show every conversation again."
	case c.targetUser != "":
		hint = "Private thread with " + c.targetUser + " — press Enter to send."
	}
	center := lipgloss.NewStyle().Width(w).Align(lipgloss.Center)
	block := center.Bold(true).Foreground(colDim).Render(truncateByWidth(title, w)) + "\n" +
		center.Foreground(colFaint).Render(truncateByWidth(hint, w))
	if h := c.vp.Height; h > 3 {
		pad := (h - 2) / 2
		block = strings.Repeat("\n", pad) + block
	}
	return block
}

// scrollbarView renders a vertical scrollbar for the transcript viewport.
// Height h includes the border interior rows. Uses ScrollPercent() to position
// the thumb proportionally. Returns a single-column string of height h.
func (c chatScreen) scrollbarView(h int) string {
	bar, _, _, _, _ := scrollbarBar(c.vp.TotalLineCount(), c.vp.Height, c.vp.YOffset, h)
	return bar
}

// scrollbarBar renders one scrollbar column for total/visible/offset state
// and reports the thumb geometry for drag hit-tests. Shared by all three
// independently scrollable panes.
func scrollbarBar(total, visible, offset, h int) (bar string, thumbTop, thumbH, trackH int, hasArrows bool) {
	if h <= 0 {
		return "", 0, 0, 0, false
	}
	// No scrolling needed: draw an empty track.
	if total <= visible || total <= 0 {
		rows := make([]string, h)
		for i := range rows {
			rows[i] = tuiScrollbarStyle.Render("│")
		}
		return strings.Join(rows, "\n"), 0, 0, h, false
	}
	trackH = h
	hasArrows = h >= 3
	if hasArrows {
		trackH = h - 2
	}
	thumbH = trackH * visible / total
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > trackH {
		thumbH = trackH
	}
	// Thumb travel maps content scroll: top at offset 0, bottom at offset
	// total-visible (bottom-stuck), independent of the percent rounding.
	maxOff := total - visible
	if maxOff <= 0 {
		maxOff = 0
	}
	thumbTop = 0
	if maxOff > 0 {
		thumbTop = int(float64(trackH-thumbH) * float64(offset) / float64(maxOff))
	}
	if thumbTop < 0 {
		thumbTop = 0
	}
	if thumbTop+thumbH > trackH {
		thumbTop = trackH - thumbH
	}
	var rows []string
	if hasArrows {
		rows = append(rows, tuiScrollbarStyle.Render("▲"))
	}
	for i := 0; i < trackH; i++ {
		if i >= thumbTop && i < thumbTop+thumbH {
			rows = append(rows, tuiScrollbarThumbStyle.Render("█"))
		} else {
			rows = append(rows, tuiScrollbarStyle.Render("│"))
		}
	}
	if hasArrows {
		rows = append(rows, tuiScrollbarStyle.Render("▼"))
	}
	return strings.Join(rows, "\n"), thumbTop, thumbH, trackH, hasArrows
}

func (c chatScreen) headerView() string {
	// App top bar: logo + promises left, signal/lock/clock right. The
	// session context (key, peer, version) moved to the room header above
	// the transcript, where it belongs to the conversation in view.
	l := c.layoutFor()
	w := c.width
	if l.frameOn {
		w -= frameChrome // banner lives inside the app shell
	}
	return topBarView(w)
}

func (c chatScreen) statusView() string {
	if c.status == "" {
		return ""
	}
	// Width-clamped: statuses carry command output (/help lists) and must
	// never break the exact-width frame contract.
	w := maxInt(c.width, 1)
	if c.layoutFor().frameOn {
		w -= frameChrome
	}
	return truncateByWidth(statusStyleFor(c.status).Render(c.status), maxInt(w, 1))
}

// statusStyleFor classifies the one-line notice strip: quiet for command
// output, amber for results, red for failures. One clear line either way.
func statusStyleFor(s string) lipgloss.Style {
	switch {
	case strings.HasPrefix(s, "Commands:"), strings.HasPrefix(s, "media"):
		return thStatusInfoStyle
	case s == serverDownMsg,
		strings.HasPrefix(s, "*"),
		strings.HasPrefix(s, "✗"),
		strings.HasPrefix(s, "usage:"),
		strings.Contains(s, "failed"),
		strings.Contains(s, "Session has ended"),
		strings.Contains(s, "outbox full"),
		strings.Contains(s, "unavailable"),
		strings.Contains(s, "cap is"):
		return thStatusErrStyle
	default:
		return thStatusWarnStyle
	}
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

// searchHeightFor is the height of the sidebar header block: a single
// inline filter row that doubles as the "/" affordance. headRows is still
// part of the signature because paint, viewport sync and hit-test all pass
// it — but the block height is width-driven only, so the list start line
// can never drift between those three callers.
func searchHeightFor(headRows, inner int) int {
	_ = headRows
	if inner >= 16 {
		return 1
	}
	return 0
}

// sidebarFill is the sidebar column height: exactly the height of the main
// column it sits beside (chat header + transcript + drawer + composer +
// hints). Paint, scrollbar geometry and viewport sync all read it, so the
// two columns can never drift apart. Mirrors View()'s assembly order.
func (c chatScreen) sidebarFill(l layout) int {
	h := l.headRows
	if l.vpHeight > 0 {
		h += l.vpHeight
	}
	h += l.hintRows
	if l.paletteRows > 0 {
		if pal := c.drawerView(maxInt(l.vpWidth+2, 0)); pal != "" {
			h += 1 + lipgloss.Height(pal) // spacer + panel rows
		}
	}
	if l.composerRows > 0 {
		h += l.composerRows + 2 // composer box + border
	} else {
		h++ // bare prompt line
	}
	return maxInt(h, 0)
}

// sidebarHeader paints the rail's one-row inline filter: the live query (or
// a faint placeholder) with the right-edge affordance. The affordance is
// the "/" drawer opener normally; with pending invites it becomes the
// settings ⚙ badge (clicking it opens /settings). It sits on a stronger
// tint so it reads as a header without spending a row on a rule.
func (c chatScreen) sidebarHeader(inner int) string {
	glyph := "⌕ "
	affordance := " "
	textBudget := inner - lipgloss.Width(glyph) - 2
	if c.pendingInvites > 0 {
		affordance = "⚙" + tuiUnreadStyle.Render(circledNum(c.pendingInvites))
		textBudget = inner - lipgloss.Width(glyph) - lipgloss.Width(affordance)
	} else if textBudget >= 1 {
		affordance = "/"
	}
	var body string
	if c.sideFilter != "" {
		caret := ""
		if c.focus == focusSidebar && !c.drawerOpen() {
			caret = thFocusBar.Render("▎")
		}
		body = thSearchStyle.Render(glyph) +
			lipgloss.NewStyle().Foreground(colText).Render(truncateByWidth(sanitizeDisplay(c.sideFilter), maxInt(textBudget-lipgloss.Width(caret), 1))) +
			caret
	} else {
		hint := "conversations"
		if textBudget < len(hint) {
			hint = "chats"
		}
		body = thSearchStyle.Render(glyph + truncateByWidth(hint, maxInt(textBudget, 1)))
	}
	gap := inner - lipgloss.Width(body) - lipgloss.Width(affordance)
	if gap < 1 {
		gap = 1
	}
	affStyle := thSearchStyle
	if c.focus == focusSidebar && !c.drawerOpen() {
		affStyle = thFocusBar
	}
	row := body + strings.Repeat(" ", gap) + affStyle.Render(affordance)
	return tintFit(thSidebarHeaderStyle, row, inner)
}

// fitRow lives in chat_theme.go (shared with the header and drawer rows).

// rosterBody paints the sidebar column: the filter header row, then one
// two-line entry per conversation (presence, name, time, unread badge,
// preview, call glyph). fill is the FULL column height — header included —
// and the returned block is always exactly fill rows of exactly
// sidebarInnerWidth() cells. Overflow scrolls inside the roster viewport
// with its own rail column.
func (c chatScreen) rosterBody(fill int) string {
	if fill <= 0 {
		return ""
	}
	inner := c.sidebarInnerWidth()
	rowW := maxInt(inner-1, 8) // last column belongs to the scroll rail
	sh := searchHeightFor(c.layoutFor().headRows, inner)
	if sh > fill {
		sh = fill
	}
	header := make([]string, 0, sh)
	for i := 0; i < sh; i++ {
		header = append(header, c.sidebarHeader(rowW))
	}
	if fill <= sh {
		return strings.Join(header, "\n")
	}
	bodyH := fill - sh
	items := c.chatItems()
	rows := c.chatItemRows(items, rowW, c.hoverPeer)
	needBar := len(rows) > bodyH

	railRows := make([]string, bodyH)
	for i := range railRows {
		railRows[i] = " "
	}
	var body []string
	if !needBar {
		body = rows
		for len(body) < bodyH {
			body = append(body, tintFit(thSidebarStyle, "", rowW))
		}
	} else {
		bar, _, _, _, _ := scrollbarBar(len(rows), bodyH, c.rosterVp.YOffset, bodyH)
		vp := c.rosterVp
		vp.Width = rowW
		vp.Height = bodyH
		vp.SetContent(strings.Join(rows, "\n"))
		body = strings.Split(vp.View(), "\n")
		if len(body) > bodyH {
			body = body[:bodyH]
		}
		for len(body) < bodyH {
			body = append(body, tintFit(thSidebarStyle, "", rowW))
		}
		barLines := strings.Split(bar, "\n")
		for i := range railRows {
			if i < len(barLines) {
				railRows[i] = barLines[i]
			}
		}
	}

	out := make([]string, 0, fill)
	for _, hr := range header {
		out = append(out, fitRow(hr, rowW)+" ")
	}
	for i, br := range body {
		rc := " "
		if i < len(railRows) {
			rc = railRows[i]
		}
		out = append(out, fitRow(br, rowW)+rc)
	}
	if len(out) > fill {
		out = out[:fill]
	}
	for len(out) < fill {
		out = append(out, tintFit(thSidebarStyle, "", inner))
	}
	return strings.Join(out, "\n")
}

// chatItemHeight is the sidebar row budget per chat: two rows (identity
// over preview) normally, one dense identity row when terminal height is
// scarce. Selection is an accent bar + tint inside the same rows.
func (c chatScreen) chatItemHeight() int {
	if compactItems(c.width, c.height) {
		return 1
	}
	return itemRowsPerChat
}

// chatItemRows renders every chat item as exactly chatItemHeight() rows.
// hover names the peer under the mouse / keyboard cursor.
func (c chatScreen) chatItemRows(items []chatItem, inner int, hover string) []string {
	h := c.chatItemHeight()
	rows := make([]string, 0, len(items)*h)
	for _, it := range items {
		// The rail only wears the accent while it is focused AND no drawer
		// is claiming the focused-surface indicator.
		rows = append(rows, chatItemRow(it, inner, hover, h, c.focus == focusSidebar && !c.drawerOpen())...)
	}
	return rows
}

// chatItemRow paints one sidebar entry as exactly h rows of exactly `inner`
// cells. Row 1 is the scan line — presence dot, name, time, unread badge;
// row 2 is the last-message preview plus a call glyph. The ACTIVE entry
// carries a tint plus an accent bar (bright when the rail owns focus, dim
// when it does not); hover repaints just the hovered name.
func chatItemRow(it chatItem, inner int, hover string, h int, focused bool) []string {
	if inner < 8 {
		inner = 8
	}
	// Presence: the room is always live, peers show online/offline, groups
	// wear the group glyph.
	presence := thPresenceStyle.Render("●")
	switch {
	case it.isRoom:
		presence = thPresenceStyle.Render("●")
	case it.tombstone:
		// Dead group: hollow group glyph in the resting tone, never the
		// live accent.
		presence = thPresenceOff.Render("▣")
	case it.isGroup:
		presence = thPresenceStyle.Render("▣")
	case it.live:
		presence = thPresenceStyle.Render("●")
	default:
		presence = thPresenceOff.Render("○")
	}

	name := it.name
	nameStyle := thChatNameStyle
	if it.unread > 0 {
		nameStyle = thChatActive
	}
	if (!it.live && !it.isRoom) || it.tombstone {
		nameStyle = tuiDimStyle
	}
	nameRendered := nameStyle.Render(name)
	if hover == it.peer && it.peer != "" && !it.active {
		nameRendered = thHoverRowStyle.Render(name)
	}

	badge := ""
	if it.unread > 0 {
		badge = " " + tuiUnreadStyle.Render(circledNum(it.unread))
	}
	timeRendered := thChatTimeStyle.Render(it.timeStr)

	// ---- identity line ---------------------------------------------------
	gap1 := inner - 1 - (1 + 1 + lipgloss.Width(nameRendered) + 1 + lipgloss.Width(timeRendered) + lipgloss.Width(badge))
	if gap1 < 1 {
		gap1 = 1
	}
	core1 := presence + " " + nameRendered + strings.Repeat(" ", gap1) + timeRendered + badge

	// ---- preview line ----------------------------------------------------
	icon := " "
	if it.inCall {
		icon = thSignalStyle.Render("◉")
	}
	preview := it.preview
	pvStyle := thPreviewStyle
	if it.unread > 0 && !it.isRoom {
		pvStyle = thPreviewUnread
	}
	if it.inCall && it.isRoom {
		pvStyle = thSignalStyle
	}
	pvRendered := pvStyle.Render(preview)
	gap2 := inner - 1 - (2 + lipgloss.Width(pvRendered) + 1 + lipgloss.Width(icon))
	if gap2 < 1 {
		gap2 = 1
	}
	core2 := "  " + pvRendered + strings.Repeat(" ", gap2) + icon

	// ---- selection / focus bar ------------------------------------------
	barChar := " "
	barStyle := thSidebarStyle
	if it.active {
		barChar = "┃"
		if focused {
			barStyle = thSelBarStyle
		} else {
			barStyle = lipgloss.NewStyle().Foreground(colEdge).Background(colSel)
		}
	}
	bar := barStyle.Render(barChar)
	rowBase := lipgloss.NewStyle()
	if it.active {
		rowBase = lipgloss.NewStyle().Background(colSel)
	}
	mkRow := func(core string) string {
		return bar + tintFit(rowBase, core, inner-1)
	}
	if h <= 1 {
		return []string{mkRow(core1)}
	}
	return []string{mkRow(core1), mkRow(core2)}
}

// syncRosterVp bakes the (scrollable) chat list into the roster viewport
// so wheel + scrollbar drags operate on live content. Content is rebuilt
// on hover/rebuild/roster changes — few rows, cheap.
func (c *chatScreen) syncRosterVp() {
	rowW := maxInt(c.sidebarInnerWidth()-1, 8)
	rows := c.chatItemRows(c.chatItems(), rowW, c.hoverPeer)
	y := c.rosterVp.YOffset
	c.rosterVp.Width = rowW
	c.rosterVp.SetContent(strings.Join(rows, "\n"))
	maxOff := c.rosterVp.TotalLineCount() - c.rosterVp.Height
	if maxOff < 0 {
		maxOff = 0
	}
	if y > maxOff {
		y = maxOff
	}
	c.rosterVp.SetYOffset(y) // preserve scroll across roster churn
}

// sidebarInnerWidth is the full width of the sidebar column (it carries no
// border — the tint and the rail do the framing).
func (c chatScreen) sidebarInnerWidth() int {
	l := c.layoutFor()
	if !l.sidebarOn {
		return rosterWidthInner
	}
	return l.sidebarWidth
}

// ---- async commands --------------------------------------------------------

// invitesPolledMsg resolves one GET /invites/mine poll (piggybacked on the
// 2s roster tick). Only NEW invite codes ring + badge.
type invitesPolledMsg struct {
	invites []groupInvite
	err     error
}

// pollInvitesCmd builds the invites poll for the roster tick cadence (nil
// on unwired screens). The settings window polls for itself while open.
func (c *chatScreen) pollInvitesCmd() tea.Cmd {
	if c.sig == nil {
		return nil
	}
	sig := &signalClient{serverURL: c.sig.serverURL, me: c.me, id: c.id}
	return func() tea.Msg {
		invites, err := sig.myInvites()
		return invitesPolledMsg{invites: invites, err: err}
	}
}

// applyInvites reconciles the pending-invite badge with server truth and
// rings the desk-based ping ONCE per invite code. The badge shows the live
// list length (declines/accepts shrink it on the next poll); the seen set
// gates the beeep, so re-polls never re-ring.
func (c *chatScreen) applyInvites(invites []groupInvite) {
	if c.seenInvites == nil {
		c.seenInvites = map[string]bool{}
	}
	n := 0
	for _, inv := range invites {
		if inv.Code == "" {
			continue
		}
		n++
		if !c.seenInvites[inv.Code] {
			c.seenInvites[inv.Code] = true
			grp := inv.GroupName
			if grp == "" {
				grp = "a group" // unnamed groups read naturally in the ping
			}
			if inviteNotifier != nil {
				inviteNotifier(inv.By, grp)
			}
		}
	}
	if n != c.pendingInvites {
		c.pendingInvites = n
		c.rebuildView()
	}
}

// scheduleRoster arms the 2s sidebar/invites/reactions tick.
func scheduleRoster() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return rosterTickMsg{} })
}

// drainNetCmd is a self-perpetuating event pump: it waits briefly for the
// next engine event and yields either it or netIdleMsg, and BOTH handlers
// re-arm the pump — so exactly one pump goroutine exists at all times and
// engine traffic always reaches Update within ~100ms. (A one-shot drain
// would strand later events in netCh forever: nothing else schedules it.)
func (c chatScreen) drainNetCmd() tea.Cmd {
	timer := c.drainTimer
	if timer == nil {
		timer = time.NewTimer(100 * time.Millisecond)
	} else {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(100 * time.Millisecond)
	}
	return func() tea.Msg {
		select {
		case m := <-c.netCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return m
		case <-timer.C:
			return netIdleMsg{}
		}
	}
}

func (c chatScreen) doSend(text, to string, seq int, q chatQuote, conv string) tea.Cmd {
	return func() tea.Msg {
		// engine.sendChatQuoted is synchronous: P2P encrypt+send, or inbox
		// seal+deposit. Map failures onto the legacy settle codes.
		msgId, err := c.eng.sendChatQuoted(to, text, q)
		if err == nil {
			return sendDoneMsg{text: text, to: to, seq: seq, msgId: msgId, code: 201, quote: q, conv: conv}
		}
		msg := err.Error()
		switch code := apiStatusCode(err); {
		case code == 429 || (code == 0 && strings.Contains(msg, "429")):
			return sendDoneMsg{text: text, to: to, seq: seq, code: 429, err: err, conv: conv}
		case code == 404 || code == 410 || strings.Contains(msg, "gone") ||
			strings.Contains(msg, "not in session") ||
			strings.Contains(msg, "Session not found"):
			return sendDoneMsg{text: text, to: to, seq: seq, code: 410, err: err, conv: conv}
		default:
			return sendDoneMsg{text: text, to: to, seq: seq, code: 500, err: err, conv: conv}
		}
	}
}

// doLeave guards the leave POST exactly-once per screen across the
// in-loop leave, repeat Ctrl+C presses, and the post-Run backup. The HOME
// session always leaves (groups have their own leave path).
func (c chatScreen) doLeave() tea.Cmd {
	return func() tea.Msg {
		if c.leftSent.Swap(true) {
			return leaveDoneMsg{}
		}
		if c.call != nil {
			c.call.stopAll() // withdraw publish announces + close the socket
		}
		c.eng.stop()
		if err := c.sig.leaveRoom(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: leave may not have registered (%v)\n", err)
		}
		return leaveDoneMsg{}
	}
}

// doLeaveGroup leaves the ACTIVE group server-side (reusing the standard
// leave POST), stops its engine and returns to the common room — the group
// leaves the sidebar live set and a tombstone row stays for the session
// (re-entry needs a fresh invite or code). The creator's crown transfers
// server-side; rooms vanish when the last member leaves.
func (c *chatScreen) doLeaveGroup(code string) tea.Cmd {
	g := c.groups[code]
	return func() tea.Msg {
		if g != nil {
			if err := g.sig.leaveRoom(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: group leave may not have registered (%v)\n", err)
			}
			g.eng.stop()
		}
		return groupLeftMsg{code: code}
	}
}

// lastAdminLeaveBlock returns the leave-refusal reason when the caller is
// the group's LAST admin: the crown must never be left ownerless by
// accident, so the leave is blocked until another admin is promoted. An
// empty string means the leave is allowed (members always are).
func (c *chatScreen) lastAdminLeaveBlock(code string) string {
	g := c.groups[code]
	if g == nil || g.eng == nil {
		return ""
	}
	admins := 0
	isAdminMe := false
	for _, m := range g.eng.peers() {
		if m.Role == "creator" || m.Role == "admin" {
			admins++
		}
		if m.Username == c.me && (m.Role == "creator" || m.Role == "admin") {
			isAdminMe = true
		}
	}
	if !isAdminMe || admins > 1 {
		return ""
	}
	return "you are the last admin — promote another admin first (/group-edit → Transfer admin)"
}

// leaveGroupCmd is the single leave path for the open group (/group-leave
// and Ctrl+C): it enforces the last-admin guard, then runs the standard
// leave. A blocked leave parks the reason on the status line and returns no
// command.
func (c *chatScreen) leaveGroupCmd(code string) tea.Cmd {
	if why := c.lastAdminLeaveBlock(code); why != "" {
		c.status = why
		c.rebuildView()
		return nil
	}
	return c.doLeaveGroup(code)
}

// dropGroupAs removes a group from the sidebar live set (its
// session ended or was left server-side), leaving a TOMBSTONE row that
// survives until a fresh attach; the local store is pruned in the same step
// so a restart can never resurrect the dead group. When the dropped group
// was the active view, the screen returns to the common room first.
// tombReason selects the tombstone's message ("left", "ended", "kicked",
// "full"); status, when non-empty, lands on the status line.
func (c *chatScreen) dropGroupAs(code, status, tombReason string) {
	g := c.groups[code]
	if g == nil {
		return
	}
	if c.activeGroup == code {
		c.exitGroup()
	}
	delete(c.groups, code)
	delete(c.lastGroupAt, code)
	if g.eng != nil {
		g.eng.stop()
	}
	c.noteGroupTombstone(code, g.name, tombReason)
	c.persistGroups() // prune: the row must never come back on restart
	if status != "" {
		c.status = status
	}
	c.rebuildView()
}

// noteGroupTombstone records the session-only placeholder row.
func (c *chatScreen) noteGroupTombstone(code, name, reason string) {
	if code == "" {
		return
	}
	if c.tombstones == nil {
		c.tombstones = map[string]groupTombstone{}
	}
	if name == "" {
		name = code
	}
	c.tombstones[code] = groupTombstone{code: code, name: name, reason: reason}
}

// shutdownSessions tears down every session this screen owns (groups +
// home). Called after the bubbletea Run returns; each engine's stop is
// idempotent, so a normal Ctrl+C leave that already stopped the home
// engine costs nothing. The local store is deliberately NOT touched here:
// a normal app exit must keep memberships for the next launch's restore.
func (c *chatScreen) shutdownSessions() {
	for code, g := range c.groups {
		if g.sig != nil {
			if err := g.sig.leaveRoom(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: group leave may not have registered (%v)\n", err)
			}
		}
		if g.eng != nil {
			g.eng.stop()
		}
		delete(c.groups, code)
	}
	if c.eng != nil {
		c.eng.stop()
	}
}

// attachGroup registers a joined group session: builds and starts its
// engine (tagged traffic keeps the sidebar fresh in the background) and
// remembers display state. The group password ("" for open groups) enables
// the engine's self-rejoin after a prune and is persisted locally so the
// row can auto-rejoin on the next launch.
//
// The call is idempotent per code: an existing group with a running engine
// is only refreshed (display meta + password), never double-attached; an
// optimistic restore row (engine still nil) is upgraded in place, keeping
// its password when the caller has none. attachGroup also clears any
// tombstone for the code: a fresh accept/join is exactly what re-establishes
// the live row.
//
// DISCOVERY/LIMITATION: the server exposes no "groups I belong to" list
// (src/ owns the API surface), so the sidebar's group set is derived
// client-side from creation, invites+accepts, and the local store restored
// at startup. Group meta (name/desc) rides the invite/create payloads and
// the PATCH response — the server has no meta GET, so members other than
// the editor see renames only after a rejoin.
func (c *chatScreen) attachGroup(code, name, desc string, sig *signalClient, password string) {
	if c.groups == nil {
		c.groups = map[string]*groupSession{}
	}
	if c.lastGroupAt == nil {
		c.lastGroupAt = map[string]time.Time{}
	}
	delete(c.tombstones, code) // a live attach supersedes the placeholder
	if existing := c.groups[code]; existing != nil {
		if name != "" {
			existing.name = name
		}
		existing.desc = desc
		if password == "" {
			password = existing.password // keep the saved secret on a meta-only refresh
		}
		existing.password = password
		existing.restoring = false
		if existing.eng != nil {
			return // already attached: never a second engine for one group
		}
		eng := c.newSessionEngine(sig)
		eng.joinPassword = password
		existing.sig = sig
		existing.eng = eng
		eng.start()
		c.persistGroups()
		return
	}
	eng := c.newSessionEngine(sig)
	eng.joinPassword = password
	c.groups[code] = &groupSession{code: code, name: name, desc: desc, sig: sig, eng: eng, password: password}
	c.lastGroupAt[code] = time.Now()
	eng.start()
	c.persistGroups()
}

// persistGroups snapshots the live group memberships for c.homeKey into the
// local store. A no-op when the store path is unwired (bare/test screens).
func (c *chatScreen) persistGroups() {
	if c.groupsPath == "" || c.homeKey == "" {
		return
	}
	list := make([]storedGroup, 0, len(c.groups))
	for _, g := range c.groups {
		if g == nil || g.restoring {
			continue // an unconfirmed restore row is not a membership yet
		}
		list = append(list, storedGroup{Code: g.code, Name: g.name, Desc: g.desc, Password: g.password})
	}
	if err := saveGroupsForRoom(c.groupsPath, c.homeKey, list); err != nil {
		fmt.Fprintf(os.Stderr, "warning: groups store not saved (%v)\n", err)
	}
}

// restorePersistedGroups paints this home room's saved memberships into the
// sidebar OPTIMISTICALLY (engine nil, preview "re-joining…") before any
// network work happens; restoreGroupsCmd then re-joins each in the
// background. Codes already present are skipped (no double-attach).
func (c *chatScreen) restorePersistedGroups() {
	if c.groupsPath == "" || c.homeKey == "" {
		return
	}
	saved := loadGroupsForRoom(c.groupsPath, c.homeKey)
	if len(saved) == 0 {
		return
	}
	if c.groups == nil {
		c.groups = map[string]*groupSession{}
	}
	if c.lastGroupAt == nil {
		c.lastGroupAt = map[string]time.Time{}
	}
	now := time.Now()
	for _, sg := range saved {
		if sg.Code == "" {
			continue
		}
		if _, ok := c.groups[sg.Code]; ok {
			continue // already attached (or restoring): avoid double-attach
		}
		name := sg.Name
		if name == "" {
			name = sg.Code
		}
		sig := &signalClient{serverURL: c.sig.serverURL, key: sg.Code, me: c.me, id: c.id}
		c.groups[sg.Code] = &groupSession{
			code: sg.Code, name: name, desc: sg.Desc,
			sig: sig, password: sg.Password, restoring: true,
		}
		c.lastGroupAt[sg.Code] = now
	}
}

// restoreGroupsCmd arms the background signed re-join for every optimistic
// row (nil when nothing was restored). Each command resolves to a
// groupRestoreDoneMsg.
func (c chatScreen) restoreGroupsCmd() tea.Cmd {
	var cmds []tea.Cmd
	for _, g := range c.groups {
		if g == nil || !g.restoring {
			continue
		}
		g := g
		cmds = append(cmds, func() tea.Msg {
			pk := ""
			if c.id != nil {
				pk = pubkeyB64(c.id)
			}
			_, _, err := g.sig.joinRoom(c.me, pk, g.password)
			return groupRestoreDoneMsg{code: g.code, err: err}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// restoreFailure classifies a startup re-join error: terminal failures
// (room gone, kicked, full, password rejected) drop the row and prune the
// store; transport/5xx failures keep the optimistic row ("re-joining…") so
// a server outage at launch never wipes real memberships.
func restoreFailure(err error) (reason string, terminal bool) {
	if err == nil {
		return "", false
	}
	if isServerDown(err) {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case apiStatusCode(err) == 404:
		return "ended", true
	case apiStatusCode(err) == 403 && strings.Contains(msg, "kicked"):
		return "kicked", true
	case apiStatusCode(err) == 403 && strings.Contains(msg, "maximum allowance"):
		return "full", true
	case apiStatusCode(err) == 401 && strings.Contains(msg, "password"):
		return "password", true
	default:
		return "", false
	}
}

// groupEndEvent classifies an engine error against a group expected to be
// dead: "session ended" (room gone), kicked (rejoin banned), full (rejoin
// 403 max allowance). ok=false keeps the ordinary error path.
func groupEndEvent(err error) (tombReason string, ok bool) {
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "session ended"):
		return "ended", true
	case strings.Contains(low, "kicked"):
		return "kicked", true
	case strings.Contains(low, "maximum allowance"):
		return "full", true
	}
	return "", false
}

// applyGroupMeta plumbs a successful PATCH /meta response into the local
// group session and repaints the sidebar immediately (the editor sees the
// rename at once). REFRESH LIMITATION: the server exposes no GET for group
// meta, so other members keep the name learned at invite/create until they
// rejoin — there is nothing for the roster/beat tick to re-read. If a meta
// fetch ever lands, re-read it there and update the session from here.
func (c *chatScreen) applyGroupMeta(code, name, desc string) {
	g := c.groups[code]
	if g == nil {
		return
	}
	if name != "" {
		g.name = name
	}
	g.desc = desc
	c.persistGroups()
	c.rebuildView()
}

// dissolveGroup applies a creator DELETE locally: the row drops to a
// tombstone, the store is pruned, and the status line states what happened
// for everyone.
func (c *chatScreen) dissolveGroup(code, name string) {
	if g := c.groups[code]; g != nil && name == "" {
		name = g.name
	}
	if name == "" {
		name = code
	}
	c.dropGroupAs(code, "", "ended")
	c.status = "group " + name + " dissolved for everyone"
	c.rebuildView()
}

// ---- tea.Model -------------------------------------------------------------

func (c chatScreen) Init() tea.Cmd {
	// No backlog (the server keeps no transcript), no WS upgrade, no beat
	// tick (the engine owns heartbeats): drain engine events, refresh the
	// sidebar roster on a slow tick, and fire the optimistic group restore
	// (no-op when no store path is wired or nothing was saved).
	return tea.Batch(c.drainNetCmd(), scheduleRoster(), c.restoreGroupsCmd())
}

// markSeen records an inbound msgId, reporting true on repeats (skip
// display, but the caller must still ack — the sender retries until it
// hears back). First sighting returns false (display it).
func (c *chatScreen) markSeen(msgId string) bool {
	if c.seenMsg == nil {
		c.seenMsg = newSeenSet(5000)
	}
	if msgId == "" {
		return false // engine drops id-less frames; display defensively
	}
	return c.seenMsg.seen("msg:" + msgId)
}

func (c *chatScreen) handleNewMessage(m chatMessage) {
	c.addMessage(m)
	// @mention receipts (general room only): a desktop ping whenever an
	// inbound message names us — ALWAYS, even while the room is focused.
	// Own sends never ping and DMs are excluded entirely; the body carries
	// exactly "<sender> mentioned you in the chat.", no message excerpt.
	if c.me != "" && m.Username != c.me && isGeneralConv(m.ConvID) &&
		mentionedIn(m.Text, c.me) {
		mentionNotifier(m.Username)
	}
}

// ---- media publishing (/audio voice calls) ------------------------------------

// currentScope resolves "wherever the user is": their DM peer, or the
// room's online members.
func (c *chatScreen) currentScope() []string {
	if c.call == nil {
		return nil
	}
	var room []string
	if c.targetUser == "" && c.eng != nil {
		room = onlineNames(c.eng.peers(), c.me)
	}
	return c.call.scopeFor(c.targetUser, room)
}

// toggleAudio runs the /audio command: publish/stop mic to the scope.
func (c *chatScreen) toggleAudio() tea.Cmd {
	if c.call == nil {
		c.status = "media unavailable here"
		return nil
	}
	if err := c.call.ToggleAudio(c.currentScope()); err != nil {
		c.status = "audio failed: " + err.Error()
	}
	c.rebuildView()
	return nil
}

// callActive reports whether this client is in a call right now:
// publishing mic or receiving a remote feed.
func (c *chatScreen) callActive() bool {
	return c.call != nil && c.call.MediaActive()
}

// trackCallEdge latches the call start wall-clock on the idle→live edge
// and releases it when the call ends, driving the call timer honestly.
func (c *chatScreen) trackCallEdge() {
	if c.callActive() {
		if c.callStart.IsZero() {
			c.callStart = time.Now()
		}
	} else if !c.callStart.IsZero() {
		c.callStart = time.Time{}
	}
}

// callElapsed formats the live call duration (HH:MM:SS) or a placeholder.
func (c *chatScreen) callElapsed() string {
	if c.callStart.IsZero() {
		return "--:--:--"
	}
	d := time.Since(c.callStart)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// callParties counts distinct call participants: self (when live) plus all
// audio publishers.
func (c *chatScreen) callParties() int {
	seen := map[string]bool{}
	n := 0
	if c.callActive() {
		seen[c.me] = true
		n = 1
	}
	if c.call != nil {
		for _, p := range c.call.AudioPublishers() {
			if !seen[p] {
				seen[p] = true
				n++
			}
		}
	}
	return n
}

// peerInCall reports whether a peer has a live audio feed right now,
// or is the current target of our own publishing.
func (c *chatScreen) peerInCall(peer string) bool {
	if peer == "" || c.call == nil {
		return false
	}
	for _, p := range c.call.AudioPublishers() {
		if p == peer {
			return true
		}
	}
	return peer == c.targetUser && c.call.AudioOn()
}

// enterPrivate switches to a 1:1 thread. There is no server history to
// deep-fetch (live messages only) — switching is instant. DMs belong to the
// HOME session: entering one from inside a group first exits the group
// view (the engines are swapped back), so a DM send can never ride the
// group mesh into the wrong thread.
func (c *chatScreen) enterPrivate(user string) tea.Cmd {
	c.exitGroup() // no-op unless a group view is open
	c.targetUser = user
	c.palette.close() // stale "/" query must not survive a mode switch
	c.mention.close() // the "@" dropdown is general-room only
	// The anchored aux rows do not belong to the new thread.
	c.pendingReactionMsgId = ""
	c.detailMsgId, c.detailEmoji = "", ""
	c.closeReplyMenu()
	c.closeReplyPick()
	delete(c.unread, user) // opening the thread clears its badge
	c.rebuildView()
	return nil
}

// exitPrivate returns to the common room; returns a system line or "".
func (c *chatScreen) exitPrivate() {
	if c.targetUser == "" {
		return
	}
	c.targetUser = ""
	c.palette.close()
	c.mention.close() // the "@" dropdown stays with the general room
	c.pendingReactionMsgId = ""
	c.detailMsgId, c.detailEmoji = "", ""
	c.roomUnread = 0 // back in the room: everything is visible again
	c.closeReplyMenu()
	c.closeReplyPick()
	c.rebuildView()
}

// exitConv leaves whichever conversation view is open (a DM thread or a
// group) back to the common room. Each exit is a no-op when inactive, so
// this is safe to call unconditionally (General-row click, quote-jump).
func (c *chatScreen) exitConv() {
	c.exitPrivate()
	c.exitGroup()
}

// exitGroup returns from a group conversation to the common room without
// leaving the group: the session stays joined and its engine keeps running
// in the background (unread/preview stay fresh). Esc and clicking General
// use this; Ctrl+C in a group actually LEAVES it (doLeaveGroup).
func (c *chatScreen) exitGroup() {
	if c.activeGroup == "" {
		return
	}
	c.activeGroup = ""
	c.key, c.sig, c.eng = c.homeKey, c.homeSig, c.homeEng
	c.targetUser = ""
	c.palette.close()
	c.mention.close() // the "@" dropdown is home-room only
	c.pendingReactionMsgId = ""
	c.detailMsgId, c.detailEmoji = "", ""
	c.roomUnread = 0 // back in the room: everything is visible again
	c.closeReplyMenu()
	c.closeReplyPick()
	c.composerQuote = nil // the pinned citation belonged to the group view
	c.syncRosterFromEngine()
	c.rebuildView()
}

// openGroup switches the active conversation to a joined group session: the
// group's client/engine become the screen's active ones, the transcript
// re-filters to the group bucket, and the group's unread clears. Media is
// home-session-bound, so a live call stops before the switch.
func (c *chatScreen) openGroup(code string) tea.Cmd {
	g := c.groups[code]
	if g == nil {
		return nil
	}
	if c.call != nil {
		c.call.stopAll()
	}
	c.exitPrivate() // any open DM thread closes (no-op in the room)
	c.activeGroup = code
	c.key, c.sig, c.eng = code, g.sig, g.eng
	g.unread = 0 // opening the group clears its badge
	c.palette.close()
	c.mention.close()
	c.pendingReactionMsgId = ""
	c.detailMsgId, c.detailEmoji = "", ""
	c.closeReplyMenu()
	c.closeReplyPick()
	c.composerQuote = nil // quotes are conversation-scoped
	delete(c.unread, code)
	c.syncRosterFromEngine()
	c.rebuildView()
	return nil
}

// ─── floating reply menu ────────────────────────────────────────────────────

// openReplyMenu pops the floating Reply / Reply-Privately menu near a
// transcript click (right mouse button or a committed /reply pick). Own
// messages offer Reply only. The menu is a small boxed panel right-aligned
// to the request point, clamped inside the main column; geometry and items
// are fixed here so paint, keyboard and click hit-testing stay in sync.
func (c *chatScreen) openReplyMenu(m chatMessage, x, y int) {
	items := []string{"Reply"}
	if m.Username != c.me {
		items = append(items, "Reply-Privately")
	}
	inner := 0
	for _, it := range items {
		if w := lipgloss.Width(it); w > inner {
			inner = w
		}
	}
	inner += 4 // breathing room inside the panel
	l := c.layoutFor()
	if maxInner := maxInt(l.vpWidth-4, 6); inner > maxInner {
		inner = maxInner
	}
	w := inner + 2 // borders
	h := len(items) + 2
	x0 := transcriptX0(l) + 1
	menuX := x - w + 1 // right edge at the click
	if menuX < x0 {
		menuX = x + 1 // no room: hang off the click's left instead
	}
	if menuX < x0 {
		menuX = x0
	}
	if menuX+w > c.width {
		menuX = c.width - w
	}
	if menuX < x0 {
		menuX = x0
	}
	menuY := y
	if menuY+h > c.height {
		menuY = c.height - h
	}
	if menuY < 0 {
		menuY = 0
	}
	c.closeReactionAux()
	c.closeReplyPick()
	c.replyMenu = &replyMenuState{msgId: m.MsgId, items: items, sel: 0, x: menuX, y: menuY, w: w, h: h}
}

// closeReplyMenu dismisses the floating menu. Stale activations are
// impossible: every entry path re-reads c.replyMenu under Update's single
// goroutine.
func (c *chatScreen) closeReplyMenu() {
	c.replyMenu = nil
}

// replyMenuMove steps the menu highlight with wrap-around.
func (c *chatScreen) replyMenuMove(step int) {
	if c.replyMenu == nil {
		return
	}
	n := len(c.replyMenu.items)
	if n <= 0 {
		return
	}
	c.replyMenu.sel = ((c.replyMenu.sel+step)%n + n) % n
}

// replyMenuItemAt maps a click onto a menu item row: (index, true) inside
// an item, (-1, false) on the border or off the panel.
func (c *chatScreen) replyMenuItemAt(x, y int) (int, bool) {
	m := c.replyMenu
	if m == nil || y < m.y || y >= m.y+m.h || x < m.x || x >= m.x+m.w {
		return -1, false
	}
	row := y - m.y
	if row == 0 || row == m.h-1 {
		return -1, false // border rows are not items
	}
	return row - 1, true
}

// replyMenuActivate runs the highlighted menu item: Reply pins the quote
// card above the composer; Reply-Privately pins it and opens the DM with
// the target's owner, carrying the card into that composer.
func (c *chatScreen) replyMenuActivate() tea.Cmd {
	m := c.replyMenu
	if m == nil {
		return nil
	}
	target, ok := c.msgById(m.msgId)
	if !ok || m.sel < 0 || m.sel >= len(m.items) {
		c.closeReplyMenu()
		return nil
	}
	switch m.items[m.sel] {
	case "Reply":
		c.pinComposerQuote(target)
		c.closeReplyMenu()
		c.focus = focusComposer
		return nil
	case "Reply-Privately":
		peer := target.Username
		if gc := groupCodeOf(target.ConvID); gc == "" && target.ConvID != "" && target.ConvID != generalConv {
			if p := peerOf(c.me, target.ConvID); p != "" && p != c.me {
				peer = p // reply targets the thread's other participant
			}
		}
		if peer == "" || peer == c.me {
			c.closeReplyMenu()
			return nil
		}
		// DMs belong to the HOME session: a group view must close first,
		// or the DM would ride the group engine into the wrong thread.
		c.exitGroup()
		c.pinComposerQuote(target)
		c.closeReplyMenu()
		return c.enterPrivate(peer)
	}
	return nil
}

// replyMenuBlock paints the menu panel: one row per item inside the shared
// palette box, the selected row wearing the palette's accent chip.
func (c *chatScreen) replyMenuBlock() []string {
	m := c.replyMenu
	inner := m.w - 2
	rows := make([]string, 0, len(m.items))
	for i, item := range m.items {
		line := padVisible(item, inner)
		if i == m.sel {
			line = tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle))
		}
		rows = append(rows, line)
	}
	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(rows, "\n"))
	return strings.Split(panel, "\n")
}

// overlayReplyMenu splices the floating menu into a painted terminal frame
// near its anchor: the panel replaces the cells it covers, the rest of each
// row keeps its own paint. The menu is transient — any repaint that
// dismisses it restores the untouched rows.
func (c *chatScreen) overlayReplyMenu(out []string, l layout) {
	if c.replyMenu == nil || len(out) == 0 {
		return
	}
	m := c.replyMenu
	for i, r := range c.replyMenuBlock() {
		y := m.y + i
		if y < 0 || y >= len(out) {
			continue
		}
		row := out[y]
		head := truncateByWidth(row, m.x)
		if lipgloss.Width(head) < m.x {
			head += strings.Repeat(" ", m.x-lipgloss.Width(head))
		}
		merged := head + r
		if w := lipgloss.Width(row); m.x+m.w < w {
			merged += ansi.Cut(row, m.x+m.w, w)
		} else if lipgloss.Width(merged) < c.width {
			merged += strings.Repeat(" ", c.width-lipgloss.Width(merged))
		}
		out[y] = merged
	}
}

// ─── pinned composer quote card ─────────────────────────────────────────────

// pinComposerQuote pins the WhatsApp-style citation card above the
// composer: an accent bar, the quoted author and a capped excerpt, with an
// X at the right edge. The next send attaches it to the chat frame.
func (c *chatScreen) pinComposerQuote(m chatMessage) {
	if m.MsgId == "" {
		return
	}
	c.composerQuote = &chatQuote{
		ReplyTo:      m.MsgId,
		ReplyAuthor:  m.Username,
		ReplyExcerpt: replyExcerptOf(m.Text),
	}
	c.closeReactionAux()
	c.closeReplyMenu()
	c.closeReplyPick()
	c.rebuildView()
}

// clearComposerQuote dismisses the pinned citation (the X click).
func (c *chatScreen) clearComposerQuote() {
	if c.composerQuote == nil {
		return
	}
	c.composerQuote = nil
	c.rebuildView()
}

// quoteComposerRow paints the single-row pinned citation card above the
// composer, full column width on the panel tint, X at the right edge.
func (c *chatScreen) quoteComposerRow(colW int) string {
	q := c.composerQuote
	if q == nil {
		return ""
	}
	bar := lipgloss.NewStyle().Foreground(colAccent).Render("▎")
	name := lipgloss.NewStyle().Bold(true).Foreground(avatarColorFor(q.ReplyAuthor)).Render(q.ReplyAuthor)
	excerpt := lipgloss.NewStyle().Foreground(colText).Render(sanitizeDisplay(q.ReplyExcerpt))
	body := bar + "  Reply to " + name + ": " + excerpt
	x := thQuoteXStyle.Render("  ✕")
	inner := maxInt(colW-3, 1) // keep the X visible at the right edge
	if lipgloss.Width(body) > inner {
		body = truncateByWidth(body, inner)
	}
	row := body + strings.Repeat(" ", inner-lipgloss.Width(body)) + x
	return tintFit(thQuoteCardStyle, row, colW)
}

// ─── quote-click jump ───────────────────────────────────────────────────────

// notifyReplyHandler is the desktop-ping entry point for inbound replies
// to my messages; tests swap it to capture the exact body.
var notifyReplyHandler = notifyReplyTo

// notifyIfRepliedToMe pings the quoted author — me — when an inbound
// message carries a citation of one of mine: EXACT body "<from> replied to
// you". Self-replies (sender quoting their own message) never notify.
func (c *chatScreen) notifyIfRepliedToMe(m chatMessage) {
	if m.ReplyTo == "" || m.ReplyAuthor != c.me {
		return
	}
	if m.Username == "" || m.Username == m.ReplyAuthor {
		return // self-reply: quoting your own message must stay silent
	}
	if notifyReplyHandler != nil {
		notifyReplyHandler(m.Username)
	}
}

// jumpToQuoted resolves a quote card click: switch to the conversation
// that owns the quoted message, scroll it to the top of the viewport, and
// paint its ENTIRE row blue for quoteJumpTicks seconds. When the original
// was trimmed from history, an inline note says so instead. Returns the
// expiry-tick command (nil when there is nothing to highlight).
func (c *chatScreen) jumpToQuoted(m chatMessage) tea.Cmd {
	target := -1
	for i := range c.history {
		if c.history[i].MsgId == m.ReplyTo {
			target = i
			break
		}
	}
	if target < 0 {
		c.appendLocal(c.activeConv(), tuiSystemStyle.Render("original message no longer in view"))
		return nil
	}
	t := c.history[target]
	// Mode switch to the quoted message's conversation ("jumps to general
	// chat" when the original lives in the room; a quoted group message
	// opens that group; a quoted DM thread opens the peer).
	if t.ConvID != c.activeConv() {
		switch {
		case t.ConvID == "" || t.ConvID == generalConv:
			c.exitConv()
		default:
			if gc := groupCodeOf(t.ConvID); gc != "" {
				c.openGroup(gc) // exits whatever view was open
			} else if peer := peerOf(c.me, t.ConvID); peer != "" && peer != c.me {
				c.enterPrivate(peer)
			}
		}
	}
	if c.quoteJump == nil {
		c.quoteJump = &quoteJumpState{}
	}
	c.quoteJumpCounter++
	c.quoteJump.gen = c.quoteJumpCounter // supersede every expiry tick still in flight
	c.quoteJump.msgId = t.MsgId
	c.quoteJump.left = quoteJumpTicks
	c.closeReplyMenu()
	c.closeReplyPick()
	c.rebuildView()
	if row := c.firstRowOfMsg(t.MsgId); row >= 0 {
		c.vp.SetYOffset(row) // quoted message lands at the viewport top
	}
	return scheduleQuoteJumpTick(c.quoteJump.gen)
}

// quoteJumpRowAt reports whether a transcript click row is the citation
// card row of a quoted message (a click there jumps instead of opening the
// reaction picker).
func (c *chatScreen) quoteJumpRowAt(y int, l layout) bool {
	top := transcriptTopRow(l)
	if l.vpHeight <= 0 || y < top || y >= top+l.vpHeight {
		return false
	}
	row := y - top + c.vp.YOffset
	return row >= 0 && row < len(c.rowCard) && c.rowCard[row]
}

// submitLine handles one committed input line. It returns the tea.Cmd that
// performs the network send (nil for local-only commands). CRITICAL: the
// caller MUST append this command — it is the ONLY thing that actually puts
// the message on the wire. Registry commands are executed here too, so a
// typed "/help" behaves exactly like one picked from the palette.
func (c *chatScreen) submitLine(text string) tea.Cmd {
	// Commands take arguments now ("/kick bob"): match the first token
	// against the registry (case-insensitive), pass the rest through in
	// original case — usernames are case-sensitive.
	if fields := strings.Fields(strings.TrimSpace(text)); len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		name := strings.ToLower(fields[0])
		arg := ""
		if len(fields) > 1 {
			arg = strings.Join(fields[1:], " ")
		}
		for _, cmd := range slashCommands {
			if name == cmd.Name {
				return c.runCommand(name, arg)
			}
		}
	}
	if c.pending != nil {
		// Cap the queue: offline/slow peers plus fast typing must not
		// grow memory without bound. Oldest queued line drops first.
		if len(c.outbox) >= maxOutbox {
			c.outbox = c.outbox[1:]
			c.status = "outbox full — oldest queued message dropped"
		}
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
// targets the peer that conversation represents (empty conv => broadcast;
// group buckets always broadcast inside their own session).
func (c *chatScreen) dispatchInConv(conv, text string) tea.Cmd {
	var peer string
	if conv != generalConv && groupCodeOf(conv) == "" {
		parts := strings.Split(conv, "|")
		for _, u := range parts {
			if u != c.me {
				peer = u
			}
		}
	}
	// The pinned citation rides on this send: capture it (and clear the
	// card) BEFORE the optimistic echo paints, so a queued second send
	// cannot inherit a quote it never asked for.
	q := chatQuote{}
	if c.composerQuote != nil {
		q = *c.composerQuote
	}
	// Optimistic echo: the same block a confirmed message will paint, held
	// faint until the delivery ack lands (netDeliveredMsg).
	echo := chatBubble(text, true, true, c.transcriptW())
	if q.ReplyTo != "" {
		echo = alignBlock(c.quoteCardBlock(q, true, c.transcriptW()), true, c.transcriptW()) + "\n" + echo
	}
	c.pushLocalLine(localLine{conv: conv, text: echo})
	c.pending = &pendingSend{text: text, conv: conv, to: peer, localIdx: len(c.localLines) - 1}
	target := peer
	if target == "" && c.pending.conv == generalConv {
		target = ""
	}
	seq := c.allocSeq()
	c.composerQuote = nil // WhatsApp-style: one send consumes the citation
	c.rebuildView()
	return c.doSend(text, target, seq, q, conv)
}

// syncViewport re-derives viewport/composer geometry from the live layout.
// Drawer open/close, status lines, and sidebar collapse all change the
// transcript geometry without any resize event; without this, caches, wraps,
// and scroll math run on stale dims until the next terminal resize.
func (c *chatScreen) syncViewport() {
	if c.width <= 0 || c.height <= 0 {
		return
	}
	l := c.layoutFor()
	// A pane that just left the screen cannot keep the focus indicator:
	// the rail collapses on narrow terminals, so hand focus back to the
	// composer rather than pointing it at a pane that paints nothing.
	if c.focus == focusSidebar && !l.sidebarOn {
		c.focus = focusComposer
	}
	vpW := l.vpWidth
	// Chat list gets its own viewport (scrollable like the transcript);
	// the filter header row lives outside the viewport.
	if l.sidebarOn {
		fill := c.sidebarFill(l)
		c.rosterVp.Width = maxInt(c.sidebarInnerWidth()-1, 8) // rail column
		// Same inner width the painter uses (rosterBody): a divergent value
		// desyncs the header row count and misroutes roster clicks.
		c.rosterVp.Height = maxInt(fill-searchHeightFor(l.headRows, c.sidebarInnerWidth()), 1)
	}
	atBottom := c.vp.AtBottom()
	offset := c.vp.YOffset
	changed := vpW != c.vp.Width || l.vpHeight != c.vp.Height
	c.vp.Width, c.vp.Height = vpW, l.vpHeight
	// Composer input spans the full column width: shrink the field so typed
	// text scrolls inside the box instead of under its border.
	transcriptOuter := l.vpWidth + 2
	inputOuter := transcriptOuter
	if want := maxInt(inputOuter-6, 1); c.input.Width != want {
		c.input.Width = want
	}
	c.syncRosterVp()
	if changed {
		if len(c.history) > 0 || len(c.localLines) > 0 {
			c.rebuildView()
		} else {
			c.refreshViewport()
		}
		if atBottom {
			c.vp.GotoBottom()
		} else {
			c.vp.SetYOffset(offset)
		}
	}
}

// callTickMsg refreshes the live call timer + participant counts.
type callTickMsg struct{}

func scheduleCallTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return callTickMsg{} })
}

func (c chatScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	c.syncViewport() // drawer/status/sidebar changes alter geometry with no resize event
	hadCall := !c.callStart.IsZero()
	c.trackCallEdge() // latch/release the call timer on the idle/live edge
	if c.callActive() && !hadCall {
		cmds = append(cmds, scheduleCallTick()) // timer ticks only while live
	}

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.hoverPeer = "" // geometry changed; stale hover is meaningless
		c.syncViewport() // re-derives vp dims + input width, re-wraps on change

	case callTickMsg:
		if c.callActive() {
			c.rebuildView() // fresh timer, counts, call icons
			cmds = append(cmds, scheduleCallTick())
		}

	case reloadPickerMsg:
		// Debounced filesystem re-read: fires ~150ms after the picker's
		// filter committed. The stale listing painted with the spinner the
		// whole time — never a flash of empty. A navigation mid-wait bumps
		// reloadGen, so a stale gen is dropped silently.
		if c.picker.isActive() && msg.gen == c.picker.reloadGen {
			c.picker.loading = false
			c.loadPickerDir()
		}
		cmds = append(cmds, c.drainNetCmd())

	case pickerTickMsg:
		// Spinner frames while a debounced reload is in flight.
		if c.picker.isActive() && c.picker.loading {
			c.picker.spin++
			cmds = append(cmds, tea.Tick(pickerTickStep, func(time.Time) tea.Msg { return pickerTickMsg{} }))
		}
		cmds = append(cmds, c.drainNetCmd())

	case rosterTickMsg:
		// Sidebar freshness from the engine's heartbeat roster (the engine
		// owns the 5s beat; this only renders, every 2s). Membership lives
		// ONLY in the Online sidebar — no join/leave lines in the
		// transcript by product direction.
		c.syncRosterFromEngine()
		// Server watchdog: while heartbeats fail with the server gone,
		// hold the down alert on the status line; clear it on recovery.
		// Guarded: bare test screens carry no engine.
		if c.eng != nil {
			if isServerDown(c.eng.beatErr()) {
				c.status = serverDownMsg
			} else if c.status == serverDownMsg {
				c.status = ""
			}
		}
		// Delivery-receipt sweep: warn on messages unacked past
		// unconfirmedAfter; forget entries past receiptExpiry (the engine's
		// own retries are long over by then — most likely a lost ack frame,
		// not a lost message). Evict repaints so dimming lifts.
		if len(c.unackedUI) > 0 {
			now := time.Now()
			stale := 0
			for id, at := range c.unackedUI {
				age := now.Sub(at)
				if age > receiptExpiry {
					delete(c.unackedUI, id)
					c.evictRenderCache(id)
				} else if age > unconfirmedAfter {
					stale++
				}
			}
			if stale > 0 {
				c.status = fmt.Sprintf("* %d message(s) unconfirmed — still retrying", stale)
			} else if strings.HasPrefix(c.status, "* ") && strings.Contains(c.status, "unconfirmed") {
				c.status = ""
			}
		} else if strings.HasPrefix(c.status, "* ") && strings.Contains(c.status, "unconfirmed") {
			c.status = ""
		}
		// Reaction truth rides the same 2s cadence: one narrowed GET for
		// the active conversation's newest messages. This is what makes
		// peer reactions (and lost P2P nudges) eventually consistent.
		if cmd := c.fetchReactionsCmd(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		// Invites ride the same tick: a fresh pending invite rings the
		// desktop ping once and lights the settings badge. The settings
		// window polls for itself while open, so this never double-fires.
		if cmd := c.pollInvitesCmd(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		cmds = append(cmds, scheduleRoster())

	case invitesPolledMsg:
		if msg.err == nil {
			c.applyInvites(msg.invites)
		}

	case groupLeftMsg:
		name := msg.code
		if g := c.groups[msg.code]; g != nil && g.name != "" {
			name = g.name
		}
		// The live row drops and a tombstone stays for the session; the
		// store is pruned so a restart never ghosts the left group.
		c.dropGroupAs(msg.code, "", "left")
		c.status = "You left group " + name + "."
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case groupRestoreDoneMsg:
		g := c.groups[msg.code]
		if g == nil || !g.restoring {
			break // superseded by an accept/attach while the re-join was in flight
		}
		if msg.err != nil && apiStatusCode(msg.err) != 409 {
			// 409 "Username already taken" means the seat SURVIVED the
			// restart (rooms outlive clients): that is a live membership,
			// handled by the success path below. Every other error is
			// classified: terminal failures drop the row + prune the store;
			// transient ones (server down, 5xx) keep the optimistic row so
			// an outage at launch never wipes real memberships.
			reason, terminal := restoreFailure(msg.err)
			if !terminal {
				c.status = "could not re-join group " + g.name + " (" + msg.err.Error() + ")"
				c.rebuildView()
				break
			}
			name := g.name
			delete(c.groups, msg.code)
			delete(c.lastGroupAt, msg.code)
			if g.eng != nil {
				g.eng.stop()
			}
			c.persistGroups() // prune: the dead membership must not come back
			switch reason {
			case "kicked":
				c.status = "you were removed from group " + name
			case "full":
				c.status = "could not re-join group " + name + " — it is full"
			case "password":
				c.status = "could not re-join group " + name + " — the saved password no longer works"
			default:
				c.status = "group " + name + " ended — removed from the sidebar"
			}
			c.rebuildView()
			break
		}
		g.restoring = false
		if g.eng == nil {
			eng := c.newSessionEngine(g.sig)
			eng.joinPassword = g.password
			g.eng = eng
			eng.start()
		}
		c.persistGroups()
		cmds = append(cmds, c.drainNetCmd())

	case netRosterMsg:
		// Engine beat learned membership moved (join/leave, possibly via
		// an epoch-triggered refresh): same sync, right now. Re-arm the
		// pump like every other net case — without this the event loop
		// strands after the first roster push.
		c.syncRosterFromEngine()
		cmds = append(cmds, c.drainNetCmd())

	case kickDoneMsg:
		if msg.err != nil {
			if isServerDown(msg.err) {
				c.status = serverDownMsg
			} else {
				c.status = "kick failed: " + msg.err.Error()
			}
		} else {
			c.eng.applyPushedRoster(msg.roster, msg.epoch)
			c.status = "kicked " + msg.target
			c.syncRosterFromEngine()
		}
		cmds = append(cmds, c.drainNetCmd())

	case roleDoneMsg:
		what := "is now an admin"
		if !msg.admin {
			what = "is no longer an admin"
		}
		if msg.err != nil {
			if !isServerDown(msg.err) {
				c.status = "admin change failed: " + msg.err.Error()
			} else {
				c.status = serverDownMsg
			}
		} else {
			c.eng.applyPushedRoster(msg.roster, msg.epoch)
			c.status = msg.target + " " + what
			c.syncRosterFromEngine()
		}
		cmds = append(cmds, c.drainNetCmd())

	case netChatMsg:
		m := msg.chat
		// Receipt already acked synchronously at queue time (see onChat):
		// display dedups here, never re-acks. Background session traffic
		// (a joined group while another conversation is active) flows
		// through the same pipeline: the message lands in its own
		// conversation bucket, badges the group row, and stays out of the
		// active transcript until that group opens.
		if c.markSeen(m.MsgId) {
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		cid := convFor(m.From, m.To)
		if msg.key != "" && msg.key != c.homeKey {
			cid = groupConv(msg.key) // group broadcast still reads To==""
		}
		cm := chatMessage{
			Seq: c.allocSeq(), MsgId: m.MsgId, Username: m.From, Kind: "chat",
			Text: m.Text, To: m.To,
			ConvID:    cid,
			CreatedAt: time.Now().Format(time.RFC3339),
			ReplyTo:   m.ReplyTo, ReplyAuthor: m.ReplyAuthor, ReplyExcerpt: m.ReplyExcerpt,
		}
		c.notifyIfRepliedToMe(cm)
		c.handleNewMessage(cm)
		cmds = append(cmds, c.drainNetCmd())

	case netFileMsg:
		f := msg.file
		// Retry copies of an already-carded file (same msgId) are
		// swallowed: the bytes are already saved and displayed. Stream
		// completions carry unique ids, so only true duplicates collapse.
		if c.markSeen(f.MsgId) {
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		// Peer-controlled filename: sanitize before it touches the
		// transcript, the drawer, or the filesystem-adjacent path display.
		f.Filename = sanitizeDisplay(f.Filename)
		c.received = append(c.received, receivedFile{
			filename: f.Filename, from: f.From, size: f.Size, path: f.Path, at: time.Now(),
		})
		// Live drawer: arrivals while browsing appear at once (newest
		// first), or the "appear here automatically" notice lies.
		if c.picker.active && c.picker.mode == modeFiles {
			c.picker.files = append([]receivedFile{{
				filename: f.Filename, from: f.From, size: f.Size, path: f.Path, at: time.Now(),
			}}, c.picker.files...)
			c.picker.notice = ""
			c.picker.clampCursor()
		}
		// Bound the drawer source: marathon sessions must not grow it
		// without limit (oldest fall off; files stay saved on disk).
		if len(c.received) > maxReceivedFiles {
			c.received = append([]receivedFile(nil), c.received[len(c.received)-maxReceivedFiles:]...)
		}
		conv := convFor(f.From, f.To)
		if msg.key != "" && msg.key != c.homeKey {
			conv = groupConv(msg.key) // group files are session broadcasts
		}
		ts := time.Now()
		// Mirror the text path: DM arrivals always bump recency; room
		// files arriving in a thread view count room-unread instead of
		// vanishing silently. Group files skip the DM maps (their unread
		// lives on the group session, bumped below).
		if groupCodeOf(conv) == "" && conv != generalConv {
			if c.lastDMAt == nil {
				c.lastDMAt = map[string]time.Time{}
			}
			if peer := peerOf(c.me, conv); peer != "" {
				c.lastDMAt[peer] = ts
			}
		} else if c.activeConv() != generalConv {
			c.roomUnread++
		}
		c.pushLocalLine(localLine{
			conv: conv,
			kind: lineFileCard,
			fileData: &fileCardData{
				filename:  f.Filename,
				username:  f.From,
				size:      humanSize(f.Size),
				time:      ts.Format("15:04"),
				createdAt: ts.Format(time.RFC3339),
			},
		})
		// Bump unread if it landed in a background thread (a group file
		// badges the group's sidebar row, a DM file its thread).
		if conv != c.activeConv() && conv != generalConv {
			if gc := groupCodeOf(conv); gc != "" {
				if g := c.groups[gc]; g != nil {
					g.unread++
					if c.lastGroupAt == nil {
						c.lastGroupAt = map[string]time.Time{}
					}
					c.lastGroupAt[gc] = ts
				}
			} else if peer := peerOf(c.me, conv); peer != "" {
				c.unread[peer]++
				c.lastDMAt[peer] = ts
			}
		}
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case netDeliveredMsg:
		// Peer acked one of ours: full confidence, undim the bubble.
		// (Acks are best-effort frames themselves; a lost ack just leaves
		// the bubble dimmed until the expiry sweep below.) Evict the
		// cached bubble or the dimmed paint would stick. Background
		// sessions never have local unacked sends, so skip them.
		if msg.key != "" && msg.key != c.key {
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		if c.unackedUI != nil {
			delete(c.unackedUI, msg.msgId)
		}
		c.evictRenderCache(msg.msgId)
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case netReactionMsg:
		// A live reaction nudge: ask the server now instead of waiting for
		// the next 2s tick. Counts are never derived from the frame itself
		// (replace/remove cannot be expressed as a delta without tracking
		// every sender's prior pick) — the fetch is the truth. Background
		// groups reconcile on their own poll when opened.
		if msg.key != "" && msg.key != c.key {
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		if msg.reaction.From != c.me {
			if cmd := c.fetchReactionsCmd(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		cmds = append(cmds, c.drainNetCmd())

	case reactionsFetchedMsg:
		if msg.err == nil && c.applyFetchedReactions(msg.ids, msg.summaries) {
			c.rebuildView()
		}

	case reactionDoneMsg:
		if msg.err != nil {
			// POST failed: roll the optimistic toggle back so the badge
			// never claims a reaction the server did not record.
			c.undoReaction(msg.msgId, msg.emoji, msg.prev)
			c.evictRenderCache(msg.msgId)
			c.rebuildView()
			if isServerDown(msg.err) {
				c.status = serverDownMsg
			} else {
				c.status = "reaction failed: " + msg.err.Error()
			}
		}

	case reactionDetailAnimMsg:
		// One reveal tick: advance the open dropdown a frame and re-arm.
		// Ticks from a superseded generation (dismissed, reopened, another
		// chip) are ignored, so a stale timer can never resurrect the card.
		if msg.gen != c.detailAnimGen || c.detailMsgId == "" {
			break
		}
		if next := min(msg.frame, reactionDetailAnimFrames-1); next > c.detailAnim {
			c.detailAnim = next
			c.rebuildView()
			if c.detailAnim < reactionDetailAnimFrames-1 {
				cmds = append(cmds, scheduleReactionDetailAnim(msg.gen, c.detailAnim+1))
			}
		}

	case replyPickAnimMsg:
		// One pointer reveal tick: advance the /reply selection pointer a
		// frame and re-arm. Ticks from a superseded session (dismissed,
		// reopened, retargeted) are fenced by the generation counter.
		if c.replyPick == nil || msg.gen != c.replyPick.gen {
			break
		}
		if next := min(msg.frame, replyPickAnimFrames-1); next > c.replyPick.anim {
			c.replyPick.anim = next
			c.rebuildView()
			if c.replyPick.anim < replyPickAnimFrames-1 {
				cmds = append(cmds, scheduleReplyPickAnim(msg.gen, c.replyPick.anim+1))
			}
		}

	case quoteJumpTickMsg:
		// One highlight second elapsed: expire the quote-jump blue tint.
		// Ticks from a superseded jump are fenced by the generation counter.
		if c.quoteJump == nil || msg.gen != c.quoteJump.gen {
			break
		}
		c.quoteJump.left--
		if c.quoteJump.left <= 0 {
			c.quoteJump = nil
		}
		c.rebuildView()
		if c.quoteJump != nil {
			cmds = append(cmds, scheduleQuoteJumpTick(msg.gen))
		}

	case mediaInfoMsg:
		// Media chatter never reaches the transcript: the latest event
		// parks on the status line, conversation stays clean.
		if msg.info != "" {
			c.status = msg.info
		}
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case callLevelMsg:
		c.callLevel = msg.level
		cmds = append(cmds, c.drainNetCmd())

	case netFileErrMsg:
		if msg.key != "" && msg.key != c.key {
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		c.status = fmt.Sprintf("file from %s failed: %s", msg.from, msg.reason)
		cmds = append(cmds, c.drainNetCmd())

	case netReadyMsg:
		// E2E established: deliberately silent in the transcript (the safety
		// code check happens in-engine; KEY SWAP attacks still surface via
		// netErrMsg). Keeps the conversation clean per product direction.
		cmds = append(cmds, c.drainNetCmd())

	case netLostMsg:
		// Transport drops stay out of the transcript; delivery continues over
		// the inbox fallback and the channel re-establishes quietly.
		cmds = append(cmds, c.drainNetCmd())

	case netErrMsg:
		// Engine errors surface on the status line, never as chat rows.
		// Server outages hold the down alert; everything else parks once.
		// A background group that died (last member left / 24h TTL /
		// server sweep / kicked / full) leaves the live sidebar instead of
		// retrying into the void forever — the engine's beat classifies
		// the ghost-room 404 (or a rejoin that finds the room gone) as
		// "session ended", and a kicked/full rejoin is terminal too. Each
		// drop leaves a tombstone for the session.
		if msg.key != "" && msg.key != c.key {
			if g := c.groups[msg.key]; g != nil {
				if tombReason, ok := groupEndEvent(msg.err); ok {
					name := g.name
					if name == "" {
						name = msg.key
					}
					status := ""
					switch tombReason {
					case "kicked":
						status = "you were removed from group " + name
					case "full":
						status = "group " + name + " is full — you were dropped from the sidebar"
					default:
						status = "group " + name + " ended — rooms vanish when emptied"
					}
					c.dropGroupAs(msg.key, status, tombReason)
				}
			}
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		if isServerDown(msg.err) {
			c.status = serverDownMsg
		} else if msg.key == c.key && c.activeGroup != "" {
			if tombReason, ok := groupEndEvent(msg.err); ok {
				// The open group died (ended/kicked/full): leave the view,
				// drop the dead session.
				c.dropGroupAs(msg.key, msg.err.Error(), tombReason)
			} else {
				c.status = msg.err.Error()
			}
		} else {
			c.status = msg.err.Error()
		}
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case netIdleMsg:
		// Pump heartbeat: nothing arrived, keep waiting.
		cmds = append(cmds, c.drainNetCmd())

	case sendDoneMsg:
		if nc := c.settleSend(msg); nc != nil {
			cmds = append(cmds, nc)
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
		// Own uploads paint their card in settleUploadDone; peers receive
		// the file over the wire. No server round-trip exists anymore.

	case leaveDoneMsg:
		return c, tea.Quit

	case tea.MouseMsg:
		if cmd := c.handleMouse(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case tea.KeyMsg:
		// Drawer parity: while a picker owns the drawer slot, Ctrl+C
		// DISMISSES the drawer (OpenCode behaviour) — the app itself never
		// quits mid-pick. With no drawer open, Ctrl+C keeps its existing
		// meaning: leave the open group, or leave the session.
		if msg.Type == tea.KeyCtrlC && c.drawerOpen() {
			c.closeDrawers()
			return c, tea.Batch(cmds...)
		}
		if msg.Type == tea.KeyCtrlC {
			// In a group, Ctrl+C LEAVES it (server-side, standard leave
			// POST) and returns to the common room — the app keeps
			// running. The last admin is refused with a promotion pointer
			// (a group must never be left crownless). In the home room it
			// leaves the session and quits.
			if c.activeGroup != "" {
				return c, c.leaveGroupCmd(c.activeGroup)
			}
			// Leave first, quit when it completes (leaveDoneMsg→Quit):
			// quitting alongside would kill the POST mid-flight.
			return c, c.doLeave()
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
		// Esc during an upload cancels it before anything else sees the key.
		// (Downloads no longer exist as a transfer: files arrive complete.)
		if msg.Type == tea.KeyEsc && c.uploadQ.isActive() {
			c.cancelUploads()
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
		// The floating reply menu owns navigation + selection keys while it
		// is open: Up/Down move the highlight, Enter runs, Esc dismisses.
		// Any typing dismisses it and falls through to the composer.
		if c.replyMenu != nil {
			switch msg.Type {
			case tea.KeyUp:
				c.replyMenuMove(-1)
				return c, tea.Batch(cmds...)
			case tea.KeyDown:
				c.replyMenuMove(1)
				return c, tea.Batch(cmds...)
			case tea.KeyEnter:
				if cmd := c.replyMenuActivate(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return c, tea.Batch(cmds...)
			case tea.KeyEsc:
				c.closeReplyMenu()
				return c, tea.Batch(cmds...)
			}
			if textEditKey(msg) {
				c.closeReplyMenu()
			}
		}
		// /reply selection mode: Up/Down move the "<" pointer across
		// messages, Enter opens the same menu on the pointed message, Esc
		// exits. Any send is impossible here (Enter is owned); typing and
		// mode switches exit too.
		if c.replyPick != nil {
			switch msg.Type {
			case tea.KeyUp:
				c.replyPickMove(-1)
				return c, tea.Batch(cmds...)
			case tea.KeyDown:
				c.replyPickMove(1)
				return c, tea.Batch(cmds...)
			case tea.KeyEnter:
				c.replyPickConfirm()
				return c, tea.Batch(cmds...)
			case tea.KeyEsc:
				c.closeReplyPick()
				return c, tea.Batch(cmds...)
			}
			if textEditKey(msg) {
				c.closeReplyPick()
			}
		}
		// "@" member dropdown eats navigation + selection keys while open,
		// exactly like the "/" drawer above it (general room only).
		if handled, action := c.handleMentionKeys(msg); handled {
			if action != nil {
				cmds = append(cmds, action())
			}
			return c, tea.Batch(cmds...)
		}
		// Ctrl+K focuses the "/" command drawer (same as typing "/").
		// Intercepted before the input so the binding works everywhere;
		// this shadows the input's emacs kill-line, which the footer
		// does not advertise.
		if msg.Type == tea.KeyCtrlK {
			c.input.SetValue("/")
			c.palette.sync("/")
			c.mention.close() // the "/" drawer owns the slot
			c.input.Focus()
			c.focus = focusComposer // the drawer rides on the composer
			c.closeReactionAux()    // one drawer slot: commands replace the aux rows
			c.closeReplyMenu()
			c.closeReplyPick()
			break
		}
		// Ctrl+L clears the composer line and repaints the screen.
		if msg.Type == tea.KeyCtrlL {
			c.input.SetValue("")
			c.palette.sync("")
			c.mention.close()
			return c, tea.ClearScreen
		}
		if msg.Type == tea.KeyEsc {
			// Esc dismisses transient state — the anchored reaction picker
			// or the reactor detail bar first, then the rail's filter, then
			// the private/group view. NEVER quits the app.
			if c.pendingReactionMsgId != "" || c.detailMsgId != "" {
				c.closeReactionAux()
				break
			}
			if c.sideFilter != "" {
				c.clearSideFilter()
			}
			if c.activeGroup != "" {
				c.exitGroup() // view-only: the group stays joined
				break
			}
			c.exitPrivate()
			break
		}
		// An open drawer keeps focus: Tab is its completion key (handled
		// above) and neither Tab nor Shift+Tab may wander focus out from
		// under a panel that is claiming the accent.
		if c.drawerOpen() && (msg.Type == tea.KeyTab || msg.Type == tea.KeyShiftTab) {
			break
		}
		// Tab / Shift+Tab walk focus around the ring:
		// composer → conversation rail → transcript → composer.
		// Landing on the rail also parks its cursor on the first peer so a
		// keyboard-only user always has a visible target.
		if msg.Type == tea.KeyTab || msg.Type == tea.KeyShiftTab {
			c.cycleFocus(msg.Type == tea.KeyShiftTab)
			break
		}
		// Home / End jump the transcript to the top or bottom (the viewport
		// does not bind them in bubbles v1).
		if msg.Type == tea.KeyHome || msg.Type == tea.KeyEnd {
			before := c.vp.YOffset
			if msg.Type == tea.KeyHome {
				c.vp.GotoTop()
			} else {
				c.vp.GotoBottom()
			}
			c.dismissAuxOnScroll(before)
			break
		}
		// With the conversation rail focused, arrows move its cursor
		// instead of scrolling the transcript.
		if c.focus == focusSidebar && (msg.Type == tea.KeyUp || msg.Type == tea.KeyDown) {
			if msg.Type == tea.KeyUp {
				c.moveRosterCursor(-1)
			} else {
				c.moveRosterCursor(1)
			}
			break
		}
		// With the rail focused, printable keys narrow the list instead of
		// reaching the composer — the inline filter the rail advertises.
		if c.focus == focusSidebar && textEditKey(msg) {
			c.editSideFilter(msg)
			break
		}
		// Keyboard reaction fallback (transcript focus): `r` opens the picker
		// anchored above the newest message, then 1-6 toggle an emoji on the
		// target and close it (one pick). Consumed before any text handling so
		// the composer never sees them.
		if c.focus == focusTranscript && msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
			r := msg.Runes[0]
			consumed := false
			if r == 'r' || r == 'R' {
				consumed = true
				next := ""
				if c.pendingReactionMsgId == "" {
					next = c.newestReactableMsgId()
				}
				if next != c.pendingReactionMsgId {
					c.pendingReactionMsgId = next
					c.detailMsgId, c.detailEmoji = "", "" // one aux row at a time
					c.rebuildView()                       // open/close repaints the anchored row
				} else if next == "" && c.detailMsgId != "" {
					c.closeReactionDetail() // nothing to pick still dismisses the detail
				}
			} else if r >= '1' && r <= '6' && c.pendingReactionMsgId != "" {
				consumed = true
				if cmd := c.toggleReaction(c.pendingReactionMsgId, reactionEmojis[int(r-'1')]); cmd != nil {
					cmds = append(cmds, cmd)
				}
			}
			if consumed {
				break
			}
		}
		// Typing dismisses the anchored reaction aux rows (transient state:
		// text and reactions never compete for the same keys).
		if textEditKey(msg) && (c.pendingReactionMsgId != "" || c.detailMsgId != "") {
			c.pendingReactionMsgId = ""
			c.detailMsgId, c.detailEmoji = "", ""
			c.rebuildView()
		}
		// Any other key that edits the composer also focuses it, so the
		// indicator always sits where the text is about to appear.
		if c.focus != focusComposer && textEditKey(msg) {
			c.focus = focusComposer
		}
		if msg.Type == tea.KeyEnter {
			c.mention.close() // a real send always dismisses the "@" dropdown
			text := strings.TrimSpace(c.input.Value())
			c.input.SetValue("")
			if text == "" {
				// Empty Enter on a highlighted roster row OPENS that
				// entry: a DM thread or a group conversation. A tombstone
				// row (left/kicked/dissolved) is not joinable: it explains
				// how to get back in instead of doing nothing.
				if c.hoverPeer != "" && c.hoverPeer != c.me && c.hoverPeer != c.targetUser {
					if code := groupPeerCode(c.hoverPeer); code != "" {
						if t, dead := c.tombstones[code]; dead {
							c.status = tombstoneStatus(t)
							c.rebuildView()
						} else {
							c.openGroup(code)
						}
					} else {
						cmd := c.enterPrivate(c.hoverPeer)
						if cmd != nil {
							cmds = append(cmds, cmd)
						}
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

	// The transcript viewport keeps its own scroll handling — but only for
	// messages it should ever see. Mouse is owned exclusively by handleMouse
	// (per-pane wheel + drag routing), and keys stay away from it while the
	// conversation rail owns focus so ↑/↓ move the rail cursor instead.
	forward := true
	switch msg.(type) {
	case tea.MouseMsg:
		forward = false
	case tea.KeyMsg:
		if c.focus == focusSidebar {
			forward = false
		}
	}
	if forward {
		before := c.vp.YOffset
		var vpc tea.Cmd
		c.vp, vpc = c.vp.Update(msg)
		c.dismissAuxOnScroll(before)
		cmds = append(cmds, vpc)
	}

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

	// Pin the echo row by its stored index (validated); scan only as a
	// fallback for state predating the handle.
	echoIdx := -1
	if c.pending != nil && c.pending.localIdx >= 0 && c.pending.localIdx < len(c.localLines) &&
		c.localLines[c.pending.localIdx].conv == pc {
		echoIdx = c.pending.localIdx
	} else {
		for i := range c.localLines {
			if c.localLines[i].conv == pc {
				echoIdx = i
			}
		}
	}

	switch {
	case msg.code == 429:
		if echoIdx >= 0 {
			c.localLines[echoIdx] = localLine{conv: pc, text: tuiErrStyle.Render("✗ slow down — try again")}
		}
	case msg.code == 410:
		if echoIdx >= 0 {
			c.localLines[echoIdx] = localLine{conv: pc, text: tuiSystemStyle.Render("* Session has ended")}
		}
	case msg.err != nil:
		if isServerDown(msg.err) {
			c.status = serverDownMsg
			if echoIdx >= 0 {
				c.localLines[echoIdx] = localLine{conv: pc, text: tuiErrStyle.Render("✗ " + serverDownMsg)}
			}
		} else if echoIdx >= 0 {
			c.localLines[echoIdx] = localLine{conv: pc, text: tuiErrStyle.Render("✗ send failed: " + msg.err.Error())}
		} else {
			c.status = "send failed: " + msg.err.Error()
		}
	default:
		to := ""
		if c.pending != nil {
			to = c.pending.to
		} else if msg.to != "" {
			to = msg.to
		}
		conv := generalConv
		if msg.conv != "" {
			conv = msg.conv // dispatch-time bucket (group sends are to="")
		} else if to != "" {
			conv = conversationKey(c.me, to)
		}
		m := chatMessage{
			Seq: msg.seq, MsgId: msg.msgId, Username: c.me, Kind: "chat",
			Text: msg.text, To: to, ConvID: conv,
			CreatedAt: time.Now().Format(time.RFC3339),
			ReplyTo:   msg.quote.ReplyTo, ReplyAuthor: msg.quote.ReplyAuthor,
			ReplyExcerpt: msg.quote.ReplyExcerpt,
		}
		// Defensive: seqs come from one allocator now, but never drop a
		// confirmed own message over a counter surprise.
		for c.rendered[m.Seq] || historyHasSeq(c.history, m.Seq) {
			c.nextSeq++
			m.Seq = c.nextSeq
		}
		c.rendered[m.Seq] = true
		c.history = append(c.history, m)
		if echoIdx >= 0 {
			c.localLines = append(c.localLines[:echoIdx], c.localLines[echoIdx+1:]...)
		}
	}

	c.pending = nil
	// Track the confirmed send for delivery receipts: the bubble stays
	// dimmed until the peer's ack arrives (netDeliveredMsg). Failures
	// (429/410/500 + errors) keep their annotations instead.
	if msg.err == nil && msg.code == 201 && msg.msgId != "" {
		if c.unackedUI == nil {
			c.unackedUI = map[string]time.Time{}
		}
		c.unackedUI[msg.msgId] = time.Now()
	}
	c.rebuildView()

	if msg.code == 410 {
		// Room ended: fail everything still queued so lines never strand
		// invisibly in the outbox.
		if len(c.outbox) > 0 {
			c.status = "Session has ended"
		}
		c.outbox = nil
		return nil
	}
	if n := len(c.outbox); n > 0 {
		next := c.outbox[0]
		c.outbox = c.outbox[1:]
		return c.dispatchInConv(next.conv, next.text)
	}
	return nil
}

// cycleFocus walks keyboard focus around the ring (composer → conversation
// rail → transcript). Landing on the rail parks its cursor on the first
// peer when nothing is highlighted yet, so Tab always leaves a visible
// target behind — that target is what an empty Enter opens. A pane that is
// not on screen (the rail collapses on narrow terminals) is skipped, so
// focus can never point at something with no indicator to paint.
func (c *chatScreen) cycleFocus(reverse bool) {
	c.closeReactionAux() // focus moves: the reaction aux rows dismiss
	for i := 0; i < 3; i++ {
		if reverse {
			c.focus = c.focus.prev()
		} else {
			c.focus = c.focus.next()
		}
		if c.focus != focusSidebar || c.layoutFor().sidebarOn {
			break
		}
	}
	if c.focus == focusSidebar {
		if c.hoverPeer == "" || c.hoverPeer == c.me {
			c.cycleRosterFocus(reverse)
		}
		c.scrollRosterToCursor()
	}
	c.syncRosterVp()
}

// moveRosterCursor steps the rail cursor through the conversation list
// (skipping the room and self, wrapping at both ends) and keeps it in view.
func (c *chatScreen) moveRosterCursor(step int) {
	items := c.chatItems()
	if len(items) == 0 {
		return
	}
	cur := c.itemIndexFor(c.hoverPeer)
	if cur < 0 {
		cur = 0
		if step > 0 {
			cur = -1
		}
	}
	for i := 0; i < len(items); i++ {
		cur += step
		if cur < 0 {
			cur = len(items) - 1
		}
		if cur >= len(items) {
			cur = 0
		}
		if !items[cur].isRoom && items[cur].peer != c.me {
			c.hoverPeer = items[cur].peer
			break
		}
	}
	c.scrollRosterToCursor()
	c.syncRosterVp()
}

// scrollRosterToCursor slides the rail's scroll window so the cursor row is
// fully visible (no selection ever parked off-screen).
func (c *chatScreen) scrollRosterToCursor() {
	idx := c.itemIndexFor(c.hoverPeer)
	if idx < 0 || c.rosterVp.Height <= 0 {
		return
	}
	h := c.chatItemHeight()
	top, bot := idx*h, idx*h+h
	if top < c.rosterVp.YOffset {
		c.rosterVp.SetYOffset(top)
	}
	if bot > c.rosterVp.YOffset+c.rosterVp.Height {
		c.rosterVp.SetYOffset(maxInt(bot-c.rosterVp.Height, 0))
	}
}

// maxSideFilter bounds the inline rail query (a marathon paste must not
// grow the sidebar's key state without limit).
const maxSideFilter = 40

// drawerOpen reports whether the "/" drawer, the "@" member dropdown, or
// the file browser owns the slot above the composer — and with it the
// focused-surface indicator, so nothing else may claim to be focused at the
// same time.
func (c chatScreen) drawerOpen() bool {
	return c.picker.isActive() || c.palette.visible() || c.mention.visible()
}

// closeDrawers dismisses whatever owns the drawer slot (Ctrl+C parity:
// Esc has its staged per-mode behaviour, Ctrl+C is the hard dismiss).
func (c *chatScreen) closeDrawers() {
	if c.picker.isActive() {
		c.closePicker(composerPlaceholder)
	}
	c.palette.close()
	c.mention.close()
}

// textEditKey reports whether a key is text input: printable characters and
// the editing keys that reshape a line. Ctrl-combos and navigation are not
// text, so they never steal focus or narrow the rail.
func textEditKey(msg tea.KeyMsg) bool {
	if msg.Alt {
		return false
	}
	switch msg.Type {
	case tea.KeyRunes:
		return len(msg.Runes) > 0
	case tea.KeySpace, tea.KeyBackspace, tea.KeyCtrlH, tea.KeyDelete:
		return true
	}
	return false
}

// editSideFilter applies one filter edit, re-syncs the list, and drops a
// cursor that the narrower list no longer contains (so an empty Enter can
// never open an invisible thread).
func (c *chatScreen) editSideFilter(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyBackspace, tea.KeyCtrlH, tea.KeyDelete:
		if c.sideFilter != "" {
			r := []rune(c.sideFilter)
			c.sideFilter = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		c.sideFilter += " "
	default:
		c.sideFilter += strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1 // never let control bytes park in the rail
			}
			return r
		}, string(msg.Runes))
	}
	if len([]rune(c.sideFilter)) > maxSideFilter {
		c.sideFilter = string([]rune(c.sideFilter)[:maxSideFilter])
	}
	if c.hoverPeer != "" && c.itemIndexFor(c.hoverPeer) < 0 {
		c.hoverPeer = ""
	}
	c.syncRosterVp()
}

// clearSideFilter widens the rail back to the full list.
func (c *chatScreen) clearSideFilter() {
	c.sideFilter = ""
	c.syncRosterVp()
}

// cycleRosterFocus moves the highlight through the conversation rail
// (skipping the room and self), wrapping at both ends. The highlighted peer
// is what an empty Enter opens — the keyboard twin of clicking a row.
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
// peerAtY maps a terminal Y coordinate onto the sidebar's chat list: the
// DM peer for thread rows, "" for the General room, filter row, or any
// point outside a chat row. itemIsGeneral reports a General-room hit.
func (c chatScreen) peerAtY(y int, l layout) string {
	peer, _ := c.itemAtY(y, l)
	return peer
}

// itemAtY maps a terminal Y onto (peer, general): peer is the DM username,
// general is true for the General room row. Both empty/false off-list.
func (c chatScreen) itemAtY(y int, l layout) (peer string, general bool) {
	if c.width == 0 || c.height == 0 || !l.sidebarOn {
		return "", false
	}
	row := y - l.rosterY0
	if row < 0 {
		return "", false
	}
	// The chat list SCROLLS beneath the fixed search row: map the visible
	// row through the roster viewport's scroll offset (same content
	// rosterBody paints) at the live item height.
	items := c.chatItems()
	idx := (row + c.rosterVp.YOffset) / c.chatItemHeight()
	if idx < 0 || idx >= len(items) {
		return "", false
	}
	if items[idx].isRoom {
		return "", true
	}
	return items[idx].peer, false
}

// atSearchBox reports a hit on the sidebar's filter row (the rows above
// rosterY0). Clicking it focuses the "/" drawer.
func (c chatScreen) atSearchBox(y int, l layout) bool {
	if c.width == 0 || c.height == 0 || !l.sidebarOn {
		return false
	}
	sh := searchHeightFor(l.headRows, l.sidebarWidth)
	return y >= l.rosterY0-sh && y < l.rosterY0
}

// transcriptTopRow is the terminal row of the transcript's first painted
// line (frame + banner + chat header). Shared by focus hit-testing and the
// anchored reaction picker geometry so they can never disagree.
func transcriptTopRow(l layout) int {
	frameOff, headOff := 0, 0
	if l.frameOn {
		frameOff = 1
	}
	if l.showHeader {
		headOff = 1
	}
	return frameOff + headOff + l.headRows
}

// reactionPickerY maps the anchored reaction picker onto the terminal: the
// picker is a transcript row, so its screen row is the transcript top plus its
// content row minus the scroll offset. Returns -1 when the picker is closed,
// not painted, or scrolled out of view.
func (c chatScreen) reactionPickerY(l layout) int {
	if c.pendingReactionMsgId == "" || c.reactionRow < 0 || l.vpHeight <= 0 {
		return -1
	}
	top := transcriptTopRow(l)
	y := top + c.reactionRow - c.vp.YOffset
	if y < top || y >= top+l.vpHeight {
		return -1
	}
	return y
}

// reactionPickerX0 is the content-column offset where the picker's bracketed
// row begins: right-aligned for own messages, flush left for peers, exactly
// mirroring the bubble it floats above. Hit-testing adds it to the content
// origin before mapping a cell through reactionCellAt.
func (c chatScreen) reactionPickerX0() int {
	m, ok := c.reactionTarget()
	if !ok || m.Username != c.me {
		return 0
	}
	if pad := c.transcriptW() - lipgloss.Width(c.reactionPickerRow(m)); pad > 0 {
		return pad
	}
	return 0
}

// msgById resolves one history message by MsgId (reaction anchors work in
// both the room and DM threads; ids are globally unique).
func (c chatScreen) msgById(msgId string) (chatMessage, bool) {
	if msgId == "" {
		return chatMessage{}, false
	}
	for _, m := range c.history {
		if m.MsgId == msgId {
			return m, true
		}
	}
	return chatMessage{}, false
}

// reactionTarget resolves the message the open picker is anchored to.
func (c chatScreen) reactionTarget() (chatMessage, bool) {
	return c.msgById(c.pendingReactionMsgId)
}

// ─── /reply pointer ─────────────────────────────────────────────────────────

// replyPointerPaint paints the selection pointer for a /reply target at its
// animation frame: a "<" that slides in from the right over
// replyPickAnimFrames (frame 0 = "still arriving", last frame = settled
// beside the message) and pulses via the accent colour. Plain runs paint
// the settled frame instantly. One row, clipped to the transcript width.
func (c *chatScreen) replyPointerPaint(m chatMessage) string {
	f := c.replyPick.anim
	if f > replyPickAnimFrames-1 {
		f = replyPickAnimFrames - 1
	}
	indent := (replyPickAnimFrames - 1 - f) * 2
	sym := "<"
	st := lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	if f < replyPickAnimFrames-1 {
		sym = "‹"
		st = lipgloss.NewStyle().Foreground(colDim)
	}
	line := strings.Repeat(" ", indent) + st.Render(sym) + " reply…"
	return truncateByWidth(line, maxInt(c.transcriptW(), 1))
}

// replyPickRowY maps the reply pointer onto the terminal: the pointer is a
// transcript row, so its screen row is the transcript top plus its content
// row minus the scroll offset. Returns -1 when picking is closed, the
// pointer is not painted, or it is scrolled out of view.
func (c *chatScreen) replyPickRowY(l layout) int {
	if c.replyPick == nil || c.replyPick.row < 0 || l.vpHeight <= 0 {
		return -1
	}
	top := transcriptTopRow(l)
	y := top + c.replyPick.row - c.vp.YOffset
	if y < top || y >= top+l.vpHeight {
		return -1
	}
	return y
}

// firstRowOfMsg is the first viewport content row painted for msgId
// (-1 when the message is not in the paint buffer).
func (c *chatScreen) firstRowOfMsg(msgId string) int {
	if msgId == "" {
		return -1
	}
	for i, id := range c.rowMsg {
		if id == msgId {
			return i
		}
	}
	return -1
}

// ensureMsgVisible scrolls the transcript just enough that the message's
// first row is on screen (a no-op when it already is). Called after the
// paint buffer is fresh, so rowMsg reflects the target.
func (c *chatScreen) ensureMsgVisible(msgId string) {
	row := c.firstRowOfMsg(msgId)
	if row < 0 {
		return
	}
	if row < c.vp.YOffset || row >= c.vp.YOffset+maxInt(c.vp.Height, 1) {
		c.vp.SetYOffset(row)
	}
}

// openReplyPick enters /reply selection mode pointed at msgId, arming the
// pointer's slide-in reveal (nil in plain runs: the settled frame paints
// immediately, no timers). Opening it dismisses every other transient.
func (c *chatScreen) openReplyPick(msgId string) tea.Cmd {
	if msgId == "" {
		return nil
	}
	if c.replyPick == nil {
		c.replyPick = &replyPickState{target: msgId, anim: 0, row: -1}
	} else {
		c.replyPick.target = msgId
		c.replyPick.anim = 0
	}
	c.replyPickCounter++
	c.replyPick.gen = c.replyPickCounter // supersede every reveal still in flight
	c.replyPickLineIdx = -1
	c.closeReactionAux() // one aux row at a time
	c.closeReplyMenu()
	if !c.animations {
		c.replyPick.anim = replyPickAnimFrames - 1 // plain run: no timers, final paint
		c.rebuildView()
		c.ensureMsgVisible(msgId)
		return nil
	}
	c.rebuildView()
	c.ensureMsgVisible(msgId)
	return scheduleReplyPickAnim(c.replyPick.gen, 1)
}

// closeReplyPick exits /reply selection mode; the pointer row vanishes with
// the next repaint. Stale reveal ticks are fenced by the generation counter.
func (c *chatScreen) closeReplyPick() {
	if c.replyPick == nil {
		return
	}
	c.replyPick = nil
	c.replyPickLineIdx = -1
	c.rebuildView()
}

// replyPickMove steps the pointer across the active conversation's messages
// (wrapping at both ends); the target scrolls into view when it moves.
func (c *chatScreen) replyPickMove(step int) {
	if c.replyPick == nil {
		return
	}
	var ids []string
	for _, m := range c.history {
		if reactableMsgId(m) != "" && c.shouldRender(m) {
			ids = append(ids, m.MsgId)
		}
	}
	if len(ids) == 0 {
		return
	}
	cur := -1
	for i, id := range ids {
		if id == c.replyPick.target {
			cur = i
			break
		}
	}
	if cur < 0 {
		cur = len(ids) - 1
	}
	nxt := (cur + step) % len(ids)
	if nxt < 0 {
		nxt += len(ids)
	}
	c.replyPick.target = ids[nxt]
	c.rebuildView()
	c.ensureMsgVisible(ids[nxt])
}

// replyPickConfirm commits the pointed message: the floating Reply /
// Reply-Privately menu opens anchored at the pointer's row (own messages
// offer Reply only). Nothing to commit closes the mode.
func (c *chatScreen) replyPickConfirm() {
	if c.replyPick == nil {
		return
	}
	m, ok := c.msgById(c.replyPick.target)
	if !ok || reactableMsgId(m) == "" {
		c.closeReplyPick()
		return
	}
	l := c.layoutFor()
	x := transcriptX0(l) + 2
	y := c.replyPickRowY(l)
	if y < 0 {
		y = transcriptTopRow(l) // pointer scrolled out: anchor at transcript top
	}
	c.openReplyMenu(m, x, y) // openReplyMenu closes the pick itself
}

// reactionDetailY maps the open detail bar's first row onto the terminal:
// anchored directly below its message, so it follows the viewport like the
// picker. Returns -1 when closed, not painted, or scrolled out of view.
func (c chatScreen) reactionDetailY(l layout) int {
	if c.detailMsgId == "" || c.detailRow < 0 || l.vpHeight <= 0 {
		return -1
	}
	top := transcriptTopRow(l)
	y := top + c.detailRow - c.vp.YOffset
	if y < top || y >= top+l.vpHeight {
		return -1
	}
	return y
}

// reactionDetailAtY reports whether a terminal row paints inside the open
// detail block (its own rows are controls: clicks there never dismiss it).
func (c chatScreen) reactionDetailAtY(y int, l layout) bool {
	if c.detailMsgId == "" || c.detailRow < 0 || c.detailRowH <= 0 {
		return false
	}
	top := transcriptTopRow(l)
	if l.vpHeight <= 0 || y < top || y >= top+l.vpHeight {
		return false
	}
	row := y - top + c.vp.YOffset
	return row >= c.detailRow && row < c.detailRow+c.detailRowH
}

// msgAtY maps a transcript click row onto the MsgId painted there ("" for
// padding, the anchored picker row, the detail block, local rows, or outside
// the transcript). rowMsg is the wrapped-row index refreshViewport builds,
// offset by the viewport's scroll position.
func (c chatScreen) msgAtY(y int, l layout) string {
	top := transcriptTopRow(l)
	if l.vpHeight <= 0 || y < top || y >= top+l.vpHeight {
		return ""
	}
	row := y - top + c.vp.YOffset
	if row < 0 || row >= len(c.rowMsg) {
		return ""
	}
	if c.pendingReactionMsgId != "" && row == c.reactionRow {
		return "" // the picker row is a control, never a message hit
	}
	return c.rowMsg[row]
}

// focusAtRow maps a click in the main column onto the pane it hit: the
// composer box, the transcript, or whatever was focused before (banner and
// drawer rows change nothing). Exactly one pane is ever focused, so exactly
// one indicator is ever painted.
func (c chatScreen) focusAtRow(y int, l layout) focusPane {
	transcriptTop := transcriptTopRow(l)
	if y >= transcriptTop && y < transcriptTop+maxInt(l.vpHeight, 0) {
		return focusTranscript
	}
	compTop := composerTopRows(l)
	boxH := l.composerRows + 2
	if l.composerRows <= 0 {
		boxH = 1
	}
	if y >= compTop && y < compTop+boxH {
		return focusComposer
	}
	if y >= transcriptTop {
		return focusTranscript // drawer / hint rows belong to the stack
	}
	return c.focus
}

// transcriptX0 is the terminal column where the main column begins (just
// past the conversation rail and its gutter).
func transcriptX0(l layout) int {
	frameOff := 0
	if l.frameOn {
		frameOff = 1
	}
	if l.sidebarOn {
		return frameOff + l.sidebarWidth + 1
	}
	return frameOff
}

// scrollBarGeoms is the paint-verified track geometry for the scrollbars
// (validated against View() by TestScrollbarDragGeometry). The transcript
// rail is the main column's last column; the list rail is the sidebar's.
func (c chatScreen) scrollBarGeoms(l layout) (chat, roster barGeom) {
	frameOff := 0
	if l.frameOn {
		frameOff = 1
	}
	headOff := 0
	if l.showHeader {
		headOff = 1
	}
	// Transcript rail top = frame + banner + chat header; the track starts
	// one row below the up-arrow when there is room for arrows.
	chatTop := frameOff + headOff + l.headRows
	chat = barGeom{
		x:       transcriptX0(l) + 1 + l.vpWidth,
		trackY0: chatTop + 1,
		trackH:  maxInt(l.vpHeight-2, 0),
	}
	if l.vpHeight < 3 {
		chat.trackY0 = chatTop
		chat.trackH = maxInt(l.vpHeight, 0)
	}
	if l.sidebarOn {
		fill := c.sidebarFill(l)
		bodyH := maxInt(fill-searchHeightFor(l.headRows, l.sidebarWidth), 0)
		roster = barGeom{
			x:       l.rosterX + l.sidebarWidth - 1,
			trackY0: l.rosterY0 + 1,
			trackH:  maxInt(bodyH-2, 0),
		}
		if bodyH < 3 {
			roster.trackY0 = l.rosterY0
			roster.trackH = bodyH
		}
	}
	return chat, roster
}

// thumbFor computes the thumb position for one pane's scrollbar.
func (c chatScreen) thumbFor(sec scrollSection, g barGeom) barGeom {
	switch sec {
	case secChat:
		g.thumbTop, g.thumbH, _, _ = thumbGeom(c.vp.TotalLineCount(), c.vp.Height, c.vp.YOffset, g.trackH+2)
	case secRoster:
		g.thumbTop, g.thumbH, _, _ = thumbGeom(c.rosterVp.TotalLineCount(), c.rosterVp.Height, c.rosterVp.YOffset, g.trackH+2)
	}
	return g
}

// thumbGeom reports thumb placement inside a track for total/visible/offset.
func thumbGeom(total, visible, offset, h int) (thumbTop, thumbH, trackH int, hasArrows bool) {
	trackH = h
	hasArrows = h >= 3
	if hasArrows {
		trackH = h - 2
	}
	if trackH <= 0 || total <= visible || total <= 0 {
		return 0, trackH, trackH, hasArrows
	}
	thumbH = trackH * visible / total
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > trackH {
		thumbH = trackH
	}
	maxOff := total - visible
	if maxOff <= 0 {
		return 0, thumbH, trackH, hasArrows
	}
	thumbTop = int(float64(trackH-thumbH) * float64(offset) / float64(maxOff))
	if thumbTop < 0 {
		thumbTop = 0
	}
	if thumbTop+thumbH > trackH {
		thumbTop = trackH - thumbH
	}
	return thumbTop, thumbH, trackH, hasArrows
}

// handleMouse routes wheel scrolling per pane, scrollbar drags, hover and
// row selection. Wheel/drag on one section never moves the others.
func (c *chatScreen) handleMouse(msg tea.MouseMsg) tea.Cmd {
	c.mouseActive = true // inputMode tracking: the mouse is in play now
	l := c.layoutFor()
	// Legacy-typed motion messages (tests, X10 paths) carry Action=0.
	action := msg.Action
	if action == tea.MouseActionPress && msg.Type == tea.MouseMotion {
		action = tea.MouseActionMotion
	}

	switch action {
	case tea.MouseActionPress:
		if c.width == 0 || c.height == 0 {
			break
		}
		// Right button: the WhatsApp-style context menu on a transcript
		// message (Reply / Reply-Privately). Any open menu closes first;
		// a right-click off any message just dismisses.
		if msg.Button == tea.MouseButtonRight {
			c.closeReplyMenu()
			if !c.drawerOpen() {
				if id := c.msgAtY(msg.Y, l); id != "" {
					if m, ok := c.msgById(id); ok && reactableMsgId(m) != "" {
						c.openReplyMenu(m, msg.X, msg.Y)
						return nil
					}
				}
				c.closeReplyPick() // right-click elsewhere also cancels picking
			}
			break
		}
		if msg.Button != tea.MouseButtonLeft {
			break
		}
		// The floating reply menu owns clicks over its own cells: a pick
		// activates the item, anything else on the panel is a miss. A
		// click ANYWHERE else dismisses the menu and is consumed (the
		// underlying row does not react to the same click).
		if c.replyMenu != nil {
			if idx, hit := c.replyMenuItemAt(msg.X, msg.Y); hit {
				c.replyMenu.sel = idx
				if cmd := c.replyMenuActivate(); cmd != nil {
					return cmd
				}
				return nil
			}
			c.closeReplyMenu()
			return nil
		}
		// Pinned quote card above the composer: the X at its right edge
		// clears the citation; the rest of the row just focuses the box.
		if c.composerQuote != nil && l.quoteRows > 0 {
			top := composerTopRows(l)
			cardY := top - l.quoteRows
			if msg.Y == cardY {
				x0 := transcriptX0(l)
				if msg.X >= x0+l.vpWidth+transcriptBorder-2 { // the "  ✕" tail
					c.clearComposerQuote()
				} else {
					c.focus = focusComposer
				}
				return nil
			}
		}
		// Clip glyph: open the upload browser (same as /upload). Painted
		// whenever the box fits it — clickable with or without the sidebar.
		clipX, clipY0, clipY1 := composerGeoms(l)
		if clipX >= 0 && msg.X == clipX && msg.Y >= clipY0 && msg.Y < clipY1 {
			if !c.picker.isActive() {
				c.focus = focusComposer // the clip belongs to the composer
				return c.openPicker()
			}
			return nil
		}
		// Scrollbars are drag-only: they never take focus or selection.
		chatG, rosterG := c.scrollBarGeoms(l)
		chatG = c.thumbFor(secChat, chatG)
		rosterG = c.thumbFor(secRoster, rosterG)
		bars := []struct {
			sec scrollSection
			b   barGeom
		}{{secChat, chatG}}
		if l.sidebarOn {
			bars = append(bars, struct {
				sec scrollSection
				b   barGeom
			}{secRoster, rosterG})
		}
		for _, g := range bars {
			if g.b.trackH <= 0 {
				continue
			}
			if msg.X == g.b.x && msg.Y >= g.b.trackY0 && msg.Y < g.b.trackY0+g.b.trackH {
				c.drag = barDrag{active: true, sec: g.sec, grabOff: msg.Y - (g.b.trackY0 + g.b.thumbTop)}
				c.dragTo(g.sec, msg.Y, g.b, l)
				return nil
			}
		}
		// Mouse+keyboard parity: a click inside the drawer selects the row
		// under the cursor (Enter/Tab still activate — every action stays
		// keyboard-reachable). Clicks on the drawer's chrome (border,
		// header, footer, empty state) are consumed: the panel owns its
		// rows and nothing beneath may react to the same click.
		if c.drawerOpen() {
			y0, y1 := c.drawerYRange(l)
			x0 := transcriptX0(l)
			x1 := x0 + drawerMaxW(c.width, l.vpWidth+transcriptBorder)
			if msg.Y >= y0 && msg.Y < y1 && msg.X >= x0 && msg.X < x1 {
				if item, ok := c.drawerSelAt(msg.Y, l); ok {
					c.drawerSelectItem(item, l)
				}
				return nil
			}
		}
		sideTop := l.rosterY0 - searchHeightFor(l.headRows, l.sidebarWidth)
		inColumn := l.sidebarOn && msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth
		if inColumn {
			if msg.Y < sideTop {
				return nil // banner row: not ours, focus untouched
			}
			// Rail column (last sidebar column) is drag-only, never selection.
			if msg.X == l.rosterX+l.sidebarWidth-1 {
				return nil
			}
			// Filter row: the right-edge affordance opens the settings window when
			// invites are pending (the ⚙ badge), else the "/" command
			// drawer; anywhere else focuses the rail so the inline filter
			// can be typed.
			if c.atSearchBox(msg.Y, l) {
				affX := l.rosterX + l.sidebarWidth - 3
				if c.pendingInvites > 0 && msg.X >= affX && msg.X < l.rosterX+l.sidebarWidth {
					c.focus = focusComposer
					return func() tea.Msg { return openSettingsMsg{} }
				}
				if msg.X == l.rosterX+l.sidebarWidth-2 {
					c.input.SetValue("/")
					c.palette.sync("/")
					c.closeReactionAux() // one drawer slot
					c.focus = focusComposer
				} else {
					c.focus = focusSidebar
				}
				return nil
			}
			// Chat row selection: General returns to the room, peers open threads,
			// group rows open their conversation.
			u, general := c.itemAtY(msg.Y, l)
			c.focus = focusSidebar
			switch {
			case general:
				c.exitConv()
				return nil
			case u == "", u == c.me:
				return nil // no row / clicking yourself is a no-op
			case u == c.targetUser:
				return nil // already chatting privately with them
			}
			if code := groupPeerCode(u); code != "" {
				if t, dead := c.tombstones[code]; dead {
					// Dead group row: explain, never open (there is no
					// session behind it).
					c.status = tombstoneStatus(t)
					return nil
				}
				if c.activeGroup != code {
					c.openGroup(code)
				}
				return nil
			}
			c.enterPrivate(u)
			return nil
		}
		// Main column. An open picker owns its anchored row: a hit on one of
		// its choices toggles it and closes the picker — a pick never opens
		// the reactor detail by itself; anywhere else on the row keeps the
		// picker open. The detail bar opens only from an explicit badge-chip
		// click for that msgId+emoji (reselecting the same chip closes it).
		// Otherwise a click on a message opens its picker directly above it —
		// a second click dismisses — and every other click drops the detail
		// bar.
		if y := c.reactionPickerY(l); y >= 0 && msg.Y == y {
			off := msg.X - (transcriptX0(l) + 1) - c.reactionPickerX0()
			if choice, ok := reactionCellAt(off); ok && choice != "+" {
				// Toggle only: one pick closes the picker and leaves no
				// detail behind (inspection is an explicit badge-chip click).
				return c.toggleReaction(c.pendingReactionMsgId, choice)
			}
			return nil
		}
		if id := c.msgAtY(msg.Y, l); id != "" && !c.drawerOpen() {
			c.focus = focusTranscript
			// Quote-card click: jump to the quoted message instead of
			// opening the reaction picker.
			if c.quoteJumpRowAt(msg.Y, l) {
				if m, ok := c.msgById(id); ok && m.repliedTo() {
					c.pendingReactionMsgId = ""
					c.detailMsgId, c.detailEmoji = "", ""
					if cmd := c.jumpToQuoted(m); cmd != nil {
						return cmd
					}
					return nil
				}
			}
			if emoji, ok := c.reactionBadgeEmojiAt(id, msg.X, msg.Y, l); ok {
				return c.toggleReactionDetail(id, emoji)
			}
			c.detailMsgId, c.detailEmoji = "", "" // any entry click drops the detail
			if c.pendingReactionMsgId == id {
				c.pendingReactionMsgId = "" // second click dismisses
			} else {
				c.pendingReactionMsgId = id
			}
			c.rebuildView() // anchor/clear paints as a transcript row
			return nil
		}
		// Pick-away: any other main-column click dismisses the detail bar,
		// unless it lands on the bar's own control rows.
		if c.detailMsgId != "" && !c.reactionDetailAtY(msg.Y, l) {
			c.closeReactionDetail()
		}
		c.focus = c.focusAtRow(msg.Y, l)
		return nil

	case tea.MouseActionMotion:
		if c.drag.active {
			chatG, rosterG := c.scrollBarGeoms(l)
			switch c.drag.sec {
			case secChat:
				c.dragTo(secChat, msg.Y, c.thumbFor(secChat, chatG), l)
			case secRoster:
				c.dragTo(secRoster, msg.Y, c.thumbFor(secRoster, rosterG), l)
			}
			return nil
		}
		// Drawer hover parity: moving over a row selects it (hover-follow,
		// fzf-style). One motion event is ignored right after a filter
		// change (drawerHoverLock) so a stale hover cannot fight the fresh
		// ranking; hover never fires while the mouse is not the input mode.
		if c.drawerOpen() {
			if c.drawerHoverLock > 0 {
				c.drawerHoverLock--
			} else {
				y0, y1 := c.drawerYRange(l)
				x0 := transcriptX0(l)
				x1 := x0 + drawerMaxW(c.width, l.vpWidth+transcriptBorder)
				if msg.Y >= y0 && msg.Y < y1 && msg.X >= x0 && msg.X < x1 {
					if item, ok := c.drawerSelAt(msg.Y, l); ok {
						c.drawerSelectItem(item, l)
						return nil
					}
				}
			}
		}
		c.hoverPeer = "" // default: outside every row
		if msg.X >= 0 && msg.X < c.width && msg.Y >= 0 && msg.Y < c.height {
			inColumn := msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth
			sideTop := l.rosterY0 - searchHeightFor(l.headRows, l.sidebarWidth)
			if inColumn && l.sidebarOn && msg.X != l.rosterX+l.sidebarWidth-1 && msg.Y >= sideTop {
				if peer, _ := c.itemAtY(msg.Y, l); peer != c.hoverPeer {
					c.hoverPeer = peer
					c.syncRosterVp() // hover repaint lives in the scroll content
				}
			}
		}
		return nil

	case tea.MouseActionRelease:
		if c.drag.active {
			c.drag.active = false
			return nil
		}
	}

	switch msg.Type {
	case tea.MouseWheelUp, tea.MouseWheelDown:
		if c.width == 0 || c.height == 0 {
			return nil
		}
		up := msg.Type == tea.MouseWheelUp
		// Drawer wheel parity: over the panel the wheel steps the
		// selection exactly like the arrow keys (same wrap, same window
		// settle) — never the transcript underneath.
		if c.drawerOpen() {
			y0, y1 := c.drawerYRange(l)
			if msg.Y >= y0 && msg.Y < y1 {
				d := 1
				if up {
					d = -1
				}
				c.drawerStep(d)
				return nil
			}
		}
		// Route by pane: conversation rail vs transcript — each scrolls only
		// itself, with steps proportional to its own height.
		sideTop := l.rosterY0 - searchHeightFor(l.headRows, l.sidebarWidth)
		if l.sidebarOn && msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth &&
			msg.Y >= sideTop {
			step := wheelStepFor(c.rosterVp.Height)
			if up {
				c.rosterVp.LineUp(step)
			} else {
				c.rosterVp.LineDown(step)
			}
			return nil
		}
		step := wheelStepFor(c.vp.Height)
		before := c.vp.YOffset
		if up {
			c.vp.LineUp(step)
		} else {
			c.vp.LineDown(step)
		}
		c.dismissAuxOnScroll(before)
		return nil
	}
	return nil
}

// dragTo maps a dragged thumb row to the pane's scroll offset.
func (c *chatScreen) dragTo(sec scrollSection, y int, g barGeom, l layout) {
	if g.trackH <= 0 {
		return
	}
	newTop := y - c.drag.grabOff - g.trackY0
	if newTop < 0 {
		newTop = 0
	}
	if newTop > g.trackH-g.thumbH {
		newTop = g.trackH - g.thumbH
	}
	frac := float64(newTop) / float64(maxInt(g.trackH-g.thumbH, 1))
	var vp *viewport.Model
	total := 0
	switch sec {
	case secChat:
		vp = &c.vp
		total = c.vp.TotalLineCount()
	case secRoster:
		vp = &c.rosterVp
		total = c.rosterVp.TotalLineCount()
	}
	if vp == nil {
		return
	}
	maxOff := total - vp.Height
	if maxOff <= 0 {
		return
	}
	before := vp.YOffset
	vp.SetYOffset(int(float64(maxOff) * frac))
	if sec == secChat {
		c.dismissAuxOnScroll(before)
	}
}

func (c chatScreen) View() string {
	// Degenerate sizes paint nothing: a 0x0 (or otherwise unmeasured)
	// terminal must never be handed a row it has no room for. bubbletea
	// delivers the first WindowSizeMsg before the shell has anything to
	// draw, so this costs only the very first frame.
	if c.width <= 0 || c.height <= 0 {
		return ""
	}
	l := c.layoutFor()
	// Main column width: focus gutter + transcript content + scroll rail.
	colW := l.vpWidth + transcriptBorder

	mainRows := make([]string, 0, l.vpHeight+l.headRows+l.hintRows+8)
	appendRows := func(block string, n int) {
		lines := strings.Split(block, "\n")
		for len(lines) < n {
			lines = append(lines, "")
		}
		if len(lines) > n {
			lines = lines[:n]
		}
		for _, ln := range lines {
			mainRows = append(mainRows, fitRow(ln, colW))
		}
	}

	// 1. chat header — one tinted band, no box.
	if l.headRows > 0 {
		head := c.roomHeaderCompact(colW)
		if l.headRows >= 2 {
			head = c.roomHeaderView(colW)
		}
		appendRows(head, l.headRows)
	}

	// 2. transcript — focus gutter, content, scroll rail.
	if l.vpHeight > 0 {
		vp := c.vp
		vp.Width = l.vpWidth
		vp.Height = l.vpHeight
		gutterCh := " "
		if c.focus == focusTranscript {
			gutterCh = thFocusBar.Render("▌")
		}
		gutter := strings.TrimSuffix(strings.Repeat(gutterCh+"\n", l.vpHeight), "\n")
		block := lipgloss.JoinHorizontal(lipgloss.Top, gutter, vp.View(), c.scrollbarView(l.vpHeight))
		appendRows(block, l.vpHeight)
	}

	// 3. drawer — pops out of the composer, above it.
	pal := ""
	if l.paletteRows > 0 {
		pal = c.drawerView(colW)
	}
	if pal != "" {
		appendRows("", 1) // lift spacer
		appendRows(pal, lipgloss.Height(pal))
	}

	// 3.5 pinned reply citation — one tinted row directly above the
	// composer, X at the right edge (WhatsApp-style quote card).
	if l.quoteRows > 0 {
		appendRows(c.quoteComposerRow(colW), l.quoteRows)
	}

	// 4. composer — the one persistently bordered surface.
	composerRows := 1
	if l.composerRows > 0 {
		composerRows = l.composerRows + 2 // box border + interior
	}
	appendRows(c.composerBox(l, colW, pal != ""), composerRows)

	// 5. truthful key hints directly under the composer.
	if l.hintRows > 0 {
		appendRows(keyHintsView(colW, c.focus), l.hintRows)
	}

	// ---- assemble: sidebar | gutter | main -------------------------------
	fill := len(mainRows)
	out := make([]string, 0, fill+2)
	if l.showHeader {
		out = append(out, fitRow(c.headerView(), c.width))
	}
	if l.sidebarOn {
		raw := strings.Split(c.rosterBody(fill), "\n")
		for i := 0; i < fill; i++ {
			row := tintFit(thSidebarStyle, "", l.sidebarWidth)
			if i < len(raw) {
				row = fitRow(raw[i], l.sidebarWidth)
			}
			out = append(out, row+" "+mainRows[i])
		}
	} else {
		out = append(out, mainRows...)
	}
	if l.statusRows == 1 {
		out = append(out, fitRow(c.statusView(), c.width))
	}
	// Every row is pinned to the terminal width before it is joined, so the
	// painted frame can never exceed the window.
	for i := range out {
		out[i] = fitRow(out[i], c.width)
	}
	// The floating reply menu overlays the frame near its anchor; any
	// repaint after it dismisses repaints the pristine rows underneath.
	if c.replyMenu != nil {
		c.overlayReplyMenu(out, l)
	}
	result := strings.Join(out, "\n")
	if l.frameOn {
		result = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Render(result)
	}
	return result
}

// composerBox renders the message field: a rounded box whose border turns
// accent when the composer owns focus (and dims while the drawer above it
// is the focused surface). The clip glyph at the right opens the upload
// browser and is hit-tested through composerGeoms.
func (c chatScreen) composerBox(l layout, colW int, drawerOpen bool) string {
	focused := c.focus == focusComposer && !drawerOpen
	if l.composerRows <= 0 {
		// Bare prompt on tiny terminals: the accent block in front of the
		// caret is the composer's focus indicator (there is no border here
		// to carry it).
		prefix := " "
		if focused {
			prefix = thFocusBar.Render("▌")
		}
		return prefix + c.input.View()
	}
	inputOuter := colW
	contentW := maxInt(inputOuter-2, 1) // border only: no inner padding
	showClip := inputOuter >= 26
	clipW := 0
	if showClip {
		clipW = lipgloss.Width("📎") + 1
	}
	field := c.input.View()
	if lipgloss.Width(field) > maxInt(contentW-clipW, 1) {
		field = truncateByWidth(field, maxInt(contentW-clipW, 1))
	}
	tail := ""
	if showClip {
		tail = " " + thClipStyle.Render("📎")
	}
	pad := contentW - lipgloss.Width(field) - lipgloss.Width(tail)
	if pad < 0 {
		pad = 0
	}
	st := thComposerBoxStyle
	if focused {
		st = thComposerFocusStyle
	}
	return st.
		Width(inputOuter - 2).
		Height(l.composerRows).
		Render(retint(field+strings.Repeat(" ", pad)+tail, st))
}

// runChatTUI is the default interactive experience (alt-screen + mouse).
// The program model is rootModel: the chat screen at rest, full-screen
// windows (/settings, /new-group) hosted on top.
func runChatTUI(serverURL, key, me string, id *identityKey, password string) {
	scr := newChatScreen(serverURL, key, me, id, password)
	// Wire the local group-membership store and paint its saved groups
	// optimistically BEFORE the roster seed: the sidebar shows the known
	// groups immediately and Init fires the signed background re-joins.
	scr.groupsPath = groupsStorePath()
	scr.restorePersistedGroups()
	// Seed the roster synchronously so the sidebar isn't empty on paint;
	// the engine beat loop keeps it fresh, rosterTickMsg renders it.
	if roster, epoch, err := scr.sig.heartbeat("", nil); err == nil {
		scr.eng.applyPushedRoster(roster, epoch)
		scr.users = onlineNames(roster, me)
	}
	scr.eng.start()
	p := tea.NewProgram(newRootModel(scr), tea.WithAltScreen(), tea.WithMouseAllMotion())
	fm, err := p.Run()
	if err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	// Post-Run teardown: stop every session engine and register any leave
	// that never went through (abnormal exits; normally a no-op thanks to
	// the in-loop guards).
	rm, ok := fm.(rootModel)
	if !ok {
		rm = newRootModel(scr)
	}
	rm.chat.shutdownSessions()
	if !rm.chat.leftSent.Load() {
		if err := rm.chat.sig.leaveRoom(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: leave may not have registered (%v)\n", err)
		}
	}
	fmt.Printf("\nYou left session %s.\n", key)
}

// onlineNames extracts online usernames (sidebar order = sorted).
func onlineNames(roster []rosterMember, me string) []string {
	var out []string
	for _, m := range roster {
		if m.Online {
			out = append(out, m.Username)
		}
	}
	sort.Strings(out)
	// keep me first for orderedUsers parity
	names := []string{me}
	for _, u := range out {
		if u != me {
			names = append(names, u)
		}
	}
	return names
}
