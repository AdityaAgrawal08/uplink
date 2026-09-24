package main

import (
	"crypto/rand"
	"strings"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	f := newFrame(frameChat, "m1", "alice", "bob")
	f.Data = "ciphertext"
	raw, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.Type != frameChat || got.MsgId != "m1" ||
		got.From != "alice" || got.To != "bob" || got.Data != "ciphertext" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestDecodeFrameRejects(t *testing.T) {
	if _, err := decodeFrame([]byte("not json")); err == nil {
		t.Fatal("expected JSON error")
	}
	if _, err := decodeFrame([]byte(`{"v":2,"type":"chat"}`)); err == nil {
		t.Fatal("expected version error")
	}
	if _, err := decodeFrame([]byte(`{"v":1,"type":"teleport"}`)); err == nil {
		t.Fatal("expected unknown-type error")
	}
}

func TestFrameTypesAccepted(t *testing.T) {
	for _, ft := range []string{frameChat, frameAck, frameFile, frameFileMeta, frameFileChunk, frameFileComplete, frameTyping} {
		f := newFrame(ft, "m", "a", "")
		raw, _ := encodeFrame(f)
		if _, err := decodeFrame(raw); err != nil {
			t.Fatalf("type %s rejected: %v", ft, err)
		}
	}
}

func TestNewMsgIdUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newMsgId()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 {
			t.Fatalf("expected 32 hex chars, got %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %s", id)
		}
		seen[id] = true
	}
}

func TestSeenSetDedupAndEvict(t *testing.T) {
	s := newSeenSet(3)
	if s.seen("a") {
		t.Fatal("first sighting must be new")
	}
	if !s.seen("a") {
		t.Fatal("second sighting must be dup")
	}
	s.seen("b")
	s.seen("c")
	s.seen("d") // evicts "a"
	if s.seen("a") != false {
		t.Fatal("evicted ID must read as new again")
	}
	if !s.seen("d") {
		t.Fatal("recent ID must still be known")
	}
}

func TestSeenSetConcurrent(t *testing.T) {
	s := newSeenSet(100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.seen("shared")
			}
		}()
	}
	wg.Wait()
	// exactly one goroutine observed it first; the rest saw dup — and no race
	if !s.seen("shared") {
		t.Fatal("must be known after concurrent writes")
	}
}

func TestSplitChunks(t *testing.T) {
	if splitChunks(nil) != nil {
		t.Fatal("empty input must yield nil")
	}
	buf := make([]byte, frameChunkSize*2+100)
	rand.Read(buf)
	chunks := splitChunks(buf)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if len(chunks[0]) != frameChunkSize || len(chunks[2]) != 100 {
		t.Fatal("wrong chunk sizes")
	}
	var rebuilt []byte
	for _, c := range chunks {
		rebuilt = append(rebuilt, c...)
	}
	if string(rebuilt) != string(buf) {
		t.Fatal("reassembly mismatch")
	}
}

func TestSafetyCodeSymmetric(t *testing.T) {
	a := []byte("alice-pubkey-32bytes-padded-000001")
	b := []byte("bob-pubkey-32bytes-padded-00000002")
	if safetyCode(a, b) != safetyCode(b, a) {
		t.Fatal("safety code must be order-independent")
	}
	c := safetyCode(a, b)
	if len(strings.Split(c, "-")) != 4 {
		t.Fatalf("expected 4 groups, got %q", c)
	}
	// attacker key must change the code
	eve := []byte("eve-pubkey-32bytes-padded-00000003")
	if safetyCode(a, eve) == c {
		t.Fatal("different key must change the code")
	}
}

// TestSignalPayloadCap pins the cross-language signaling cap shared with the
// TS server (signalMaxPayloadBytes). Both sides must stay in sync.
func TestSignalPayloadCap(t *testing.T) {
	if signalMaxPayloadBytes != 16*1024 {
		t.Fatalf("signal cap drifted: %d", signalMaxPayloadBytes)
	}
	if !signalPayloadTooBig(string(make([]byte, 16*1024+1))) {
		t.Fatal("oversize SDP should be flagged")
	}
	if signalPayloadTooBig(string(make([]byte, 16*1024))) {
		t.Fatal("cap-sized SDP should pass")
	}
}
