package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── full-screen windows: /settings, /new-group and /invite ─────────────────
//
// All open as a full-screen model over the chat, hosted by rootModel (the
// program model runChatTUI now starts). Windows own the whole terminal like
// the landing: keyboard-first with mouse support where rows carry actions.
// Esc / Ctrl+C close them back to the chat (chat's own Ctrl+C stays the
// single app exit). While a window is open the chat's 2s tick and event
// pump pause; rootModel re-arms both on close.

// ─── window messages ─────────────────────────────────────────────────────────

// openSettingsMsg / openNewGroupMsg travel from the chat command handlers
// to rootModel, which hosts the windows.
type openSettingsMsg struct{}
type openNewGroupMsg struct{}

// closeOverlayMsg dismisses the open window and restores the chat pumps.
type closeOverlayMsg struct{}

// acceptGroupDoneMsg carries a successful invite accept from the settings
// window to rootModel: attach the session, open it, close the window.
type acceptGroupDoneMsg struct {
	code     string
	name     string
	password string // group password ("" = open) for the engine's self-rejoin
}

// createGroupDoneMsg carries a finished /new-group creation: the fresh
// session code plus per-invitee results (already-in-session notes etc.).
type createGroupDoneMsg struct {
	code  string
	name  string
	notes []string
}

// ─── settings window (invites inbox) ─────────────────────────────────────────

// maxInviteRows caps the painted invite list; longer lists scroll with the
// highlight (same window discipline as the command drawer).
const maxInviteRows = 8

// settingsModel is the full-screen settings window: an invite inbox listing
// "X invited you to group Y" rows with X (decline) and ✓ (accept) actions,
// plus an account section. Accepting a protected group opens the two-step
// password modal (401-required → modal; wrong → retry in place; Esc back).
type settingsModel struct {
	w, h      int
	me        string
	serverURL string
	id        *identityKey
	sig       *signalClient
	invites   []groupInvite
	sel       int
	off       int // scroll window offset into the invite list
	notice    string
	busy      bool
	// step-2 password modal (accepting a protected group)
	passModal bool
	passCode  string
	passErr   string
	passBusy  bool
	passInput textinput.Model
	// completed accept: payload for acceptGroupDoneMsg
	doneCode     string
	doneName     string
	donePassword string
}

// invitePollTickMsg drives the window's own 2s invites refresh (the chat
// tick is paused while the window is open).
type invitePollTickMsg struct{}

// acceptDoneMsg resolves one accept attempt (step 1 or the modal's retry).
type acceptDoneMsg struct {
	code string
	name string
	err  error
}

// declineDoneMsg resolves one decline attempt.
type declineDoneMsg struct {
	code string
	err  error
}

// invitesFetchedMsg carries one GET /invites/mine result.
type invitesFetchedMsg struct {
	invites []groupInvite
	err     error
}

func scheduleInvitePoll() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return invitePollTickMsg{} })
}

func newSettingsModel(serverURL, me string, id *identityKey, w, h int) settingsModel {
	pi := textinput.New()
	pi.Placeholder = "••••••••"
	pi.CharLimit = 64
	pi.Prompt = "> "
	pi.EchoMode = textinput.EchoPassword
	pi.EchoCharacter = '•'
	pi.Width = 40
	pi.TextStyle = lipgloss.NewStyle().Foreground(colText)
	pi.PlaceholderStyle = lipgloss.NewStyle().Foreground(colFaint)
	return settingsModel{
		me:        me,
		serverURL: serverURL,
		id:        id,
		sig:       &signalClient{serverURL: serverURL, me: me, id: id},
		w:         w,
		h:         h,
		passInput: pi,
	}
}

func (m settingsModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.fetchInvitesCmd())
}

// fetchInvitesCmd polls GET /invites/mine; the result re-arms the 2s tick,
// so exactly one poll chain exists while the window is open.
func (m settingsModel) fetchInvitesCmd() tea.Cmd {
	return func() tea.Msg {
		invites, err := m.sig.myInvites()
		return invitesFetchedMsg{invites: invites, err: err}
	}
}

// removeInvite consumes a row locally (declined, accepted, expired, full).
func (m *settingsModel) removeInvite(code string) {
	for i, inv := range m.invites {
		if inv.Code == code {
			m.invites = append(m.invites[:i], m.invites[i+1:]...)
			break
		}
	}
	if m.sel >= len(m.invites) {
		m.sel = len(m.invites) - 1
	}
	m.clampSel()
}

// clampSel keeps the highlight + scroll window inside the live list.
func (m *settingsModel) clampSel() {
	n := len(m.invites)
	if n <= 0 {
		m.sel, m.off = 0, 0
		return
	}
	if m.sel >= n {
		m.sel = n - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
	visible := m.visibleRows()
	maxOff := maxInt(n-visible, 0)
	if m.off > maxOff {
		m.off = maxOff
	}
	if m.sel < m.off {
		m.off = m.sel
	}
	if m.sel >= m.off+visible {
		m.off = m.sel - visible + 1
	}
}

// visibleRows is how many invite rows the card can paint at this height.
func (m settingsModel) visibleRows() int {
	v := m.h - 16 // title + sections + account + hints + borders
	if v < 3 {
		v = 3
	}
	if v > maxInviteRows {
		v = maxInviteRows
	}
	return v
}

// acceptRow starts the accept flow for the selected invite (or the modal
// retry when the password prompt is open).
func (m *settingsModel) acceptCurrent() tea.Cmd {
	if m.busy || m.passBusy || m.sel < 0 || m.sel >= len(m.invites) {
		return nil
	}
	code, name := m.invites[m.sel].Code, m.invites[m.sel].GroupName
	password := ""
	if m.passModal {
		code, name = m.passCode, m.doneName
		password = m.passInput.Value()
	}
	m.busy = true
	m.passBusy = m.passModal
	return m.doAccept(code, name, password)
}

func (m *settingsModel) doAccept(code, name, password string) tea.Cmd {
	sig := m.sig
	pk := ""
	if m.id != nil {
		pk = pubkeyB64(m.id)
	}
	return func() tea.Msg {
		roster, epoch, err := sig.acceptInvite(code, pk, password)
		_ = roster
		_ = epoch
		return acceptDoneMsg{code: code, name: name, err: err}
	}
}

// declineCurrent consumes the selected invite.
func (m *settingsModel) declineCurrent() tea.Cmd {
	if m.busy || m.sel < 0 || m.sel >= len(m.invites) {
		return nil
	}
	code := m.invites[m.sel].Code
	m.busy = true
	sig := m.sig
	return func() tea.Msg {
		return declineDoneMsg{code: code, err: sig.declineInvite(code)}
	}
}

func (m settingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case invitePollTickMsg:
		return m, m.fetchInvitesCmd()

	case invitesFetchedMsg:
		if msg.err != nil {
			if len(m.invites) == 0 {
				m.notice = "invites unavailable — " + msg.err.Error()
			}
			return m, scheduleInvitePoll()
		}
		m.invites = msg.invites
		m.clampSel()
		return m, scheduleInvitePoll()

	case acceptDoneMsg:
		m.busy = false
		m.passBusy = false
		if msg.err != nil {
			code := apiStatusCode(msg.err)
			switch {
			case code == 401 && m.passModal && strings.Contains(msg.err.Error(), "Incorrect session password"):
				m.passErr = "incorrect password"
				return m, nil
			case code == 401 && strings.Contains(msg.err.Error(), "Password is required for this session"):
				if !m.passModal {
					// Step 1 answered "password required": open the modal.
					m.passModal = true
					m.passCode = msg.code
					m.doneName = msg.name
					m.passErr = ""
					m.passInput.SetValue("")
					m.passInput.Focus()
					return m, nil
				}
				m.passErr = "password required or incorrect"
				return m, nil
			case code == 403 && strings.Contains(msg.err.Error(), "Maximum allowance is reached"):
				m.removeInvite(msg.code) // server consumed it: terminal
				m.notice = "Maximum allowance is reached"
				return m, nil
			case code == 404:
				m.removeInvite(msg.code)
				m.notice = "invite no longer exists"
				return m, nil
			case isServerDown(msg.err):
				m.notice = serverDownMsg
				return m, nil
			default:
				m.notice = msg.err.Error()
				return m, nil
			}
		}
		// Joined: close the window through rootModel, which attaches the
		// session and opens the group conversation. The modal's typed
		// password rides along (the engine needs it for self-rejoin).
		password := ""
		if m.passModal {
			password = m.passInput.Value()
		}
		m.passModal = false
		m.passInput.SetValue("")
		m.doneCode = msg.code
		m.doneName = msg.name
		m.donePassword = password
		return m, func() tea.Msg {
			return acceptGroupDoneMsg{code: m.doneCode, name: m.doneName, password: m.donePassword}
		}

	case declineDoneMsg:
		m.busy = false
		if msg.err != nil && apiStatusCode(msg.err) != 404 {
			if isServerDown(msg.err) {
				m.notice = serverDownMsg
			} else {
				m.notice = msg.err.Error()
			}
			return m, nil
		}
		// 200 or 404 ("Invite not found"): the row is gone either way.
		m.removeInvite(msg.code)
		return m, nil

	case tea.KeyMsg:
		// Step 2: the password modal owns the keyboard.
		if m.passModal {
			switch msg.Type {
			case tea.KeyEsc, tea.KeyCtrlC:
				m.passModal = false
				m.passCode = ""
				m.passErr = ""
				m.passInput.SetValue("")
				m.passInput.Blur()
				m.busy = false
				m.passBusy = false
				return m, nil
			case tea.KeyEnter:
				if m.passBusy {
					return m, nil
				}
				m.passErr = ""
				return m, m.acceptCurrent()
			case tea.KeyCtrlK:
				return m, m.acceptCurrent() // convenience: Ctrl+K submits
			default:
				var cmd tea.Cmd
				m.passInput, cmd = m.passInput.Update(msg)
				return m, cmd
			}
		}
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			return m, func() tea.Msg { return closeOverlayMsg{} }
		case tea.KeyUp:
			if n := len(m.invites); n > 0 {
				m.sel = ((m.sel-1)%n + n) % n
				m.clampSel()
			}
			return m, nil
		case tea.KeyDown:
			if n := len(m.invites); n > 0 {
				m.sel = (m.sel + 1) % n
				m.clampSel()
			}
			return m, nil
		case tea.KeyEnter:
			if len(m.invites) > 0 {
				return m, m.acceptCurrent()
			}
			return m, nil
		case tea.KeyBackspace, tea.KeyCtrlH, tea.KeyDelete:
			if len(m.invites) > 0 {
				return m, m.declineCurrent()
			}
			return m, nil
		}
	}
	return m, nil
}

// settingsGeom is the shared paint/hit-test geometry of the centered card.
type settingsGeom struct {
	outerW   int
	innerW   int
	top      int
	left     int
	invFirst int // first invite row (terminal Y)
	invN     int // painted invite rows
}

func (m settingsModel) geom() settingsGeom {
	outerW := m.w - 4
	if outerW < 60 {
		outerW = 60
	}
	if outerW > 78 {
		outerW = 78
	}
	innerW := outerW - 4 // border + padding leave 2 cells per side
	n := len(m.invites)
	v := m.visibleRows()
	if n > v {
		n = v
	}
	// Card interior rows: title, section, n invite rows, notice/blank,
	// account section, account line, blank, hint.
	outerH := 8 + n + 2 // +2 border
	top := (m.h - outerH) / 2
	if top < 0 {
		top = 0
	}
	left := (m.w - outerW) / 2
	if left < 0 {
		left = 0
	}
	return settingsGeom{outerW: outerW, innerW: innerW, top: top, left: left, invFirst: top + 1 + 2, invN: n}
}

func (m settingsModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Type != tea.MouseLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.passModal {
		return m, nil // modal is keyboard-driven
	}
	g := m.geom()
	if msg.Y < g.invFirst || msg.Y >= g.invFirst+g.invN {
		return m, nil
	}
	row := msg.Y - g.invFirst
	idx := m.off + row
	if idx < 0 || idx >= len(m.invites) {
		return m, nil
	}
	m.sel = idx
	m.clampSel()
	// " ✕ ✓" tail at the row's right edge: ✕ at +1, ✓ at +3 of the tail.
	// Content starts at left + 2 (border + padding).
	tail := " ✕ ✓"
	tailX := g.left + 2 + g.innerW - lipgloss.Width(tail)
	switch msg.X {
	case tailX + 1:
		return m, m.declineCurrent()
	case tailX + 3:
		return m, m.acceptCurrent()
	}
	return m, nil
}

func (m settingsModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	g := m.geom()
	inner := g.innerW

	fit := func(s string) string { return fitRow(s, inner) }
	rows := make([]string, 0, 8+g.invN)
	rows = append(rows, fit(lipgloss.NewStyle().Bold(true).Foreground(colAccent).Render("◆ SETTINGS")))
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colDim).Bold(true).Render("NOTIFICATIONS")))

	if len(m.invites) == 0 {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render("  No pending invitations.")))
	} else {
		for i := 0; i < g.invN; i++ {
			idx := m.off + i
			if idx >= len(m.invites) {
				break
			}
			inv := m.invites[idx]
			by := inv.By
			if by == "" {
				by = "someone"
			}
			grp := inv.GroupName
			if grp == "" {
				grp = "a group"
			}
			body := fmt.Sprintf("%s invited you to group %s", by, grp)
			if len([]rune(body)) > inner-8 {
				body = truncateStringPlain(body, inner-8)
			}
			tail := lipgloss.NewStyle().Foreground(colDim).Render(" ✕ ") +
				lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render("✓")
			line := fit(padVisible(body, inner-lipgloss.Width(tail)) + tail)
			if idx == m.sel {
				line = fit(tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle)))
			}
			rows = append(rows, line)
		}
		if len(m.invites) > g.invN {
			rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render(
				fmt.Sprintf("  … +%d more", len(m.invites)-g.invN))))
		}
	}

	if m.notice != "" {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colRed).Render(m.notice)))
	} else {
		rows = append(rows, " ")
	}
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colDim).Bold(true).Render("ACCOUNT")))
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colText).Render(
		"  "+sanitizeDisplay(m.me)+"  ·  "+sanitizeDisplay(m.serverURL))))
	rows = append(rows, " ")
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render(
		"↑↓ select · enter ✓ accept · backspace ✕ decline · esc close")))

	card := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colEdge).
		Background(colPanel).
		Width(inner).
		Padding(0, 1).
		Render(strings.Join(rows, "\n"))

	centered := lipgloss.NewStyle().Width(m.w).Height(m.h).
		Align(lipgloss.Center).AlignVertical(lipgloss.Center).Render(card)
	if m.passModal {
		return overlayCenter(centered, m.passwordModalView(), m.w, m.h)
	}
	return centered
}

// passwordModalView is the step-2 prompt for protected groups, mirroring
// the landing's modal: masked input, retry in place, Esc back.
func (m settingsModel) passwordModalView() string {
	innerW := m.w - 8
	if innerW < 20 {
		innerW = 20
	}
	if innerW > 48 {
		innerW = 48
	}
	m.passInput.Width = innerW - 6
	box := landInputBlurStyle.Render(m.passInput.View())
	title := lipgloss.NewStyle().Bold(true).Foreground(colAccent).Render("GROUP PASSWORD")
	sub := landSubtitleStyle.Render(truncateByWidth("This group is password-protected.", maxInt(innerW-4, 10)))
	var lines []string
	lines = append(lines, title, sub, box)
	if m.passErr != "" {
		lines = append(lines, landErrorStyle.Render(m.passErr))
	}
	if m.passBusy {
		lines = append(lines, landLabelStyle.Render("  joining…"))
	}
	lines = append(lines, landHintStyle.Render("ENTER submit  ·  ESC back"))
	inner := strings.Join(lines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colAccent).
		Background(colPanel).
		Padding(1, 2).
		Render(inner)
}

// ─── /new-group window ───────────────────────────────────────────────────────

// newGroupModel is the creation window: the form (Name, Description,
// Max-People) with inline validation, opening DIRECTLY — members are added
// later through /invite (the invitation window owns the member multi-select).
// Submitting creates the group (creator crowned server-side) with no
// invites; the status line announces the fresh session.
type groupModel struct {
	w, h       int
	me         string
	serverURL  string
	id         *identityKey
	parentCode string
	nameInput  textinput.Model
	descInput  textinput.Model
	maxInput   textinput.Model
	formFocus  int // 0..2 = inputs, 3 = Create
	errMsg     string
	notice     string
	busy       bool
}

// createGroupErrMsg resolves a failed creation attempt (stays in the form).
type createGroupErrMsg struct{ err error }

func newGroupModel(serverURL, me string, id *identityKey, parentCode string, w, h int) groupModel {
	ni := textinput.New()
	ni.Placeholder = "Design Team"
	ni.CharLimit = 64
	ni.Prompt = "> "
	ni.Width = 40
	ni.TextStyle = lipgloss.NewStyle().Foreground(colText)
	ni.PlaceholderStyle = lipgloss.NewStyle().Foreground(colFaint)
	ni.Focus()

	di := textinput.New()
	di.Placeholder = "what is this group about?"
	di.CharLimit = 256
	di.Prompt = "> "
	di.Width = 40
	di.TextStyle = lipgloss.NewStyle().Foreground(colText)
	di.PlaceholderStyle = lipgloss.NewStyle().Foreground(colFaint)

	mi := textinput.New()
	mi.Placeholder = "Any"
	mi.CharLimit = 7
	mi.Prompt = "> "
	mi.Width = 8
	mi.TextStyle = lipgloss.NewStyle().Foreground(colText)
	mi.PlaceholderStyle = lipgloss.NewStyle().Foreground(colFaint)

	return groupModel{
		me:         me,
		serverURL:  serverURL,
		id:         id,
		parentCode: parentCode,
		w:          w,
		h:          h,
		nameInput:  ni,
		descInput:  di,
		maxInput:   mi,
	}
}

func (m groupModel) Init() tea.Cmd { return textinput.Blink }

func (m groupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, func() tea.Msg { return closeOverlayMsg{} }
		}
		return m.updateForm(msg)

	case createGroupErrMsg:
		m.busy = false
		if msg.err != nil {
			if isServerDown(msg.err) {
				m.errMsg = serverDownMsg
			} else {
				m.errMsg = msg.err.Error()
			}
		}
		return m, nil
	}
	return m, nil
}

// updateForm handles the creation form: Tab/Enter walk the fields, the
// Create button submits with inline validation.
func (m groupModel) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyTab:
		if msg.String() == "shift+tab" {
			m.formFocus = (m.formFocus + 3) % 4
		} else {
			m.formFocus = (m.formFocus + 1) % 4
		}
		m.syncFormFocus()
		return m, nil
	case tea.KeyShiftTab:
		m.formFocus = (m.formFocus + 3) % 4
		m.syncFormFocus()
		return m, nil
	case tea.KeyEnter:
		if m.busy {
			return m, nil
		}
		if m.formFocus < 3 {
			m.formFocus = (m.formFocus + 1) % 4
			m.syncFormFocus()
			return m, nil
		}
		return m, m.submit()
	default:
		// Route editing to the focused field. The input's Update returns a
		// NEW model — it must be stored back or the keystroke vanishes.
		switch m.formFocus {
		case 0:
			var cmd tea.Cmd
			m.nameInput, cmd = m.nameInput.Update(msg)
			return m, cmd
		case 1:
			var cmd tea.Cmd
			m.descInput, cmd = m.descInput.Update(msg)
			return m, cmd
		case 2:
			var cmd tea.Cmd
			m.maxInput, cmd = m.maxInput.Update(msg)
			return m, cmd
		}
		return m, nil
	}
}

func (m *groupModel) syncFormFocus() {
	m.nameInput.Blur()
	m.descInput.Blur()
	m.maxInput.Blur()
	switch m.formFocus {
	case 0:
		m.nameInput.Focus()
	case 1:
		m.descInput.Focus()
	case 2:
		m.maxInput.Focus()
	}
}

// submit validates the form and creates the group. The server crowns the
// creator automatically; invites are NOT sent here (the /invite window owns
// member management), so creation cannot half-fail on an invitee.
func (m *groupModel) submit() tea.Cmd {
	m.errMsg = ""
	name := strings.TrimSpace(m.nameInput.Value())
	if e := validateGroupName(name); e != "" {
		m.errMsg = e
		return nil
	}
	desc := strings.TrimSpace(m.descInput.Value())
	if e := validateGroupDesc(desc); e != "" {
		m.errMsg = e
		return nil
	}
	maxMembers, e := validateMaxMembers(m.maxInput.Value())
	if e != "" {
		m.errMsg = e
		return nil
	}
	m.busy = true
	serverURL := m.serverURL
	me := m.me
	id := m.id
	parentCode := m.parentCode
	return func() tea.Msg {
		sig := &signalClient{serverURL: serverURL, me: me, id: id}
		pk := ""
		if id != nil {
			pk = pubkeyB64(id)
		}
		code, err := sig.createGroupRoom(me, pk, name, desc, maxMembers, parentCode)
		if err != nil {
			return createGroupErrMsg{err: err}
		}
		return createGroupDoneMsg{code: code, name: name}
	}
}

func (m groupModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	return m.formView()
}

func (m groupModel) formView() string {
	outerW := m.w - 4
	if outerW < 60 {
		outerW = 60
	}
	if outerW > 78 {
		outerW = 78
	}
	inner := outerW - 4

	fit := func(s string) string { return fitRow(s, inner) }
	labelOf := func(text string, f int) string {
		st := landLabelStyle
		if m.formFocus == f {
			st = landLabelFocusStyle
		}
		return st.Width(14).Render(text)
	}
	box := func(ti textinput.Model, f int) string {
		st := landInputBlurStyle
		if m.formFocus == f {
			st = landInputFocusedStyle
		}
		return st.Render(ti.View())
	}
	row := func(label, input string) string {
		return lipgloss.JoinHorizontal(lipgloss.Top, label, " ", input)
	}

	rows := []string{
		fit(lipgloss.NewStyle().Bold(true).Foreground(colAccent).Render("◆ NEW GROUP")),
		fit(lipgloss.NewStyle().Foreground(colDim).Render(
			fmt.Sprintf("creating inside session %s · add members with /invite later", sanitizeDisplay(m.parentCode)))),
		" ",
		row(labelOf("Name :", 0), box(m.nameInput, 0)),
		row(labelOf("Description :", 1), box(m.descInput, 1)),
		row(labelOf("Max-People :", 2), box(m.maxInput, 2)),
		fit(lipgloss.NewStyle().Foreground(colFaint).Render("  empty = unlimited")),
		" ",
	}
	btnText := "CREATE"
	if m.busy {
		btnText = "creating…"
	}
	btnW := lipgloss.Width(btnText) + 4
	btnStyle := landButtonBlurStyle.Width(btnW).Padding(0, 1)
	if m.formFocus == 3 && !m.busy {
		btnStyle = landButtonSelectedStyle.Width(btnW).Padding(0, 1)
	}
	btnRow := lipgloss.NewStyle().Width(inner).Align(lipgloss.Center).Render(btnStyle.Render(btnText))
	rows = append(rows, btnRow)
	if m.errMsg != "" {
		rows = append(rows, landErrorStyle.Render(m.errMsg))
	} else if m.notice != "" {
		rows = append(rows, lipgloss.NewStyle().Foreground(colDim).Render(m.notice))
	}
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render(
		"tab moves · enter submits · esc close")))

	card := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colEdge).
		Background(colPanel).
		Width(inner).
		Padding(0, 1).
		Render(strings.Join(rows, "\n"))
	return lipgloss.NewStyle().Width(m.w).Height(m.h).
		Align(lipgloss.Center).AlignVertical(lipgloss.Center).Render(card)
}

// roleSuffixTag annotates picker rows like the palette does ("" for plain
// members).
func roleSuffixTag(role string) string {
	switch role {
	case "creator":
		return "  (main admin)"
	case "admin":
		return "  (admin)"
	}
	return ""
}

// ─── /invite window ──────────────────────────────────────────────────────────

// openInviteMsg travels from the "/invite" command handler to rootModel,
// which hosts the window (the command refuses to open outside a group, so
// the message only ever arrives with a group conversation active).
type openInviteMsg struct{}

// inviteSentMsg resolves one batch send: per-user notes (409 already-in-
// session skips reported inline, the rest continue).
type inviteSentMsg struct {
	notes []string
}

// inviteModel is the full-screen invitation window for a GROUP conversation:
// it lists the parent room's users minus the group's existing members, with
// one-by-one AND Shift+Up/Down range multi-select. Enter toggles the
// highlighted member (the window stays open for more picks), Tab moves to
// the Send button, Enter there POSTs /invites for EVERY selected user —
// 409 already-in-session notes are reported inline and the rest continue.
// Esc closes without sending anything.
//
// The window paints in EXACTLY the command drawer's craft (chat_drawer.go):
// the shared single top border line, the "Invite — <group>" header with
// the esc hint, the full-row accent cursor bar, muted unselected rows,
// fzf-matched-char highlight, role-grouped Admins/Members headers with
// blank separators, the overflow marker, a centered Send button row and
// the footer contract (nav hints left, "n selected — Enter to send"
// right). All list chrome renders through the SHARED drawerPanelView
// painter — the invite window maintains no second renderer. The ✓ rides
// the bright contrast ink on every bar, 80x24 caps apply, mouse parity
// walks the same windowed plan, and the 50ms staged open reveal applies.
type inviteModel struct {
	w, h   int
	code   string
	name   string
	sig    *signalClient // group session client: key = the group code
	cands  []rosterMember
	sel    int
	off    int // scroll window offset into cands (item space)
	anchor int // range anchor (Shift+Up/Down); -1 = no active range
	picked map[string]bool
	focus  int // 0 = member list, 1 = Send button
	busy   bool
	notice string
	notes  []string
	// reveal is the painted stage of the open window (0 = bare chrome, the
	// last frame = fully expanded); revealGen fences stale reveal ticks.
	reveal    int
	revealGen int
	// pulseItem is the candidate row flashing its toggle (the 1-tick
	// selection pulse; -1 = none); pulseGen fences stale pulse ticks the
	// exact way revealGen fences reveal ticks — a second toggle supersedes
	// the first's flash.
	pulseItem  int
	pulseGen   int
	pulseFrame int
	// animations mirrors chatScreen.animations (wired from the env at
	// construction) so tests can pin the staged reveal deterministically.
	animations bool
}

// inviteRevealFrames is the number of painted stages in the window's open
// reveal (0 = bare chrome, the last frame = settled); inviteRevealStep
// spaces the tea.Tick frames 50ms apart — the same staged reveal as the
// reaction dropdown and the /reply pointer. Plain/CI runs stay static (the
// window paints fully expanded, no timers).
const (
	inviteRevealFrames = 4
	inviteRevealStep   = 50 * time.Millisecond
)

// inviteRevealMsg advances the window's open reveal to painted stage frame.
// gen fences out ticks from a superseded open (close + reopen).
type inviteRevealMsg struct{ gen, frame int }

func scheduleInviteReveal(gen, frame int) tea.Cmd {
	return tea.Tick(inviteRevealStep, func(time.Time) tea.Msg {
		return inviteRevealMsg{gen: gen, frame: frame}
	})
}

// invitePulseFrames is the painted stage count of the toggle pulse: frame 0
// is the raw flash, the LAST frame settles into the picked bar (one
// invitePulseStep tick between them). Same generation-fenced staging as the
// open reveal; plain/CI runs skip the pulse entirely (the pick paints its
// settled bar directly).
const (
	invitePulseFrames = 2
	invitePulseStep   = 50 * time.Millisecond
)

// invitePulseMsg advances the toggle pulse to painted stage frame; gen
// fences out ticks from a superseded toggle.
type invitePulseMsg struct{ gen, frame int }

func scheduleInvitePulse(gen, frame int) tea.Cmd {
	return tea.Tick(invitePulseStep, func(time.Time) tea.Msg {
		return invitePulseMsg{gen: gen, frame: frame}
	})
}

// inviteFooterHints is the dim keymap legend under the member list (the
// drawer footer contract: hints left, count right).
const inviteFooterHints = "↑↓ move · shift+↑↓ range · enter toggle · tab send · esc close"

// inviteFooterRow paints the invite window footer: the keymap hints left,
// the LIVE picked counter right. The counter matches the spec verbatim —
// "n selected — Enter to send" — in accent bold the moment any pick
// exists, in the plain hint tone ("0 selected") before that, so the send
// affordance tracks every toggle instantly. Rendered through the shared
// drawer footer painter (drawerFooterRowCount), the one the strip paints.
func inviteFooterRow(inner, picked int) string {
	count := fmt.Sprintf("%d selected", picked)
	emph := false
	if picked > 0 {
		count += " — Enter to send"
		emph = true
	}
	return drawerFooterRowCount(inner, inviteFooterHints, count, emph)
}

// newInviteModel builds the window from the live chat screen: candidates
// are the parent room's roster minus the group's existing members (and
// self), in stable username order. sig is the GROUP's session client, so
// every invite POST targets the group session.
func newInviteModel(c chatScreen, code, name string, sig *signalClient, w, h int) inviteModel {
	cands := c.inviteCandidates()
	return inviteModel{
		code:       code,
		name:       name,
		sig:        sig,
		cands:      cands,
		anchor:     -1,
		picked:     map[string]bool{},
		pulseItem:  -1,
		w:          w,
		h:          h,
		animations: chatAnimationsEnabled(), // newChatScreen wires exactly this
	}
}

// inviteCandidates builds the window's list: the parent room's roster (the
// home engine's live presence when wired, the sidebar snapshot otherwise)
// minus the group's existing members and self, stable by username.
func (c *chatScreen) inviteCandidates() []rosterMember {
	var room []rosterMember
	if c.homeEng != nil {
		room = c.homeEng.peers()
	} else {
		for _, u := range c.users {
			room = append(room, rosterMember{Username: u, Online: true})
		}
	}
	inGroup := map[string]bool{}
	if g := c.groups[c.activeGroup]; g != nil && g.eng != nil {
		for _, m := range g.eng.peers() {
			if m.Username != "" {
				inGroup[m.Username] = true
			}
		}
	}
	out := make([]rosterMember, 0, len(room))
	for _, m := range room {
		if m.Username == "" || m.Username == c.me || inGroup[m.Username] {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// Init arms the window's staged open reveal (the chat's pumps are paused
// while the window is open, so the window drives its own). Plain/CI runs
// stay static: the candidates paint fully expanded with no timers.
func (m inviteModel) Init() tea.Cmd {
	if !m.animations {
		return nil
	}
	return scheduleInviteReveal(m.revealGen, 1)
}

// visibleRows is the window size for the candidate list: the shared drawer
// cap (at most 10 rows, scaled by half the terminal — never the whole
// screen), with a 3-row floor so a tiny terminal still gets a usable list.
func (m inviteModel) visibleRows() int {
	v := drawerMaxRows(m.h)
	if v < 3 {
		v = 3
	}
	return v
}

// invitePlan builds the candidate display plan: role-grouped under
// Admins/Members headers (the same grouped member standard as the "/"
// palette's member stage), windowed by visibleRows.
func (m inviteModel) invitePlan() drawerPlan {
	return buildDrawerPlan(len(m.cands), len(m.cands) > 0,
		func(i int) string { return userGroupLabel(m.cands[i]) }, m.visibleRows())
}

// inviteWindowRows returns the windowed display rows for the current
// selection — the single source paint, geometry and the mouse hit-test all
// walk, so the painted list can never drift from the hit-tested one.
func (m inviteModel) inviteWindowRows() (rows []drawerRow, below, above int) {
	plan := m.invitePlan()
	rows, below, above = windowDrawerPlan(plan, m.off, m.sel)
	return rows, below, above
}

// settleWindow re-anchors the highlight + scroll window inside the live
// candidate list, plan-aware (group headers shift display rows — the same
// follow the palette and the picker use).
func (m *inviteModel) settleWindow() {
	plan := m.invitePlan()
	n := len(m.cands)
	if n <= 0 {
		m.sel, m.off = 0, 0
		return
	}
	if m.sel >= n {
		m.sel = n - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
	settleDrawerWindow(&m.sel, &m.off, plan)
}

// moveBy steps the highlight through the PAINTED order of the grouped plan
// (headers shift display rows — the walk follows what the user sees, never
// the raw candidate array), then re-settles the window.
func (m *inviteModel) moveBy(d int) {
	if len(m.cands) == 0 {
		m.sel, m.off = 0, 0
		return
	}
	disp := m.invitePlan().displayOrder()
	cur := 0
	for i, it := range disp {
		if it == m.sel {
			cur = i
			break
		}
	}
	cur = (cur + d + len(disp)) % len(disp)
	m.sel = disp[cur]
	m.settleWindow()
}

// paintedRows is how many of the windowed rows actually paint at the
// current reveal stage (0 = bare chrome, the settled frame = all of them).
// Plain/CI runs always paint fully expanded.
func (m inviteModel) paintedRows(total int) int {
	frame := m.reveal
	if !m.animations {
		frame = inviteRevealFrames - 1
	}
	if frame >= inviteRevealFrames-1 {
		return total
	}
	return total * frame / (inviteRevealFrames - 1)
}

// selectedUsers returns the picked usernames in candidate order (stable).
func (m inviteModel) selectedUsers() []string {
	var out []string
	for _, c := range m.cands {
		if m.picked[c.Username] {
			out = append(out, c.Username)
		}
	}
	return out
}

// pickedCount is the live footer counter: how many candidates are picked.
func (m inviteModel) pickedCount() int {
	n := 0
	for _, c := range m.cands {
		if m.picked[c.Username] {
			n++
		}
	}
	return n
}

// armPulse starts the 1-tick selection pulse on cands[item] (generation-
// fenced: a newer toggle supersedes an older flash). Plain/CI runs settle
// immediately into the picked bar with no timers, exactly like the reveal.
func (m *inviteModel) armPulse(item int) tea.Cmd {
	m.pulseGen++
	m.pulseItem = item
	if !m.animations {
		m.pulseFrame = invitePulseFrames - 1
		return nil
	}
	m.pulseFrame = 0
	return scheduleInvitePulse(m.pulseGen, 1)
}

// togglePicked flips the pick state of cands[item] and arms the selection
// pulse — the one code path every toggle (keyboard Enter, range Enter,
// mouse click) walks, so the flash fires exactly once per toggle.
func (m *inviteModel) togglePicked(item int) tea.Cmd {
	if item < 0 || item >= len(m.cands) {
		return nil
	}
	u := m.cands[item].Username
	if m.picked[u] {
		delete(m.picked, u)
	} else {
		m.picked[u] = true
	}
	return m.armPulse(item)
}

// sendAll POSTs /invites for every picked user through the group's client:
// 409 "already in this session" skips are reported inline and the rest
// continue. The window stays open so the invitee list can be extended.
func (m *inviteModel) sendAll() tea.Cmd {
	if m.busy {
		return nil
	}
	users := m.selectedUsers()
	if len(users) == 0 {
		m.notice = "no one selected — pick members first"
		return nil
	}
	m.busy = true
	sig := m.sig
	return func() tea.Msg {
		var notes []string
		for _, u := range users {
			if err := sig.sendInvite(u); err != nil {
				switch apiStatusCode(err) {
				case 409:
					notes = append(notes, u+" is already in this session")
				default:
					notes = append(notes, u+": "+err.Error())
				}
			} else {
				notes = append(notes, "invited "+u)
			}
		}
		return inviteSentMsg{notes: notes}
	}
}

func (m inviteModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.settleWindow()
		return m, nil

	case inviteRevealMsg:
		if msg.gen != m.revealGen {
			return m, nil // stale reveal tick from a superseded open
		}
		m.reveal = msg.frame
		if m.reveal < inviteRevealFrames-1 {
			return m, scheduleInviteReveal(m.revealGen, m.reveal+1)
		}
		return m, nil

	case invitePulseMsg:
		if msg.gen != m.pulseGen {
			return m, nil // stale pulse tick from a superseded toggle
		}
		m.pulseFrame = msg.frame
		if m.pulseFrame < invitePulseFrames-1 {
			return m, scheduleInvitePulse(m.pulseGen, m.pulseFrame+1)
		}
		m.pulseItem = -1 // settled: the picked bar itself carries the state
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case inviteSentMsg:
		m.busy = false
		m.notice = ""
		m.notes = msg.notes
		// Picks are consumed: the window stays open for the next round.
		m.picked = map[string]bool{}
		m.anchor = -1
		m.pulseItem = -1
		m.focus = 0
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m inviteModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, func() tea.Msg { return closeOverlayMsg{} }
	}
	// Send button: Enter sends, Tab/Shift+Tab return to the list.
	if m.focus == 1 {
		switch msg.Type {
		case tea.KeyEsc:
			return m, func() tea.Msg { return closeOverlayMsg{} }
		case tea.KeyEnter:
			return m, m.sendAll()
		case tea.KeyTab, tea.KeyShiftTab:
			m.focus = 0
			return m, nil
		}
		return m, nil
	}
	// Member list: arrows move (Shift extends a range), Enter toggles the
	// highlighted member and STAYS open, Tab walks to the Send button.
	switch msg.Type {
	case tea.KeyEsc:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyUp, tea.KeyShiftUp:
		if msg.Type == tea.KeyShiftUp {
			if m.anchor < 0 {
				m.anchor = m.sel
			}
		} else {
			m.anchor = -1
		}
		m.moveBy(-1)
		return m, nil
	case tea.KeyDown, tea.KeyShiftDown:
		if msg.Type == tea.KeyShiftDown {
			if m.anchor < 0 {
				m.anchor = m.sel
			}
		} else {
			m.anchor = -1
		}
		m.moveBy(+1)
		return m, nil
	case tea.KeyEnter:
		// A Shift+Up/Down range (anchor set) toggles EVERY member in the
		// range; otherwise just the highlighted one. Either way the window
		// STAYS open for the next pick, and the toggled row flashes its
		// selection pulse.
		if m.anchor >= 0 {
			lo, hi := m.anchor, m.sel
			if lo > hi {
				lo, hi = hi, lo
			}
			for i := lo; i <= hi && i < len(m.cands); i++ {
				u := m.cands[i].Username
				if m.picked[u] {
					delete(m.picked, u)
				} else {
					m.picked[u] = true
				}
			}
			m.anchor = -1
			m.notice = ""
			return m, m.armPulse(m.sel)
		}
		if m.sel >= 0 && m.sel < len(m.cands) {
			m.notice = ""
			return m, m.togglePicked(m.sel)
		}
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab:
		m.focus = 1
		return m, nil
	}
	return m, nil
}

// rangeBounds returns the Shift+Up/Down range under the cursor as an
// inclusive item interval (-1, -1 = no active range). The "> " row marker
// and the range toggle both walk this one truth.
func (m inviteModel) rangeBounds() (lo, hi int) {
	if m.anchor < 0 {
		return -1, -1
	}
	lo, hi = m.anchor, m.sel
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi
}

// invitePanelRows builds the invite window's full painted row plan — the
// shared drawer chrome (chat_drawer.go): the single top border line, the
// "Invite — <group>" header + esc hint, the windowed candidate rows under
// role-grouped headers, the overflow marker, the post-send notes/notice
// row, the centered Send button and the footer contract. The staged open
// reveal clips how many candidate rows the plan carries at each frame
// (frame 0 = bare chrome, the settled frame = the full window); border,
// header, button and footer paint from the first frame, exactly like the
// old card. The plan IS the paint: View renders it through the shared
// drawerPanelView and the mouse hit-test walks its drItem rows, so the
// painted list can never drift from the clickable one.
func (m inviteModel) invitePanelRows() []drawerRow {
	title := "Invite — " + sanitizeDisplay(m.name)
	if m.name == "" {
		title = "Invite — " + sanitizeDisplay(m.code)
	}
	panel := make([]drawerRow, 0, 12)
	panel = append(panel, drawerRow{kind: drBorder}, drawerRow{kind: drHeader, text: title})
	if len(m.cands) == 0 {
		panel = append(panel, drawerRow{kind: drEmpty, text: "No users to invite"})
	} else {
		wrows, below, above := m.inviteWindowRows()
		painted := m.paintedRows(len(wrows))
		for i := 0; i < painted && i < len(wrows); i++ {
			r := wrows[i]
			switch r.kind {
			case drHeader, drBlank:
				panel = append(panel, r)
			case drItem:
				cand := m.cands[r.item]
				rr := drawerRow{
					kind: drItem,
					item: r.item,
					text: sanitizeDisplay(cand.Username),
					desc: roleSuffixTag(cand.Role),
					hit:  m.invitePlan().hitOf(r.item),
					lead: "  ",
				}
				rr.picked = m.picked[cand.Username]
				rr.flash = rr.picked && m.pulseItem == r.item && m.pulseFrame == 0 && m.pulseGen > 0
				if lo, hi := m.rangeBounds(); r.item >= lo && r.item <= hi {
					rr.lead = "> "
				}
				panel = append(panel, rr)
			}
		}
		if below > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: below, text: "more"})
		} else if above > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: above, text: "above"})
		}
	}
	// Post-send results ride a dim notice row; the inline error a red one.
	if len(m.notes) > 0 {
		panel = append(panel, drawerRow{kind: drNotice,
			text: lipgloss.NewStyle().Foreground(colDim).Render(strings.Join(m.notes, " · "))})
	} else if m.notice != "" {
		panel = append(panel, drawerRow{kind: drNotice,
			text: lipgloss.NewStyle().Foreground(colRed).Render(m.notice)})
	}
	// The Send button: a full-row accent bar while focused (the drawer's
	// one-button language), the plain tone otherwise.
	btn := "SEND"
	if m.busy {
		btn = "sending…"
	}
	focusN := 0
	if m.focus == 1 && !m.busy {
		focusN = 1
	}
	panel = append(panel, drawerRow{kind: drButton, text: btn, n: focusN})
	// The footer contract: nav hints left, the LIVE picked counter right.
	count := fmt.Sprintf("%d selected", m.pickedCount())
	emph := false
	if m.pickedCount() > 0 {
		count += " — Enter to send"
		emph = true
	}
	panel = append(panel, drawerRow{kind: drFooter, text: inviteFooterHints, count: count, countEmph: emph})
	return panel
}

// inviteGeom is the shared paint/hit-test geometry of the centered strip:
// the window is a full-screen overlay, and the strip (exactly
// len(invitePanelRows) rows tall, innerW cells wide) centers in it.
type inviteGeom struct {
	outerW    int
	innerW    int
	top       int
	left      int
	rowFirst  int // first candidate row (terminal Y)
	rowN      int // painted candidate rows
	btnRow    int // Send button row (terminal Y)
	btnFirstX int // button's first clickable X
	btnLastX  int // button's last clickable X
}

func (m inviteModel) geom() inviteGeom {
	// The drawer standard's width cap: the terminal edge minus two cells,
	// never past 80 (with a 30-cell floor so a tiny terminal still gets
	// usable chrome).
	innerW := m.w - 2
	if innerW > 80 {
		innerW = 80
	}
	if innerW < 30 {
		innerW = 30
	}
	left := (m.w - innerW) / 2
	if left < 0 {
		left = 0
	}
	rows := m.invitePanelRows()
	// Vertical centering: the strip occupies exactly its row count.
	top := (m.h - len(rows)) / 2
	if top < 0 {
		top = 0
	}
	g := inviteGeom{outerW: innerW, innerW: innerW, top: top, left: left}
	// The windowed candidate rows (headers + items + separators) paint at
	// plan indexes [2, 2+painted) — right after the border + header — and
	// the mouse hit-test maps a click row back into inviteWindowRows, so
	// rowN must be the painted WINDOWED count, not just the item rows.
	g.rowFirst = top + 2
	wrows, _, _ := m.inviteWindowRows()
	g.rowN = m.paintedRows(len(wrows))
	for i, r := range rows {
		if r.kind == drButton {
			g.btnRow = top + i
		}
	}
	// The button renders centered: " label ", lp spacer cells on each
	// side — identical math to drawerPanelView's drButton case.
	label := " " + "SEND" + " "
	if m.busy {
		label = " " + "sending…" + " "
	}
	lp := maxInt((innerW-lipgloss.Width(label))/2, 0)
	g.btnFirstX = left + lp
	g.btnLastX = left + lp + lipgloss.Width(label) - 1
	return g
}

func (m inviteModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Type != tea.MouseLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	g := m.geom()
	// Send button click: send to every picked user.
	if msg.Y == g.btnRow && msg.X >= g.btnFirstX && msg.X <= g.btnLastX {
		return m, m.sendAll()
	}
	// Candidate row click: select AND toggle the member (mouse parity with
	// keyboard Enter), the window stays open. The hit-test walks the SAME
	// windowed row plan the painter emits, so a click on a group header or
	// a separator is chrome and does nothing.
	if msg.Y < g.rowFirst || msg.Y >= g.rowFirst+g.rowN {
		return m, nil
	}
	wrows, _, _ := m.inviteWindowRows()
	row := msg.Y - g.rowFirst
	if row < 0 || row >= len(wrows) || wrows[row].kind != drItem {
		return m, nil
	}
	idx := wrows[row].item
	if idx < 0 || idx >= len(m.cands) {
		return m, nil
	}
	m.sel = idx
	m.settleWindow()
	m.anchor = -1
	m.notice = ""
	return m, m.togglePicked(idx)
}

func (m inviteModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	g := m.geom()
	inner := g.innerW

	// The full window is one drawer strip (chat_drawer.go) — the single
	// top border line, the header, the windowed rows, the button and the
	// footer — painted by the SHARED painter and centered on the overlay.
	rows := m.invitePanelRows()
	body := drawerPanelView(inner, rows, m.sel)
	bodyLines := strings.Split(body, "\n")

	out := make([]string, m.h)
	for i := range out {
		out[i] = ""
	}
	for i, ln := range bodyLines {
		y := g.top + i
		if y >= 0 && y < m.h {
			out[y] = strings.Repeat(" ", g.left) + ln
		}
	}
	return strings.Join(out, "\n")
}

// inviteItemRowView paints ONE candidate row through the SHARED drawer
// row painter (drawerItemRow) — the invite window has no second renderer.
// The row carries the range marker + pick check gutter and the three-state
// row language: the cursor bar, the picked accent bar (the ✓ always on
// bright contrast ink), and the one-tick toggle pulse.
func (m inviteModel) inviteItemRowView(item int, hit fuzzyHit, inner int) string {
	if item < 0 || item >= len(m.cands) {
		return ""
	}
	cand := m.cands[item]
	r := drawerRow{
		kind: drItem,
		item: item,
		text: sanitizeDisplay(cand.Username),
		desc: roleSuffixTag(cand.Role),
		hit:  hit,
		lead: "  ",
	}
	r.picked = m.picked[cand.Username]
	r.flash = r.picked && m.pulseItem == item && m.pulseFrame == 0 && m.pulseGen > 0
	if lo, hi := m.rangeBounds(); item >= lo && item <= hi {
		r.lead = "> "
	}
	return drawerItemRow(r, 26, inner, item == m.sel)
}

// ─── root model: hosts the chat + full-screen windows ───────────────────────

// rootModel is the program model runChatTUI starts: the chat screen at
// rest, a full-screen window (settings / new group) on top. Windows own all
// input while open; closing one re-arms the chat's event pump + 2s tick.
type rootModel struct {
	chat    chatScreen
	overlay tea.Model
}

func newRootModel(c chatScreen) rootModel {
	return rootModel{chat: c}
}

func (r rootModel) Init() tea.Cmd { return r.chat.Init() }

func (r rootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case openSettingsMsg:
		if r.overlay == nil {
			r.overlay = newSettingsModel(r.chat.sig.serverURL, r.chat.me, r.chat.id, r.chat.width, r.chat.height)
			// Init arms the window's OWN invite fetch chain
			// (the chat's roster tick is paused while the window is
			// open — without this the inbox would never poll).
			return r, r.overlay.Init()
		}
		return r, nil
	case openNewGroupMsg:
		if r.overlay == nil {
			r.overlay = newGroupModel(r.chat.sig.serverURL, r.chat.me, r.chat.id, r.chat.key,
				r.chat.width, r.chat.height)
			return r, r.overlay.Init()
		}
		return r, nil
	case openInviteMsg:
		if r.overlay == nil {
			code := r.chat.activeGroup
			g := r.chat.groups[code]
			if g == nil || g.sig == nil {
				return r, nil // a group conversation must be open
			}
			name := ""
			if g.name != "" {
				name = g.name
			}
			r.overlay = newInviteModel(r.chat, code, name, g.sig, r.chat.width, r.chat.height)
			return r, r.overlay.Init()
		}
		return r, nil
	case closeOverlayMsg:
		if r.overlay != nil {
			r.overlay = nil
			return r, tea.Batch(r.chat.drainNetCmd(), scheduleRoster())
		}
		return r, nil
	case acceptGroupDoneMsg:
		r.overlay = nil
		sig := &signalClient{serverURL: r.chat.sig.serverURL, key: m.code, me: r.chat.me, id: r.chat.id}
		r.chat.attachGroup(m.code, m.name, "", sig, m.password)
		r.chat.openGroup(m.code)
		r.chat.status = "joined group " + m.name
		return r, tea.Batch(r.chat.drainNetCmd(), scheduleRoster())
	case createGroupDoneMsg:
		r.overlay = nil
		sig := &signalClient{serverURL: r.chat.sig.serverURL, key: m.code, me: r.chat.me, id: r.chat.id}
		r.chat.attachGroup(m.code, m.name, "", sig, "")
		r.chat.openGroup(m.code)
		if len(m.notes) > 0 {
			r.chat.status = strings.Join(m.notes, " · ")
		} else {
			r.chat.status = "created group " + m.name
		}
		return r, tea.Batch(r.chat.drainNetCmd(), scheduleRoster())
	}

	if r.overlay != nil {
		ov, cmd := r.overlay.Update(msg)
		r.overlay = ov
		return r, cmd
	}
	m, cmd := r.chat.Update(msg)
	if cs, ok := m.(chatScreen); ok {
		r.chat = cs
		return r, cmd
	}
	return m, cmd
}

func (r rootModel) View() string {
	if r.overlay != nil {
		return r.overlay.View()
	}
	return r.chat.View()
}
