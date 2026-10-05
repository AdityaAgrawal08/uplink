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

// Invite-row choice cursor: the focused row carries a choice between its two
// action targets. ← moves it onto ✕ (decline), → onto ✓ (accept); Enter
// applies whichever target is highlighted. The cursor defaults to ✓ so the
// long-standing Enter=accept binding is unchanged.
const (
	inviteChoiceDecline = iota
	inviteChoiceAccept
)

// settingsModel is the full-screen settings window: an invite inbox listing
// "X invited you to group Y" rows, each exposing two explicit action targets
// — ✕ (decline) and ✓ (accept) — plus an account section. The focused row
// carries a ←/→ choice cursor over those targets; Enter applies the
// highlighted one. Mouse clicks hit the SAME two glyph cells, resolved per
// row (any other cell is inert). Accepting a protected group opens the
// two-step password modal (401-required → modal; wrong → retry in place; Esc
// back).
type settingsModel struct {
	w, h      int
	me        string
	serverURL string
	id        *identityKey
	sig       *signalClient
	invites   []groupInvite
	sel       int
	choice    int // inviteChoiceDecline / inviteChoiceAccept on the focused row
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
		choice:    inviteChoiceAccept,
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
		case tea.KeyLeft:
			// Choice cursor: ← picks the ✕ (decline) target on the focused row.
			if len(m.invites) > 0 {
				m.choice = inviteChoiceDecline
			}
			return m, nil
		case tea.KeyRight:
			// → picks the ✓ (accept) target on the focused row.
			if len(m.invites) > 0 {
				m.choice = inviteChoiceAccept
			}
			return m, nil
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
				if m.choice == inviteChoiceDecline {
					return m, m.declineCurrent()
				}
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
	rowW     int // painted row width (the card's text area: innerW - padding)
	top      int
	left     int
	invFirst int // first invite row (terminal Y)
	invN     int // painted invite rows
	declineX int // terminal X of the ✕ (decline) action glyph
	acceptX  int // terminal X of the ✓ (accept) action glyph
}

// settingsTailW is the painted width of an invite row's two action chips:
// " ✕ " then " ✓ ". Only the glyph cells are live hit targets.
const settingsTailW = 6

func (m settingsModel) geom() settingsGeom {
	outerW := m.w - 4
	if outerW < 60 {
		outerW = 60
	}
	if outerW > 78 {
		outerW = 78
	}
	innerW := outerW - 4 // card style width (padding sits INSIDE it)
	// lipgloss Width includes the horizontal padding, so the card's text
	// area — and its word-wrap boundary — is innerW-2 cells. Rows must be
	// fit to rowW or they wrap a trailing cell onto a second line.
	rowW := innerW - 2
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
	// Terminal-precise text origin: the card block (innerW + 2 border cells)
	// centers one cell right of `left` (the outerW - innerW + 2 difference),
	// then the border and padding put the row text at left+3. The two action
	// chips ride the row's right edge; their glyphs sit at the centre of
	// their 3-cell chips and are the only live cells.
	contentX := left + 3
	tailX := contentX + rowW - settingsTailW
	return settingsGeom{
		outerW: outerW, innerW: innerW, rowW: rowW, top: top, left: left,
		invFirst: top + 1 + 2, invN: n,
		declineX: tailX + 1, acceptX: tailX + 4,
	}
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
	// Each row exposes exactly two live targets: the ✕ and ✓ glyph cells.
	// Any other cell (the row body, the chip padding, the chrome) is inert —
	// the click resolves the specific target, not merely the row.
	switch msg.X {
	case g.declineX:
		m.sel = idx
		m.choice = inviteChoiceDecline
		m.clampSel()
		return m, m.declineCurrent()
	case g.acceptX:
		m.sel = idx
		m.choice = inviteChoiceAccept
		m.clampSel()
		return m, m.acceptCurrent()
	}
	return m, nil
}

// inviteTail paints one invite row's two action chips, " ✕ " (decline) then
// " ✓ " (accept). On the FOCUSED row the active choice wears the shared
// cursor-bar style (tuiPaletteSelStyle) and the inactive chip rests muted;
// unfocused rows keep the resting language (✕ dim, ✓ accent). The glyph
// cells are exactly the mouse hit targets — the surrounding pad is inert.
func (m settingsModel) inviteTail(focused bool) string {
	decline := lipgloss.NewStyle().Foreground(colDim)
	accept := lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	if focused {
		if m.choice == inviteChoiceDecline {
			decline = tuiPaletteSelStyle
			accept = lipgloss.NewStyle().Foreground(colDim)
		} else {
			accept = tuiPaletteSelStyle
			decline = lipgloss.NewStyle().Foreground(colDim)
		}
	}
	return decline.Render(" ✕ ") + accept.Render(" ✓ ")
}

func (m settingsModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	g := m.geom()
	inner := g.innerW
	rowW := g.rowW

	fit := func(s string) string { return fitRow(s, rowW) }
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
			if len([]rune(body)) > rowW-8 {
				body = truncateStringPlain(body, rowW-8)
			}
			// The focused row carries the cursor marker; its ACTIVE choice
			// chip wears the shared cursor-bar style. The two glyph cells are
			// exactly the mouse hit targets (settingsGeom.declineX/acceptX).
			lead := "  "
			if idx == m.sel {
				lead = lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render("> ")
			}
			tail := m.inviteTail(idx == m.sel)
			rows = append(rows, fit(padVisible(lead+body, rowW-lipgloss.Width(tail))+tail))
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
		"↑↓ select · ←→ choose · enter apply · esc close")))

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

// ─── /group-edit and /group-members windows ──────────────────────────────────
//
// Both are hosted by rootModel like /settings and /invite. /group-edit owns
// the group's administration: rename + description (admin-only PATCH), admin
// transfer (multi-select member picker -> batch setRole admin calls), and the
// creator-only DELETE dissolve behind an explicit confirmation. Members open
// the same window read-only with the reason, never a dead end. /group-members
// is the read-only roster in the shared drawer craft: role-grouped rows,
// Admins first.

// openGroupEditMsg / openGroupMembersMsg travel from the /group-edit and
// /group-members command handlers to rootModel (the commands refuse outside
// a group view, so these only arrive with a group conversation active).
type openGroupEditMsg struct{}
type openGroupMembersMsg struct{}

// groupMetaSavedMsg resolves a PATCH /meta attempt: err nil = the server's
// resulting display fields; both rootModel (plumb into the session + store)
// and the window (busy/notice) consume it.
type groupMetaSavedMsg struct {
	code, name, desc string
	err              error
}

// groupAdminsAppliedMsg resolves one batch of setRole(admin=true) calls:
// notes carries per-user outcomes (successes and refusals), promoted the
// users the server actually promoted, roster/epoch the last fresh roster for
// an immediate sidebar refresh.
type groupAdminsAppliedMsg struct {
	code     string
	notes    []string
	promoted []string
	roster   []rosterMember
	epoch    int64
	err      error
}

// groupDeletedMsg resolves the creator DELETE dissolve.
type groupDeletedMsg struct {
	code, name string
	err        error
}

// Window modes for groupEditModel.
const (
	geModeForm = iota
	geModeAdmins
	geModeDelete
)

// Form focus rows for groupEditModel.
const (
	geFocusName = iota
	geFocusDesc
	geFocusTransfer
	geFocusDelete
	geFocusSave
	geFocusCount
)

// groupEditModel is the /group-edit window. Admin view: editable Name /
// Description with a SAVE button, a Transfer-admin action that opens the
// multi-select member picker, and a Delete-group action (creator only) with
// a confirmation spelling out the dissolve. Member view: the same card
// read-only, with the reason ("only admins can change group details") —
// never a screen that silently refuses keys.
type groupEditModel struct {
	w, h      int
	me        string
	code      string
	name      string // server-known name (updated from the PATCH response)
	desc      string
	sig       *signalClient // group session client
	role      string        // creator | admin | member
	members   []rosterMember
	mode      int
	focus     int
	nameInput textinput.Model
	descInput textinput.Model
	picked    map[string]bool
	pickSel   int
	pickOff   int
	pickApply bool
	busy      bool
	notice    string
	errMsg    string
}

func (m groupEditModel) isAdmin() bool   { return m.role == "creator" || m.role == "admin" }
func (m groupEditModel) isCreator() bool { return m.role == "creator" }

// groupRoleLabel names my role in the window's context line.
func groupRoleLabel(role string) string {
	switch role {
	case "creator":
		return "creator (main admin)"
	case "admin":
		return "admin"
	default:
		return "member"
	}
}

// groupRoleRank orders members for display: creator, admins, members.
func groupRoleRank(role string) int {
	switch role {
	case "creator":
		return 0
	case "admin":
		return 1
	default:
		return 2
	}
}

// sortGroupMembers returns the roster in the display order every group
// surface uses: creator first, then admins, then members, alpha within each
// class (case-insensitive).
func sortGroupMembers(ms []rosterMember) []rosterMember {
	out := append([]rosterMember(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := groupRoleRank(out[i].Role), groupRoleRank(out[j].Role)
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(out[i].Username) < strings.ToLower(out[j].Username)
	})
	return out
}

// groupMembersOf snapshots a group session's live roster (sorted). Bare
// sessions without an engine report nothing.
func groupMembersOf(g *groupSession) []rosterMember {
	if g == nil || g.eng == nil {
		return nil
	}
	return sortGroupMembers(g.eng.peers())
}

func newGroupEditModel(serverURL, me string, id *identityKey, code, name, desc, role string, members []rosterMember, w, h int) groupEditModel {
	ni := textinput.New()
	ni.Placeholder = "Design Team"
	ni.CharLimit = 64
	ni.Prompt = "> "
	ni.Width = 40
	ni.TextStyle = lipgloss.NewStyle().Foreground(colText)
	ni.PlaceholderStyle = lipgloss.NewStyle().Foreground(colFaint)
	ni.SetValue(name)

	di := textinput.New()
	di.Placeholder = "what is this group about?"
	di.CharLimit = 256
	di.Prompt = "> "
	di.Width = 40
	di.TextStyle = lipgloss.NewStyle().Foreground(colText)
	di.PlaceholderStyle = lipgloss.NewStyle().Foreground(colFaint)
	di.SetValue(desc)

	// The picker works on OTHER members only (self needs no promotion).
	others := make([]rosterMember, 0, len(members))
	for _, m := range sortGroupMembers(members) {
		if m.Username == "" || m.Username == me {
			continue
		}
		others = append(others, m)
	}
	m := groupEditModel{
		me:        me,
		code:      code,
		name:      name,
		desc:      desc,
		sig:       &signalClient{serverURL: serverURL, key: code, me: me, id: id},
		role:      role,
		members:   others,
		picked:    map[string]bool{},
		nameInput: ni,
		descInput: di,
		w:         w,
		h:         h,
	}
	if m.isAdmin() {
		m.nameInput.Focus()
	}
	return m
}

func (m groupEditModel) Init() tea.Cmd { return textinput.Blink }

func (m groupEditModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case groupMetaSavedMsg:
		m.busy = false
		m.mode = geModeForm
		if msg.err != nil {
			if isServerDown(msg.err) {
				m.errMsg = serverDownMsg
			} else {
				m.errMsg = msg.err.Error()
			}
			return m, nil
		}
		m.name, m.desc = msg.name, msg.desc
		m.nameInput.SetValue(msg.name)
		m.descInput.SetValue(msg.desc)
		m.focus = geFocusSave
		m.syncFocus()
		m.notice = "group details saved"
		m.errMsg = ""
		return m, nil

	case groupAdminsAppliedMsg:
		m.busy = false
		m.mode = geModeForm
		m.focus = geFocusTransfer
		m.picked = map[string]bool{}
		m.pickApply = false
		m.notice = ""
		m.errMsg = ""
		// Reflect the promotions locally so the picker never offers an
		// already-promoted member twice.
		promoted := map[string]bool{}
		for _, u := range msg.promoted {
			promoted[u] = true
		}
		for i := range m.members {
			if promoted[m.members[i].Username] {
				m.members[i].Role = "admin"
			}
		}
		if len(msg.notes) > 0 {
			m.notice = strings.Join(msg.notes, " · ")
		}
		if msg.err != nil && len(msg.notes) == 0 {
			m.errMsg = msg.err.Error()
		}
		return m, nil

	case groupDeletedMsg:
		m.busy = false
		if msg.err != nil {
			if isServerDown(msg.err) {
				m.errMsg = serverDownMsg
			} else {
				m.errMsg = "delete failed: " + msg.err.Error()
			}
			m.mode = geModeForm
			return m, nil
		}
		return m, func() tea.Msg { return closeOverlayMsg{} }

	case tea.KeyMsg:
		if m.busy {
			return m, nil // one request at a time; the window settles on its own
		}
		switch m.mode {
		case geModeAdmins:
			return m.handleAdminKeys(msg)
		case geModeDelete:
			return m.handleDeleteKeys(msg)
		default:
			return m.handleFormKeys(msg)
		}
	}
	return m, nil
}

// handleFormKeys: Tab walks the focus rows, Enter advances/open/submits
// depending on the row, Esc closes. Text edits ride the focused input only
// for admins (the member view accepts nothing).
func (m groupEditModel) handleFormKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyEsc:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyTab, tea.KeyShiftTab:
		step := 1
		if msg.Type == tea.KeyShiftTab || msg.String() == "shift+tab" {
			step = geFocusCount - 1
		}
		m.focus = (m.focus + step) % geFocusCount
		m.syncFocus()
		return m, nil
	case tea.KeyEnter:
		switch m.focus {
		case geFocusName, geFocusDesc:
			if !m.isAdmin() {
				m.errMsg = "only admins can change group details"
				return m, nil
			}
			m.focus = (m.focus + 1) % geFocusCount
			m.syncFocus()
			return m, nil
		case geFocusTransfer:
			if !m.isAdmin() {
				m.errMsg = "only admins can change group details"
				return m, nil
			}
			m.mode = geModeAdmins
			m.picked = map[string]bool{}
			m.pickSel, m.pickOff, m.pickApply = 0, 0, false
			m.notice, m.errMsg = "", ""
			return m, nil
		case geFocusDelete:
			if !m.isCreator() {
				if m.isAdmin() {
					m.errMsg = "Only the group creator can delete this group"
				} else {
					m.errMsg = "only admins can change group details"
				}
				return m, nil
			}
			m.mode = geModeDelete
			m.notice, m.errMsg = "", ""
			return m, nil
		case geFocusSave:
			if !m.isAdmin() {
				m.errMsg = "only admins can change group details"
				return m, nil
			}
			return m, m.submitMeta()
		}
		return m, nil
	default:
		if !m.isAdmin() {
			return m, nil // read-only: no field ever receives text
		}
		switch m.focus {
		case geFocusName:
			var cmd tea.Cmd
			m.nameInput, cmd = m.nameInput.Update(msg)
			return m, cmd
		case geFocusDesc:
			var cmd tea.Cmd
			m.descInput, cmd = m.descInput.Update(msg)
			return m, cmd
		}
		return m, nil
	}
}

// syncFocus keeps exactly one field (or none) focused.
func (m *groupEditModel) syncFocus() {
	m.nameInput.Blur()
	m.descInput.Blur()
	if !m.isAdmin() {
		return
	}
	switch m.focus {
	case geFocusName:
		m.nameInput.Focus()
	case geFocusDesc:
		m.descInput.Focus()
	}
}

// submitMeta PATCHes only the changed fields, with the same inline
// validation as /new-group (the server re-validates).
func (m *groupEditModel) submitMeta() tea.Cmd {
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
	var namePtr, descPtr *string
	if name != m.name {
		namePtr = &name
	}
	if desc != m.desc {
		descPtr = &desc
	}
	if namePtr == nil && descPtr == nil {
		m.notice = "nothing changed"
		return nil
	}
	m.busy = true
	m.notice = ""
	sig := m.sig
	code := m.code
	return func() tea.Msg {
		n, d, err := sig.patchGroupMeta(namePtr, descPtr)
		return groupMetaSavedMsg{code: code, name: n, desc: d, err: err}
	}
}

// handleAdminKeys drives the multi-select member picker: arrows walk the
// grouped display order, Space/Enter toggles a plain member, Tab moves to
// the APPLY button, Enter there fires the batch.
func (m groupEditModel) handleAdminKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyEsc:
		m.mode = geModeForm
		m.focus = geFocusTransfer
		m.errMsg = ""
		return m, nil
	case tea.KeyUp:
		m.movePick(-1)
		return m, nil
	case tea.KeyDown:
		m.movePick(+1)
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab:
		m.pickApply = !m.pickApply
		return m, nil
	case tea.KeySpace:
		m.togglePick()
		return m, nil
	case tea.KeyEnter:
		if m.pickApply {
			return m, m.applyPicks()
		}
		m.togglePick()
		return m, nil
	}
	return m, nil
}

// movePick steps the highlight through the PAINTED display order of the
// grouped plan (role headers shift display rows), exactly like the /invite
// picker.
func (m *groupEditModel) movePick(d int) {
	n := len(m.members)
	if n == 0 {
		m.pickSel, m.pickOff = 0, 0
		return
	}
	plan := buildDrawerPlan(n, true, func(i int) string { return userGroupLabel(m.members[i]) }, drawerMaxRows(m.h))
	disp := plan.displayOrder()
	cur := 0
	for i, it := range disp {
		if it == m.pickSel {
			cur = i
			break
		}
	}
	cur = (cur + d + len(disp)) % len(disp)
	m.pickSel = disp[cur]
	settleDrawerWindow(&m.pickSel, &m.pickOff, plan)
}

// togglePick flips the highlighted member's pick state. The creator and
// existing admins are not promotable (the server refuses creator changes and
// no-ops admin grants), so they answer with a note instead.
func (m *groupEditModel) togglePick() {
	if m.pickSel < 0 || m.pickSel >= len(m.members) {
		return
	}
	u := m.members[m.pickSel]
	if u.Role == "creator" || u.Role == "admin" {
		m.notice = u.Username + " is already " + map[bool]string{true: "the main admin", false: "an admin"}[u.Role == "creator"]
		m.errMsg = ""
		return
	}
	if m.picked[u.Username] {
		delete(m.picked, u.Username)
	} else {
		m.picked[u.Username] = true
	}
	m.notice, m.errMsg = "", ""
}

// applyPicks POSTs /admin for every picked member (the same setRole client
// path /admin and /unadmin use). Per-user failures become notes; the rest
// continue.
func (m *groupEditModel) applyPicks() tea.Cmd {
	picks := make([]string, 0, len(m.picked))
	for _, u := range m.members {
		if m.picked[u.Username] {
			picks = append(picks, u.Username)
		}
	}
	if len(picks) == 0 {
		m.notice = "pick at least one member first"
		return nil
	}
	m.busy = true
	m.notice = ""
	sig := m.sig
	code := m.code
	return func() tea.Msg {
		var notes, promoted []string
		var roster []rosterMember
		var epoch int64
		var firstErr error
		for _, u := range picks {
			r, e, err := sig.setRole(u, true)
			if err != nil {
				notes = append(notes, u+": "+err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			roster, epoch = r, e
			promoted = append(promoted, u)
			notes = append(notes, u+" is now an admin")
		}
		return groupAdminsAppliedMsg{code: code, notes: notes, promoted: promoted, roster: roster, epoch: epoch, err: firstErr}
	}
}

// handleDeleteKeys: Enter confirms, Esc backs out.
func (m groupEditModel) handleDeleteKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, func() tea.Msg { return closeOverlayMsg{} }
	case tea.KeyEsc:
		m.mode = geModeForm
		m.focus = geFocusDelete
		return m, nil
	case tea.KeyEnter:
		if !m.isCreator() {
			m.mode = geModeForm
			m.errMsg = "Only the group creator can delete this group"
			return m, nil
		}
		m.busy = true
		sig := m.sig
		code, name := m.code, m.name
		return m, func() tea.Msg {
			return groupDeletedMsg{code: code, name: name, err: sig.deleteGroupRoom()}
		}
	}
	return m, nil
}

// paintStrip centers a drawer panel (exact row count, inner cells wide) in
// the full-screen overlay. Shared by the /group-edit picker and the
// /group-members list.
func paintStrip(body string, inner, w, h int) string {
	if body == "" {
		return strings.Repeat("\n", maxInt(h-1, 0))
	}
	lines := strings.Split(body, "\n")
	top := (h - len(lines)) / 2
	if top < 0 {
		top = 0
	}
	left := (w - inner) / 2
	if left < 0 {
		left = 0
	}
	out := make([]string, maxInt(h, 0))
	for i, ln := range lines {
		y := top + i
		if y >= 0 && y < len(out) {
			out[y] = strings.Repeat(" ", left) + ln
		}
	}
	return strings.Join(out, "\n")
}

// adminPanelRows builds the /group-edit transfer picker in the shared drawer
// craft: role-grouped member rows with pick checks, the APPLY button and the
// keymap footer.
func (m groupEditModel) adminPanelRows() []drawerRow {
	win := drawerMaxRows(m.h)
	plan := buildDrawerPlan(len(m.members), true, func(i int) string { return userGroupLabel(m.members[i]) }, win)
	wrows, below, above := windowDrawerPlan(plan, m.pickOff, m.pickSel)
	panel := make([]drawerRow, 0, 12)
	panel = append(panel, drawerRow{kind: drBorder},
		drawerRow{kind: drHeader, text: "Transfer admin — " + sanitizeDisplay(m.name)})
	if len(m.members) == 0 {
		panel = append(panel, drawerRow{kind: drEmpty, text: "No other members to promote"})
	} else {
		for _, r := range wrows {
			if r.kind == drItem {
				cand := m.members[r.item]
				rr := drawerRow{
					kind: drItem, item: r.item,
					text: sanitizeDisplay(cand.Username),
					desc: roleSuffixTag(cand.Role),
					hit:  plan.hitOf(r.item),
					lead: "  ",
				}
				rr.picked = m.picked[cand.Username]
				panel = append(panel, rr)
			} else {
				panel = append(panel, r)
			}
		}
		if below > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: below, text: "more"})
		} else if above > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: above, text: "above"})
		}
	}
	if m.notice != "" {
		panel = append(panel, drawerRow{kind: drNotice,
			text: lipgloss.NewStyle().Foreground(colDim).Render(m.notice)})
	} else if m.errMsg != "" {
		panel = append(panel, drawerRow{kind: drNotice,
			text: lipgloss.NewStyle().Foreground(colRed).Render(m.errMsg)})
	}
	btnN := 0
	if m.pickApply {
		btnN = 1
	}
	btn := "MAKE ADMINS"
	if m.busy {
		btn = "promoting…"
	}
	panel = append(panel, drawerRow{kind: drButton, text: btn, n: btnN})
	panel = append(panel, drawerRow{kind: drFooter,
		text: "↑↓ move · space toggle · tab apply · esc back", n: len(m.members)})
	return panel
}

func (m groupEditModel) adminView() string {
	inner := m.w - 2
	if inner > 80 {
		inner = 80
	}
	if inner < 30 {
		inner = 30
	}
	return paintStrip(drawerPanelView(inner, m.adminPanelRows(), m.pickSel), inner, m.w, m.h)
}

// deleteConfirmView is the modal over the form: the exact consequences are
// spelled out, Enter confirms, Esc backs out.
func (m groupEditModel) deleteConfirmView() string {
	innerW := m.w - 8
	if innerW < 24 {
		innerW = 24
	}
	if innerW > 56 {
		innerW = 56
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(colRed).Render("DELETE GROUP?")
	body := "This dissolves " + sanitizeDisplay(m.name) + " for everyone;"
	body2 := "members lose access on their next heartbeat."
	if w := innerW - 2; w > 0 {
		body = truncateByWidth(body, w)
		body2 = truncateByWidth(body2, w)
	}
	warn := "This cannot be undone."
	lines := []string{
		title,
		landSubtitleStyle.Render(body),
		landSubtitleStyle.Render(body2),
		landErrorStyle.Render(warn),
		"",
		landHintStyle.Render("ENTER confirm  ·  ESC cancel"),
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colRed).
		Background(colPanel).
		Padding(1, 2).
		Render(strings.Join(lines, "\n"))
}

// formView paints the administration card: the context line, the Name and
// Description sections (editable for admins, static text for members), the
// Transfer and Delete action rows (disabled with the reason where the role
// does not allow them), the SAVE button and the notice/error line.
func (m groupEditModel) formView() string {
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
		if m.focus == f && m.isAdmin() {
			st = landLabelFocusStyle
		}
		return st.Width(14).Render(text)
	}
	box := func(ti textinput.Model, f int) string {
		st := landInputBlurStyle
		if m.focus == f && m.isAdmin() {
			st = landInputFocusedStyle
		}
		return st.Render(ti.View())
	}
	row := func(label, input string) string {
		return lipgloss.JoinHorizontal(lipgloss.Top, label, " ", input)
	}
	actionRow := func(label, note string, f int, enabled bool) string {
		w := maxInt(inner-2, 1)
		st := landButtonBlurStyle.Width(w)
		if m.focus == f && enabled {
			st = landButtonSelectedStyle.Width(w)
		}
		text := "  " + label
		if note != "" {
			text += "  ·  " + note
		}
		if !enabled {
			st = lipgloss.NewStyle().Foreground(colFaint)
		}
		return fit(st.Render(text))
	}

	rows := []string{
		fit(lipgloss.NewStyle().Bold(true).Foreground(colAccent).Render("◆ GROUP EDIT — " + sanitizeDisplay(m.name))),
		fit(lipgloss.NewStyle().Foreground(colDim).Render(
			fmt.Sprintf("key %s  ·  you are %s", sanitizeDisplay(m.code), groupRoleLabel(m.role)))),
	}
	if !m.isAdmin() {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colRed).Render(
			"Read-only — only admins can change group details.")))
	}
	rows = append(rows, " ")
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colDim).Bold(true).Render("NAME")))
	if m.isAdmin() {
		rows = append(rows, row(labelOf("Name :", geFocusName), box(m.nameInput, geFocusName)))
	} else {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colText).Render("  "+sanitizeDisplay(m.name))))
	}
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colDim).Bold(true).Render("DESCRIPTION")))
	if m.isAdmin() {
		rows = append(rows, row(labelOf("Description :", geFocusDesc), box(m.descInput, geFocusDesc)))
	} else {
		text := sanitizeDisplay(m.desc)
		if text == "" {
			text = "(none)"
		}
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colText).Render("  "+text)))
	}
	rows = append(rows, " ")
	transferNote := "make members admins"
	if n := len(m.picked); n > 0 {
		transferNote = fmt.Sprintf("%d selected", n)
	}
	if !m.isAdmin() {
		transferNote = "admins only"
	}
	rows = append(rows, actionRow("Transfer admin", transferNote, geFocusTransfer, m.isAdmin()))
	deleteNote := "creator only — dissolves for everyone"
	if m.isCreator() {
		deleteNote = "dissolves for everyone"
	} else if m.isAdmin() {
		deleteNote = "creator only"
	} else {
		deleteNote = "admins only"
	}
	rows = append(rows, actionRow("Delete group", deleteNote, geFocusDelete, m.isCreator()))
	if m.isAdmin() {
		btnText := "SAVE"
		if m.busy {
			btnText = "saving…"
		}
		btnW := lipgloss.Width(btnText) + 4
		btnStyle := landButtonBlurStyle.Width(btnW).Padding(0, 1)
		if m.focus == geFocusSave && !m.busy {
			btnStyle = landButtonSelectedStyle.Width(btnW).Padding(0, 1)
		}
		rows = append(rows, lipgloss.NewStyle().Width(inner).Align(lipgloss.Center).Render(btnStyle.Render(btnText)))
	}
	if m.errMsg != "" {
		rows = append(rows, fit(landErrorStyle.Render(m.errMsg)))
	} else if m.notice != "" {
		rows = append(rows, fit(lipgloss.NewStyle().Foreground(colDim).Render(m.notice)))
	}
	rows = append(rows, fit(lipgloss.NewStyle().Foreground(colFaint).Render(
		"tab move · enter open/save · esc close")))

	card := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colEdge).
		Background(colPanel).
		Width(inner).
		Padding(0, 1).
		Render(strings.Join(rows, "\n"))
	centered := lipgloss.NewStyle().Width(m.w).Height(m.h).
		Align(lipgloss.Center).AlignVertical(lipgloss.Center).Render(card)
	if m.mode == geModeDelete {
		return overlayCenter(centered, m.deleteConfirmView(), m.w, m.h)
	}
	return centered
}

func (m groupEditModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	if m.mode == geModeAdmins {
		return m.adminView()
	}
	return m.formView()
}

// ─── /group-members window ───────────────────────────────────────────────────

// groupMembersModel is the read-only member list: role-grouped rows in the
// shared drawer craft, admins first, with the group name in the header. Esc
// closes.
type groupMembersModel struct {
	w, h    int
	code    string
	name    string
	members []rosterMember
}

func newGroupMembersModel(code, name string, members []rosterMember, w, h int) groupMembersModel {
	if name == "" {
		name = code
	}
	return groupMembersModel{code: code, name: name, members: sortGroupMembers(members), w: w, h: h}
}

func (m groupMembersModel) Init() tea.Cmd { return nil }

func (m groupMembersModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyEsc || msg.Type == tea.KeyCtrlC {
			return m, func() tea.Msg { return closeOverlayMsg{} }
		}
	}
	return m, nil
}

// panelRows builds the member list plan: role headers ("Admins" first), one
// row per member with the role tag, the overflow marker, and the footer
// count. Read-only: no cursor, no picked state.
func (m groupMembersModel) panelRows() []drawerRow {
	win := drawerMaxRows(m.h)
	plan := buildDrawerPlan(len(m.members), true, func(i int) string { return userGroupLabel(m.members[i]) }, win)
	rows, below, above := windowDrawerPlan(plan, 0, -1)
	panel := make([]drawerRow, 0, len(rows)+4)
	panel = append(panel, drawerRow{kind: drBorder},
		drawerRow{kind: drHeader, text: "Members — " + sanitizeDisplay(m.name)})
	if len(m.members) == 0 {
		panel = append(panel, drawerRow{kind: drEmpty, text: "No members reported yet"})
	} else {
		for _, r := range rows {
			if r.kind == drItem {
				u := m.members[r.item]
				panel = append(panel, drawerRow{
					kind: drItem, item: r.item,
					text: sanitizeDisplay(u.Username),
					desc: roleSuffixTag(u.Role),
				})
			} else {
				panel = append(panel, r)
			}
		}
		if below > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: below, text: "more"})
		} else if above > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: above, text: "above"})
		}
	}
	count := fmt.Sprintf("%d member", len(m.members))
	if len(m.members) != 1 {
		count += "s"
	}
	panel = append(panel, drawerRow{kind: drFooter, text: "esc close", count: count})
	return panel
}

func (m groupMembersModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	inner := m.w - 2
	if inner > 80 {
		inner = 80
	}
	if inner < 30 {
		inner = 30
	}
	return paintStrip(drawerPanelView(inner, m.panelRows(), -1), inner, m.w, m.h)
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
	case openGroupEditMsg:
		if r.overlay == nil {
			code := r.chat.activeGroup
			g := r.chat.groups[code]
			if g == nil || g.sig == nil {
				return r, nil // a group conversation must be open
			}
			r.overlay = newGroupEditModel(r.chat.sig.serverURL, r.chat.me, r.chat.id,
				code, g.name, g.desc, r.chat.myRole(), groupMembersOf(g),
				r.chat.width, r.chat.height)
			return r, r.overlay.Init()
		}
		return r, nil
	case openGroupMembersMsg:
		if r.overlay == nil {
			code := r.chat.activeGroup
			g := r.chat.groups[code]
			if g == nil {
				return r, nil // a group conversation must be open
			}
			name := g.name
			if name == "" {
				name = code
			}
			r.overlay = newGroupMembersModel(code, name, groupMembersOf(g), r.chat.width, r.chat.height)
			return r, r.overlay.Init()
		}
		return r, nil
	case closeOverlayMsg:
		if r.overlay != nil {
			r.overlay = nil
			return r, tea.Batch(r.chat.drainNetCmd(), scheduleRoster())
		}
		return r, nil
	case groupMetaSavedMsg:
		// Plumb a successful rename/description straight into the local
		// session + sidebar (the server has no meta GET for others — see
		// applyGroupMeta), then let the window settle its own state.
		if m.err == nil {
			r.chat.applyGroupMeta(m.code, m.name, m.desc)
		}
		return r.forwardOverlay(m)
	case groupAdminsAppliedMsg:
		if m.roster != nil {
			if g := r.chat.groups[m.code]; g != nil && g.eng != nil {
				g.eng.applyPushedRoster(m.roster, m.epoch)
			}
			r.chat.syncRosterFromEngine()
		}
		return r.forwardOverlay(m)
	case groupDeletedMsg:
		if m.err != nil {
			return r.forwardOverlay(m)
		}
		r.overlay = nil
		r.chat.dissolveGroup(m.code, m.name)
		return r, tea.Batch(r.chat.drainNetCmd(), scheduleRoster())
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

// forwardOverlay hands a window-scoped result message to the open window
// (busy/notice settle); nil-safe when the window already closed.
func (r rootModel) forwardOverlay(msg tea.Msg) (tea.Model, tea.Cmd) {
	if r.overlay == nil {
		return r, nil
	}
	ov, cmd := r.overlay.Update(msg)
	r.overlay = ov
	return r, cmd
}

func (r rootModel) View() string {
	if r.overlay != nil {
		return r.overlay.View()
	}
	return r.chat.View()
}
