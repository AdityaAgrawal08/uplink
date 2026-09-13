package main

import (
	"testing"
)

func testLandingModel(tab landingTab, user, pass, code string) *landingModel {
	m := newLandingModel("http://localhost:3000")
	m.tab = tab
	m.userInput.SetValue(user)
	m.passInput.SetValue(pass)
	m.codeInput.SetValue(code)
	return &m
}

// Session codes are always exactly 6 digits: anything else is a typo and
// must be rejected locally, never sent as a doomed join that 404s with a
// misleading "session not found".
func TestLandingValidateCodeStrict(t *testing.T) {
	cases := []struct {
		code string
		ok   bool
	}{
		{"123456", true},
		{"042917", true}, // leading zero is significant
		{"", false},
		{"12345", false}, // the old len>=4 fallback admitted this
		// NOTE: the input widget truncates to CharLimit=6 before validation
		// runs. "1234567" therefore arrives as the valid "123456"
		// (accepted); " 123456 " arrives as " 12345" and fails closed.
		// Either way, what you see in the box is what gets validated.
		{"1234567", true},
		{"abcdef", false},
		{" 123456 ", false},
	}
	for _, tc := range cases {
		m := testLandingModel(tabJoin, "alice", "", tc.code)
		err := m.validate()
		if tc.ok && err != "" {
			t.Errorf("code %q rejected: %s", tc.code, err)
		}
		if !tc.ok && err == "" {
			t.Errorf("code %q accepted, want rejection", tc.code)
		}
	}
}

func TestLandingValidateUsername(t *testing.T) {
	m := testLandingModel(tabJoin, "ab", "", "123456")
	if err := m.validate(); err == "" {
		t.Error("short username accepted")
	}
	m = testLandingModel(tabJoin, "this_username_is_way_too_long", "", "123456")
	// Overlong typing never reaches validation: the widget truncates to
	// CharLimit (20 = server max), so the submitted value stays valid.
	if got := m.userInput.Value(); len(got) > 20 {
		t.Errorf("widget did not truncate: %q", got)
	}
	if err := m.validate(); err != "" {
		t.Errorf("truncated username rejected: %s", err)
	}
	m = testLandingModel(tabCreate, "alice_42", "", "")
	if err := m.validate(); err != "" {
		t.Errorf("valid create rejected: %s", err)
	}
}
