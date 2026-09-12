package main

import (
	"bytes"
	"testing"
)

// driveHandshake runs a full XX handshake between two identities over
// in-memory pipes, returning both ready sessions.
func driveHandshake(t *testing.T, a, b *identityKey, aname, bname string) (*peerSession, *peerSession) {
	t.Helper()
	pa, m1, err := beginNoise(a, aname, true)
	if err != nil {
		t.Fatalf("begin initiator: %v", err)
	}
	pb, _, err := beginNoise(b, bname, false)
	if err != nil {
		t.Fatalf("begin responder: %v", err)
	}
	// m1: a -> b; responder replies m2
	m2, err := pb.stepNoise(m1)
	if err != nil {
		t.Fatalf("responder m1: %v", err)
	}
	if m2 == nil {
		t.Fatal("expected m2 reply")
	}
	// m2: b -> a; initiator replies m3 (completes initiator)
	m3, err := pa.stepNoise(m2)
	if err != nil {
		t.Fatalf("initiator m2: %v", err)
	}
	if !pa.isReady() {
		t.Fatal("initiator must be ready after m3")
	}
	// m3 may be empty payload-carrying final; feed only if non-nil.
	// In XX m3 always exists (may be zero-length payload but framed).
	if m3 == nil {
		t.Fatal("expected m3")
	}
	reply, err := pb.stepNoise(m3)
	if err != nil {
		t.Fatalf("responder m3: %v", err)
	}
	if reply != nil {
		t.Fatal("no reply expected after final message")
	}
	if !pb.isReady() {
		t.Fatal("responder must be ready after m3")
	}
	return pa, pb
}

func TestNoiseHandshakeRoundTrip(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	pa, pb := driveHandshake(t, a, b, "alice", "bob")

	// identities bind: each side sees the other's true static key
	if !bytes.Equal(pa.remoteKey(), b.publicKey()) {
		t.Fatal("initiator learned wrong remote key")
	}
	if !bytes.Equal(pb.remoteKey(), a.publicKey()) {
		t.Fatal("responder learned wrong remote key")
	}

	// message round-trip both directions
	ct, err := pa.encrypt([]byte("hello bob"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := pb.decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "hello bob" {
		t.Fatalf("wrong plaintext: %q", pt)
	}
	ct2, _ := pb.encrypt([]byte("hello alice"))
	pt2, err := pa.decrypt(ct2)
	if err != nil || string(pt2) != "hello alice" {
		t.Fatalf("reverse round-trip failed: %v %q", err, pt2)
	}
}

func TestNoiseTamperRejected(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	pa, pb := driveHandshake(t, a, b, "alice", "bob")

	ct, _ := pa.encrypt([]byte("secret"))
	ct[len(ct)-1] ^= 0xFF // flip a tag bit
	if _, err := pb.decrypt(ct); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	// Noise fail-closes: a decryption failure burns the transport nonce to
	// its maximum, permanently killing the channel (this blocks
	// chosen-ciphertext probing). The peers must tear down and re-handshake
	// — subsequent decrypts on the dead channel MUST keep failing.
	ct2, _ := pa.encrypt([]byte("again"))
	if _, err := pb.decrypt(ct2); err == nil {
		t.Fatal("dead channel must stay dead after forgery; re-handshake required")
	}
}

func TestNoiseWrongPeerFails(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	eve, _ := generateIdentity()
	pa, _ := driveHandshake(t, a, b, "alice", "bob")

	// eve cannot read alice->bob traffic even holding the bytes
	pe, _, _ := beginNoise(eve, "alice", false)
	_ = pe
	ct, _ := pa.encrypt([]byte("secret"))
	// eve has no channel with alice: decrypt without handshake fails
	if _, err := pe.decrypt(ct); err == nil {
		t.Fatal("decrypt without handshake must fail")
	}
}

func TestNoiseNotReadyGuards(t *testing.T) {
	a, _ := generateIdentity()
	ps, _, err := beginNoise(a, "bob", true)
	if err != nil {
		t.Fatal(err)
	}
	if ps.isReady() {
		t.Fatal("must not be ready before handshake")
	}
	if _, err := ps.encrypt([]byte("x")); err == nil {
		t.Fatal("encrypt before ready must fail")
	}
	if _, err := ps.stepNoise([]byte("garbage")); err == nil {
		t.Fatal("garbage handshake bytes must fail")
	}
}

func TestSafetyCodeAgainstHandshake(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	pa, pb := driveHandshake(t, a, b, "alice", "bob")

	// both sides derive the same user-visible code from authenticated keys
	ca := safetyCode(a.publicKey(), pa.remoteKey())
	cb := safetyCode(b.publicKey(), pb.remoteKey())
	if ca != cb {
		t.Fatalf("safety codes differ: %q vs %q", ca, cb)
	}
}
