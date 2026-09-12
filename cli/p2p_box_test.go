package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()

	box, err := sealBox(a, b.publicKey(), []byte("hello bob"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := openBox(b, a.publicKey(), box)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "hello bob" {
		t.Fatalf("wrong plaintext: %q", pt)
	}

	// reverse direction uses the same shared secret
	box2, _ := sealBox(b, a.publicKey(), []byte("hello alice"))
	pt2, err := openBox(a, b.publicKey(), box2)
	if err != nil || string(pt2) != "hello alice" {
		t.Fatalf("reverse failed: %v %q", err, pt2)
	}
}

func TestBoxTamperRejected(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	box, _ := sealBox(a, b.publicKey(), []byte("secret"))
	raw, _ := base64.StdEncoding.DecodeString(box)
	raw[len(raw)-1] ^= 0xFF
	bad := base64.StdEncoding.EncodeToString(raw)
	if _, err := openBox(b, a.publicKey(), bad); err == nil {
		t.Fatal("tampered box must fail")
	}
}

func TestBoxWrongPeerFails(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	eve, _ := generateIdentity()
	box, _ := sealBox(a, b.publicKey(), []byte("secret"))
	// eve tries with her own key: ECDH differs, auth fails
	if _, err := openBox(eve, a.publicKey(), box); err == nil {
		t.Fatal("wrong peer must fail")
	}
	// bob with the wrong sender key also fails
	if _, err := openBox(b, eve.publicKey(), box); err == nil {
		t.Fatal("wrong sender key must fail")
	}
}

func TestBoxNonceRandomness(t *testing.T) {
	a, _ := generateIdentity()
	b, _ := generateIdentity()
	x, _ := sealBox(a, b.publicKey(), []byte("same"))
	y, _ := sealBox(a, b.publicKey(), []byte("same"))
	if x == y {
		t.Fatal("identical plaintexts must produce different boxes (random nonce)")
	}
	px, _ := openBox(b, a.publicKey(), x)
	py, _ := openBox(b, a.publicKey(), y)
	if !bytes.Equal(px, py) {
		t.Fatal("both must decrypt to the same plaintext")
	}
}

func TestBoxBadInputs(t *testing.T) {
	a, _ := generateIdentity()
	if _, err := sealBox(a, []byte("short"), []byte("x")); err == nil {
		t.Fatal("short peer key must fail")
	}
	b, _ := generateIdentity()
	if _, err := openBox(b, a.publicKey(), "!!!not-base64!!!"); err == nil {
		t.Fatal("bad base64 must fail")
	}
	if _, err := openBox(b, a.publicKey(), base64.StdEncoding.EncodeToString([]byte("tiny"))); err == nil {
		t.Fatal("short box must fail")
	}
}
