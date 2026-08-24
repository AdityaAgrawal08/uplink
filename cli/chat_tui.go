package main

import (
	"fmt"
	"os"
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

	tuiRosterTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("15")).
				Background(lipgloss.Color("62")).
				Width(rosterWidthInner).
				Padding(0, 0)

	tuiRosterBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("240")).
				Width(rosterWidthInner)

	tuiRosterSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Background(lipgloss.Color("236"))
)

// ---- layout constants --------------------------------------------------------

const (
	rosterTotalWidth  = 24 // outer width of the sidebar incl. borders
	rosterWidthInner  = rosterTotalWidth - 2
	rosterMaxVisible  = 12 // max user rows shown before "... +N more"
	inputChromeHeight = 3  // rounded-border box around the text input
	headerHeight      = 1  // top banner line
	minViewportHeight = 4  // never give the transcript fewer lines
	minViewportWidth  = 30 // never narrow the transcript below this
)

// layout is the single source of truth for frame geometry. Both View() and the
// mouse hit-test derive their math from this struct so a click always maps to
// exactly what is on screen.
type layout struct {
	vpWidth    int // transcript viewport inner width
	vpHeight   int // transcript viewport visible rows
	rosterX    int // leftmost column of the sidebar
	rosterY0   int // first terminal row inside the sidebar that holds a user
	statusRows int // extra rows consumed by the status line (0 or 1)
}

// computeLayout derives frame geometry purely from terminal size and whether
// the status line is visible. Pure function => trivially unit-testable.
func computeLayout(termW, termH int, showStatus bool) layout {
	l := layout{}
	if termW <= 0 || termH <= 0 {
		return l
	}
	l.statusRows = 0
	if showStatus {
		l.statusRows = 1
	}
	fixed := headerHeight + inputChromeHeight + l.statusRows

	l.vpHeight = termH - fixed
	if l.vpHeight < minViewportHeight {
		l.vpHeight = minViewportHeight
	}
	// Sidebar sits to the RIGHT of the bordered transcript.
	// Border adds 2 columns; keep 1 spacer col between panels.
	l.rosterX = termW - rosterTotalWidth
	l.vpWidth = l.rosterX - 3 // 2 border cols + 1 spacer
	if l.vpWidth < minViewportWidth {
		l.vpWidth = minViewportWidth
		l.rosterX = l.vpWidth + 3
	}
	// Row just after the header is the top border; first user row is inside.
	l.rosterY0 = headerHeight + 2 // header + top border row
	return l
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
	ended   bool
	err     error
}
type beatDoneMsg struct {
	users []string
	err   error
}
type sendDoneMsg struct {
	text string
	seq  int
	code int
	err  error
}
type leaveDoneMsg struct{}

// ---- model -----------------------------------------------------------------

type chatScreen struct {
	client       *chatClient
	key          string
	me           string
	width        int
	height       int
	lines        []string
	rendered     map[int]bool // server seqs already on screen (dedupes optimistic echo)
	pending      *pendingSend // single in-flight send (nil = idle)
	outbox       []string     // queued lines waiting for the in-flight send to settle
	users        []string
	vp           viewport.Model
	input        textinput.Model
	status       string
	targetUser   string // private-chat peer; "" = common room
	beatFailures int
	pollFailures int
}

// pendingSend tracks the optimistic echo line for the in-flight send so the
// confirmation can swap it in place (or mark failure) without ambiguity.
type pendingSend struct {
	lineIdx int
	text    string
}

func newChatScreen(serverURL, key, me string) chatScreen {
	ti := textinput.New()
	ti.Placeholder = "Type a message… (/help)"
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "> "
	vp := viewport.New(80, 20)
	return chatScreen{
		client:   newChatClient(serverURL, key, me),
		key:      key,
		me:       me,
		vp:       vp,
		input:    ti,
		rendered: map[int]bool{},
		outbox:   nil,
	}
}

// isOwnPresence reports whether a system presence line refers to me.
// Server format is strictly "<username> joined" / "<username> left", so an
// exact prefix+" joined/left" match avoids the bob/bobby false positive that
// a bare strings.Contains would produce.
func isOwnPresence(systemText, me string) bool {
	return systemText == me+" joined" || systemText == me+" left"
}

// mentionsUser reports whether a presence line concerns the given user.
func mentionsUser(systemText, user string) bool {
	return systemText == user+" joined" || systemText == user+" left"
}

// shouldRender decides visibility BEFORE any styling, so filtered messages
// never leave blank husks in the transcript.
func (c *chatScreen) shouldRender(m chatMessage) bool {
	if m.Kind == "system" {
		if isOwnPresence(m.Text, c.me) {
			return false // never announce my own entry/exit to myself
		}
		if c.targetUser != "" &&
			!mentionsUser(m.Text, c.targetUser) && !mentionsUser(m.Text, c.me) {
			return false // private view: only presence involving the pair
		}
		return true
	}
	if c.targetUser != "" && m.Username != c.me && m.Username != c.targetUser {
		return false
	}
	return true
}

func (c *chatScreen) renderLine(m chatMessage) string {
	ts := tuiTimeStyle.Render("[--:--]")
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		ts = tuiTimeStyle.Render("[" + t.Local().Format("15:04") + "]")
	}
	if m.Kind == "system" {
		return ts + " " + tuiSystemStyle.Render("* "+m.Text)
	}
	name := tuiNameStyle.Render(m.Username)
	if m.Username == c.me {
		name = tuiMeStyle.Render(name + " (you)")
	}
	if c.targetUser != "" && m.Username == c.targetUser {
		name = tuiRosterSelectedStyle.Render(m.Username)
	}
	return ts + " " + name + ": " + m.Text
}

// addMessage renders a confirmed server message exactly once — and only when
// the current view wants it (own presence suppressed, private-mode filter).
func (c *chatScreen) addMessage(m chatMessage) {
	if c.rendered[m.Seq] {
		return
	}
	c.rendered[m.Seq] = true
	if !c.shouldRender(m) {
		return // filtered: no blank line, nothing appended
	}
	c.appendLine(c.renderLine(m))
}

func (c *chatScreen) appendLine(s string) {
	c.lines = append(c.lines, s)
	c.vp.SetContent(strings.Join(c.lines, "\n"))
	c.vp.GotoBottom()
}

// refreshViewport re-serializes the transcript (used after in-place edits).
func (c *chatScreen) refreshViewport() {
	c.vp.SetContent(strings.Join(c.lines, "\n"))
	c.vp.GotoBottom()
}

func (c chatScreen) headerView() string {
	mode := ""
	if c.targetUser != "" {
		mode = fmt.Sprintf(" · private: %s (Esc to exit)", c.targetUser)
	}
	return tuiHeaderStyle.Render(fmt.Sprintf(
		" uplink chat · key %s · you are %s · %d online%s ", c.key, c.me, len(c.users), mode))
}

func (c chatScreen) statusView() string {
	if c.status == "" {
		return ""
	}
	return tuiErrStyle.Render(c.status)
}

// rosterRow renders one sidebar entry; selected highlights the active peer.
func rosterRow(u, me, target string) string {
	switch {
	case u == me:
		return tuiMeStyle.Render("· " + u + " (you)")
	case u == target:
		return tuiRosterSelectedStyle.Render("● " + u)
	default:
		return "○ " + u
	}
}

// rosterBody produces the raw (bordered) sidebar content. Height is fully
// deterministic: title + rosterMaxVisible slots (+2 border rows) regardless of
// participant count — the overflow indicator REPLACES the final slot.
func (c chatScreen) rosterBody() string {
	rows := []string{tuiRosterTitleStyle.Render("Users")}
	for i := 0; i < rosterMaxVisible; i++ {
		switch {
		case i >= len(c.users):
			rows = append(rows, "")
		case i == rosterMaxVisible-1 && len(c.users) > rosterMaxVisible:
			more := len(c.users) - (rosterMaxVisible - 1)
			rows = append(rows, tuiTimeStyle.Render(fmt.Sprintf("… +%d more", more)))
		default:
			rows = append(rows, rosterRow(c.users[i], c.me, c.targetUser))
		}
	}
	return tuiRosterBoxStyle.Render(strings.Join(rows, "\n"))
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
		return pollDoneMsg{newMsgs: newMsgs, ended: ended, err: err}
	}
}

func (c chatScreen) doBeat() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		hb, err := client.beatOnce()
		return beatDoneMsg{users: hb.ActiveUsers, err: err}
	}
}

func (c chatScreen) doSend(text string) tea.Cmd {
	client := c.client
	return func() tea.Msg {
		code, msg, err := client.sendMessage(text)
		return sendDoneMsg{text: text, seq: msg.Seq, code: code, err: err}
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

// ---- tea.Model -------------------------------------------------------------

func (c chatScreen) Init() tea.Cmd {
	return tea.Batch(c.fetchBacklogCmd(), c.doBeat(), schedulePoll(0), scheduleBeat())
}

func (c *chatScreen) handleNewMessage(m chatMessage) {
	c.addMessage(m)
}

// enterPrivate switches to a 1:1 view; returns a system line to append.
func (c *chatScreen) enterPrivate(user string) {
	c.targetUser = user
	c.appendLine(tuiSystemStyle.Render("* Private chat with " + user + " — Esc for common room"))
}

// exitPrivate returns to the common room; returns a system line or "".
func (c *chatScreen) exitPrivate() {
	if c.targetUser == "" {
		return
	}
	c.targetUser = ""
	c.appendLine(tuiSystemStyle.Render("* Back in the common room"))
}

func (c *chatScreen) submitLine(text string) {
	switch strings.ToLower(text) {
	case "/users":
		c.appendLine(tuiSystemStyle.Render("* Online: " + strings.Join(c.users, ", ")))
	case "/help":
		hint := "* Commands: /users · /exit · click a name in the sidebar for private chat"
		c.appendLine(tuiSystemStyle.Render(hint))
	default:
		if c.pending != nil {
			c.outbox = append(c.outbox, text) // one wire message at a time
			return
		}
		c.dispatchSend(text)
	}
}
func (c *chatScreen) dispatchSend(text string) {
	c.lines = append(c.lines, tuiMeStyle.Render("[you →] "+text))
	idx := len(c.lines) - 1
	c.pending = &pendingSend{lineIdx: idx, text: text}
	c.refreshViewport()
}

func (c chatScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		l := computeLayout(c.width, c.height, c.status != "")
		c.vp.Width = l.vpWidth
		c.vp.Height = l.vpHeight

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
		}
		cmds = append(cmds, scheduleBeat())

	case sendDoneMsg:
		c.settleSend(msg)

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
			if strings.EqualFold(text, "/exit") || strings.EqualFold(text, "/quit") {
				c.appendLine(tuiSystemStyle.Render("* You left the session."))
				cmds = append(cmds, c.doLeave(), tea.Quit)
				return c, tea.Batch(cmds...)
			}
			c.submitLine(text)
			var ic tea.Cmd
			c.input, ic = c.input.Update(msg)
			cmds = append(cmds, ic)
			break
		}
		var ic tea.Cmd
		c.input, ic = c.input.Update(msg)
		cmds = append(cmds, ic)
	}

	// Viewport keeps its own scroll handling for every message.
	var vpc tea.Cmd
	c.vp, vpc = c.vp.Update(msg)
	cmds = append(cmds, vpc)

	return c, tea.Batch(cmds...)
}

// settleSend resolves the optimistic echo for the completed send and pumps
// the outbox so queued lines go out one at a time.
func (c *chatScreen) settleSend(msg sendDoneMsg) {
	var replacement string
	switch {
	case msg.code == 429:
		replacement = tuiErrStyle.Render("✗ slow down — try again")
	case msg.code == 410:
		replacement = tuiSystemStyle.Render("* Session has ended")
	case msg.err != nil:
		replacement = tuiErrStyle.Render("✗ send failed: " + msg.err.Error())
	default:
		m := chatMessage{Seq: msg.seq, Username: c.me, Kind: "chat", Text: msg.text}
		c.rendered[m.Seq] = true
		if c.shouldRender(m) {
			replacement = c.renderLine(m)
		} else {
			replacement = "" // filtered mid-flight (rare): drop the echo
		}
	}

	if c.pending != nil && c.pending.lineIdx >= 0 && c.pending.lineIdx < len(c.lines) {
		if replacement == "" {
			_ = copy(c.lines[c.pending.lineIdx:], c.lines[c.pending.lineIdx+1:])
			c.lines = c.lines[:len(c.lines)-1]
		} else {
			c.lines[c.pending.lineIdx] = replacement
		}
	} else if replacement != "" {
		c.appendLine(replacement)
	}
	if msg.code == 410 {
		c.refreshViewport()
		return
	}
	c.pending = nil
	c.refreshViewport()

	// Drain exactly one queued line per settled send.
	if n := len(c.outbox); n > 0 {
		next := c.outbox[0]
		c.outbox = c.outbox[1:]
		c.dispatchSend(next)
	}
}

// handleMouse translates a click into a sidebar selection using the SAME
// geometry View() will paint. Returns a tea.Cmd (send-free) or nil.
func (c *chatScreen) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Type != tea.MouseLeft {
		return nil
	}
	if c.width == 0 || c.height == 0 {
		return nil
	}
	// Reject coordinates outside the painted terminal area entirely.
	if msg.X < 0 || msg.X >= c.width || msg.Y < 0 || msg.Y >= c.height {
		return nil
	}
	l := computeLayout(c.width, c.height, c.status != "")
	inColumn := msg.X >= l.rosterX && msg.X < l.rosterX+rosterTotalWidth
	row := msg.Y - l.rosterY0
	if !inColumn || row < 0 || row >= rosterMaxVisible || row >= len(c.users) {
		return nil
	}
	u := c.users[row]
	if u == c.me {
		return nil // clicking yourself is a no-op
	}
	if u == c.targetUser {
		return nil // already chatting privately with them
	}
	c.enterPrivate(u)
	return nil
}

func (c chatScreen) View() string {
	if c.width == 0 || c.height == 0 {
		return "connecting…"
	}
	l := computeLayout(c.width, c.height, c.status != "")

	// Keep viewport dims in lockstep with the painted layout.
	vp := c.vp
	vp.Width = l.vpWidth
	vp.Height = l.vpHeight

	left := tuiBorderStyle.Render(vp.View())

	sidebar := c.rosterBody()
	// Pad the sidebar to the transcript height so both columns align at top.
	sidebarH := lipgloss.Height(left)
	sidebar = lipgloss.NewStyle().Height(sidebarH).Render(sidebar)

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", sidebar)

	rows := []string{c.headerView(), body}
	input := tuiBorderStyle.Render(c.input.View())
	rows = append(rows, input)
	if c.status != "" {
		rows = append(rows, c.statusView())
	}
	return strings.Join(rows, "\n")
}

// runChatTUI is the default interactive experience (alt-screen + mouse).
func runChatTUI(serverURL, key, me string) {
	scr := newChatScreen(serverURL, key, me)
	p := tea.NewProgram(scr, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	scr.client.leave()
	fmt.Printf("\nYou left session %s.\n", key)
}
