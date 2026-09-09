package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// exact colors from screenshot
const (
	landSelectedBg     = "#0a2a52"
	landSelectedBorder = "#1a7af0"
)

var (
	landPlusBorder = lipgloss.Border{
		Top:         "─",
		Bottom:      "─",
		Left:        "│",
		Right:       "│",
		TopLeft:     "+",
		TopRight:    "+",
		BottomLeft:  "+",
		BottomRight: "+",
		MiddleLeft:  "+",
		MiddleRight: "+",
		Middle:      "+",
	}

	landOuterStyle = lipgloss.NewStyle().
			Border(landPlusBorder).
			BorderForeground(lipgloss.Color("15"))

	landTabSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color(landSelectedBg)).
				Foreground(lipgloss.Color("15")).
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color(landSelectedBorder)).
				Bold(true).
				Align(lipgloss.Center)

	landTabUnselectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#3a3a3a")).
				Foreground(lipgloss.Color("15")).
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color("#555555")).
				Align(lipgloss.Center)

	landLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")).
			Bold(true)

	landInputFocusedStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color(landSelectedBorder))

	landInputBlurStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color("15"))

	landButtonSelectedStyle = lipgloss.NewStyle().
				Background(lipgloss.Color(landSelectedBg)).
				Foreground(lipgloss.Color("15")).
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color(landSelectedBorder)).
				Bold(true).
				Align(lipgloss.Center)

	landButtonBlurStyle = lipgloss.NewStyle().
				Background(lipgloss.Color(landSelectedBg)).
				Foreground(lipgloss.Color("15")).
				Border(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color(landSelectedBorder)).
				Align(lipgloss.Center)

	landErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("203"))

	landTitleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#1a7af0")).
			Background(lipgloss.Color("#0a2a52")).
			Bold(true).
			Padding(0, 1).
			Align(lipgloss.Center)

	landSubtitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#8aa0c8")).
				Faint(true).
				Align(lipgloss.Center)

	landHintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6b7280")).
			Faint(true).
			Align(lipgloss.Center)
)

type landingTab int

const (
	tabCreate landingTab = iota
	tabJoin
)

type landingFocus int

const (
	focusUser landingFocus = iota
	focusPass
	focusCode
	focusSubmit
)

type landingResult struct {
	Mode     landingTab
	Username string
	Password string
	Code     string // for join
	Key      string // session key returned from server
}

type landingModel struct {
	tab        landingTab
	focus      landingFocus
	w, h       int
	userInput  textinput.Model
	passInput  textinput.Model
	codeInput  textinput.Model
	errMsg     string
	submitting bool
	serverURL  string
	result     *landingResult
	shouldQuit bool
}

func newLandingModel(serverURL string) landingModel {
	ui := textinput.New()
	ui.Placeholder = "alice_42"
	ui.CharLimit = 32
	ui.Prompt = "> "
	ui.Width = 40
	ui.Focus()
	ui.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ui.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280")).Faint(true)

	pi := textinput.New()
	pi.Placeholder = "••••••••"
	pi.CharLimit = 64
	pi.Prompt = "> "
	pi.EchoMode = textinput.EchoPassword
	pi.EchoCharacter = '•'
	pi.Width = 40
	pi.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	pi.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280")).Faint(true)

	ci := textinput.New()
	ci.Placeholder = "482716"
	ci.Prompt = "> "
	ci.CharLimit = 6
	ci.Width = 40
	ci.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ci.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b7280")).Faint(true)

	return landingModel{
		tab:       tabCreate,
		focus:     focusUser,
		userInput: ui,
		passInput: pi,
		codeInput: ci,
		serverURL: serverURL,
	}
}

func (m landingModel) Init() tea.Cmd { return textinput.Blink }

func (m landingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		if msg.Type == tea.MouseLeft {
			// tab bar at y=1 (inside outer border)
			// outer border top is at row 0, tab row at 1
			if msg.Y == 1 {
				outerW := m.outerWidth()
				tabW := outerW / 2
				if msg.X < tabW {
					m.tab = tabCreate
					if m.focus == focusCode {
						m.focus = focusUser
					}
					m.syncFocus()
				} else {
					m.tab = tabJoin
					m.syncFocus()
				}
				return m, nil
			}
			// inputs: approximate y positions
			l := m.layout()
			// y offsets inside outer: tabH(3) + 1 divider =4, then user row at 6, pass at 8, code at 10, button at 13
			if m.hitInput(msg.X, msg.Y, l) {
				return m, nil
			}
			if m.hitButton(msg.X, msg.Y, l) {
				return m, m.submit()
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			// Esc quits landing; CtrlC also quits
			m.shouldQuit = true
			return m, tea.Quit
		case tea.KeyTab:
			if msg.String() == "shift+tab" {
				m.focusPrev()
			} else {
				m.focusNext()
			}
			m.syncFocus()
			return m, nil
		case tea.KeyShiftTab:
			m.focusPrev()
			m.syncFocus()
			return m, nil
		case tea.KeyLeft, tea.KeyRight:
			// switch tabs with arrows
			if m.tab == tabCreate && msg.Type == tea.KeyRight {
				m.tab = tabJoin
				if m.focus == focusCode {
					m.focus = focusUser
				}
				m.syncFocus()
				return m, nil
			}
			if m.tab == tabJoin && msg.Type == tea.KeyLeft {
				m.tab = tabCreate
				if m.focus == focusCode {
					m.focus = focusUser
				}
				m.syncFocus()
				return m, nil
			}
		case tea.KeyEnter:
			if m.focus == focusSubmit {
				return m, m.submit()
			}
			// Enter on inputs advances to next field; on last input submits
			if m.focus == focusCode && m.tab == tabJoin {
				m.focus = focusSubmit
				m.syncFocus()
				return m, nil
			}
			if m.focus == focusPass && m.tab == tabCreate {
				m.focus = focusSubmit
				m.syncFocus()
				return m, nil
			}
			if m.focus == focusUser {
				m.focusNext()
				m.syncFocus()
				return m, nil
			}
			if m.focus == focusPass && m.tab == tabJoin {
				m.focus = focusCode
				m.syncFocus()
				return m, nil
			}
		}

		// typing goes to focused input or button
		if m.focus == focusSubmit && (msg.Type == tea.KeyRunes && len(msg.Runes) > 0) {
			// space/enter already handled
			return m, nil
		}
		var cmd tea.Cmd
		switch m.focus {
		case focusUser:
			m.userInput, cmd = m.userInput.Update(msg)
			cmds = append(cmds, cmd)
		case focusPass:
			m.passInput, cmd = m.passInput.Update(msg)
			cmds = append(cmds, cmd)
		case focusCode:
			if m.tab == tabJoin {
				m.codeInput, cmd = m.codeInput.Update(msg)
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case landingDoneMsg:
		if msg.err != "" {
			m.submitting = false
			m.errMsg = msg.err
			return m, nil
		}
		m.result = msg.result
		return m, tea.Quit

	case landingCreateOkMsg:
		m.submitting = false
		m.result = &landingResult{Mode: tabCreate, Username: msg.username, Password: msg.password, Key: msg.key}
		return m, tea.Quit

	case landingJoinOkMsg:
		m.submitting = false
		m.result = &landingResult{Mode: tabJoin, Username: msg.username, Password: msg.password, Code: msg.code, Key: msg.code}
		return m, tea.Quit
	}

	return m, tea.Batch(cmds...)
}

type landingLayout struct {
	outerW, outerH int
	tabW           int
	inputW         int
	labelW         int
	buttonW        int
}

func (m landingModel) outerWidth() int {
	w := m.w - 4
	if w < 60 {
		w = 60
	}
	if w > 78 {
		w = 78
	}
	if w <= 0 {
		w = 60
	}
	return w
}

func (m landingModel) layout() landingLayout {
	outerW := m.outerWidth()
	labelW := 12
	innerW := outerW - 2
	inputW := 48
	// cap to available space
	maxInputW := innerW - labelW - 3
	if maxInputW < 20 {
		maxInputW = 20
	}
	if inputW > maxInputW {
		inputW = maxInputW
	}
	tabOuterW := innerW / 2
	return landingLayout{
		outerW: outerW,
		outerH: 16,
		tabW:   tabOuterW, // outer width per tab (including its border)
		inputW: inputW,
		labelW: labelW,
		buttonW: 20,
	}
}

func (m *landingModel) focusNext() {
	if m.tab == tabCreate {
		switch m.focus {
		case focusUser:
			m.focus = focusPass
		case focusPass:
			m.focus = focusSubmit
		case focusSubmit:
			m.focus = focusUser
		default:
			m.focus = focusUser
		}
	} else {
		switch m.focus {
		case focusUser:
			m.focus = focusPass
		case focusPass:
			m.focus = focusCode
		case focusCode:
			m.focus = focusSubmit
		case focusSubmit:
			m.focus = focusUser
		default:
			m.focus = focusUser
		}
	}
}

func (m *landingModel) focusPrev() {
	if m.tab == tabCreate {
		switch m.focus {
		case focusUser:
			m.focus = focusSubmit
		case focusPass:
			m.focus = focusUser
		case focusSubmit:
			m.focus = focusPass
		default:
			m.focus = focusUser
		}
	} else {
		switch m.focus {
		case focusUser:
			m.focus = focusSubmit
		case focusPass:
			m.focus = focusUser
		case focusCode:
			m.focus = focusPass
		case focusSubmit:
			m.focus = focusCode
		default:
			m.focus = focusUser
		}
	}
}

func (m *landingModel) syncFocus() {
	m.userInput.Blur()
	m.passInput.Blur()
	m.codeInput.Blur()
	switch m.focus {
	case focusUser:
		m.userInput.Focus()
	case focusPass:
		m.passInput.Focus()
	case focusCode:
		if m.tab == tabJoin {
			m.codeInput.Focus()
		} else {
			m.userInput.Focus()
			m.focus = focusUser
		}
	case focusSubmit:
		// button focused -> blur all inputs
	}
}

func (m *landingModel) hitInput(x, y int, l landingLayout) bool {
	left := l.labelW + 3
	right := left + l.inputW + 2
	if x < left || x > right {
		return false
	}
	if y == 5 {
		m.focus = focusUser
		m.syncFocus()
		return true
	}
	if y == 7 {
		m.focus = focusPass
		m.syncFocus()
		return true
	}
	if m.tab == tabJoin && y == 9 {
		m.focus = focusCode
		m.syncFocus()
		return true
	}
	return false
}

func (m *landingModel) hitButton(x, y int, l landingLayout) bool {
	if y < 12 || y > 14 {
		return false
	}
	center := l.outerW / 2
	left := center - l.buttonW/2
	right := left + l.buttonW
	return x >= left && x <= right
}

func (m landingModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	l := m.layout()
	// apply input widths for rendering
	m.userInput.Width = l.inputW
	m.passInput.Width = l.inputW
	m.codeInput.Width = l.inputW
	outerW := l.outerW

	// tabs - tabW is outer width per tab (including border), so content width = tabW-2
	tabW := l.tabW
	tabH := 3
	tabContentW := tabW - 2
	if tabContentW < 10 {
		tabContentW = 10
	}
	createFocused := m.tab == tabCreate
	joinFocused := m.tab == tabJoin
	createStyle := landTabUnselectedStyle.Width(tabContentW).Height(tabH-2).Padding(0, 1)
	joinStyle := landTabUnselectedStyle.Width(tabContentW).Height(tabH-2).Padding(0, 1)
	if createFocused {
		createStyle = landTabSelectedStyle.Width(tabContentW).Height(tabH-2).Padding(0, 1)
	}
	if joinFocused {
		joinStyle = landTabSelectedStyle.Width(tabContentW).Height(tabH-2).Padding(0, 1)
	}
	createTab := createStyle.Render("CREATE")
	joinTab := joinStyle.Render("JOIN")
	tabsRow := lipgloss.JoinHorizontal(lipgloss.Top, createTab, joinTab)

	// divider handled by outer border; tabsRow already has borders
	// form content
	// labels aligned to same width
	labelUser := landLabelStyle.Width(l.labelW).Render("UserName :")
	labelPass := landLabelStyle.Width(l.labelW).Render("Password :")
	labelCode := landLabelStyle.Width(l.labelW).Render("Code     :")

	// input boxes
	userBox := landInputBlurStyle.Render(m.userInput.View())
	passBox := landInputBlurStyle.Render(m.passInput.View())
	if m.focus == focusUser {
		userBox = landInputFocusedStyle.Render(m.userInput.View())
	}
	if m.focus == focusPass {
		passBox = landInputFocusedStyle.Render(m.passInput.View())
	}
	codeBox := ""
	if m.tab == tabJoin {
		codeBox = landInputBlurStyle.Render(m.codeInput.View())
		if m.focus == focusCode {
			codeBox = landInputFocusedStyle.Render(m.codeInput.View())
		}
	}

	// ensure input widths
	// build rows
	rowUser := lipgloss.JoinHorizontal(lipgloss.Top, labelUser, " ", userBox)
	rowPass := lipgloss.JoinHorizontal(lipgloss.Top, labelPass, " ", passBox)
	var rows []string
	rows = append(rows, rowUser, "", rowPass, "")
	if m.tab == tabJoin {
		rowCode := lipgloss.JoinHorizontal(lipgloss.Top, labelCode, " ", codeBox)
		rows = append(rows, rowCode, "")
	}
	// button centered
	btnText := "CREATE"
	if m.tab == tabJoin {
		btnText = "JOIN"
	}
	btnStyle := landButtonBlurStyle.Width(l.buttonW).Height(3)
	if m.focus == focusSubmit {
		btnStyle = landButtonSelectedStyle.Width(l.buttonW).Height(3)
	}
	btn := btnStyle.Render(btnText)
	// center button
	btnRow := lipgloss.NewStyle().Width(outerW - 2).Align(lipgloss.Center).Render(btn)
	rows = append(rows, "", btnRow)
	// hint
	hint := landHintStyle.Width(outerW - 4).Render("Tab / Click to move  •  Enter to submit  •  ← → switch tabs  •  Esc quit")
	rows = append(rows, hint)

	if m.errMsg != "" {
		rows = append(rows, "", landErrorStyle.Render(m.errMsg))
	}
	if m.submitting {
		msg := "  creating…"
		if m.tab == tabJoin {
			msg = "  joining…"
		}
		rows = append(rows, landLabelStyle.Render(msg))
	}

	formContent := strings.Join(rows, "\n")
	// formContent wrapped in outer
	// combine tabs + form
	inner := lipgloss.JoinVertical(lipgloss.Left, tabsRow, formContent)
	// outer container with + border — Width is content width (innerW)
	outer := landOuterStyle.Width(outerW - 2).Render(inner)
	// header above outer — more attractive
	title := landTitleStyle.Render("◆ UPLINK ◆")
	subtitle := landSubtitleStyle.Render("secure  •  ephemeral  •  p2p")
	header := lipgloss.JoinVertical(lipgloss.Center, title, subtitle, "")
	content := lipgloss.JoinVertical(lipgloss.Center, header, outer)
	// center on screen
	centered := lipgloss.NewStyle().Width(m.w).Height(m.h).Align(lipgloss.Center).AlignVertical(lipgloss.Center).Render(content)
	return centered
}

// validation
var validUserRe = regexp.MustCompile(`^[a-zA-Z0-9_]{3,20}$`)

func (m *landingModel) validate() string {
	u := strings.TrimSpace(m.userInput.Value())
	if u == "" {
		return "username required"
	}
	if !validUserRe.MatchString(u) {
		return "username: " + chatUsernameHint
	}
	if m.tab == tabJoin {
		c := strings.TrimSpace(m.codeInput.Value())
		if c == "" {
			return "code required"
		}
		if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(c) {
			// also allow alphanum fallback like session tokens
			if len(c) < 4 {
				return "code must be 6 digits"
			}
		}
	}
	return ""
}

type landingDoneMsg struct {
	result *landingResult
	err    string
}
type landingCreateOkMsg struct{ username, password, key string }
type landingJoinOkMsg struct{ username, password, code string }

func (m *landingModel) submit() tea.Cmd {
	if m.submitting {
		return nil
	}
	if msg := m.validate(); msg != "" {
		m.errMsg = msg
		return nil
	}
	m.submitting = true
	m.errMsg = ""
	username := strings.TrimSpace(m.userInput.Value())
	password := strings.TrimSpace(m.passInput.Value())
	if m.tab == tabCreate {
		return m.doCreate(username, password)
	}
	code := strings.TrimSpace(m.codeInput.Value())
	return m.doJoin(username, password, code)
}

func (m *landingModel) doCreate(username, password string) tea.Cmd {
	serverURL := m.serverURL
	return func() tea.Msg {
		// small delay for UX
		time.Sleep(100 * time.Millisecond)
		payload := map[string]any{"username": username, "duration": 600}
		if password != "" {
			payload["password"] = password
		}
		code, body, err := postJSON(serverURL+"/api/v1/session/create", payload, nil)
		if err != nil {
			return landingDoneMsg{err: "could not reach server: " + err.Error()}
		}
		if code != 201 {
			var e struct{ Error string `json:"error"`}
			_ = jsonDecode(body, &e)
			if e.Error == "" {
				e.Error = string(body)
			}
			return landingDoneMsg{err: e.Error}
		}
		var r sessionCreateResponse
		if err := jsonDecode(body, &r); err != nil || r.SessionID == "" {
			return landingDoneMsg{err: "unexpected server response"}
		}
		return landingCreateOkMsg{username: username, password: password, key: r.SessionID}
	}
}

func (m *landingModel) doJoin(username, password, code string) tea.Cmd {
	serverURL := m.serverURL
	return func() tea.Msg {
		time.Sleep(100 * time.Millisecond)
		payload := map[string]any{"username": username}
		if password != "" {
			payload["password"] = password
		}
		c, body, err := postJSON(serverURL+"/api/v1/session/"+code+"/join", payload, nil)
		if err != nil {
			return landingDoneMsg{err: "could not reach server: " + err.Error()}
		}
		if c == 200 {
			return landingJoinOkMsg{username: username, password: password, code: code}
		}
		var e struct{ Error string `json:"error"`}
		_ = jsonDecode(body, &e)
		switch c {
		case 403:
			return landingDoneMsg{err: "incorrect password"}
		case 409:
			return landingDoneMsg{err: "'" + username + "' already in this session"}
		case 410:
			return landingDoneMsg{err: "session has ended"}
		case 404:
			return landingDoneMsg{err: "session not found"}
		default:
			if e.Error == "" {
				e.Error = string(body)
			}
			return landingDoneMsg{err: e.Error}
		}
	}
}

func jsonDecode(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
