package main

import "testing"

// B36 regression: private file cards must land in the canonical conversation
// bucket, not the raw recipient username. The server now returns convId, and
// fileConv must prefer it while falling back to sender/recipient derivation.
func TestFileConvPrefersServerConvID(t *testing.T) {
	f := sessionFile{Username: "alice", To: "bob", ConvID: "alice|bob"}
	if got := fileConv(f); got != "alice|bob" {
		t.Fatalf("expected alice|bob, got %q", got)
	}
}

func TestFileConvFallsBackToDerivation(t *testing.T) {
	f := sessionFile{Username: "alice", To: "bob"} // legacy doc, no convId
	if got := fileConv(f); got != conversationKey("alice", "bob") {
		t.Fatalf("expected %q, got %q", conversationKey("alice", "bob"), got)
	}
}

func TestFileConvGeneral(t *testing.T) {
	f := sessionFile{Username: "alice"}
	if got := fileConv(f); got != generalConv {
		t.Fatalf("expected %q, got %q", generalConv, got)
	}
}

// The canonical key is order-independent, so a file sent by bob to alice and
// one sent by alice to bob must resolve to the same bucket.
func TestFileConvIsSymmetric(t *testing.T) {
	a := fileConv(sessionFile{Username: "alice", To: "bob"})
	b := fileConv(sessionFile{Username: "bob", To: "alice"})
	if a != b {
		t.Fatalf("conversation buckets differ: %q vs %q", a, b)
	}
}
