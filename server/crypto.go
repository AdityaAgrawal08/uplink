package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/hkdf"
)

// KeyPair holds an ephemeral X25519 keypair for a session.
type KeyPair struct {
	privateKey *ecdh.PrivateKey
	publicKey  *ecdh.PublicKey
}

// GenerateKeyPair generates a new ephemeral X25519 keypair.
func GenerateKeyPair() (*KeyPair, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	return &KeyPair{
		privateKey: priv,
		publicKey:  priv.PublicKey(),
	}, nil
}

// PublicB64 returns the public key as a base64-encoded string.
func (kp *KeyPair) PublicB64() string {
	return base64.RawStdEncoding.EncodeToString(kp.publicKey.Bytes())
}

// PublicBytes returns the raw public key bytes.
func (kp *KeyPair) PublicBytes() []byte {
	return kp.publicKey.Bytes()
}

// DeriveSharedSecret computes an ECDH shared secret from our private key
// and a peer's public key (base64-encoded).
func (kp *KeyPair) DeriveSharedSecret(peerPubB64 string) ([]byte, error) {
	peerBytes, err := base64.RawStdEncoding.DecodeString(peerPubB64)
	if err != nil {
		return nil, fmt.Errorf("decode peer public key: %w", err)
	}
	peerPub, err := ecdh.X25519().NewPublicKey(peerBytes)
	if err != nil {
		return nil, fmt.Errorf("parse peer public key: %w", err)
	}
	secret, err := kp.privateKey.ECDH(peerPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	return secret, nil
}

// DeriveEncryptionKey derives a 32-byte AES-256 key from a shared secret
// using HKDF-SHA256.
func DeriveEncryptionKey(sharedSecret []byte, salt, info string) []byte {
	hkdfReader := hkdf.New(sha256.New, sharedSecret, []byte(salt), []byte(info))
	key := make([]byte, 32)
	_, _ = hkdfReader.Read(key)
	return key
}
