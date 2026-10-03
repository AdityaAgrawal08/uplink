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
	showHeader   bool // staged degradation: hide banner on tiny heights
	sidebarWidth int  // density-scaled conversation column
	composerRows int  // writable rows inside the composer box (1/0)
	frameOn      bool // reserved shell margin; the chat shell is edge-to-edge
}

// totalRows reports the exact number of terminal rows a frame will occupy.
func (l layout) totalRows() int {
	h := l.vpHeight + l.statusRows + l.paletteRows + l.headRows + l.hintRows
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
	return computeLayoutMedia(termW, termH, showStatus, paletteRows)
}

// computeLayoutMedia is THE pure geometry pass: terminal size + status +
// drawer budget in, every pane's rectangle out.
func computeLayoutMedia(termW, termH int, showStatus bool, paletteRows int) layout {
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
type netChatMsg struct{ chat engineChat }
type netFileMsg struct{ file engineFile }
type netFileErrMsg struct {
	msgId, from, reason string
}
type netReadyMsg struct {
	user, code string
}
type netLostMsg struct{ user string }
type netErrMsg struct{ err error }
type rosterTickMsg struct{}

// netRosterMsg arrives when the engine's beat learns membership moved
// (join/leave): refresh the sidebar now instead of waiting for the tick.
type netRosterMsg struct{}

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
func (c *chatScreen) syncRosterFromEngine() {
	if c.eng == nil {
		return // bare/test screens carry no engine
	}
	roster := c.eng.peers()
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
	if c.hoverPeer != "" && !live[c.hoverPeer] {
		c.hoverPeer = ""
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
type netDeliveredMsg struct{ msgId string }

// netReactionMsg carries one inbound reaction nudge. It never mutates local
// counts directly: the nudge schedules an immediate GET /reactions, keeping
// the server the single source of truth (and correctly handling replace and
// remove, which a delta-only frame cannot express without per-sender state).
type netReactionMsg struct{ reaction engineReaction }

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
}
type leaveDoneMsg struct{}

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
	// reaction drawer bar is open ("" = closed).
	reactionCounts       map[string]map[string]int
	myReactions          map[string]string
	pendingReactionMsgId string
	// lineMsg is parallel to lines: the MsgId each transcript entry belongs
	// to ("" for cards/system/pinned rows). refreshViewport expands it into
	// rowMsg (viewport content row -> MsgId) so a click maps to a message.
	lineMsg []string
	rowMsg  []string
	// roomUnread counts room broadcasts that arrived while a DM thread is
	// in view (broadcasts otherwise paint nowhere and badge nothing — a
	// message can sit in history looking "missing"). Cleared on return to
	// the room. DM unreads keep using the per-peer map.
	roomUnread int
	users      []string
	// call owns the media lifecycle (publish/subscribe; nil-safe).
	call         *mediaManager
	callLevel    float64        // mic loudness for the status meter
	callStart    time.Time      // latched while a call is live (timer source)
	rosterVp     viewport.Model // scrollable users list (wheel + scrollbar)
	vp           viewport.Model
	drag         barDrag // scrollbar drag state (any of the three panes)
	input        textinput.Model
	palette      paletteState    // "/" command drawer above the composer
	picker       pickerState     // file-browser mode of that drawer (/upload)
	uploadBuf    []string        // persistent upload buffer (survives picker close)
	uploadBufSet map[string]bool // set view of uploadBuf for O(1) lookups
	uploadQ      uploadState     // sequential session-file transfer queue
	received     []receivedFile  // files arrived this session (for /download)
	status       string
	targetUser   string       // private-chat peer; "" = general room
	leftSent     *atomic.Bool // per-screen leave guard (pointer: screen is copied by value)
	drainTimer   *time.Timer  // reused pump timer (no time.After alloc per cycle)
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
// "general" in the common room, the canonical pair key inside a thread.
func (c *chatScreen) activeConv() string {
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
	sig := &signalClient{serverURL: serverURL, key: key, me: me}
	// Engine callbacks only ever push into netCh (never touch the screen:
	// they run on network goroutines). The drain command below feeds them
	// into Update on the main loop.
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
			if tryEnqueue(netCh, netChatMsg{chat: c}) {
				_ = eng.sendAck(c.From, c.MsgId)
			}
		},
		onFile:       func(f engineFile) { push(netFileMsg{file: f}) },
		onFileErr:    func(msgId, from, reason string) { push(netFileErrMsg{msgId: msgId, from: from, reason: reason}) },
		onPeerReady:  func(user, code string) { push(netReadyMsg{user: user, code: code}) },
		onPeerLost:   func(user string) { push(netLostMsg{user: user}) },
		onRoster:     func() { push(netRosterMsg{}) },
		onDelivered:  func(msgId string) { push(netDeliveredMsg{msgId: msgId}) },
		onReaction:   func(r engineReaction) { push(netReactionMsg{reaction: r}) },
		onError:      func(err error) { push(netErrMsg{err: err}) },
		onSignalNote: func(n signalNote) { callMgr.onSignalNote(n) },
	})
	eng.joinPassword = password // enables engine self-rejoin after prune
	callMgr.SetRoster(func() map[string][]byte { return rosterMap(eng.peers()) })
	return chatScreen{
		sig:            sig,
		eng:            eng,
		call:           callMgr,
		key:            key,
		me:             me,
		vp:             vp,
		input:          ti,
		netCh:          netCh,
		leftSent:       &atomic.Bool{},
		rendered:       map[int]bool{},
		renderCache:    map[int]string{},
		tsCache:        map[int]time.Time{},
		wrapCache:      map[string]string{},
		unread:         map[string]int{},
		lastDMAt:       map[string]time.Time{},
		outbox:         nil,
		reactionCounts: map[string]map[string]int{},
		myReactions:    map[string]string{},
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
	w := c.transcriptW()
	display := name
	var nameStyle lipgloss.Style
	if isOwn {
		display = "You"
		nameStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	} else {
		nameStyle = lipgloss.NewStyle().Bold(true).Foreground(avatarColorFor(name))
	}
	var line string
	if isOwn {
		line = thMsgTimeStyle.Render(tsPlain) + "  " + nameStyle.Render(display)
	} else {
		line = nameStyle.Render(display) + "  " + thMsgTimeStyle.Render(tsPlain)
	}
	if isOwn {
		return lipgloss.NewStyle().Width(w).Align(lipgloss.Right).Render(line)
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
// so "sent" is never confused with "received".
func chatBubble(text string, isOwn, dim bool, availWidth int) string {
	textRendered := renderMarkdown(text)
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
	body := chatBubble(m.Text, m.Username == c.me, dim, availWidth)
	if badge := c.reactionBadge(m); badge != "" {
		body += "\n" + badge
	}
	return body
}

// reactionBadge renders the chips under a bubble (`👍2 ❤️1`), in allowlist
// order, aligned to the same side as the bubble. Counts live in the map, so
// evictRenderCache must run whenever they move.
func (c *chatScreen) reactionBadge(m chatMessage) string {
	if m.MsgId == "" {
		return ""
	}
	counts := c.reactionCounts[m.MsgId]
	if len(counts) == 0 {
		return ""
	}
	var chips []string
	for _, e := range reactionEmojis {
		if n := counts[e]; n > 0 {
			chips = append(chips, tuiUnreadStyle.Render(fmt.Sprintf(" %s%d ", e, n)))
		}
	}
	if len(chips) == 0 {
		return ""
	}
	line := strings.Join(chips, " ")
	w := c.transcriptW()
	if m.Username == c.me {
		return lipgloss.NewStyle().Width(w).Align(lipgloss.Right).Render(line)
	}
	return truncateByWidth(line, w)
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

// applyFetchedReactions replaces the requested scope with the server's
// aggregate (the truth): ids absent from the response clear to zero.
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
		if c.myReactions[id] == newMine && reactionCountsEqual(c.reactionCounts[id], newCounts) {
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
		changed = append(changed, id)
	}
	for _, id := range changed {
		c.evictRenderCache(id)
	}
	return len(changed) > 0
}

// toggleReaction applies the optimistic toggle, evicts the stale bubble and
// returns the command that persists it (server POST + best-effort peer nudge).
func (c *chatScreen) toggleReaction(msgId, emoji string) tea.Cmd {
	if msgId == "" || !reactionEmojiAllowed(emoji) {
		return nil
	}
	prev := c.myReactions[msgId]
	c.applyLocalReaction(msgId, emoji)
	c.evictRenderCache(msgId)
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
	push := func(entry, msgId string) {
		if entry == "" {
			return // suppressed (own presence line): leave no blank row
		}
		c.lines = append(c.lines, entry)
		c.lineMsg = append(c.lineMsg, msgId)
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
			push(header+card, "")
			ci++
		} else if hasHist {
			m := visibleHistory[hi]
			withHeader := startGroup(m.Username, m.Kind, c.messageTime(m))
			push(c.renderedEntry(m, withHeader), reactableMsgId(m))
			hi++
		} else {
			break
		}
	}
	// Append transient pinned lines (pending echo, progress bars, errors) at bottom.
	for _, ll := range pinned {
		push(ll.text, "")
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
	// count + 1 screen rows (the viewport never re-wraps).
	c.rowMsg = c.rowMsg[:0]
	for i, wl := range wrapped {
		id := ""
		if i < len(c.lineMsg) {
			id = c.lineMsg[i]
		}
		n := strings.Count(wl, "\n") + 1
		for j := 0; j < n; j++ {
			c.rowMsg = append(c.rowMsg, id)
		}
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
// a faint placeholder) with the "/" affordance right-aligned. It sits on a
// stronger tint so it reads as a header without spending a row on a rule.
func (c chatScreen) sidebarHeader(inner int) string {
	glyph := "⌕ "
	affordance := " "
	textBudget := inner - lipgloss.Width(glyph) - 2
	if textBudget >= 1 {
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
	// Presence: the room is always live, peers show online/offline.
	presence := thPresenceStyle.Render("●")
	switch {
	case it.isRoom:
		presence = thPresenceStyle.Render("●")
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
	if !it.live && !it.isRoom {
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

func (c chatScreen) doSend(text, to string, seq int) tea.Cmd {
	return func() tea.Msg {
		// engine.sendChat is synchronous: P2P encrypt+send, or inbox seal+
		// deposit. Map failures onto the legacy settle codes.
		msgId, err := c.eng.sendChat(to, text)
		if err == nil {
			return sendDoneMsg{text: text, to: to, seq: seq, msgId: msgId, code: 201}
		}
		msg := err.Error()
		switch code := apiStatusCode(err); {
		case code == 429 || (code == 0 && strings.Contains(msg, "429")):
			return sendDoneMsg{text: text, to: to, seq: seq, code: 429, err: err}
		case code == 404 || code == 410 || strings.Contains(msg, "gone") ||
			strings.Contains(msg, "not in session") ||
			strings.Contains(msg, "Session not found"):
			return sendDoneMsg{text: text, to: to, seq: seq, code: 410, err: err}
		default:
			return sendDoneMsg{text: text, to: to, seq: seq, code: 500, err: err}
		}
	}
}

// doLeave guards the leave POST exactly-once per screen across the
// in-loop leave, repeat Ctrl+C presses, and the post-Run backup.
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

// ---- tea.Model -------------------------------------------------------------

func (c chatScreen) Init() tea.Cmd {
	// No backlog (the server keeps no transcript), no WS upgrade, no beat
	// tick (the engine owns heartbeats): just drain engine events and
	// refresh the sidebar roster on a slow tick.
	return tea.Batch(c.drainNetCmd(), scheduleRoster())
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
// deep-fetch (live messages only) — switching is instant.
func (c *chatScreen) enterPrivate(user string) tea.Cmd {
	c.targetUser = user
	c.palette.close()           // stale "/" query must not survive a mode switch
	c.pendingReactionMsgId = "" // the reacted row is not in the new thread
	delete(c.unread, user)      // opening the thread clears its badge
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
	c.pendingReactionMsgId = ""
	c.roomUnread = 0 // back in the room: everything is visible again
	c.rebuildView()
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
	// Optimistic echo: the same block a confirmed message will paint, held
	// faint until the delivery ack lands (netDeliveredMsg).
	echo := chatBubble(text, true, true, c.transcriptW())
	c.pushLocalLine(localLine{conv: conv, text: echo})
	c.pending = &pendingSend{text: text, conv: conv, to: peer, localIdx: len(c.localLines) - 1}
	target := peer
	if target == "" && c.pending.conv == generalConv {
		target = ""
	}
	seq := c.allocSeq()
	c.rebuildView()
	return c.doSend(text, target, seq)
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
		cmds = append(cmds, scheduleRoster())

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
		// display dedups here, never re-acks.
		if c.markSeen(m.MsgId) {
			cmds = append(cmds, c.drainNetCmd())
			break
		}
		cm := chatMessage{
			Seq: c.allocSeq(), MsgId: m.MsgId, Username: m.From, Kind: "chat",
			Text: m.Text, To: m.To,
			ConvID:    convFor(m.From, m.To),
			CreatedAt: time.Now().Format(time.RFC3339),
		}
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
		ts := time.Now()
		// Mirror the text path: DM arrivals always bump recency; room
		// files arriving in a thread view count room-unread instead of
		// vanishing silently.
		if conv != generalConv {
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
		// Bump unread if it landed in a background thread.
		if conv != c.activeConv() && conv != generalConv {
			if peer := peerOf(c.me, conv); peer != "" {
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
		// cached bubble or the dimmed paint would stick.
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
		// every sender's prior pick) — the fetch is the truth.
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
		if isServerDown(msg.err) {
			c.status = serverDownMsg
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
		if msg.Type == tea.KeyCtrlC {
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
		// Ctrl+K focuses the "/" command drawer (same as typing "/").
		// Intercepted before the input so the binding works everywhere;
		// this shadows the input's emacs kill-line, which the footer
		// does not advertise.
		if msg.Type == tea.KeyCtrlK {
			c.input.SetValue("/")
			c.palette.sync("/")
			c.input.Focus()
			c.focus = focusComposer     // the drawer rides on the composer
			c.pendingReactionMsgId = "" // one drawer slot: commands replace the bar
			break
		}
		// Ctrl+L clears the composer line and repaints the screen.
		if msg.Type == tea.KeyCtrlL {
			c.input.SetValue("")
			c.palette.sync("")
			return c, tea.ClearScreen
		}
		if msg.Type == tea.KeyEsc {
			// Esc dismisses transient state — the reaction bar first, then
			// the rail's filter, then the private view. NEVER quits the app.
			if c.pendingReactionMsgId != "" {
				c.pendingReactionMsgId = ""
				break
			}
			if c.sideFilter != "" {
				c.clearSideFilter()
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
			if msg.Type == tea.KeyHome {
				c.vp.GotoTop()
			} else {
				c.vp.GotoBottom()
			}
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
		// Keyboard reaction fallback (transcript focus): `r` opens the bar
		// on the newest message, then 1-6 toggle an emoji on the target.
		// Consumed before any text handling so the composer never sees them.
		if c.focus == focusTranscript && msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
			r := msg.Runes[0]
			consumed := false
			if r == 'r' || r == 'R' {
				consumed = true
				if c.pendingReactionMsgId == "" {
					c.pendingReactionMsgId = c.newestReactableMsgId()
				} else {
					c.pendingReactionMsgId = ""
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
		// Typing dismisses the transient reaction bar (the drawer slot is
		// modal: text and reactions never compete for the same keys).
		if textEditKey(msg) && c.pendingReactionMsgId != "" {
			c.pendingReactionMsgId = ""
		}
		// Any other key that edits the composer also focuses it, so the
		// indicator always sits where the text is about to appear.
		if c.focus != focusComposer && textEditKey(msg) {
			c.focus = focusComposer
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
		var vpc tea.Cmd
		c.vp, vpc = c.vp.Update(msg)
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
		if to != "" {
			conv = conversationKey(c.me, to)
		}
		m := chatMessage{
			Seq: msg.seq, MsgId: msg.msgId, Username: c.me, Kind: "chat",
			Text: msg.text, To: to, ConvID: conv,
			CreatedAt: time.Now().Format(time.RFC3339),
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
	c.pendingReactionMsgId = "" // focus moves: the transient bar dismisses
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

// drawerOpen reports whether the "/" drawer or the file browser owns the
// slot above the composer — and with it the focused-surface indicator, so
// nothing else may claim to be focused at the same time.
func (c chatScreen) drawerOpen() bool {
	return c.picker.isActive() || c.palette.visible()
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
// reaction drawer geometry so they can never disagree.
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

// reactionBarRow is the terminal row the reaction drawer bar paints on:
// right below the transcript, after the drawer's one-row lift spacer.
func reactionBarRow(l layout) int {
	return transcriptTopRow(l) + maxInt(l.vpHeight, 0) + 1
}

// msgAtY maps a transcript click row onto the MsgId painted there ("" for
// padding, local rows, or outside the transcript). rowMsg is the wrapped-row
// index refreshViewport builds, offset by the viewport's scroll position.
func (c chatScreen) msgAtY(y int, l layout) string {
	top := transcriptTopRow(l)
	if l.vpHeight <= 0 || y < top || y >= top+l.vpHeight {
		return ""
	}
	row := y - top + c.vp.YOffset
	if row < 0 || row >= len(c.rowMsg) {
		return ""
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
	l := c.layoutFor()
	// Legacy-typed motion messages (tests, X10 paths) carry Action=0.
	action := msg.Action
	if action == tea.MouseActionPress && msg.Type == tea.MouseMotion {
		action = tea.MouseActionMotion
	}

	switch action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft || c.width == 0 || c.height == 0 {
			break
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
			// Filter row: the "/" glyph at its right edge opens the command
			// drawer; anywhere else focuses the rail so the inline filter
			// can be typed.
			if c.atSearchBox(msg.Y, l) {
				if msg.X == l.rosterX+l.sidebarWidth-2 {
					c.input.SetValue("/")
					c.palette.sync("/")
					c.pendingReactionMsgId = "" // one drawer slot
					c.focus = focusComposer
				} else {
					c.focus = focusSidebar
				}
				return nil
			}
			// Chat row selection: General returns to the room, peers open threads.
			u, general := c.itemAtY(msg.Y, l)
			c.focus = focusSidebar
			switch {
			case general:
				if c.targetUser != "" {
					c.exitPrivate()
				}
				return nil
			case u == "", u == c.me:
				return nil // no row / clicking yourself is a no-op
			case u == c.targetUser:
				return nil // already chatting privately with them
			}
			c.enterPrivate(u)
			return nil
		}
		// Main column. An open reaction bar owns its row: a hit on one of
		// its choices toggles it; anywhere else on the row keeps the bar
		// open. Otherwise a click on a message bubble opens its reaction
		// bar (a second click on the same message closes it), and any other
		// row simply takes focus.
		if c.pendingReactionMsgId != "" && msg.Y == reactionBarRow(l) {
			if choice, ok := reactionCellAt(msg.X - transcriptX0(l)); ok && choice != "+" {
				if cmd := c.toggleReaction(c.pendingReactionMsgId, choice); cmd != nil {
					return cmd
				}
			}
			return nil
		}
		if id := c.msgAtY(msg.Y, l); id != "" && !c.drawerOpen() {
			c.focus = focusTranscript
			if c.pendingReactionMsgId == id {
				c.pendingReactionMsgId = "" // second click dismisses
			} else {
				c.pendingReactionMsgId = id
			}
			return nil
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
		if up {
			c.vp.LineUp(step)
		} else {
			c.vp.LineDown(step)
		}
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
	vp.SetYOffset(int(float64(maxOff) * frac))
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
func runChatTUI(serverURL, key, me string, id *identityKey, password string) {
	scr := newChatScreen(serverURL, key, me, id, password)
	// Seed the roster synchronously so the sidebar isn't empty on paint;
	// the engine beat loop keeps it fresh, rosterTickMsg renders it.
	if roster, epoch, err := scr.sig.heartbeat("", nil); err == nil {
		scr.eng.applyPushedRoster(roster, epoch)
		scr.users = onlineNames(roster, me)
	}
	scr.eng.start()
	p := tea.NewProgram(scr, tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	scr.eng.stop()
	if !scr.leftSent.Load() {
		// Backup for abnormal exits where doLeave never ran; normally a no-op.
		if err := scr.sig.leaveRoom(); err != nil {
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
