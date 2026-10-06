package main

// chat_sidebar_live_test.go — LIVE sidebar-update regression tests (branch
// Groups). The unit probes step the models in ranked space and miss the
// REAL wiring, so these drive model.Update through ACTUAL tick messages —
// real engine beats against the fake server, the real pushed net* events
// off the wire channel (exactly what drainNetCmd feeds Update), the real
// roster tick, and assertions on the PAINTED sidebar rows (chatItems +
// rosterBody), never on intermediate primed state:
//
//  1. users joining/leaving the home room move their sidebar row in/out on
//     the roster cadence (netRosterMsg + rosterTickMsg);
//  2. a group created by me paints its sidebar row and keeps it after
//     returning to the common room;
//  3. a group the server FORGETS (24h TTL sweep / redis flush / last-one-
//     out destroy) leaves the sidebar — the engine's ghost-room 401 gets
//     rejoin-classified, the rejoin's 404 "Session not found" is
//     recognized as the room being GONE, and "session ended" drops the
//     row instead of rotting behind "rejoin failed" forever;
//  4. accepting an invite adds the row; a later re-invite into a group the
//     client already dropped must NOT resurrect a dead row (the row only
//     returns through a fresh accept/attach).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sidebarNames(c *chatScreen) []string {
	var out []string
	for _, it := range c.chatItems() {
		out = append(out, it.name)
	}
	return out
}

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// drainEndEvent pumps the screen's wire channel like drainNetCmd does and
// feeds every message into Update until the group-end event actually
// LANDS — the netErrMsg the beat's rejoin-classification pushes when a
// dissolved room answers 404 (emitEndedOnce). The seeding beats push
// netRosterMsg ahead of it, and a transient channel-open failure can
// interleave FIRST (a beat that learned a peer before the prune can land
// its note after it, surfacing a raw 401), so stopping at the first
// netErrMsg would return before the row dropped. Returns the settled
// rootModel.
func drainEndEvent(t *testing.T, r rootModel) rootModel {
	t.Helper()
	for i := 0; i < 1000; i++ {
		msg := <-r.chat.netCh
		rn, _ := r.Update(msg)
		r = rn.(rootModel)
		em, ok := msg.(netErrMsg)
		if !ok {
			continue
		}
		if em.key == r.chat.key {
			continue // home-session errors never end a group
		}
		if _, ok := groupEndEvent(em.err); ok {
			return r
		}
	}
	t.Fatal("drainEndEvent: group-end event never landed")
	return r
}

// TestSidebarLiveUserJoinLeave drives the REAL join/leave cadence: a peer
// joins the home room (real HTTP), the engine's next beat sees her, the
// pushed roster event and the 2s tick re-render — her sidebar row appears;
// she leaves — the row disappears. Assertions run on the painted sidebar.
func TestSidebarLiveUserJoinLeave(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	aliceID, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	if _, err := home.createRoom("alice", pubkeyB64(aliceID), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"
	for _, u := range []string{"carol", "dave"} {
		uid, _ := generateIdentity()
		usig := &signalClient{serverURL: srv.URL, key: "123456", me: u, id: uid}
		if _, _, err := usig.joinRoom(u, pubkeyB64(uid), ""); err != nil {
			t.Fatal(err)
		}
	}

	scr := newChatScreen(srv.URL, "123456", "alice", aliceID, "")
	r := newRootModel(scr)
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)

	// REAL seed: the engine's beat learns the roster, the roster tick
	// renders it.
	r.chat.homeEng.beatOnce()
	rn, _ = r.Update(rosterTickMsg{})
	r = rn.(rootModel)
	if !inList(r.chat.users, "carol") || !inList(r.chat.users, "dave") {
		t.Fatalf("seed: users=%v want carol+dave", r.chat.users)
	}

	// ── eve JOINS the home room (real HTTP join) ──
	eveID, _ := generateIdentity()
	esig := &signalClient{serverURL: srv.URL, key: "123456", me: "eve", id: eveID}
	if _, _, err := esig.joinRoom("eve", pubkeyB64(eveID), ""); err != nil {
		t.Fatal(err)
	}
	// The engine's beat sees the new member and PUSHES the roster event;
	// Update handles it through the real message type.
	r.chat.homeEng.beatOnce()
	rn, _ = r.Update(netRosterMsg{})
	r = rn.(rootModel)
	if !inList(r.chat.users, "eve") {
		t.Fatalf("after join: users=%v want eve (netRosterMsg path)", r.chat.users)
	}
	if !inList(sidebarNames(&r.chat), "eve") {
		t.Fatalf("after join: sidebar rows=%v want eve", sidebarNames(&r.chat))
	}
	if !strings.Contains(r.chat.rosterBody(r.chat.sidebarFill(r.chat.layoutFor())), "eve") {
		t.Fatal("the PAINTED sidebar must show eve after her join")
	}

	// ── eve LEAVES the home room (real HTTP leave) ──
	if err := esig.leaveRoom(); err != nil {
		t.Fatal(err)
	}
	r.chat.homeEng.beatOnce()
	rn, _ = r.Update(rosterTickMsg{}) // the 2s tick cadence
	r = rn.(rootModel)
	if inList(r.chat.users, "eve") {
		t.Fatalf("after leave: users=%v want eve GONE", r.chat.users)
	}
	if inList(sidebarNames(&r.chat), "eve") {
		t.Fatalf("after leave: sidebar rows=%v want eve GONE", sidebarNames(&r.chat))
	}
	if strings.Contains(r.chat.rosterBody(r.chat.sidebarFill(r.chat.layoutFor())), "eve") {
		t.Fatal("the PAINTED sidebar must drop eve after her leave")
	}
}

// TestSidebarLiveCreatePaintsRow drives the REAL /new-group completion:
// the createGroupDoneMsg lands through rootModel, the group opens, and its
// sidebar row stays after Esc returns to the common room.
func TestSidebarLiveCreatePaintsRow(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	aliceID, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	if _, err := home.createRoom("alice", pubkeyB64(aliceID), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"

	scr := newChatScreen(srv.URL, "123456", "alice", aliceID, "")
	r := newRootModel(scr)
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)

	// The /new-group form mints a fresh key-less client (groupModel.submit
	// does exactly this) and the done message arrives through rootModel.
	formSig := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	code, err := formSig.createGroupRoom("alice", pubkeyB64(aliceID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	rn, _ = r.Update(createGroupDoneMsg{code: code, name: "Design"})
	r = rn.(rootModel)
	if r.chat.activeGroup != code {
		t.Fatalf("created group must open: active=%q", r.chat.activeGroup)
	}
	if !inList(sidebarNames(&r.chat), "Design") {
		t.Fatalf("inside the group the sidebar must show it: %v", sidebarNames(&r.chat))
	}
	// Esc back to the common room: the joined group row must REMAIN.
	r.chat.exitGroup()
	if !inList(sidebarNames(&r.chat), "Design") {
		t.Fatalf("back home the group row must stay: %v", sidebarNames(&r.chat))
	}
	if !strings.Contains(r.chat.rosterBody(r.chat.sidebarFill(r.chat.layoutFor())), "Design") {
		t.Fatal("the PAINTED sidebar must keep the created group row")
	}
}

// TestSidebarLiveGroupDissolveDropsRow is the failing-first repro for the
// dead-row bug: the server FORGETS a group (24h TTL sweep, redis flush,
// last-one-out destroy). The background engine's next beat answers the
// ghost-room 401, the rejoin answers 404 "Session not found" — and the
// engine must classify that as "session ended" so the row drops. Before
// the fix the rejoin failure surfaced as "rejoin failed" and the dead
// group sat in the sidebar forever.
func TestSidebarLiveGroupDissolveDropsRow(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	aliceID, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	if _, err := home.createRoom("alice", pubkeyB64(aliceID), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"

	asig := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	code, err := asig.createGroupRoom("alice", pubkeyB64(aliceID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	bobID, _ := generateIdentity()
	bsig := &signalClient{serverURL: srv.URL, key: code, me: "bob", id: bobID}
	if _, _, err := bsig.joinRoom("bob", pubkeyB64(bobID), ""); err != nil {
		t.Fatal(err)
	}

	scr := newChatScreen(srv.URL, "123456", "alice", aliceID, "")
	r := newRootModel(scr)
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)

	gsig := &signalClient{serverURL: srv.URL, key: code, me: "alice", id: aliceID}
	r.chat.attachGroup(code, "Design", "", gsig, "")
	r.chat.groups[code].eng.beatOnce() // seed group presence
	rn, _ = r.Update(rosterTickMsg{})
	r = rn.(rootModel)
	if !inList(sidebarNames(&r.chat), "Design") {
		t.Fatalf("fixture: group row must paint: %v", sidebarNames(&r.chat))
	}

	// ── the server FORGETS the room (TTL expiry / restart / last-out) ──
	fake.mu.Lock()
	delete(fake.members, code)
	delete(fake.groupMeta, code)
	fake.mu.Unlock()

	// The next beat hits the ghost-room 401 -> rejoin -> 404 "Session not
	// found" -> "session ended" -> the pushed netErrMsg. Drain the wire
	// exactly like the app's event pump.
	r.chat.groups[code].eng.beatOnce()
	r = drainEndEvent(t, r)
	if _, live := r.chat.groups[code]; live {
		t.Fatal("dissolved group must leave the live set")
	}
	// The agreed tombstone contract: the dead group keeps a visible
	// placeholder row (so the end is acknowledged), never a live group row.
	found := false
	for _, it := range r.chat.chatItems() {
		if it.isGroup && it.tombstone && it.name == "Design" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dissolved group must leave a tombstone row: %v", sidebarNames(&r.chat))
	}
	if !strings.Contains(r.chat.status, "Design") || !strings.Contains(r.chat.status, "ended") {
		t.Fatalf("the drop must name the ended group on the status line: %q", r.chat.status)
	}
	// A later tick must not resurrect a LIVE row either (no dead sessions).
	rn, _ = r.Update(rosterTickMsg{})
	r = rn.(rootModel)
	if _, live := r.chat.groups[code]; live {
		t.Fatalf("a later roster tick must not resurrect the dead session: %v", sidebarNames(&r.chat))
	}
}

// TestSidebarLiveGroupDissolveActiveView: the same destroy while the group
// is the OPEN conversation returns the user to the common room and drops
// the row.
func TestSidebarLiveGroupDissolveActiveView(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	aliceID, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	if _, err := home.createRoom("alice", pubkeyB64(aliceID), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"

	asig := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	code, err := asig.createGroupRoom("alice", pubkeyB64(aliceID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}

	scr := newChatScreen(srv.URL, "123456", "alice", aliceID, "")
	r := newRootModel(scr)
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)

	gsig := &signalClient{serverURL: srv.URL, key: code, me: "alice", id: aliceID}
	r.chat.attachGroup(code, "Design", "", gsig, "")
	r.chat.openGroup(code)
	if r.chat.activeGroup != code {
		t.Fatalf("fixture: group view must be open")
	}

	fake.mu.Lock()
	delete(fake.members, code)
	delete(fake.groupMeta, code)
	fake.mu.Unlock()

	r.chat.groups[code].eng.beatOnce()
	r = drainEndEvent(t, r)
	if r.chat.activeGroup != "" {
		t.Fatalf("the destroyed open group must return to the common room, active=%q", r.chat.activeGroup)
	}
	if _, live := r.chat.groups[code]; live {
		t.Fatalf("the destroyed group must leave the live set: %v", sidebarNames(&r.chat))
	}
	if tmb, ok := r.chat.tombstones[code]; !ok || tmb.name != "Design" {
		t.Fatalf("the destroyed group must leave a tombstone: %+v", r.chat.tombstones)
	}
}

// TestSidebarLiveAcceptAddsRowAndReinviteKeepsDead: accepting an invite
// (the REAL accept HTTP path, then acceptGroupDoneMsg) paints the group
// row; when the server later forgets the group the row drops; a fresh
// re-invite into a new room with the same code must badge the inbox but
// must NOT resurrect the dropped row — the sidebar row only returns
// through a fresh accept/attach.
func TestSidebarLiveAcceptAddsRowAndReinviteKeepsDead(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	aliceID, _ := generateIdentity()
	bobID, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	if _, err := home.createRoom("alice", pubkeyB64(aliceID), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"

	asig := &signalClient{serverURL: srv.URL, me: "alice", id: aliceID}
	code, err := asig.createGroupRoom("alice", pubkeyB64(aliceID), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := asig.sendInvite("bob"); err != nil {
		t.Fatal(err)
	}

	// bob's screen: home room + the pending invite.
	bhome := &signalClient{serverURL: srv.URL, key: "123456", me: "bob", id: bobID}
	if _, _, err := bhome.joinRoom("bob", pubkeyB64(bobID), ""); err != nil {
		t.Fatal(err)
	}
	bscr := newChatScreen(srv.URL, "123456", "bob", bobID, "")
	r := newRootModel(bscr)
	rn, _ := r.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	r = rn.(rootModel)

	// Bob accepts through the REAL signed HTTP path; the window's done
	// message attaches + opens the group through rootModel.
	sig := &signalClient{serverURL: srv.URL, key: code, me: "bob", id: bobID}
	roster, epoch, err := sig.acceptInvite(code, pubkeyB64(bobID), "")
	if err != nil || len(roster) == 0 || epoch <= 0 {
		t.Fatalf("accept: %v roster=%v epoch=%d", err, roster, epoch)
	}
	rn, _ = r.Update(acceptGroupDoneMsg{code: code, name: "Design"})
	r = rn.(rootModel)
	if !inList(sidebarNames(&r.chat), "Design") {
		t.Fatalf("accepted group must paint: %v", sidebarNames(&r.chat))
	}

	// ── server forgets the group; the engine's beat classifies it ended
	// and the live row drops to a tombstone (the active-view drop path). ──
	fake.mu.Lock()
	delete(fake.members, code)
	delete(fake.groupMeta, code)
	fake.mu.Unlock()
	r.chat.groups[code].eng.beatOnce()
	r = drainEndEvent(t, r)
	if _, live := r.chat.groups[code]; live {
		t.Fatalf("dead group must leave the live set: %v", sidebarNames(&r.chat))
	}
	if tmb, ok := r.chat.tombstones[code]; !ok || tmb.name != "Design" {
		t.Fatalf("dead group must keep a tombstone row: %+v", r.chat.tombstones)
	}

	// ── a NEW room reuses the code and alice re-invites bob: the 2s tick
	// must badge the invite but must NOT resurrect a LIVE row; the
	// tombstone stays until bob accepts. ──
	fake.mu.Lock()
	fake.members[code] = map[string]rosterMember{
		"alice": {Username: "alice", Pubkey: pubkeyB64(aliceID), Online: true, Role: "creator"},
	}
	fake.groupMeta[code] = fakeGroupMeta{name: "Design", max: -1}
	fake.invites["bob"] = append([]groupInvite{{Code: code, GroupName: "Design", By: "alice", At: 1}}, fake.invites["bob"]...)
	fake.mu.Unlock()

	rn, cmd2 := r.Update(rosterTickMsg{}) // the 2s tick arms the invites poll
	r = rn.(rootModel)
	msgs := runBatchCmd(t, cmd2)
	polled, ok := findMsg[invitesPolledMsg](msgs)
	if !ok {
		t.Fatal("the roster tick must arm the /invites/mine poll")
	}
	rn, _ = r.Update(polled)
	r = rn.(rootModel)
	if _, live := r.chat.groups[code]; live {
		t.Fatalf("a re-invite must not resurrect a live group row: %v", sidebarNames(&r.chat))
	}
	if _, dead := r.chat.tombstones[code]; !dead {
		t.Fatal("a pending re-invite must keep the tombstone until a fresh accept")
	}
	if r.chat.pendingInvites != 1 {
		t.Fatalf("the re-invite must still badge: %d", r.chat.pendingInvites)
	}
}
