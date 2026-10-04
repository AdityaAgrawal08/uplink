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

// ─── full-screen windows: /settings and /new-group ───────────────────────────
//
// Both open as a full-screen model over the chat, hosted by rootModel (the
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

const (
	groupStagePick = iota
	groupStageForm
)

// newGroupModel is the creation window: stage 1 picks room members to
// invite (multi-select dropdown mirroring the "/" palette), stage 2 is the
// creation form (Name, Description, Max-People) with inline validation.
// Submitting creates the group (creator crowned server-side) and invites
// every pick; 409 "already in this session" notes ride along.
type groupModel struct {
	w, h       int
	me         string
	serverURL  string
	id         *identityKey
	parentCode string
	members    []rosterMember
	filter     string
	sel        int
	picked     map[string]bool
	stage      int
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

func newGroupModel(serverURL, me string, id *identityKey, parentCode string, w, h int, members []rosterMember) groupModel {
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

	out := make([]rosterMember, 0, len(members))
	for _, mm := range members {
		if mm.Username != "" && mm.Username != me {
			out = append(out, mm)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return groupModel{
		me:         me,
		serverURL:  serverURL,
		id:         id,
		parentCode: parentCode,
		members:    out,
		picked:     map[string]bool{},
		w:          w,
		h:          h,
		nameInput:  ni,
		descInput:  di,
		maxInput:   mi,
	}
}

func (m groupModel) Init() tea.Cmd { return textinput.Blink }

// candidates ranks room members for the current filter (prefix-first, the
// palette's ordering contract).
func (m groupModel) candidates() []rosterMember {
	return rankUsers(m.members, m.filter)
}

func (m groupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, func() tea.Msg { return closeOverlayMsg{} }
		}
		if m.stage == groupStagePick {
			return m.updatePick(msg)
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

// updatePick handles the member multi-select stage: typing narrows the
// list, Space toggles the highlighted member, Enter moves to the form.
func (m groupModel) updatePick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyUp:
		if n := len(m.candidates()); n > 0 {
			m.sel = ((m.sel-1)%n + n) % n
		}
		return m, nil
	case tea.KeyDown:
		if n := len(m.candidates()); n > 0 {
			m.sel = (m.sel + 1) % n
		}
		return m, nil
	case tea.KeySpace:
		users := m.candidates()
		if m.sel >= 0 && m.sel < len(users) {
			u := users[m.sel].Username
			if m.picked[u] {
				delete(m.picked, u)
			} else {
				m.picked[u] = true
			}
		}
		return m, nil
	case tea.KeyEnter:
		// Commit the current picks (possibly empty) and open the form.
		m.stage = groupStageForm
		m.nameInput.Focus()
		m.formFocus = 0
		return m, nil
	case tea.KeyBackspace, tea.KeyCtrlH, tea.KeyDelete:
		if m.filter != "" {
			r := []rune(m.filter)
			m.filter = string(r[:len(r)-1])
			m.sel = 0
		}
		return m, nil
	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			m.filter += strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f {
					return -1
				}
				return r
			}, string(msg.Runes))
			if len([]rune(m.filter)) > 40 {
				m.filter = string([]rune(m.filter)[:40])
			}
			m.sel = 0
		}
		return m, nil
	}
}

// updateForm handles the creation form: Tab/Enter walk the fields, the
// Create button submits with inline validation.
func (m groupModel) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.stage = groupStagePick // back to the member pick, picks kept
		return m, nil
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

// selectedUsers returns the picked usernames in member order (stable).
func (m groupModel) selectedUsers() []string {
	var out []string
	for _, mm := range m.members {
		if m.picked[mm.Username] {
			out = append(out, mm.Username)
		}
	}
	return out
}

// submit validates the form, creates the group and invites every pick.
// The server crowns the creator automatically; invite errors (notably 409
// "already in this session") become notes and the rest continue.
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
	users := m.selectedUsers()
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
		return createGroupDoneMsg{code: code, name: name, notes: notes}
	}
}

func (m groupModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	if m.stage == groupStagePick {
		return m.pickView()
	}
	return m.formView()
}

func (m groupModel) pickView() string {
	outerW := m.w - 4
	if outerW < 60 {
		outerW = 60
	}
	if outerW > 78 {
		outerW = 78
	}
	inner := outerW - 4
	users := m.candidates()
	m.sel = clampInt(m.sel, 0, maxInt(len(users)-1, 0))

	fit := func(s string) string { return fitRow(s, inner) }
	rows := make([]string, 0, len(users)+8)
	rows = append(rows, fit(lipgloss.NewStyle().Bold(true).Foreground(colAccent).Render("◆ NEW GROUP")))
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colDim).Render(
		"pick members to invite — everyone here can join by invite.")))
	rows = append(rows, " ")

	// Multi-select dropdown: the palette's row discipline with a leading
	// pick mark. Empty filter paints every member.
	q := strings.ToLower(m.filter)
	for i, u := range users {
		if q != "" && !strings.HasPrefix(strings.ToLower(u.Username), q) {
			continue
		}
		mark := " "
		if m.picked[u.Username] {
			mark = tuiPaletteSelStyle.Render("✓")
		}
		line := fit(fmt.Sprintf(" %s  %s%s", mark, u.Username, roleSuffixTag(u.Role)))
		if i == m.sel {
			line = fit(tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle)))
		}
		rows = append(rows, line)
	}
	if len(users) == 0 {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render("  no members to invite.")))
	}
	rows = append(rows, " ")
	if m.filter != "" {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render("  filter: "+sanitizeDisplay(m.filter))))
	}
	if m.notice != "" {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colRed).Render(m.notice)))
	}
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render(
		"type to filter · space toggle · enter create · esc close")))

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
			fmt.Sprintf("creating inside session %s · %d invited", sanitizeDisplay(m.parentCode), len(m.selectedUsers())))),
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
		"tab moves · enter submits · esc back to members")))

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

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
		}
		return r, nil
	case openNewGroupMsg:
		if r.overlay == nil {
			members := r.chat.memberList()
			r.overlay = newGroupModel(r.chat.sig.serverURL, r.chat.me, r.chat.id, r.chat.key,
				r.chat.width, r.chat.height, members)
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
