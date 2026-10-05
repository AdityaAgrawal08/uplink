package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// LIVE-REPRO regression tests (branch Groups) — the previous unit probes
// drove the model in RANKED space and missed the real interactions:
//
//  1. BUG 1 (command panel: "only the first item is ever selected"):
//     with an EMPTY "/" query the drawer PAINTS commands grouped under
//     category headers, but the arrow keys step the selection through the
//     alphabetically-ranked array. The second painted row of a group is
//     unreachable by arrow, Down jumps across groups, and Enter runs
//     whatever ranked slot the walk landed on. These tests assert in
//     PAINTED (display) space exactly like the user sees it.
//
//  2. BUG 2 (sidebar: "inside a group only General + that group show"):
//     openGroup swaps c.eng to the GROUP's engine and syncRosterFromEngine
//     then rebuilds c.users from the group's peer set, hiding main-room
//     users. The sidebar must always render the HOME roster + groups.
// ---------------------------------------------------------------------------

// displayItems returns the plan's item (ranked-index) sequence in the
// ORDER the drawer actually paints them — what the user sees.
func displayItems(plan drawerPlan) []int {
	var out []int
	for _, r := range plan.rows {
		if r.kind == drItem {
			out = append(out, r.item)
		}
	}
	return out
}

// TestPaletteDownFollowsPaintedGroupedOrder is the failing-first repro for
// the LIVE report: open the panel with "/", press Down 1-2 times, press
// Enter — the highlight must land on the second/third PAINTED row (not the
// second/third ranked slot) and Enter must RUN that command.
func TestPaletteDownFollowsPaintedGroupedOrder(t *testing.T) {
	c := auditScreen(t)
	c, _ = typeKeys(&c, "/")
	ranked := c.rankedCommands("/")
	plan := commandsPlan(ranked, len(ranked), true) // grouped: empty query
	disp := displayItems(plan)
	if len(disp) < 3 {
		t.Fatalf("grouped plan paints %d items; need >= 3 (%v)", len(disp), disp)
	}

	// (a) Down ONCE: the highlight must sit on the second painted row and
	// Enter must RUN that item — the user says it always stays on the
	// first/alphabetical slot instead.
	c, _ = step(&c, tea.KeyMsg{Type: tea.KeyDown})
	if c.palette.sel != disp[1] {
		t.Fatalf("down once: sel=%d (%s); want the second PAINTED item %d (%s); painted order %v",
			c.palette.sel, ranked[c.palette.sel].Name, disp[1], ranked[disp[1]].Name, disp)
	}
	want := ranked[disp[1]].Name
	got, _ := step(&c, tea.KeyMsg{Type: tea.KeyEnter})
	if got.palette.visible() {
		// TakesUser commands morph into the member stage instead of
		// firing with an empty argument — the highlight's command, never
		// the first one.
		if want != "/kick" && want != "/admin" && want != "/unadmin" {
			t.Fatalf("enter must close the drawer after picking %s", want)
		}
		if got.input.Value() != want+" " {
			t.Fatalf("enter must complete %s into the member picker, input=%q", want, got.input.Value())
		}
		if _, _, ok := got.paletteUsers(); !ok {
			t.Fatalf("enter on %s must open the member-picker stage", want)
		}
	} else {
		switch want {
		case "/kick":
			if !strings.Contains(got.status, "usage: /kick") {
				t.Fatalf("enter must RUN %s (the second painted item), status=%q", want, got.status)
			}
		case "/reply":
			if got.replyPick == nil {
				t.Fatalf("enter must RUN %s (the second painted item)", want)
			}
		}
	}

	// (a2) Down TWICE: the third PAINTED row, still sequential.
	c2 := auditScreen(t)
	c2, _ = typeKeys(&c2, "/")
	c2, _ = step(&c2, tea.KeyMsg{Type: tea.KeyDown})
	c2, _ = step(&c2, tea.KeyMsg{Type: tea.KeyDown})
	if c2.palette.sel != disp[2] {
		t.Fatalf("down twice: sel=%d (%s); want the third PAINTED item %d (%s)",
			c2.palette.sel, ranked[c2.palette.sel].Name, disp[2], ranked[disp[2]].Name)
	}
}

// TestPaletteMouseSecondRowGrouped locks the mouse parity on the GROUPED
// panel: hover-follow then click on the second painted row must select that
// row's item, and Enter must run it (the user's (c) repro path).
func TestPaletteMouseSecondRowGrouped(t *testing.T) {
	c := auditScreen(t)
	c, _ = typeKeys(&c, "/")
	ranked := c.rankedCommands("/")
	plan := commandsPlan(ranked, len(ranked), true)
	disp := displayItems(plan)
	if len(disp) < 2 {
		t.Fatalf("grouped plan paints %d items; need >= 2", len(disp))
	}
	l := c.layoutFor()
	y0, _ := c.drawerYRange(l)
	x := transcriptX0(l) + 2
	rows := c.palettePanelRows()
	secondRow := -1
	for i, r := range rows {
		if r.kind == drItem && r.item == disp[1] {
			secondRow = i
			break
		}
	}
	if secondRow < 0 {
		t.Fatalf("second painted item %d not in panel rows", disp[1])
	}
	motion := tea.MouseMsg{Type: tea.MouseLeft, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: x, Y: y0 + secondRow}
	c.handleMouse(motion) // first motion is consumed by the post-typing hover lock
	c.handleMouse(motion) // hover-follow selects the row under the cursor
	if c.palette.sel != disp[1] {
		t.Fatalf("hover second row: sel=%d (%s); want %d (%s)",
			c.palette.sel, ranked[c.palette.sel].Name, disp[1], ranked[disp[1]].Name)
	}
	c.handleMouse(mouseAt(x, y0+secondRow)) // click: selection stays on the row
	if c.palette.sel != disp[1] {
		t.Fatalf("click second row: sel=%d (%s); want %d (%s)",
			c.palette.sel, ranked[c.palette.sel].Name, disp[1], ranked[disp[1]].Name)
	}
	// Enter on the picked row: /kick (TakesUser, no arg) morphs into the
	// member stage with the command completed — the highlighted row's
	// command, never the first one.
	got, _ := step(&c, tea.KeyMsg{Type: tea.KeyEnter})
	if want := ranked[disp[1]].Name; want == "/kick" {
		if got.input.Value() != "/kick " {
			t.Fatalf("enter after mouse pick must complete %s into the member stage, input=%q", want, got.input.Value())
		}
		if _, _, ok := got.paletteUsers(); !ok {
			t.Fatal("enter after picking /kick must open the member-picker stage")
		}
	} else if got.palette.visible() {
		t.Fatal("enter must close the drawer after the mouse pick")
	}
}

// groupRosterNames extracts the sidebar names (General + users + groups)
// as the user sees them.
func groupRosterNames(c *chatScreen) []string {
	var out []string
	for _, it := range c.chatItems() {
		out = append(out, it.name)
	}
	return out
}

// TestSidebarFullRosterInsideGroup is the failing-first repro for the LIVE
// report: after entering a group the sidebar must STILL show General +
// every main-room user + every joined group; switching back to General
// must not change the list. Today the group view rebuilds the roster from
// the GROUP engine's peer set and main-room users vanish.
func TestSidebarFullRosterInsideGroup(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	t.Cleanup(srv.Close)
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(80, 24)
	c.id, _ = generateIdentity()
	c.groups = map[string]*groupSession{}
	c.lastGroupAt = map[string]time.Time{}
	wireTestEngine(t, c, srv, "bob", "alice", "carol", "dave")
	c.homeEng = c.eng
	c.eng.beatOnce()          // server heartbeat seeds presence (the live shape)
	c.syncRosterFromEngine()  // runChatTUI seeds users from the heartbeat
	if len(c.users) < 4 {
		t.Fatalf("fixture: home roster only %v; need bob + 3 users", c.users)
	}
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	c = &sc

	// The group session owns its own engine (the live shape), whose peer
	// set is the GROUP's membership — a SUBSET of the room (only alice is
	// in Design; carol/dave are room-only).
	gsig := &signalClient{serverURL: srv.URL, key: "740001", me: "bob", id: c.id}
	g := &groupSession{code: "740001", name: "Design", sig: gsig}
	g.eng = c.newSessionEngine(gsig)
	g.eng.applyPushedRoster([]rosterMember{{Username: "alice", Online: true}}, 1)
	c.groups["740001"] = g

	want := []string{"General", "alice", "carol", "dave", "Design"}
	check := func(t *testing.T, label string) {
		t.Helper()
		got := groupRosterNames(c)
		for _, w := range want {
			found := false
			for _, n := range got {
				if n == w {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s: sidebar is missing %q — got %v", label, w, got)
			}
		}
	}

	// Enter the group: the FULL list must stay (users must not collapse to
	// the group's own peer set).
	homeUsers := append([]string(nil), c.users...)
	c.openGroup("740001")
	if c.activeGroup != "740001" {
		t.Fatalf("fixture: openGroup failed, active=%q", c.activeGroup)
	}
	check(t, "inside Design")
	if len(c.users) != len(homeUsers) {
		t.Fatalf("inside Design: c.users = %v; want the full home roster %v", c.users, homeUsers)
	}

	// Back to General: the same stable list.
	c.exitGroup()
	if c.activeGroup != "" {
		t.Fatalf("fixture: exitGroup failed, active=%q", c.activeGroup)
	}
	check(t, "in General")
}

// TestSidebarClickEachRowInsideGroup drives the user's click flow FROM
// INSIDE the group: every painted row still opens its conversation.
func TestSidebarClickEachRowInsideGroup(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	t.Cleanup(srv.Close)
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(80, 24)
	c.id, _ = generateIdentity()
	c.groups = map[string]*groupSession{}
	c.lastGroupAt = map[string]time.Time{}
	wireTestEngine(t, c, srv, "bob", "alice", "carol", "dave")
	c.homeEng = c.eng
	c.eng.beatOnce()
	c.syncRosterFromEngine()
	m, _ := c.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	sc := m.(chatScreen)
	c = &sc
	gsig := &signalClient{serverURL: srv.URL, key: "740001", me: "bob", id: c.id}
	g := &groupSession{code: "740001", name: "Design", sig: gsig}
	g.eng = c.newSessionEngine(gsig)
	g.eng.applyPushedRoster([]rosterMember{{Username: "alice", Online: true}}, 1)
	c.groups["740001"] = g
	c.openGroup("740001")
	c.width, c.height = 120, 40
	l := c.layoutFor()

	clickRow := func(t *testing.T, name string) {
		t.Helper()
		items := c.chatItems()
		idx := -1
		for i, it := range items {
			if it.name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("row %q missing from the sidebar: %v", name, groupRosterNames(c))
		}
		c.handleMouse(mouseAt(l.rosterX+5, l.rosterY0+idx*c.chatItemHeight()+1))
	}

	// General row: back to the room.
	clickRow(t, "General")
	if c.activeGroup != "" || c.targetUser != "" {
		t.Fatalf("click General: active=%q target=%q; want the room view", c.activeGroup, c.targetUser)
	}
	// Design row: back into the group.
	clickRow(t, "Design")
	if c.activeGroup != "740001" {
		t.Fatalf("click Design: active=%q; want 740001", c.activeGroup)
	}
	// alice row: private thread with alice.
	clickRow(t, "alice")
	if c.targetUser != "alice" {
		t.Fatalf("click alice: target=%q; want alice", c.targetUser)
	}
	// carol row: private thread with carol (users stay reachable from a
	// group view — the reported vanish case).
	clickRow(t, "carol")
	if c.targetUser != "carol" {
		t.Fatalf("click carol: target=%q; want carol", c.targetUser)
	}
}

// TestSyncRosterKeepsHomeUsersInsideGroup pins the roster-sync contract at
// the source: while a group view is open, syncRosterFromEngine must feed
// the sidebar from the HOME engine, never from the group's peer set.
func TestSyncRosterKeepsHomeUsersInsideGroup(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	t.Cleanup(srv.Close)
	c := newFilterScreen("bob", "")
	c.id, _ = generateIdentity()
	c.groups = map[string]*groupSession{}
	c.lastGroupAt = map[string]time.Time{}
	wireTestEngine(t, c, srv, "bob", "alice", "carol", "dave")
	c.homeEng = c.eng
	c.eng.beatOnce()
	c.syncRosterFromEngine()
	home := append([]string(nil), c.users...)
	if len(home) != 4 {
		t.Fatalf("fixture: home roster %v", home)
	}

	gsig := &signalClient{serverURL: srv.URL, key: "740001", me: "bob", id: c.id}
	g := &groupSession{code: "740001", name: "Design", sig: gsig}
	g.eng = c.newSessionEngine(gsig)
	g.eng.applyPushedRoster([]rosterMember{{Username: "alice", Online: true}}, 1) // group sees only alice
	c.groups["740001"] = g

	c.openGroup("740001") // c.eng is now the GROUP engine
	c.syncRosterFromEngine()
	if len(c.users) != len(home) {
		t.Fatalf("sync inside the group shrank the sidebar roster: users=%v; want %v", c.users, home)
	}
	for i := range home {
		if c.users[i] != home[i] {
			t.Fatalf("sync inside the group replaced the roster: users=%v; want %v", c.users, home)
		}
	}
	// The group's own peer set must not leak into the sidebar either.
	for _, it := range c.chatItems() {
		if it.isGroup && it.name == "Design" {
			continue
		}
		if it.peer == "alice" || it.peer == "carol" || it.peer == "dave" {
			continue
		}
		if it.peer != "" && it.peer != c.me {
			t.Fatalf("unexpected sidebar row %q after group sync", it.peer)
		}
	}
}