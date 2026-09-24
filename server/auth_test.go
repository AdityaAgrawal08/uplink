package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// ─── Password hashing (B37) ────────────────────────────────────────────────

func TestHashPasswordRandomSalt(t *testing.T) {
	// Same password must produce different hashes (random per-password salt).
	h1, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("expected different hashes due to random salt")
	}
	if h1 == "" || h2 == "" {
		t.Fatal("hashPassword returned empty string")
	}
	if !strings.HasPrefix(h1, "$argon2id$") {
		t.Fatalf("unexpected hash format: %s", h1)
	}
}

func TestVerifyPasswordRoundTrip(t *testing.T) {
	h, err := hashPassword("s3cr3t!")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword("s3cr3t!", h) {
		t.Fatal("correct password should verify")
	}
	if verifyPassword("wrong", h) {
		t.Fatal("wrong password must not verify")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$v=19$m=16384,t=3,p=1$onlyfourparts",
		"$argon2id$v=19$m=16384,t=3,p=1$!!!notb64$!!!notb64",
	}
	for _, c := range cases {
		if verifyPassword("x", c) {
			t.Fatalf("malformed hash should not verify: %q", c)
		}
	}
}

// ─── Public key validation + distribution (B37) ────────────────────────────

func TestClientPublicKeyValidation(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	valid := kp.PublicB64()
	raw, err := base64.RawStdEncoding.DecodeString(valid)
	if err != nil || len(raw) != 32 {
		t.Fatalf("test key should be 32 bytes: %v len=%d", err, len(raw))
	}
}
