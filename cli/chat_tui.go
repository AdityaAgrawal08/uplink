package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
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
			BorderForeground(lipgloss.Color("#1e293b"))

	tuiSectionTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("250"))

	tuiDimStyle = lipgloss.NewStyle().Faint(true)

	tuiRosterBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#1e293b"))

	tuiHoverStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("213")) // pink — hover only, one row max

	tuiUnreadStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")). // white digits
			Background(lipgloss.Color("27"))  // blue disc: unread dot

	tuiComposerStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#1e293b")).
				Padding(0, 1)

	tuiScrollbarStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240"))
	tuiScrollbarThumbStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("245")).
				Background(lipgloss.Color("240"))

	// Bubble styles mirror the theme: own = blue right, other = navy left.
	tuiOwnBubbleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#1d4ed8")).
				Foreground(lipgloss.Color("#ffffff")).
				Padding(0, 1).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#1d4ed8"))

	tuiOtherBubbleStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#131b2e")).
				Foreground(lipgloss.Color("15")).
				Padding(0, 1).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#1e293b"))

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

// sidebarWidthFor picks a reading column that grows FLUIDLY with the
// terminal: ~1 extra cell per 10 terminal columns, clamped to a usable
// chat list (narrow enough to leave the transcript room, wide enough for
// avatar + name + time + preview). Every resize step visibly rebalances.
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

// videoSidebarWidthFor is the boosted column while video streams (room for
// a ~40-cell braille picture; roster still fits beneath).
const videoSidebarWidth = 44

func sidebarWidthVideo(termW int, videoOn bool) int {
	w := sidebarWidthFor(termW)
	if videoOn && termW >= 100 {
		return videoSidebarWidth
	}
	return w
}

// composerRowsFor gives the message box breathing room on tall screens and
// shrinks gracefully on small ones (0 => bare prompt, no border). Roomy
// terminals earn a fourth row; every step rebalances the transcript.
func composerRowsFor(termH int) int {
	switch {
	case termH >= 34:
		return composerRowsMax + 1
	case termH >= 22:
		return composerRowsMax
	case termH >= 16:
		return 2
	case termH >= 11:
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
	rosterX         int  // leftmost column of the sidebar (LEFT column)
	rosterY0        int  // first terminal row inside the sidebar that holds content
	rosterSlots     int  // legacy: how many roster rows fit (peerAtY now mirrors rosterBody directly)
	videoRows       int  // legacy sidebar video box (0: feeds live in the bottom strip)
	headRows        int  // room-header rows above the transcript (0 when collapsed)
	camRows         int  // bottom Live Cameras strip rows (0 when collapsed)
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
	h := l.vpHeight + l.statusRows + l.paletteRows + l.headRows + l.camRows
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
	return computeLayoutMedia(termW, termH, showStatus, paletteRows, false)
}

// computeLayoutMedia is the full layout: videoOn widens the sidebar column
// so the ASCII video pane gets a picture-width that matches its viewport.
func computeLayoutMedia(termW, termH int, showStatus bool, paletteRows int, videoOn bool) layout {
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
	l.sidebarWidth = sidebarWidthVideo(termW, videoOn)

	innerW := termW - frameChrome
	if !l.frameOn {
		innerW = termW
	}

	// --- width pass ---------------------------------------------------------
	// The sidebar is the LEFT column (chat list); the transcript paints to
	// its right. rosterX is therefore the frame inset, not the right edge.
	l.sidebarOn = termW >= minSidebarTermW
	if l.sidebarOn {
		l.rosterX = 0
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

// The engine pushes network events from its own goroutines; they arrive here
// through netCh (see drainNetCmd) because bubbletea Update must stay on the
// main loop. Buffer is generous; drops are safe (inbox redelivers unacked,
// P2P is already reliably delivered — a dropped paint is just a missed row).
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

// Media UI messages: publish status lines + VU level.
type mediaInfoMsg struct{ info string }
type callLevelMsg struct{ level float64 }

// netVideoMsg carries one decoded ASCII video frame for the drawer pane.
type netVideoMsg struct{ lines []string }

// netSelfVideoMsg carries one local-camera preview frame.
type netSelfVideoMsg struct{ lines []string }

// paneContent paints ONE feed: the remote publisher's video. The local
// self-view shows only when nobody else is publishing (it never stacks
// under a remote feed — two pictures in one small pane read as a glitch).
func (c *chatScreen) paneContent() []string {
	if len(c.videoLines) > 0 {
		return append([]string(nil), c.videoLines...)
	}
	if len(c.selfLines) == 0 {
		return nil
	}
	return append([]string{tuiPaletteHintStyle.Render("— you —")}, c.selfLines...)
}

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
	hoverPeer  string               // sidebar row under the mouse ("" = none)
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
	// roomUnread counts room broadcasts that arrived while a DM thread is
	// in view (broadcasts otherwise paint nowhere and badge nothing — a
	// message can sit in history looking "missing"). Cleared on return to
	// the room. DM unreads keep using the per-peer map.
	roomUnread int
	users      []string
	// call owns the media lifecycle (publish/subscribe; nil-safe).
	call         *mediaManager
	callLevel    float64 // mic loudness for the status meter
	videoLines   []string
	selfLines    []string
	cameraOffset int
	vidCols      int // last tile geometry pushed via SetVideoSize (change-gated)
	vidRows      int
	videoVp      viewport.Model // scrollable video pane (wheel + scrollbar)
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
	targetUser   string // private-chat peer; "" = general room
}

// scrollSection identifies one independently scrollable pane.
type scrollSection int

const (
	secChat scrollSection = iota
	secVideo
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

func newChatScreen(serverURL, key, me string, id *identityKey, password string) chatScreen {
	ti := textinput.New()
	ti.Placeholder = composerPlaceholder
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "❯ "
	ti.Width = 36
	vp := viewport.New(80, 20)
	videoVp := viewport.New(40, 10)
	netCh := make(chan tea.Msg, 256)
	sig := &signalClient{serverURL: serverURL, key: key, me: me}
	// Engine callbacks only ever push into netCh (never touch the screen:
	// they run on network goroutines). The drain command below feeds them
	// into Update on the main loop.
	push := func(m tea.Msg) {
		select {
		case netCh <- m:
		default:
		}
	}
	var eng *engine
	callMgr := newMediaManager(me, id,
		func(to, noteType, payload string) error { return sig.signalSend(to, noteType, payload) },
		nil, // roster bound below once eng exists
		mediaUICallbacks{
			onInfo:       func(info string) { push(mediaInfoMsg{info: info}) },
			onLevel:      func(level float64) { push(callLevelMsg{level: level}) },
			onVideoFrame: func(lines []string) { push(netVideoMsg{lines: lines}) },
			onSelfFrame:  func(lines []string) { push(netSelfVideoMsg{lines: lines}) },
		})
	eng = newEngine(me, id, sig, engineCallbacks{
		onChat: func(c engineChat) {
			// Ack at receipt, not at paint: a dropped queue slot must not
			// silence the sender's backstop (the paint path still dedups).
			_ = eng.sendAck(c.From, c.MsgId)
			push(netChatMsg{chat: c})
		},
		onFile:       func(f engineFile) { push(netFileMsg{file: f}) },
		onFileErr:    func(msgId, from, reason string) { push(netFileErrMsg{msgId: msgId, from: from, reason: reason}) },
		onPeerReady:  func(user, code string) { push(netReadyMsg{user: user, code: code}) },
		onPeerLost:   func(user string) { push(netLostMsg{user: user}) },
		onDelivered:  func(msgId string) { push(netDeliveredMsg{msgId: msgId}) },
		onError:      func(err error) { push(netErrMsg{err: err}) },
		onSignalNote: func(n signalNote) { callMgr.onSignalNote(n) },
	})
	eng.joinPassword = password // enables engine self-rejoin after prune
	callMgr.SetRoster(func() map[string][]byte { return rosterMap(eng.peers()) })
	return chatScreen{
		sig:         sig,
		eng:         eng,
		call:        callMgr,
		key:         key,
		me:          me,
		vp:          vp,
		videoVp:     videoVp,
		input:       ti,
		netCh:       netCh,
		rendered:    map[int]bool{},
		renderCache: map[int]string{},
		tsCache:     map[int]time.Time{},
		wrapCache:   map[string]string{},
		unread:      map[string]int{},
		lastDMAt:    map[string]time.Time{},
		outbox:      nil,
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

func (c *chatScreen) renderLine(m chatMessage) string {
	// Timestamp - used for system lines and as bubble timestamp
	tsPlain := "--:--"
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		tsPlain = t.Local().Format("15:04")
	}
	ts := tuiTimeStyle.Render("[" + tsPlain + "]")
	bubbleTs := thBubbleTimeStyle.Render(tsPlain)
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

	// Bubble inner: plain text + timestamp. The sender line (avatar + name
	// + time) paints ABOVE the bubble, never inside it.
	innerWithTs := textRendered + "  " + bubbleTs

	// Compact hug: needed = content width + padding/border
	needed := lipgloss.Width(textRendered) + lipgloss.Width(tsPlain) + 6
	if needed < 14 {
		needed = 14
	}
	bubbleW := needed
	if bubbleW > maxBubbleW {
		bubbleW = maxBubbleW
	}
	bubbleInner := innerWithTs
	if lipgloss.Width(textRendered) > maxBubbleW-10 {
		bubbleInner = textRendered + "\n" + strings.Repeat(" ", max(0, bubbleW-lipgloss.Width(tsPlain)-4)) + bubbleTs
	}

	var style lipgloss.Style
	if isOwn {
		style = thOwnBubbleStyle
		// Unconfirmed own message (sent, no delivery ack yet): render dim
		// so "sent" is never confused with "received". The ack
		// (netDeliveredMsg) restores full brightness.
		if _, ok := c.unackedUI[m.MsgId]; ok && m.MsgId != "" {
			style = style.Faint(true)
		}
	} else {
		style = thOtherBubbleStyle
	}
	bubble := style.Width(bubbleW).Render(bubbleInner)

	name := m.Username
	if isOwn {
		name = "You"
	}
	nameStyle := lipgloss.NewStyle().Foreground(avatarColorFor(m.Username))
	if isOwn {
		nameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(thText))
	}
	// Cramped transcripts drop the avatar chip; the colored name + time
	// carry identity on one slimmer line.
	sender := nameStyle.Render(name) + "  " + thMsgTimeStyle.Render(tsPlain)
	if !compactTranscript(c.width) {
		sender = avatarCell(m.Username) + "  " + sender
	}

	if isOwn {
		// Right side: sender line + bubble both right-aligned.
		st := lipgloss.NewStyle().Width(availWidth).Align(lipgloss.Right)
		return st.Render(sender) + "\n" + st.Render(bubble)
	}
	return sender + "\n" + bubble
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

// renderedLine returns the cached bubble for a history message, rendering
// and memoizing on miss.
func (c *chatScreen) renderedLine(m chatMessage) string {
	if s, ok := c.renderCache[m.Seq]; ok {
		return s
	}
	s := c.renderLine(m)
	c.renderCache[m.Seq] = s
	return s
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
			card := fileAttachmentCard(fd.filename, fd.username, fd.size, fd.time, c.vp.Width)
			if fd.username == c.me {
				card = lipgloss.NewStyle().Width(c.vp.Width).Align(lipgloss.Right).Render(card)
			}
			c.lines = append(c.lines, card)
			ci++
		} else if hasHist {
			c.lines = append(c.lines, c.renderedLine(visibleHistory[hi]))
			hi++
		} else {
			break
		}
	}
	// Append transient pinned lines (pending echo, progress bars, errors) at bottom.
	for _, ll := range pinned {
		c.lines = append(c.lines, ll.text)
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
	c.vp.SetContent(strings.Join(wrapped, "\n"))
	if atBottom {
		c.vp.GotoBottom()
	}
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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

// rosterBody paints the chat list: search row, then one 2-row item per
// conversation (General room + DM threads) with avatars, previews, times and
// unread dots. fill = interior rows of the sidebar box. Overflow scrolls in
// the roster viewport with its own scrollbar column.
func (c chatScreen) rosterBody(fill int) string {
	inner := c.sidebarInnerWidth()
	search := c.searchRow(inner)
	if fill < 2 {
		return search // search only; border lives in View
	}
	bodyH := fill - 1 // search owns the first row
	items := c.chatItems()
	rows := c.chatItemRows(items, inner, c.hoverPeer)
	needBar := len(rows) > bodyH
	barW := 0
	if needBar {
		barW = 1 // chats overflow: the list scrolls, scrollbar joins
	}
	if !needBar {
		out := make([]string, 0, fill)
		out = append(out, search)
		out = append(out, rows...)
		for len(out) < fill {
			out = append(out, "")
		}
		return strings.Join(out, "\n")
	}
	// Overflow: the chat list scrolls inside its own viewport; the bar
	// column rides beside it.
	vp := c.rosterVp
	vp.Width = maxInt(inner-barW, 8)
	vp.Height = bodyH
	vp.SetContent(strings.Join(rows, "\n"))
	bar, _, _, _, _ := scrollbarBar(len(rows), bodyH, vp.YOffset, bodyH)
	body := lipgloss.JoinHorizontal(lipgloss.Top, vp.View(), bar)
	return search + "\n" + body
}

// searchRow paints the fixed sidebar search hint. Clicking it opens "/".
func (c chatScreen) searchRow(inner int) string {
	hint := thSearchStyle.Render("⌕ Search chats…")
	slash := thSearchStyle.Render("/")
	gap := inner - lipgloss.Width("⌕ Search chats…") - 1
	if gap < 1 {
		gap = 1
	}
	return hint + strings.Repeat(" ", gap) + slash
}

// chatItemHeight is the sidebar row budget per chat: two comfortable rows
// (name + preview) normally, one compact row (name only) when terminal
// height is scarce — short windows fit twice the chats without scrolling.
func (c chatScreen) chatItemHeight() int {
	if compactItems(c.width, c.height) {
		return 1
	}
	return itemRowsPerChat
}

// chatItemRows renders every chat item as exactly chatItemHeight() rows.
// hover names the peer under the mouse ("breadcrumb" highlight).
func (c chatScreen) chatItemRows(items []chatItem, inner int, hover string) []string {
	h := c.chatItemHeight()
	rows := make([]string, 0, len(items)*h)
	for _, it := range items {
		l1, l2 := chatItemRow(it, inner, hover)
		rows = append(rows, l1)
		if h > 1 {
			rows = append(rows, l2)
		}
	}
	return rows
}

// chatItemRow paints one sidebar item: avatar + name + time, then preview +
// unread dot. The active chat gets a tinted background with an accent bar;
// the hovered peer gets a highlight. Pure function of its inputs.
func chatItemRow(it chatItem, inner int, hover string) (string, string) {
	av := avatarCell(it.name)
	if it.isRoom {
		av = roomAvatarCell()
	}
	name := truncateStringPlain(it.name, maxInt(inner-12, 1))
	nameRendered := thChatNameStyle.Render(name)
	if hover == it.peer && it.peer != "" {
		nameRendered = thHoverRowStyle.Render(name)
	}
	if !it.live && !it.isRoom {
		nameRendered = tuiDimStyle.Render(name)
	}
	timeRendered := thChatTimeStyle.Render(it.timeStr)
	dot1 := " "
	if it.unread > 0 {
		dot1 = thUnreadDotStyle.Render("●")
	}
	// Plain widths drive the padding (ANSI-aware Width would agree, but the
	// plain parts are cheaper and exact here).
	gap1 := inner - (lipgloss.Width(av) + 1 + len([]rune(name)) + 1 + len([]rune(it.timeStr)) + 1 + 1)
	if gap1 < 1 {
		gap1 = 1
	}
	l1 := av + " " + nameRendered + strings.Repeat(" ", gap1) + timeRendered + " " + dot1

	preview := truncateStringPlain(it.preview, maxInt(inner-6, 1))
	pvRendered := thPreviewStyle.Render(preview)
	if it.unread > 0 && !it.isRoom {
		pvRendered = thUnreadNewStyle.Render(preview)
	}
	gap2 := inner - (4 + len([]rune(preview)) + 1 + 1)
	if gap2 < 1 {
		gap2 = 1
	}
	dot2 := " "
	if it.unread > 0 {
		dot2 = thUnreadDotStyle.Render("●")
	}
	l2 := "    " + pvRendered + strings.Repeat(" ", gap2) + dot2
	if it.active {
		// Tinted selection with an accent bar. The tint style deliberately
		// sets NO Width (Width would wrap overlong rows and break the
		// fixed item height); rows are space-padded to the column instead.
		bar := thSelBarStyle.Render("▌")
		barW := lipgloss.Width("▌")
		padRow := func(s string) string {
			if w := lipgloss.Width(s); w < inner-barW {
				s += strings.Repeat(" ", inner-barW-w)
			}
			return thSelRowStyle.Render(s)
		}
		l1 = bar + padRow(" "+l1)
		l2 = bar + padRow(" "+l2)
	}
	return l1, l2
}

// syncRosterVp bakes the (scrollable) chat list into the roster viewport
// so wheel + scrollbar drags operate on live content. Content is rebuilt
// on hover/rebuild/roster changes — few rows, cheap.
func (c *chatScreen) syncRosterVp() {
	inner := c.sidebarInnerWidth() - 1
	rows := c.chatItemRows(c.chatItems(), inner, c.hoverPeer)
	y := c.rosterVp.YOffset
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

// rosterRow paints one users-list row: presence dot, name, media badges,
// unread chip, hover highlight. Extracted so the scrollable viewport and
// the hit-test (peerAtY) read the same truth.
func (c chatScreen) rosterRow(u string, inner int, trunc func(string) string) string {
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

	// Media badges: publishing state at a glance — my row shows what
	// I send (▶ camera, ♪ mic), others' rows show what they share.
	if c.call != nil {
		mark := ""
		if u == c.me {
			if c.call.VideoOn() {
				mark += " ▶"
			}
			if c.call.AudioOn() {
				mark += " ♪"
			}
		} else {
			for _, p := range c.call.VideoPublishers() {
				if p == u {
					mark += " ▶"
				}
			}
			for _, p := range c.call.AudioPublishers() {
				if p == u {
					mark += " ♪"
				}
			}
		}
		line += tuiPaletteHintStyle.Render(mark)
	}

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
	return line
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

func scheduleRoster() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return rosterTickMsg{} })
}

// drainNetCmd is a self-perpetuating event pump: it waits briefly for the
// next engine event and yields either it or netIdleMsg, and BOTH handlers
// re-arm the pump — so exactly one pump goroutine exists at all times and
// engine traffic always reaches Update within ~100ms. (A one-shot drain
// would strand later events in netCh forever: nothing else schedules it.)
func (c chatScreen) drainNetCmd() tea.Cmd {
	return func() tea.Msg {
		select {
		case m := <-c.netCh:
			return m
		case <-time.After(100 * time.Millisecond):
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

// leftSent guards the leave POST exactly-once across the in-loop leave,
// repeat Ctrl+C presses, and the post-Run backup below.
var leftSent atomic.Bool

func (c chatScreen) doLeave() tea.Cmd {
	return func() tea.Msg {
		if leftSent.Swap(true) {
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

// ---- media publishing (/video + /audio) --------------------------------------

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

// toggleVideo runs the /video command: publish/stop camera to the scope.
func (c *chatScreen) toggleVideo() tea.Cmd {
	if c.call == nil {
		c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* media unavailable here"))
		return nil
	}
	if err := c.call.ToggleVideo(c.currentScope()); err != nil {
		c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* video failed: "+err.Error()))
	}
	return nil
}

// toggleAudio runs the /audio command: publish/stop mic to the scope.
func (c *chatScreen) toggleAudio() tea.Cmd {
	if c.call == nil {
		c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* media unavailable here"))
		return nil
	}
	if err := c.call.ToggleAudio(c.currentScope()); err != nil {
		c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* audio failed: "+err.Error()))
	}
	return nil
}

// mediaStatus renders the header chips for active media ("" when idle).
func (c *chatScreen) mediaStatus() string {
	if c.call == nil {
		return ""
	}
	var parts []string
	if scope := c.call.VideoScope(); scope != "" {
		parts = append(parts, "● VID → "+scope)
	}
	if c.call.Watching() {
		parts = append(parts, "● VID ← "+strings.Join(c.call.VideoPublishers(), ","))
	}
	if scope := c.call.AudioScope(); scope != "" {
		parts = append(parts, "● MIC → "+scope)
	}
	if c.call.Hearing() {
		parts = append(parts, "● MIC ← "+strings.Join(c.call.AudioPublishers(), ","))
	}
	if len(parts) == 0 {
		return ""
	}
	s := " · " + strings.Join(parts, " · ")
	if c.call.AudioOn() && c.callLevel > 0.02 {
		s += " " + vuBar(c.callLevel)
	}
	return s
}

func vuBar(level float64) string {
	n := int(level*8 + 0.5)
	if n < 0 {
		n = 0
	}
	if n > 8 {
		n = 8
	}
	full, empty := "", ""
	for i := 0; i < n; i++ {
		full += "▂"
	}
	for i := n; i < 8; i++ {
		empty += "·"
	}
	return full + empty
}

// enterPrivate switches to a 1:1 thread. There is no server history to
// deep-fetch (live messages only) — switching is instant.
func (c *chatScreen) enterPrivate(user string) tea.Cmd {
	c.targetUser = user
	c.palette.close()      // stale "/" query must not survive a mode switch
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
	c.roomUnread = 0 // back in the room: everything is visible again
	c.rebuildView()
}

// submitLine handles one committed input line. It returns the tea.Cmd that
// performs the network send (nil for local-only commands). CRITICAL: the
// caller MUST append this command — it is the ONLY thing that actually puts
// the message on the wire. Registry commands are executed here too, so a
// typed "/help" behaves exactly like one picked from the palette.
func (c *chatScreen) submitLine(text string) tea.Cmd {
	t := strings.ToLower(strings.TrimSpace(text))
	// /video + /audio take no username (publish to the current scope):
	// trailing words are ignored, never sent as chat.
	for _, name := range []string{"/video", "/audio"} {
		if t == name || strings.HasPrefix(t, name+" ") {
			return c.runCommand(name)
		}
	}
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
// transcript box without any resize event; without this, caches, wraps,
// and scroll math run on stale dims until the next terminal resize.
func (c *chatScreen) syncViewport() {
	if c.width <= 0 || c.height <= 0 {
		return
	}
	l := c.layoutFor()
	vpW := l.vpWidth
	if l.vpHeight > 0 && vpW > 10 {
		vpW--
	}
	// Bottom-strip tiles own the render size now (the retired sidebar pane
	// used to): push the TRUE tile geometry into the manager so frames
	// render at the tile's real width with full truecolor (no crop, no
	// downstream strip/re-wrap). Same pane-size contract as main.
	// Change-gated: re-rendering the pump on every keystroke would churn.
	if l.camRows > 0 && c.call != nil {
		inner := max(tileMinFor(camStripContentW(c.width, l.frameOn)), camTileInner(camStripContentW(c.width, l.frameOn), len(c.camFeeds())))
		rows := max(3, l.camRows-5)
		if inner != c.vidCols || rows != c.vidRows {
			c.call.SetVideoSize(inner, rows)
			c.vidCols, c.vidRows = inner, rows
		}
	} else {
		c.vidCols, c.vidRows = 0, 0
	}
	// Chat list gets its own viewport (scrollable like the transcript);
	// the search row lives outside the viewport.
	if l.sidebarOn {
		rosterH := l.headRows + l.vpHeight - l.videoRows
		c.rosterVp.Width = maxInt(c.sidebarInnerWidth()-1, 8) // scrollbar col
		c.rosterVp.Height = maxInt(rosterH-1, 1)              // search row
	}
	atBottom := c.vp.AtBottom()
	offset := c.vp.YOffset
	changed := vpW != c.vp.Width || l.vpHeight != c.vp.Height
	c.vp.Width, c.vp.Height = vpW, l.vpHeight
	// Composer input shares its row with the Send button: shrink the field
	// so typed text scrolls inside the box instead of under the button.
	transcriptOuter := l.vpWidth + 2
	inputOuter := transcriptOuter - sendBtnWidthFor(transcriptOuter) - 1
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

func (c chatScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	c.syncViewport() // drawer/status/sidebar changes alter geometry with no resize event

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.hoverPeer = "" // geometry changed; stale hover is meaningless
		c.syncViewport() // re-derives vp dims + input width, re-wraps on change

	case rosterTickMsg:
		// Sidebar freshness from the engine's heartbeat roster (the engine
		// owns the 5s beat; this only renders, every 2s). Membership lives
		// ONLY in the Online sidebar — no join/leave lines in the
		// transcript by product direction. On ANY change, rebuild
		// immediately: the tick used to update c.users with no repaint, so
		// the list visibly refreshed only when the next message arrived.
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
		cmds = append(cmds, scheduleRoster())

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

	case mediaInfoMsg:
		if msg.info != "" {
			c.appendLocal(c.activeConv(), tuiSystemStyle.Render("* "+msg.info))
		}
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case callLevelMsg:
		c.callLevel = msg.level
		cmds = append(cmds, c.drainNetCmd())

	case netVideoMsg:
		c.videoLines = msg.lines
		c.videoVp.SetContent(strings.Join(c.paneContent(), "\n"))
		c.syncViewport()
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case netSelfVideoMsg:
		c.selfLines = msg.lines
		c.videoVp.SetContent(strings.Join(c.paneContent(), "\n"))
		c.syncViewport()
		c.rebuildView()
		cmds = append(cmds, c.drainNetCmd())

	case netFileErrMsg:
		c.appendLocal(c.activeConv(), tuiErrStyle.Render(fmt.Sprintf("✗ file from %s failed: %s", msg.from, msg.reason)))
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
		c.appendLocal(c.activeConv(), tuiErrStyle.Render("* "+msg.err.Error()))
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
		if echoIdx >= 0 {
			c.localLines[echoIdx] = localLine{conv: pc, text: tuiErrStyle.Render("✗ send failed: " + msg.err.Error())}
		} else {
			c.appendLocal(c.activeConv(), tuiErrStyle.Render("✗ send failed: "+msg.err.Error()))
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
// peerAtY maps a terminal Y coordinate onto the sidebar's chat list: the
// DM peer for thread rows, "" for the General room, search box, or any
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

// atSearchBox reports a hit on the fixed sidebar search row (rosterY0-1).
// Clicking it focuses the "/" command drawer.
func (c chatScreen) atSearchBox(y int, l layout) bool {
	if c.width == 0 || c.height == 0 || !l.sidebarOn {
		return false
	}
	return y == l.rosterY0-1
}

// transcriptX0 is the terminal column of the transcript box's left border.
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
// (constants validated against View() by TestScrollbarDrag).
func (c chatScreen) scrollBarGeoms(l layout) (chat, video, roster barGeom) {
	frameOff := 0
	if l.frameOn {
		frameOff = 1
	}
	headOff := 0
	if l.showHeader {
		headOff = 1
	}
	// Transcript interior top = frame + top bar + room header + border;
	// the track starts one row below the up-arrow.
	chat = barGeom{
		x:       transcriptX0(l) + l.vpWidth,
		trackY0: frameOff + headOff + l.headRows + 2,
		trackH:  maxInt(l.vpHeight-2, 0),
	}
	// Sidebar video box is gone (feeds live in the bottom strip): the
	// video geom stays zero so wheel/drag routing skips it.
	if l.videoRows > 0 {
		video = barGeom{
			x:       l.rosterX + l.sidebarWidth - 2,
			trackY0: l.rosterY0 - l.videoRows + 1,
			trackH:  maxInt(l.videoRows-4, 0),
		}
	}
	if l.sidebarOn {
		rosterH := l.headRows + l.vpHeight - l.videoRows
		roster = barGeom{
			x:       l.rosterX + l.sidebarWidth - 2,
			trackY0: l.rosterY0 + 1,
			trackH:  maxInt(rosterH-3, 0),
		}
	}
	return chat, video, roster
}

// thumbFor computes the thumb position for one pane's scrollbar.
func (c chatScreen) thumbFor(sec scrollSection, g barGeom) barGeom {
	switch sec {
	case secChat:
		g.thumbTop, g.thumbH, _, _ = thumbGeom(c.vp.TotalLineCount(), c.vp.Height, c.vp.YOffset, g.trackH+2)
	case secVideo:
		g.thumbTop, g.thumbH, _, _ = thumbGeom(c.videoVp.TotalLineCount(), c.videoVp.Height, c.videoVp.YOffset, g.trackH+2)
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
		// Send button: submit the composer exactly like Enter.
		sendX0, sendX1, sendY0, sendY1, clipX := composerGeoms(l, c.width, c.height)
		if sendX0 >= 0 && msg.X >= sendX0 && msg.X < sendX1 && msg.Y >= sendY0 && msg.Y < sendY1 {
			text := strings.TrimSpace(c.input.Value())
			c.input.SetValue("")
			if text == "" {
				return nil
			}
			return c.submitLine(text)
		}
		// Clip glyph: open the upload browser (same as /upload).
		if clipX >= 0 && msg.X == clipX && msg.Y >= sendY0 && msg.Y < sendY1 && l.sidebarOn {
			if !c.picker.isActive() {
				return c.openPicker()
			}
			return nil
		}
		if !l.sidebarOn {
			break
		}
		chatG, videoG, rosterG := c.scrollBarGeoms(l)
		chatG = c.thumbFor(secChat, chatG)
		videoG = c.thumbFor(secVideo, videoG)
		rosterG = c.thumbFor(secRoster, rosterG)
		for _, g := range []struct {
			sec scrollSection
			b   barGeom
		}{{secChat, chatG}, {secVideo, videoG}, {secRoster, rosterG}} {
			if g.b.trackH <= 0 {
				continue
			}
			if msg.X == g.b.x && msg.Y >= g.b.trackY0 && msg.Y < g.b.trackY0+g.b.trackH {
				c.drag = barDrag{active: true, sec: g.sec, grabOff: msg.Y - (g.b.trackY0 + g.b.thumbTop)}
				c.dragTo(g.sec, msg.Y, g.b, l)
				return nil
			}
		}
		inColumn := msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth
		if !inColumn {
			return nil // clicks outside the sidebar never select
		}
		// Sidebar scrollbar column is drag-only, never selection.
		if msg.X == l.rosterX+l.sidebarWidth-2 {
			return nil
		}
		// Search row focuses the "/" drawer.
		if c.atSearchBox(msg.Y, l) {
			c.input.SetValue("/")
			c.palette.sync("/")
			return nil
		}
		// Chat row selection: General returns to the room, peers open threads.
		u, general := c.itemAtY(msg.Y, l)
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

	case tea.MouseActionMotion:
		if c.drag.active {
			_, videoG, rosterG := c.scrollBarGeoms(l)
			chatG, _, _ := c.scrollBarGeoms(l)
			switch c.drag.sec {
			case secChat:
				c.dragTo(secChat, msg.Y, c.thumbFor(secChat, chatG), l)
			case secVideo:
				c.dragTo(secVideo, msg.Y, c.thumbFor(secVideo, videoG), l)
			case secRoster:
				c.dragTo(secRoster, msg.Y, c.thumbFor(secRoster, rosterG), l)
			}
			return nil
		}
		c.hoverPeer = "" // default: outside every row
		if msg.X >= 0 && msg.X < c.width && msg.Y >= 0 && msg.Y < c.height {
			inColumn := msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth
			if inColumn && l.sidebarOn && msg.X != l.rosterX+l.sidebarWidth-2 {
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
		frameOffset, headerOffset := 0, 0
		if l.frameOn {
			frameOffset = 1
		}
		if l.showHeader {
			headerOffset = headerHeight
		}
		cameraY := frameOffset + headerOffset + l.headRows + l.vpHeight
		if l.boxedTranscript && l.vpHeight > 0 {
			cameraY += transcriptBorder
		} else if l.headRows > 0 && l.vpHeight == 0 {
			// body holds the room header only; no transcript border
		}
		if l.camRows > 0 && msg.Y >= cameraY && msg.Y < cameraY+l.camRows {
			// Wheels over an overflowing strip pan it horizontally;
			// otherwise they fall through to the transcript.
			width := camStripContentW(c.width, l.frameOn)
			count := len(c.camFeeds())
			inner := max(tileMinFor(width), camTileInner(width, count))
			if maxOffset := max(0, count*(inner+3)-1-width); maxOffset > 0 {
				step := wheelStepFor(l.camRows) * 4
				if up {
					step = -step
				}
				c.cameraOffset = min(maxOffset, max(0, c.cameraOffset+step))
				return nil
			}
		}
		// Route by pane: sidebar list vs transcript — each scrolls only
		// itself with steps proportional to its own height.
		if l.sidebarOn && msg.X >= l.rosterX && msg.X < l.rosterX+l.sidebarWidth &&
			msg.Y >= l.rosterY0-1 {
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
	case secVideo:
		vp = &c.videoVp
		total = c.videoVp.TotalLineCount()
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

	// Transcript box (right of the sidebar).
	transcriptOuter := l.vpWidth + 2
	var transcript string
	switch {
	case l.vpHeight == 0:
		// viewport.View() emits one padded blank row even at Height 0;
		// omitting the block keeps the exact-row contract intact.
		transcript = ""
	case l.boxedTranscript:
		if scrollW > 0 {
			bar := c.scrollbarView(l.vpHeight)
			// Viewport content + scrollbar joined, then bordered.
			inner := lipgloss.JoinHorizontal(lipgloss.Top, vp.View(), bar)
			transcript = tuiBorderStyle.Render(inner)
		} else {
			transcript = tuiBorderStyle.Render(vp.View()) // exactly vpHeight+2 rows
		}
	default:
		if scrollW > 0 {
			bar := c.scrollbarView(l.vpHeight)
			transcript = lipgloss.JoinHorizontal(lipgloss.Top, vp.View(), bar)
		} else {
			transcript = vp.View() // degraded: border dropped on tiny terminals
		}
	}

	// Center column: room header on top of the transcript box. Short or
	// narrow terminals get the 1-row compact heading (headRows == 1).
	roomHead := ""
	if l.headRows == 1 {
		roomHead = c.roomHeaderCompact(transcriptOuter)
	} else if l.headRows >= 2 {
		roomHead = c.roomHeaderView(transcriptOuter)
	}
	chatCol := transcript
	if roomHead != "" && transcript != "" {
		chatCol = roomHead + "\n" + transcript
	} else if roomHead != "" {
		chatCol = roomHead
	}

	body := chatCol
	if l.sidebarOn && body != "" {
		// LEFT sidebar column: the chat list. Its outer height matches the
		// center column (room header + transcript box) so bottoms align.
		rosterH := l.headRows + l.vpHeight - l.videoRows
		col := tuiRosterBoxStyle.
			Width(c.sidebarInnerWidth()).
			Height(rosterH). // interior rows; border completes the column
			MaxHeight(rosterH).
			Render(c.rosterBody(rosterH))
		body = lipgloss.JoinHorizontal(lipgloss.Top, col, " ", chatCol)
	}
	rows := make([]string, 0, 8)
	if l.showHeader {
		rows = append(rows, c.headerView())
	}
	if body != "" {
		rows = append(rows, body)
	}
	// Bottom Live Cameras strip (full content width, budgeted in layout).
	if l.camRows > 0 {
		w := c.width
		if l.frameOn {
			w -= frameChrome
		}
		rows = append(rows, c.camerasStripView(maxInt(w, 0)))
	}
	// Composer row: input box + Send button (icon on narrow transcripts),
	// aligned under the transcript (indent keeps the left edge truthful).
	indent := strings.Repeat(" ", composerIndent(l))
	var input string
	switch {
	case l.composerRows > 0:
		boxH := l.composerRows + 2
		sendW := sendBtnWidthFor(transcriptOuter)
		inputOuter := transcriptOuter - sendW - 1
		contentW := maxInt(inputOuter-2-2, 1) // border + padding, never wrap
		showClip := inputOuter >= 26
		clipW := 0
		if showClip {
			clipW = lipgloss.Width("📎") + 1
		}
		field := c.input.View()
		if lipgloss.Width(field) > contentW-clipW {
			field = truncateByWidth(field, contentW-clipW)
		}
		tail := ""
		if showClip {
			tail = " " + thClipStyle.Render("📎")
		}
		pad := contentW - lipgloss.Width(field) - lipgloss.Width(tail)
		if pad < 0 {
			pad = 0
		}
		box := tuiComposerStyle.
			Width(inputOuter - 2).
			Height(l.composerRows).
			Render(field + strings.Repeat(" ", pad) + tail)
		input = lipgloss.JoinHorizontal(lipgloss.Top, box, " ", sendButtonView(boxH, sendW))
		input = indent + strings.ReplaceAll(input, "\n", "\n"+indent)
	case l.inputBoxed:
		input = indent + tuiBorderStyle.Render(c.input.View())
	default:
		input = indent + "❯ " + c.input.View()
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
func runChatTUI(serverURL, key, me string, id *identityKey, password string) {
	scr := newChatScreen(serverURL, key, me, id, password)
	// Seed the roster synchronously so the sidebar isn't empty on paint;
	// the engine beat loop keeps it fresh, rosterTickMsg renders it.
	if roster, err := scr.sig.heartbeat("", nil); err == nil {
		scr.eng.setRoster(roster)
		scr.users = onlineNames(roster, me)
	}
	scr.eng.start()
	p := tea.NewProgram(scr, tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	scr.eng.stop()
	if !leftSent.Load() {
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
