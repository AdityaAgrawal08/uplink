package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ─── Pairwise message boxes (async E2E for the inbox path) ─────────────────
//
// Noise transport (p2p_noise.go) only exists on live data channels. The
// inbox fallback must encrypt for peers with no live connection — possibly
// offline for an hour. Pairwise boxes solve exactly this: ECDH between my
// static private key and the peer's roster-advertised static public key,
// HKDF to a message key, AES-GCM per message with a random nonce.
//
// Differences from the Noise transport, stated plainly:
//   - No forward secrecy per message (static-static ECDH). The live path
//     re-handshakes per connection and is preferred; boxes are the
//     offline/fallback path. Safety codes still authenticate the peer key,
//     so a key-swap attack is exposed identically.
//   - Ciphertext + nonce are bundled (nonce prepended); the server sees
//     only opaque bytes either way.
//
// Wire: base64(box) goes in the inbox payload with kind "p2p"; the box
// itself unwraps to one full frame JSON (see p2p_proto.go).

const boxInfo = "uplink-p2p-box-v1"

func deriveBoxKey(self *identityKey, peerPub []byte) ([]byte, error) {
	peerKey, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("bad peer key: %w", err)
	}
	secret, err := self.priv.ECDH(peerKey)
	if err != nil {
		return nil, fmt.Errorf("ECDH failed: %w", err)
	}
	key := make([]byte, 32)
	kdf := hkdf.New(sha256.New, secret, nil, []byte(boxInfo))
	if _, err := io.ReadFull(kdf, key); err != nil {
		return nil, err
	}
	return key, nil
}

// sealBox encrypts plaintext for the peer holding peerPub.
func sealBox(self *identityKey, peerPub, plaintext []byte) (string, error) {
	key, err := deriveBoxKey(self, peerPub)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// openBox decrypts a box from the peer holding peerPub. Tampered boxes fail.
func openBox(self *identityKey, peerPub []byte, boxB64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(boxB64)
	if err != nil {
		return nil, err
	}
	key, err := deriveBoxKey(self, peerPub)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("box too short")
	}
	nonce, sealed := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, sealed, nil)
}
