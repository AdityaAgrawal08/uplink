package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// joinFixture is a minimal fake of POST /api/v1/session/{code}/join with the
// server's two-step password semantics: no password -> 401 "Password is
// required for this session", wrong password -> 401 "Incorrect session
// password", right/absent password -> 200. Modes cover the inline-error
// paths (404/409/unknown 401).
type joinFixture struct {
	mu       sync.Mutex
	required bool   // room is password-protected
	password string // correct password when required
	mode     string // "", "missing", "taken", "odd401"
	joins    []map[string]any
}

func newJoinFixture() *joinFixture {
	return &joinFixture{}
}

func (f *joinFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.joins = append(f.joins, body)
	writeErr := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	writeOK := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"sessionId": "123456", "participants": []string{}})
	}
	switch f.mode {
	case "missing":
		writeErr(http.StatusNotFound, "Session not found")
		return
	case "taken":
		writeErr(http.StatusConflict, "already in session")
		return
	case "odd401":
		writeErr(http.StatusUnauthorized, "mysterious body")
		return
	}
	if !f.required {
		writeOK()
		return
	}
	pw, has := body["password"].(string)
	if !has || pw == "" {
		writeErr(http.StatusUnauthorized, "Password is required for this session")
		return
	}
	if pw != f.password {
		writeErr(http.StatusUnauthorized, "Incorrect session password")
		return
	}
	writeOK()
}

// newJoinTestModel builds a JOIN-tab landing model pointed at the fixture:
// identity key isolated in a temp HOME, username + code prefilled.
func newJoinTestModel(t *testing.T, srv *httptest.Server) landingModel {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := newLandingModel(srv.URL)
	m.tab = tabJoin
	m.userInput.SetValue("alice")
	m.codeInput.SetValue("123456")
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return nm.(landingModel)
}

func runCmd(cmd tea.Cmd) tea.Msg {
	return cmd()
}

// The JOIN tab is a two-field form (username + code); the password row
// exists only on CREATE (and inside the step-2 modal).
func TestLandingJoinFormHasNoPasswordField(t *testing.T) {
	m := newLandingModel("http://localhost:3000")
	m.tab = tabJoin
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = nm.(landingModel)
	view := m.View()
	if strings.Contains(view, "Password :") {
		t.Error("JOIN tab must not render a password field")
	}
	if !strings.Contains(view, "UserName :") || !strings.Contains(view, "Code     :") {
		t.Error("JOIN tab must render username and code fields")
	}
	// create keeps its optional password field
	m.tab = tabCreate
	view = m.View()
	if !strings.Contains(view, "Password :") {
		t.Error("CREATE tab must keep its optional password field")
	}
	if strings.Contains(view, "Code     :") {
		t.Error("CREATE tab must not render the code field")
	}
}

// Tab-cycle on the JOIN tab skips the password field: user -> code -> submit,
// and Enter advances along the same two-field path.
func TestLandingJoinFocusCycleSkipsPassword(t *testing.T) {
	m := testLandingModel(tabJoin, "alice", "", "123456")
	for _, want := range []landingFocus{focusCode, focusSubmit, focusUser} {
		m.focusNext()
		if m.focus != want {
			t.Fatalf("focusNext: want %v, got %v", want, m.focus)
		}
	}
	m.focus = focusUser
	for _, want := range []landingFocus{focusSubmit, focusCode, focusUser} {
		m.focusPrev()
		if m.focus != want {
			t.Fatalf("focusPrev: want %v, got %v", want, m.focus)
		}
	}
	m.focus = focusUser
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := nm.(landingModel)
	if mm.focus != focusCode {
		t.Fatalf("Enter from user: want focusCode, got %v", mm.focus)
	}
	nm, _ = mm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm = nm.(landingModel)
	if mm.focus != focusSubmit {
		t.Fatalf("Enter from code: want focusSubmit, got %v", mm.focus)
	}
	// switching to JOIN parks a focusPass (only reachable on CREATE) on user
	cm := testLandingModel(tabCreate, "alice", "", "123456")
	cm.focus = focusPass
	cm.tab = tabJoin
	cm.parkFocusForTab()
	if cm.focus != focusUser {
		t.Fatalf("parkFocusForTab: want focusUser, got %v", cm.focus)
	}
}

// A room without a password joins straight from the two-field form, and the
// request carries no password payload key even if the (hidden-to-JOIN)
// password input held one.
func TestLandingJoinNoPasswordRoomJoins(t *testing.T) {
	f := newJoinFixture()
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := newJoinTestModel(t, srv)
	m.passInput.SetValue("sneaky") // must be ignored on the JOIN tab
	msg := runCmd(m.submit())
	ok, isOK := msg.(landingJoinOkMsg)
	if !isOK {
		t.Fatalf("want landingJoinOkMsg, got %#v", msg)
	}
	if ok.username != "alice" || ok.code != "123456" || ok.password != "" {
		t.Fatalf("join result mismatch: %+v", ok)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.joins) != 1 {
		t.Fatalf("want 1 join request, got %d", len(f.joins))
	}
	if _, has := f.joins[0]["password"]; has {
		t.Error("step-1 join must not send a password payload key")
	}
}

// A protected session answers 401 "Password is required": the form opens the
// modal with the masked input focused and the join form untouched beneath.
func TestLandingJoinProtectedOpensModal(t *testing.T) {
	f := newJoinFixture()
	f.required = true
	f.password = "s3cret"
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := newJoinTestModel(t, srv)
	msg := runCmd(m.submit())
	dm, isDone := msg.(landingDoneMsg)
	if !isDone || !dm.needPass || dm.err != "" {
		t.Fatalf("want needPass landingDoneMsg, got %#v", msg)
	}
	nm, _ := m.Update(dm)
	m = nm.(landingModel)
	if !m.passModal {
		t.Fatal("modal must open on 401 password-required")
	}
	if m.focus != focusPass {
		t.Fatalf("modal must focus the password input, got %v", m.focus)
	}
	view := m.View()
	if !strings.Contains(view, "SESSION PASSWORD") {
		t.Error("modal not rendered in view")
	}
	if strings.Contains(view, "Password :") {
		t.Error("join form must not show a password row even behind the modal")
	}
	if m.userInput.Value() != "alice" || m.codeInput.Value() != "123456" {
		t.Fatal("form inputs must survive opening the modal")
	}
}

// Wrong password keeps the modal open with an inline error; the next attempt
// with the right password joins, closes the modal and lands in chat with the
// password carried for the engine's self-rejoin.
func TestLandingJoinWrongPasswordThenRetrySucceeds(t *testing.T) {
	f := newJoinFixture()
	f.required = true
	f.password = "s3cret"
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := newJoinTestModel(t, srv)
	// step 1: open the modal
	nm, _ := m.Update(runCmd(m.submit()))
	m = nm.(landingModel)
	if !m.passModal {
		t.Fatal("modal should be open")
	}
	// step 2: wrong password stays in the modal
	m.passInput.SetValue("nope")
	msg := runCmd(m.submitPassword())
	dm, isDone := msg.(landingDoneMsg)
	if !isDone || !dm.modalErr || dm.err != "incorrect password" {
		t.Fatalf("want modalErr landingDoneMsg, got %#v", msg)
	}
	nm, _ = m.Update(dm)
	m = nm.(landingModel)
	if !m.passModal {
		t.Fatal("modal must stay open after an incorrect password")
	}
	if m.passErr != "incorrect password" {
		t.Fatalf("modal error = %q, want %q", m.passErr, "incorrect password")
	}
	if m.errMsg != "" {
		t.Fatalf("modal errors must not leak into the form error line: %q", m.errMsg)
	}
	if !strings.Contains(m.View(), "SESSION PASSWORD") {
		t.Error("modal must still render after an incorrect password")
	}
	// step 3: correct password joins and lands in chat
	m.passInput.SetValue("s3cret")
	msg = runCmd(m.submitPassword())
	jok, isOK := msg.(landingJoinOkMsg)
	if !isOK || jok.password != "s3cret" {
		t.Fatalf("want landingJoinOkMsg with password, got %#v", msg)
	}
	nm, _ = m.Update(jok)
	m = nm.(landingModel)
	if m.passModal {
		t.Fatal("modal must close on successful join")
	}
	if m.result == nil || m.result.Password != "s3cret" {
		t.Fatalf("landing result must carry the password for eng self-rejoin: %+v", m.result)
	}
}

// Esc closes the modal back to the form: username+code preserved, password
// cleared, no modal in the view; re-submitting reopens it.
func TestLandingJoinModalEscPreservesInputs(t *testing.T) {
	f := newJoinFixture()
	f.required = true
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := newJoinTestModel(t, srv)
	nm, _ := m.Update(runCmd(m.submit()))
	m = nm.(landingModel)
	if !m.passModal {
		t.Fatal("modal should be open")
	}
	m.passInput.SetValue("typed-secret")
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(landingModel)
	if m.passModal {
		t.Fatal("Esc must close the modal")
	}
	if m.passInput.Value() != "" {
		t.Fatalf("Esc must clear the typed password, got %q", m.passInput.Value())
	}
	if m.userInput.Value() != "alice" || m.codeInput.Value() != "123456" {
		t.Fatal("Esc must preserve username and code")
	}
	if strings.Contains(m.View(), "SESSION PASSWORD") {
		t.Error("modal must not render after Esc")
	}
	// re-submitting from the form opens the modal again (password cleared)
	nm, _ = m.Update(runCmd(m.submit()))
	m = nm.(landingModel)
	if !m.passModal {
		t.Fatal("re-submit must reopen the modal for a protected session")
	}
}

// 404 / 409 / unknown-401 keep today's inline form errors; no modal opens.
func TestLandingJoinInlineErrorsUnchanged(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"missing", "session not found"},
		{"taken", "already in this session"},
		{"odd401", "password required or incorrect"},
	}
	for _, tc := range cases {
		f := newJoinFixture()
		f.mode = tc.mode
		srv := httptest.NewServer(f)
		m := newJoinTestModel(t, srv)
		msg := runCmd(m.submit())
		dm, isDone := msg.(landingDoneMsg)
		if !isDone || dm.needPass || dm.modalErr {
			t.Fatalf("[%s] want plain error landingDoneMsg, got %#v", tc.mode, msg)
		}
		if !strings.Contains(dm.err, tc.want) {
			t.Fatalf("[%s] err = %q, want substring %q", tc.mode, dm.err, tc.want)
		}
		nm, _ := m.Update(dm)
		m = nm.(landingModel)
		if m.passModal {
			t.Fatalf("[%s] inline error must not open the modal", tc.mode)
		}
		if m.errMsg != dm.err {
			t.Fatalf("[%s] form error line not set", tc.mode)
		}
		srv.Close()
	}
}

// A second-step join that 404s (the room vanished while the password was
// being typed) closes the modal and surfaces the form error inline.
func TestLandingJoinModalRetry404ClosesToForm(t *testing.T) {
	f := newJoinFixture()
	f.required = true
	f.password = "s3cret"
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := newJoinTestModel(t, srv)
	nm, _ := m.Update(runCmd(m.submit()))
	m = nm.(landingModel)
	if !m.passModal {
		t.Fatal("modal should be open")
	}
	// the room dies mid-flow: the fixture stops answering joins
	f.mu.Lock()
	f.mode = "missing"
	f.mu.Unlock()
	m.passInput.SetValue("s3cret")
	msg := runCmd(m.submitPassword())
	dm, isDone := msg.(landingDoneMsg)
	if !isDone || dm.modalErr || dm.needPass {
		t.Fatalf("want plain error, got %#v", msg)
	}
	nm, _ = m.Update(dm)
	m = nm.(landingModel)
	if m.passModal {
		t.Fatal("modal must close on non-401 retry failure")
	}
	if !strings.Contains(m.errMsg, "session not found") {
		t.Fatalf("form error = %q", m.errMsg)
	}
	if m.passInput.Value() != "" {
		t.Fatal("password must clear when the modal closes")
	}
}