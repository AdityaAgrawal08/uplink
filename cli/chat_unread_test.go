package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ---------------------------------------------------------------------------
// Recency ordering algorithm (pure) — newest DM sender rises, others sink
// ---------------------------------------------------------------------------

func TestOrderedUsersRecency(t *testing.T) {
	now := time.Now()
	me := "me"

	t.Run("no DMs preserves roster order with me pinned first", func(t *testing.T) {
		got := orderedUsers([]string{me, "zoe", "adam"}, me, map[string]time.Time{})
		want := []string{me, "zoe", "adam"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("newest sender tops the peers", func(t *testing.T) {
		lm := map[string]time.Time{
			"bob":   now.Add(-10 * time.Minute),
			"carol": now.Add(-1 * time.Minute), // newest
			"adam":  now.Add(-30 * time.Minute),
		}
		got := orderedUsers([]string{me, "bob", "carol", "adam", "dave"}, me, lm)
		want := []string{me, "carol", "bob", "adam", "dave"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("never-messaged peers keep roster order below messaged ones", func(t *testing.T) {
		lm := map[string]time.Time{"dave": now}
		got := orderedUsers([]string{me, "zed", "yara", "dave"}, me, lm)
		want := []string{me, "dave", "zed", "yara"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v want %v", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// Unread lifecycle through the real addMessage path
// ---------------------------------------------------------------------------

func TestUnreadLifecycle(t *testing.T) {
	c := newFilterScreen("p1", "", "p1", "alice", "bob")
	c.vp = *viewportPtr(60, 20)
	dm := func(seq int, from, text string) chatMessage {
		return chatMessage{Seq: seq, Username: from, Kind: "chat", Text: text,
			To: "p1", ConvID: conversationKey("p1", from)}
	}

	// Arrivals while sitting in GENERAL accumulate per-peer.
	c.addMessage(dm(1, "alice", "hey"))
	c.addMessage(dm(2, "alice", "you there"))
	c.addMessage(dm(3, "bob", "yo"))
	if c.unread["alice"] != 2 || c.unread["bob"] != 1 {
		t.Fatalf("unread = %v", c.unread)
	}

	// Opening the thread clears ONLY its badge.
	c.enterPrivate("alice")
	if n := c.unread["alice"]; n != 0 {
		t.Fatalf("badge not cleared on open: %d", n)
	}
	if c.unread["bob"] != 1 {
		t.Fatalf("unrelated badge disturbed: %v", c.unread)
	}

	// While INSIDE alice's thread her messages must not re-count…
	before := len(c.history)
	c.addMessage(dm(4, "alice", "still here"))
	if c.unread["alice"] != 0 {
		t.Fatal("active thread counted as unread")
	}
	if len(c.history) != before+1 {
		t.Fatal("active-thread message not stored")
	}
	// …but recency still bumps so she stays pinned near the top.
	if _, ok := c.lastDMAt["alice"]; !ok {
		t.Fatal("recency not bumped for viewed thread")
	}

	// Own outgoing copies never count.
	mine := dm(5, "p1", "my side")
	c.addMessage(mine)
	if c.unread["p1"] != 0 {
		t.Fatal("self message counted as unread")
	}
}

func TestSidebarRendersAndClearsBadge(t *testing.T) {
	c := newFilterScreen("p1", "", "p1", "alice")
	c.width, c.height = 100, 30
	c.vp = *viewportPtr(60, 16)

	c.addMessage(chatMessage{Seq: 1, Username: "alice", Kind: "chat", Text: "a",
		To: "p1", ConvID: conversationKey("p1", "alice")})
	c.addMessage(chatMessage{Seq: 2, Username: "alice", Kind: "chat", Text: "b",
		To: "p1", ConvID: conversationKey("p1", "alice")})

	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)

	// Package-level styles were built before the profile was forced; use a
	// fresh style so the SGR assertion sees the forced ANSI256 profile.
	fresh := lipgloss.NewStyle().
		Foreground(lipgloss.Color("15")).
		Background(lipgloss.Color("27"))
	chip := fresh.Render(circledNum(2))
	out := c.rosterBody(computeLayout(100, 30, false).rosterSlots)
	plain := stripANSI(out)
	if !strings.Contains(plain, "alice") {
		t.Fatalf("alice missing from chat list:\n%s", plain)
	}
	// The unread dot paints on alice's item (blue ● beside her rows).
	if !strings.Contains(plain, "●") {
		t.Fatalf("unread dot missing from chat list:\n%s", plain)
	}
	// lipgloss may emit 256-colour (48;5;27) or compact 4-bit (44) blue bg;
	// accept either, but REQUIRE white fg + SOME blue background.
	hasWhiteFg := strings.Contains(chip, "38;5;15") || strings.Contains(chip, "97")
	hasBlueBg := strings.Contains(chip, "48;5;27") || strings.Contains(chip, ";44m") || strings.Contains(chip, "[44;")
	if !hasWhiteFg || !hasBlueBg {
		t.Fatalf("chip colours wrong (%q): whiteFg=%v blueBg=%v", stripANSIRaw(chip), hasWhiteFg, hasBlueBg)
	}

	c.enterPrivate("alice")
	out = c.rosterBody(computeLayout(100, 30, false).rosterSlots)
	if strings.Contains(out, chip) {
		t.Fatalf("badge survived open:\n%s", stripANSI(out))
	}

	// Saturation guard: >50 collapses onto ㊿.
	if got := circledNum(51); got != string(rune(0x32BF)) {
		t.Errorf("circledNum(51) = %q", got)
	}
	if got := circledNum(1); got != "①" || got == circledNum(3) {
		t.Errorf("circledNum basics broken: %q", got)
	}
}

// stripANSI removes SGR sequences so width/position checks see plain text.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestBeatPrunesDepartedPeers(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("me", "", "me")
	wireTestEngine(t, c, srv, "me", "alice")
	now := time.Now()
	c.unread = map[string]int{"ghost": 4}
	c.lastDMAt = map[string]time.Time{"ghost": now}

	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	scr := m.(chatScreen)
	// Pull the heartbeat roster (me+alice on the fake server): ghost never
	// existed server-side, so the tick must prune ghost entries.
	scr.eng.beatOnce()
	nm, _ := scr.Update(rosterTickMsg{})
	got := nm.(chatScreen)

	if len(got.users) != 2 {
		t.Fatalf("roster = %v; want [me alice]", got.users)
	}

	if _, ok := got.unread["ghost"]; ok {
		t.Error("unread entry for departed peer not pruned")
	}
	if _, ok := got.lastDMAt["ghost"]; ok {
		t.Error("recency entry for departed peer not pruned")
	}
}

// Mouse mapping must agree with the REORDERED display list.
func TestMouseFollowsRecencyOrder(t *testing.T) {
	const W, H = 110, 34
	now := time.Now()
	c := newFilterScreen("me", "", "me", "zed", "ana")
	c.lastDMAt["ana"] = now // ana should surface above zed
	c.width, c.height = W, H
	c.vp = *viewportPtr(60, 16)

	l := c.layoutFor()
	// Items: General at +0/+1, then recency-ordered ana at +2/+3.
	c.handleMouse(mouseAt(l.rosterX+5, l.rosterY0+2)) // first peer row
	if c.targetUser != "ana" {
		t.Fatalf("clicked row selected %q; want ana (recency-ordered)", c.targetUser)
	}
}
