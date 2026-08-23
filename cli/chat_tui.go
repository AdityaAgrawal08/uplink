package main

import (
	"fmt"
	"os"
	"strconv"
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
type pollResultMsg struct {
	ended bool
	err   error
}
type beatResultMsg struct {
	hb  heartbeatResponse
	err error
}
type sendResultMsg struct {
	code int
	err  error
	text string
}

// ---- model -----------------------------------------------------------------

type chatScreen struct {
	client       *chatClient
	key          string
	me           string
	width        int
	height       int
	lines        []string
	users        []string
	vp           viewport.Model
	input        textinput.Model
	status       string
	beatFailures int
	lastSeqSeen  bool
}

func (c chatScreen) renderLine(m chatMessage) string {
	ts := tuiTimeStyle.Render("[--:--]")
	if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
		ts = tuiTimeStyle.Render("[" + t.Local().Format("15:04") + "]")
	}
	if m.Kind == "system" {
		return ts + " " + tuiSystemStyle.Render("* "+m.Text)
	}
	name := m.Username
	body := m.Text
	if m.Username == c.me {
		name = tuiMeStyle.Render(name)
	} else {
		name = tuiNameStyle.Render(name)
	}
	return ts + " " + name + ": " + body
}

func (c *chatScreen) appendLine(s string) {
	c.lines = append(c.lines, s)
	c.vp.SetContent(strings.Join(c.lines, "\n"))
	c.vp.GotoBottom()
}

func (c chatScreen) headerView() string {
	online := strconv.Itoa(len(c.users))
	title := fmt.Sprintf(" uplink chat · key %s · you are %s · %s online ", c.key, c.me, online)
	return tuiHeaderStyle.Render(title)
}

func (c chatScreen) statusView() string {
	if c.status == "" {
		return ""
	}
	return "\n" + tuiErrStyle.Render(c.status)
}

// ---- tea.Model -------------------------------------------------------------

func (c chatScreen) Init() tea.Cmd {
	return tea.Batch(schedulePoll(), scheduleBeat())
}

func schedulePoll() tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return pollTickMsg{} })
}
func scheduleBeat() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg { return beatTickMsg{} })
}

func (c chatScreen) doPoll() tea.Cmd {
	ended, err := c.client.pollOnce()
	return func() tea.Msg { return pollResultMsg{ended: ended, err: err} }
}

func (c chatScreen) doBeat() tea.Cmd {
	hb, err := c.client.beatOnce()
	return func() tea.Msg { return beatResultMsg{hb: hb, err: err} }
}

func (c chatScreen) doSend(text string) tea.Cmd {
	code, err := c.client.sendMessage(text)
	return func() tea.Msg { return sendResultMsg{code: code, err: err, text: text} }
}

func (c chatScreen) doLeave() tea.Cmd {
	return func() tea.Msg {
		c.client.leave()
		return leaveDoneMsg{}
	}
}

type leaveDoneMsg struct{}

func (c chatScreen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width, c.height = msg.Width, msg.Height
		c.vp.Width = c.width - 2
		c.vp.Height = c.height - 7 // header + input + borders + status headroom
		if !c.lastSeqSeen {
			c.lastSeqSeen = true
			c.appendLine(tuiSystemStyle.Render(fmt.Sprintf(
				"Connected to session %s as '%s' — type /exit to leave.", c.key, c.me)))
		}
		c.vp.SetContent(strings.Join(c.lines, "\n"))

	case pollTickMsg:
		cmds = append(cmds, c.doPoll(), schedulePoll())

	case pollResultMsg:
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
			c.status = "network hiccup: " + msg.err.Error()
		} else {
			c.status = ""
		}
		cmds = append(cmds, schedulePoll())

	case beatTickMsg:
		cmds = append(cmds, c.doBeat(), scheduleBeat())

	case beatResultMsg:
		if msg.err != nil {
			c.beatFailures++
			if c.beatFailures >= 3 {
				c.status = "connection lost… retrying"
			}
		} else {
			c.beatFailures = 0
			c.status = ""
		}
		cmds = append(cmds, scheduleBeat())

	case sendResultMsg:
		switch {
		case msg.code == 429:
			c.status = "slow down — too many messages"
		case msg.code == 410 || (msg.err != nil && msg.code == 410):
			c.appendLine(tuiSystemStyle.Render("* Session has ended"))
			cmds = append(cmds, c.doLeave(), tea.Quit)
			return c, tea.Batch(cmds...)
		case msg.code != 201:
			c.status = "send failed (" + strconv.Itoa(msg.code) + ")"
		default:
			c.status = ""
		}

	case leaveDoneMsg:
		return c, tea.Quit

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			cmds = append(cmds, c.doLeave(), tea.Quit)
			return c, tea.Batch(cmds...)
		}
		var inputCmd tea.Cmd
		if c.input.Focused() {
			switch {
			case msg.Type == tea.KeyEnter:
				text := strings.TrimSpace(c.input.Value())
				c.input.SetValue("")
				if text != "" {
					switch strings.ToLower(text) {
					case "/exit", "/quit":
						c.appendLine(tuiSystemStyle.Render("* You left the session."))
						cmds = append(cmds, c.doLeave(), tea.Quit)
						return c, tea.Batch(cmds...)
					case "/users":
						c.appendLine(tuiSystemStyle.Render("* Online: " + strings.Join(c.users, ", ")))
					case "/help":
						c.appendLine(tuiSystemStyle.Render("* Commands: /users · /exit · anything else sends"))
					default:
						cmds = append(cmds, c.doSend(text))
					}
				}
			default:
				c.input, inputCmd = c.input.Update(msg)
				cmds = append(cmds, inputCmd)
			}
		} else {
			var ic tea.Cmd
			c.input, ic = c.input.Update(msg)
			cmds = append(cmds, ic)
		}
	}
	var vpCmd tea.Cmd
	c.vp, vpCmd = c.vp.Update(msg)
	cmds = append(cmds, vpCmd)

	return c, tea.Batch(cmds...)
}

func (c chatScreen) View() string {
	if c.width == 0 {
		return "loading…"
	}
	input := tuiBorderStyle.Render(c.input.View())
	body := c.headerView() + "\n" +
		tuiBorderStyle.Render(c.vp.View()) + "\n" +
		input + c.statusView()
	return body
}

// runChatTUI is the default interactive experience (alt-screen).
func runChatTUI(serverURL, key, me string) {
	client := newChatClient(serverURL, key, me)

	ti := textinput.New()
	ti.Placeholder = "Type a message… (/help)"
	ti.Focus()
	ti.CharLimit = 500
	ti.Prompt = "> "

	vp := viewport.New(80, 20)

	scr := &chatScreen{
		client: client,
		key:    key,
		me:     me,
		vp:     vp,
		input:  ti,
	}

	client.onMessage = func(m chatMessage) {
		line := scr.renderLine(m)
		scr.lines = append(scr.lines, line)
		scr.vp.SetContent(strings.Join(scr.lines, "\n"))
		scr.vp.GotoBottom()
	}
	client.onUsers = func(users []string) { scr.users = users }

	if err := client.fetchBacklog(); err != nil {
		fmt.Printf("✗ Failed to load session: %v\n", err)
		os.Exit(1)
	}
	for _, m := range scr.lines {
		_ = m // backlog already rendered through onMessage
	}

	p := tea.NewProgram(*scr, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("chat UI error: %v\n", err)
		os.Exit(1)
	}
	client.leave()
	fmt.Printf("\nYou left session %s.\n", key)
}
