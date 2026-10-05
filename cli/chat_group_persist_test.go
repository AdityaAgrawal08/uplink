package main

// chat_group_persist_test.go — local group persistence + restart restore.
//
// The store (~/.uplink/groups.json, keyed by home room) is wired into the
// real attach/drop paths; on TUI start the saved rows paint optimistically
// and each is re-joined with its saved password in the background. Failures
// are classified: 404/kicked/full/password drops the row AND prunes the
// store (no ghosts); transport errors keep the row.

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestGroupStorePersistAttachDropAndPrune drives the live persist hook: an
// attach writes the membership (name/desc/password), a drop/prune removes it
// so a restart can never resurrect the left group.
func TestGroupStorePersistAttachDropAndPrune(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	path := filepath.Join(t.TempDir(), "groups.json")
	id, _ := generateIdentity()

	c := groupTestScreen("alice")
	c.id = id
	c.netCh = make(chan tea.Msg, 256)
	c.groupsPath = path
	c.homeKey = "111111"

	gsig := &signalClient{serverURL: srv.URL, key: "740001", me: "alice", id: id}
	c.attachGroup("740001", "Design", "the team", gsig, "s3cret")
	stored := loadGroupsForRoom(path, "111111")
	if len(stored) != 1 {
		t.Fatalf("store = %+v; want the attached group", stored)
	}
	if stored[0].Code != "740001" || stored[0].Name != "Design" || stored[0].Desc != "the team" || stored[0].Password != "s3cret" {
		t.Fatalf("stored group = %+v; want code/name/desc/password", stored[0])
	}

	c.dropGroupAs("740001", "", "left")
	if got := loadGroupsForRoom(path, "111111"); len(got) != 0 {
		t.Fatalf("store after leave = %+v; want pruned (no ghost rows)", got)
	}
	c.shutdownSessions()
}

// TestGroupRestartRestoreRejoinsAndPrunesGhosts is the restart contract
// against the fake server: saved rows paint optimistically, the background
// signed re-join keeps the live ones (including a protected group re-joined
// with its saved password), a 404 room is dropped AND pruned, and an
// already-seated identity (409) stays live.
func TestGroupRestartRestoreRejoinsAndPrunesGhosts(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	if _, err := home.createRoom("alice", pubkeyB64(id), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"

	// Group A: alice is the creator (seat survives the restart -> 409 on
	// re-join, which must count as a live membership).
	asig := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	codeA, err := asig.createGroupRoom("alice", pubkeyB64(id), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}

	// Group C: bob owns a password-protected group; alice joined before the
	// "restart" (her seat too survives, but the saved password must be the
	// one the restore sends).
	bobID, _ := generateIdentity()
	bsig := &signalClient{serverURL: srv.URL, me: "bob", id: bobID}
	codeC, err := bsig.createGroupRoom("bob", pubkeyB64(bobID), "Vault", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	meta := fake.groupMeta[codeC]
	meta.pass = "s3cret"
	fake.groupMeta[codeC] = meta
	fake.mu.Unlock()
	csig := &signalClient{serverURL: srv.URL, key: codeC, me: "alice", id: id}
	if _, _, err := csig.joinRoom("alice", pubkeyB64(id), "s3cret"); err != nil {
		t.Fatalf("seed protected join: %v", err)
	}

	path := filepath.Join(t.TempDir(), "groups.json")
	saveGroupsForRoom(path, "123456", []storedGroup{
		{Code: codeA, Name: "Design"},
		{Code: "700999", Name: "Ghost"},
		{Code: codeC, Name: "Vault", Password: "s3cret"},
	})

	scr := newChatScreen(srv.URL, "123456", "alice", id, "")
	scr.groupsPath = path
	scr.restorePersistedGroups()
	if len(scr.groups) != 3 {
		t.Fatalf("optimistic rows = %d; want 3 painted before any network", len(scr.groups))
	}
	for _, g := range scr.groups {
		if !g.restoring || g.eng != nil {
			t.Fatalf("optimistic row %s must be restoring with no engine: %+v", g.code, g)
		}
	}
	if !strings.Contains(scr.groups[codeC].password, "s3cret") {
		t.Fatal("restored row must carry the saved password")
	}

	cmd := scr.restoreGroupsCmd()
	if cmd == nil {
		t.Fatal("restore must arm one re-join command per saved row")
	}
	for _, msg := range runBatchCmd(t, cmd) {
		scr = stepChat(scr, msg)
	}

	if g := scr.groups[codeA]; g == nil || g.restoring || g.eng == nil {
		t.Fatalf("live group A must settle into a running session: %+v", g)
	}
	if g := scr.groups[codeC]; g == nil || g.restoring || g.eng == nil || g.password != "s3cret" {
		t.Fatalf("protected group C must re-join with its saved password: %+v", g)
	}
	if _, ok := scr.groups["700999"]; ok {
		t.Fatal("the 404 ghost must drop from the sidebar")
	}
	stored := loadGroupsForRoom(path, "123456")
	if len(stored) != 2 {
		t.Fatalf("store after restore = %+v; want only the two live rows", stored)
	}
	for _, sg := range stored {
		if sg.Code == "700999" {
			t.Fatal("the ghost must be pruned from the store too")
		}
	}
	if !strings.Contains(scr.status, "Ghost") {
		t.Fatalf("status must name the dropped ghost, got %q", scr.status)
	}
	// No ghost row paints either.
	for _, it := range scr.chatItems() {
		if it.isGroup && it.name == "Ghost" {
			t.Fatalf("a dead restored group must not paint a sidebar row: %+v", it)
		}
	}
	scr.shutdownSessions()
}

// TestGroupRestoreWrongPasswordDropsAndPrunes: a saved password the server
// now rejects is terminal — the row drops and the stale secret is pruned.
func TestGroupRestoreWrongPasswordDropsAndPrunes(t *testing.T) {
	srv, fake := newGroupTestServer(t)
	id, _ := generateIdentity()

	home := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	if _, err := home.createRoom("alice", pubkeyB64(id), ""); err != nil {
		t.Fatal(err)
	}
	home.key = "123456"

	bobID, _ := generateIdentity()
	bsig := &signalClient{serverURL: srv.URL, me: "bob", id: bobID}
	code, err := bsig.createGroupRoom("bob", pubkeyB64(bobID), "Vault", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	meta := fake.groupMeta[code]
	meta.pass = "s3cret"
	fake.groupMeta[code] = meta
	fake.mu.Unlock()

	path := filepath.Join(t.TempDir(), "groups.json")
	saveGroupsForRoom(path, "123456", []storedGroup{{Code: code, Name: "Vault", Password: "wrong"}})

	scr := newChatScreen(srv.URL, "123456", "alice", id, "")
	scr.groupsPath = path
	scr.restorePersistedGroups()
	for _, msg := range runBatchCmd(t, scr.restoreGroupsCmd()) {
		scr = stepChat(scr, msg)
	}
	if _, ok := scr.groups[code]; ok {
		t.Fatal("a rejected password must drop the restored row")
	}
	if got := loadGroupsForRoom(path, "123456"); len(got) != 0 {
		t.Fatalf("stale password must be pruned, store=%+v", got)
	}
	if !strings.Contains(scr.status, "password") {
		t.Fatalf("status must explain the password rejection, got %q", scr.status)
	}
	scr.shutdownSessions()
}

// TestGroupRestoreSkipsAlreadyAttached: restore never double-attaches — a
// code already present is skipped, and a fresh attach after restore reuses
// the running engine.
func TestGroupRestoreSkipsAlreadyAttached(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, _ := generateIdentity()
	path := filepath.Join(t.TempDir(), "groups.json")
	saveGroupsForRoom(path, "111111", []storedGroup{{Code: "740001", Name: "Design"}})

	c := groupTestScreen("alice")
	c.id = id
	c.netCh = make(chan tea.Msg, 256)
	c.groupsPath = path
	gsig := &signalClient{serverURL: srv.URL, key: "740001", me: "alice", id: id}
	c.attachGroup("740001", "Design", "", gsig, "")
	engBefore := c.groups["740001"].eng

	c.restorePersistedGroups()
	if len(c.groups) != 1 {
		t.Fatalf("restore must skip an already-attached code, groups=%d", len(c.groups))
	}
	c.attachGroup("740001", "Design", "", gsig, "")
	if c.groups["740001"].eng != engBefore {
		t.Fatal("attachGroup over a running session must not swap in a second engine")
	}
	c.shutdownSessions()
}

// TestGroupTombstoneOnLeaveAndReattach pins the tombstone contract: leaving
// (or being dropped) keeps a visible placeholder row on the sidebar for the
// session, and a fresh attach (invite accept / join) clears it and restores
// the live row. Tombstones are never persisted, so a restart shows no ghost.
func TestGroupTombstoneOnLeaveAndReattach(t *testing.T) {
	srv, _ := newGroupTestServer(t)
	id, _ := generateIdentity()
	asig := &signalClient{serverURL: srv.URL, me: "alice", id: id}
	code, err := asig.createGroupRoom("alice", pubkeyB64(id), "Design", "", nil, "123456")
	if err != nil {
		t.Fatal(err)
	}

	c := groupTestScreen("alice")
	c.id = id
	c.netCh = make(chan tea.Msg, 256)
	c.groupsPath = filepath.Join(t.TempDir(), "groups.json")
	gsig := &signalClient{serverURL: srv.URL, key: code, me: "alice", id: id}
	c.attachGroup(code, "Design", "", gsig, "")

	c.dropGroupAs(code, "", "left")
	if _, live := c.groups[code]; live {
		t.Fatal("left group must leave the live set")
	}
	found := false
	for _, it := range c.chatItems() {
		if it.isGroup && it.tombstone {
			found = true
			if it.name != "Design" || !strings.Contains(it.preview, "you left") {
				t.Fatalf("tombstone row = %+v; want Design + leave preview", it)
			}
		}
	}
	if !found {
		t.Fatal("leaving must keep a visible tombstone row until re-invited/join")
	}
	// Tombstones never persist: the store is already empty.
	if got := loadGroupsForRoom(c.groupsPath, c.homeKey); len(got) != 0 {
		t.Fatalf("tombstone must not be persisted, store=%+v", got)
	}

	// A fresh attach (accept/join path) clears the tombstone.
	c.attachGroup(code, "Design", "", gsig, "")
	if _, dead := c.tombstones[code]; dead {
		t.Fatal("a live attach must clear the tombstone")
	}
	live, tomb := 0, 0
	for _, it := range c.chatItems() {
		if it.isGroup && it.tombstone {
			tomb++
		} else if it.isGroup {
			live++
		}
	}
	if live != 1 || tomb != 0 {
		t.Fatalf("after re-attach: live=%d tombstone=%d; want 1/0", live, tomb)
	}
	c.shutdownSessions()
}
