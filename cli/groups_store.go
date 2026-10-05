package main

// groups_store.go — the local group-membership store (~/.uplink/groups.json).
//
// The server exposes no "groups I belong to" list (src/ owns the API
// surface), so the CLI remembers its memberships locally and re-joins them on
// the next launch. The file is keyed BY HOME ROOM CODE so one machine can
// hold memberships for several home rooms without collisions:
//
//	{ "rooms": { "<homeCode>": { "groups": [
//	    { "code": "740001", "name": "Design", "desc": "", "password": "" }
//	] } } }
//
// SECURITY TRADEOFF (deliberate, revisit when a keyring lands): the group
// password is stored in this file in PLAINTEXT — that is what lets a
// protected group auto-rejoin after a restart without asking again. The file
// itself is 0600 and its directory 0755 (like ~/.uplink/identity), so only
// the owning user can read it. A future revision could gate the secret
// behind the OS keyring (Secret Service / Keychain / DPAPI) and keep only a
// keyring reference here; until then the plaintext tradeoff must stay
// documented at every touch point.
//
// Writes are atomic (temp file in the same directory + rename), so a crash
// mid-save can never leave a truncated store that would drop memberships.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// storedGroup is one persisted membership: the session code plus the display
// meta the sidebar needs at paint time and the password (if any) for the
// background re-join.
type storedGroup struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Desc     string `json:"desc,omitempty"`
	Password string `json:"password,omitempty"` // plaintext — see the security note above
}

// storedRoom is one home room's membership list.
type storedRoom struct {
	Groups []storedGroup `json:"groups"`
}

// groupsStore is the whole file.
type groupsStore struct {
	Rooms map[string]storedRoom `json:"rooms"`
}

func newGroupsStore() *groupsStore {
	return &groupsStore{Rooms: map[string]storedRoom{}}
}

// groupsStorePath resolves ~/.uplink/groups.json. UPLINK_GROUPS_FILE is a
// test hook so suites never touch a developer's real home directory.
func groupsStorePath() string {
	if p := os.Getenv("UPLINK_GROUPS_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", ".uplink", "groups.json")
	}
	return filepath.Join(home, ".uplink", "groups.json")
}

// loadGroupsStore reads the store; a missing file is an empty store (first
// run). A malformed file is ignored with a warning — never half-restored —
// matching LoadConfig's tolerance.
func loadGroupsStore(path string) *groupsStore {
	st := newGroupsStore()
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return st
	}
	if err := json.Unmarshal(data, st); err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring malformed groups store %s: %v\n", path, err)
		return newGroupsStore()
	}
	if st.Rooms == nil {
		st.Rooms = map[string]storedRoom{}
	}
	return st
}

// saveGroupsStore writes the store atomically: marshal, temp file in the
// same directory, fsync-close, rename. The temp file is created 0600 and the
// destination keeps that mode on every rewrite.
func saveGroupsStore(path string, st *groupsStore) error {
	if st == nil {
		st = newGroupsStore()
	}
	if st.Rooms == nil {
		st.Rooms = map[string]storedRoom{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".groups-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// loadGroupsForRoom returns the saved memberships for one home room, in
// stable code order.
func loadGroupsForRoom(path, room string) []storedGroup {
	if room == "" {
		return nil
	}
	st := loadGroupsStore(path)
	groups := append([]storedGroup(nil), st.Rooms[room].Groups...)
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Code < groups[j].Code })
	return groups
}

// saveGroupsForRoom replaces one home room's membership list (removing the
// room key entirely when the list empties, so the store can never keep a
// ghost room behind).
func saveGroupsForRoom(path, room string, groups []storedGroup) error {
	if room == "" {
		return nil
	}
	st := loadGroupsStore(path)
	if len(groups) == 0 {
		delete(st.Rooms, room)
	} else {
		sorted := append([]storedGroup(nil), groups...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Code < sorted[j].Code })
		st.Rooms[room] = storedRoom{Groups: sorted}
	}
	return saveGroupsStore(path, st)
}
