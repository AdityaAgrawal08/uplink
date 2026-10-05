package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// Local group store: ~/.uplink/groups.json (0600, atomic, keyed by home room)
// ---------------------------------------------------------------------------

// TestGroupsStoreRoundTripAndKeyedByRoom pins the file contract: memberships
// are keyed BY HOME ROOM CODE (one machine, several home rooms), the list
// survives a write/read round trip with name/desc/password intact, and
// pruning one room's list never touches the others.
func TestGroupsStoreRoundTripAndKeyedByRoom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	if err := saveGroupsForRoom(path, "123456", []storedGroup{
		{Code: "740002", Name: "Chess", Desc: "clubs"},
		{Code: "740001", Name: "Design", Password: "s3cret"},
	}); err != nil {
		t.Fatalf("save room A: %v", err)
	}
	if err := saveGroupsForRoom(path, "999999", []storedGroup{
		{Code: "700001", Name: "Other"},
	}); err != nil {
		t.Fatalf("save room B: %v", err)
	}

	a := loadGroupsForRoom(path, "123456")
	if len(a) != 2 {
		t.Fatalf("room A groups = %d; want 2", len(a))
	}
	// Stable code order.
	if a[0].Code != "740001" || a[1].Code != "740002" {
		t.Fatalf("room A order = %+v; want 740001 then 740002", a)
	}
	if a[0].Name != "Design" || a[0].Password != "s3cret" || a[0].Desc != "" {
		t.Fatalf("Design row = %+v; want name+password preserved", a[0])
	}
	if a[1].Name != "Chess" || a[1].Desc != "clubs" {
		t.Fatalf("Chess row = %+v", a[1])
	}
	b := loadGroupsForRoom(path, "999999")
	if len(b) != 1 || b[0].Code != "700001" || b[0].Name != "Other" {
		t.Fatalf("room B = %+v; want its own single group", b)
	}
	if c := loadGroupsForRoom(path, "000000"); len(c) != 0 {
		t.Fatalf("unknown room = %+v; want empty", c)
	}

	// Pruning room A leaves room B intact (and drops the empty room key).
	if err := saveGroupsForRoom(path, "123456", nil); err != nil {
		t.Fatalf("prune room A: %v", err)
	}
	if got := loadGroupsForRoom(path, "123456"); len(got) != 0 {
		t.Fatalf("pruned room A = %+v; want empty", got)
	}
	if got := loadGroupsForRoom(path, "999999"); len(got) != 1 {
		t.Fatalf("room B must survive room A's prune, got %+v", got)
	}
	st := loadGroupsStore(path)
	if _, ok := st.Rooms["123456"]; ok {
		t.Fatal("an emptied room key must be removed from the store")
	}
}

// TestGroupsStoreFileModeAndShape pins the on-disk shape: rooms keyed by
// home code, groups as a JSON list, and the file mode 0600 (the store holds
// group passwords in plaintext — see groups_store.go's security note).
func TestGroupsStoreFileModeAndShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")
	if err := saveGroupsForRoom(path, "123456", []storedGroup{
		{Code: "740001", Name: "Design", Password: "s3cret"},
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store mode = %o; want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("store is not valid JSON: %q", raw)
	}
	var decoded struct {
		Rooms map[string]struct {
			Groups []struct {
				Code     string `json:"code"`
				Name     string `json:"name"`
				Password string `json:"password"`
			} `json:"groups"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	room, ok := decoded.Rooms["123456"]
	if !ok || len(room.Groups) != 1 {
		t.Fatalf("decoded store = %+v; want rooms.123456 with one group", decoded)
	}
	if room.Groups[0].Code != "740001" || room.Groups[0].Password != "s3cret" {
		t.Fatalf("decoded group = %+v", room.Groups[0])
	}
}

// TestGroupsStoreAtomicReplace: a rewrite fully replaces the previous file
// (no append, no leftover temp files), and a malformed store is ignored as
// empty rather than crashing or half-restoring.
func TestGroupsStoreAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	if err := saveGroupsForRoom(path, "123456", []storedGroup{
		{Code: "740001", Name: "Design"},
		{Code: "740002", Name: "Chess"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveGroupsForRoom(path, "123456", []storedGroup{
		{Code: "740001", Name: "Design"},
	}); err != nil {
		t.Fatal(err)
	}
	got := loadGroupsForRoom(path, "123456")
	if len(got) != 1 || got[0].Code != "740001" {
		t.Fatalf("rewrite = %+v; want the fully replaced single-row list", got)
	}
	// The atomic temp file must never survive a successful save.
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".groups-*"))
	if len(leftovers) != 0 {
		t.Fatalf("atomic save left temp files behind: %v", leftovers)
	}

	// Corrupt store: load returns an empty store (warning only), so the
	// next save self-heals instead of half-restoring ghosts.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := loadGroupsStore(path); len(st.Rooms) != 0 {
		t.Fatalf("corrupt store must load empty, got %+v", st.Rooms)
	}
	if err := saveGroupsForRoom(path, "123456", []storedGroup{{Code: "740009", Name: "Fresh"}}); err != nil {
		t.Fatal(err)
	}
	if got := loadGroupsForRoom(path, "123456"); len(got) != 1 || got[0].Name != "Fresh" {
		t.Fatalf("post-corruption save = %+v", got)
	}
}

// TestGroupsStorePathEnvOverride: UPLINK_GROUPS_FILE (test hook) wins so
// suites never write into a developer's real ~/.uplink.
func TestGroupsStorePathEnvOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.json")
	t.Setenv("UPLINK_GROUPS_FILE", p)
	if got := groupsStorePath(); got != p {
		t.Fatalf("groupsStorePath = %q; want the env override %q", got, p)
	}
}
