package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestGenerateKeyPair(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if kp.PublicB64() == "" {
		t.Fatal("empty public key")
	}
	// Decode and verify it's 32 bytes (X25519).
	pubBytes, err := base64.RawStdEncoding.DecodeString(kp.PublicB64())
	if err != nil {
		t.Fatal(err)
	}
	if len(pubBytes) != 32 {
		t.Fatalf("expected 32-byte public key, got %d", len(pubBytes))
	}
}

func TestDeriveSharedSecret(t *testing.T) {
	kp1, _ := GenerateKeyPair()
	kp2, _ := GenerateKeyPair()

	// Both parties derive the same shared secret.
	secret1, err := kp1.DeriveSharedSecret(kp2.PublicB64())
	if err != nil {
		t.Fatal(err)
	}
	secret2, err := kp2.DeriveSharedSecret(kp1.PublicB64())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secret1, secret2) {
		t.Fatal("shared secrets do not match")
	}
	if len(secret1) != 32 {
		t.Fatalf("expected 32-byte shared secret, got %d", len(secret1))
	}
}

func TestDeriveEncryptionKey(t *testing.T) {
	shared := make([]byte, 32)
	rand.Read(shared)

	key1 := DeriveEncryptionKey(shared, "salt", "info")
	key2 := DeriveEncryptionKey(shared, "salt", "info")
	if !bytes.Equal(key1, key2) {
		t.Fatal("same inputs should produce same key")
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key1))
	}

	// Different salt should produce different key.
	key3 := DeriveEncryptionKey(shared, "other-salt", "info")
	if bytes.Equal(key1, key3) {
		t.Fatal("different salt should produce different key")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	kp1, _ := GenerateKeyPair()
	kp2, _ := GenerateKeyPair()

	shared, _ := kp1.DeriveSharedSecret(kp2.PublicB64())
	encKey := DeriveEncryptionKey(shared, "uplink-chat-v1", "message")

	plaintext := []byte("hello, world! this is a test message 🔐")

	// Encrypt with AES-GCM.
	block, _ := aes.NewCipher(encKey)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Decrypt with derived key from the other side.
	shared2, _ := kp2.DeriveSharedSecret(kp1.PublicB64())
	encKey2 := DeriveEncryptionKey(shared2, "uplink-chat-v1", "message")

	block2, _ := aes.NewCipher(encKey2)
	gcm2, _ := cipher.NewGCM(block2)
	decrypted, err := gcm2.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted text does not match: got %q", decrypted)
	}
}

func TestPublicKeyB64RoundTrip(t *testing.T) {
	kp, _ := GenerateKeyPair()
	b64 := kp.PublicB64()

	decoded, err := base64.RawStdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(decoded))
	}
}
