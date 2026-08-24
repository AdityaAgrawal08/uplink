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
)

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
	pendingIdx   int          // index of unconfirmed outgoing line (-1 = none)
	users        []string
	vp           viewport.Model
	input        textinput.Model
	status       string
	showRoster   bool
	beatFailures int
	pollFailures int
}

func newChatScreen(serverURL, key, me string) chatScreen {
	ti := textinput.New()
	ti.Placeholder = "Type a message… (/help)"
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "> "
	vp := viewport.New(80, 20)
	return chatScreen{
		client:     newChatClient(serverURL, key, me),
		key:        key,
		me:         me,
		vp:         vp,
		input:      ti,
		pendingIdx: -1,
		rendered:   map[int]bool{},
		showRoster: false,
	}
}

func (c *chatScreen) renderLine(m chatMessage) string {
	ts := tuiTimeStyle.Render("[--:--]")
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		ts = tuiTimeStyle.Render("[" + t.Local().Format("15:04") + "]")
	}
	if m.Kind == "system" {
		// Hide own entry/exit messages from the viewing user
		if m.Text != "" && strings.Contains(m.Text, c.me) {
			return ""
		}
		return ts + " " + tuiSystemStyle.Render("* "+m.Text)
	}
	name := tuiNameStyle.Render(m.Username)
	if m.Username == c.me {
		name = tuiMeStyle.Render(name + " (you)")
	}
	return ts + " " + name + ": " + m.Text
}

// addMessage renders a confirmed server message exactly once.
func (c *chatScreen) addMessage(m chatMessage) {
	if c.rendered[m.Seq] {
		return
	}
	c.rendered[m.Seq] = true
	c.appendLine(c.renderLine(m))
}

func (c *chatScreen) appendLine(s string) {
	c.lines = append(c.lines, s)
	c.vp.SetContent(strings.Join(c.lines, "\n"))
	c.vp.GotoBottom()
}

func (c chatScreen) headerView() string {
	return tuiHeaderStyle.Render(fmt.Sprintf(
		" uplink chat · key %s · you are %s · %d online ", c.key, c.me, len(c.users)))
}

func (c chatScreen) statusView() string {
	if c.status == "" {
		return ""
	}
	return "\n" + tuiErrStyle.Render(c.status)
}

func (c chatScreen) rosterView() string {
	if !c.showRoster {
		return ""
	}
	// Build roster lines: current user marked with ↦
	var roster []string
	roster = append(roster, tuiHeaderStyle.Render(" Users "))
	for _, u := range c.users {
		if u == c.me {
			roster = append(roster, tuiMeStyle.Render(" ↦ "+u+" (you)"))
		} else {
			roster = append(roster, fmt.Sprintf("   %s", u))
		}
	}
	roster = append(roster, tuiHeaderStyle.Render("────────────────"))
	return strings.Join(roster, "\n")
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

func (c chatScreen) initialBeatCmd() tea.Cmd {
	return c.doBeat()
}

// ---- tea.Model -------------------------------------------------------------

func (c chatScreen) Init() tea.Cmd {
	return tea.Batch(c.fetchBacklogCmd(), c.initialBeatCmd(), schedulePoll(0), scheduleBeat())
}

func (c *chatScreen) handleNewMessage(m chatMessage) {
	c.addMessage(m)
}

func (c chatScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.vp.Width = c.width - 2
		c.vp.Height = c.height - 7

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
			if len(msg.users) > 0 {
				c.users = msg.users
			}
		}
		cmds = append(cmds, scheduleBeat())

	case sendDoneMsg:
		switch {
		case msg.code == 429:
			c.lines[c.pendingIdx] = tuiErrStyle.Render("✗ slow down — try again")
			c.pendingIdx = -1
			c.vp.SetContent(strings.Join(c.lines, "\n"))
			c.vp.GotoBottom()
		case msg.code == 410:
			c.appendLine(tuiSystemStyle.Render("* Session has ended"))
			cmds = append(cmds, c.doLeave(), tea.Quit)
			return c, tea.Batch(cmds...)
		case msg.err != nil:
			c.lines[c.pendingIdx] = tuiErrStyle.Render("✗ send failed: " + msg.err.Error())
			c.pendingIdx = -1
			c.vp.SetContent(strings.Join(c.lines, "\n"))
			c.vp.GotoBottom()
		default:
			// Confirmed by server — replace the pending echo with the real one.
			m := chatMessage{Seq: msg.seq, Username: c.me, Kind: "chat", Text: msg.text}
			c.rendered[m.Seq] = true
			if c.pendingIdx >= 0 && c.pendingIdx < len(c.lines) {
				c.lines[c.pendingIdx] = c.renderLine(m)
			} else {
				c.appendLine(c.renderLine(m))
			}
			c.pendingIdx = -1
			c.vp.SetContent(strings.Join(c.lines, "\n"))
			c.vp.GotoBottom()
		}

	case leaveDoneMsg:
		return c, tea.Quit

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			cmds = append(cmds, c.doLeave(), tea.Quit)
			return c, tea.Batch(cmds...)
		}
		if msg.Type == tea.KeyEnter {
			text := strings.TrimSpace(c.input.Value())
			c.input.SetValue("")
			if text == "" {
				break
			}
			switch strings.ToLower(text) {
			case "/exit", "/quit":
				c.appendLine(tuiSystemStyle.Render("* You left the session."))
				cmds = append(cmds, c.doLeave(), tea.Quit)
				return c, tea.Batch(cmds...)
			case "/users":
				c.appendLine(tuiSystemStyle.Render("* Online: " + strings.Join(c.users, ", ")))
			case "/roster":
				c.showRoster = !c.showRoster
				status := "on"
				if !c.showRoster {
					status = "off"
				}
				c.appendLine(tuiSystemStyle.Render("* Roster visibility: "+status))
			case "/help":
				c.appendLine(tuiSystemStyle.Render("* Commands: /users · /exit · anything else sends"))
			default:
				// Optimistic echo — WhatsApp-style instant feedback.
				c.lines = append(c.lines, tuiMeStyle.Render("[you →] "+text))
				c.pendingIdx = len(c.lines) - 1
				c.vp.SetContent(strings.Join(c.lines, "\n"))
				c.vp.GotoBottom()
				cmds = append(cmds, c.doSend(text))
			}
			var ic tea.Cmd
			c.input, ic = c.input.Update(msg)
			cmds = append(cmds, ic)
			break
		}
		var ic tea.Cmd
		c.input, ic = c.input.Update(msg)
		cmds = append(cmds, ic)
	}

	// Viewport keeps its own scroll/resize handling for every message.
	var vpc tea.Cmd
	c.vp, vpc = c.vp.Update(msg)
	cmds = append(cmds, vpc)

	return c, tea.Batch(cmds...)
}

func (c chatScreen) View() string {
	if c.width == 0 {
		return "connecting…"
	}
	input := tuiBorderStyle.Render(c.input.View())
	// Roster column width (fixed when visible)
	rosterWidth := 0
	if c.showRoster {
		rosterWidth = 20
	}
	// Viewport width: remaining space after roster and borders
	vpWidth := c.width - 2 - rosterWidth
	if vpWidth < 40 {
		vpWidth = 40
		rosterWidth = c.width - 2 - vpWidth
	}
	// Header takes full width, then messages, then input/footer
	body := c.headerView() + "\n"
	// Messages viewport (width adjusted for roster)
	c.vp.Width = vpWidth
	body += tuiBorderStyle.Render(c.vp.View()) + "\n"
	body += input + c.statusView()
	// Append roster column on the right if visible
	if c.showRoster && rosterWidth > 0 {
		body += "\n" + c.rosterView()
	}
	return body
}

// runChatTUI is the default interactive experience (alt-screen).
func runChatTUI(serverURL, key, me string) {
	scr := newChatScreen(serverURL, key, me)
	p := tea.NewProgram(scr, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	scr.client.leave()
	fmt.Printf("\nYou left session %s.\n", key)
}
