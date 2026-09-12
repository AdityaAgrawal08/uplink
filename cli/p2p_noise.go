package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/flynn/noise"
)

// ─── E2E sessions (Noise_XX) ────────────────────────────────────────────────
//
// Every pair of peers runs one Noise_XX handshake inside their WebRTC data
// channel, then chats through the resulting encrypted transport. Properties:
//
//   - Mutual authentication: both sides prove possession of the static
//     X25519 key advertised for them in the room roster. A stranger's key
//     fails the handshake; a swapped key changes the safety code.
//   - Forward secrecy: an ephemeral ECDH per handshake; recorded traffic is
//     worthless if a device key later leaks.
//   - Server-blind: the signaling relay forwards handshake bytes it cannot
//     use (it holds neither private key). A relay swapping keys is exposed
//     by the safety-code check (see p2p_proto.go).
//   - Tamper-evident: every post-handshake message carries a Poly1305 tag;
//     forged/modified bytes fail Decrypt and are dropped.
//
// Handshake messages ride inside `signal` notes with types "noise1/2/3".

const (
	noiseSig1 = "noise1"
	noiseSig2 = "noise2"
	noiseSig3 = "noise3"
)

// noiseSuite is fixed for v1: X25519 ECDH, ChaCha20-Poly1305 AEAD, SHA256,
// no pre-messages (XX = mutual authentication without prior knowledge).
var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// identityKey is a device's long-lived X25519 static keypair. The 32-byte
// public half is published in the room roster (base64); the private half
// never leaves the device. Noise_XX authenticates exactly this key, so the
// roster entry and the crypto identity are one and the same — no key
// conversion, no parallel keypair to keep in sync.
type identityKey struct {
	priv *ecdh.PrivateKey
	pub  []byte
}

// generateIdentity creates a fresh device keypair.
func generateIdentity() (*identityKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &identityKey{priv: priv, pub: priv.PublicKey().Bytes()}, nil
}

// publicKey returns the roster-advertised public half.
func (id *identityKey) publicKey() []byte {
	return append([]byte{}, id.pub...)
}

// peerSession is one E2E channel to one peer: handshake state, then the
// rolling transport CipherStates. All methods are mutex-guarded — exactly
// one handshake and one message stream per peer.
type peerSession struct {
	mu        sync.Mutex
	peer      string
	hs        *noise.HandshakeState
	initiator bool
	// Transport states, set once the handshake completes.
	send, recv *noise.CipherState
	ready      bool
	// Remote static key, captured at handshake completion for the
	// safety-code check against the roster-advertised key.
	remoteStatic []byte
}

// beginNoise starts (initiator=true) or prepares (initiator=false) a
// handshake. Returns the first message for initiators, nil for responders.
func beginNoise(self *identityKey, peer string, initiator bool) (*peerSession, []byte, error) {
	cfg := noise.Config{
		CipherSuite:   noiseSuite,
		Pattern:       noise.HandshakeXX,
		Initiator:     initiator,
		StaticKeypair: noise.DHKey{Private: self.priv.Bytes(), Public: self.pub},
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, nil, err
	}
	ps := &peerSession{peer: peer, hs: hs, initiator: initiator}
	if !initiator {
		return ps, nil, nil
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return ps, msg, nil
}

// stepNoise feeds one handshake message and returns the reply (nil on the
// final message). When the handshake completes, transport states are
// installed and ready() flips true.
func (ps *peerSession) stepNoise(in []byte) (reply []byte, err error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.ready {
		return nil, fmt.Errorf("handshake already complete with %s", ps.peer)
	}
	var cs1, cs2 *noise.CipherState
	if ps.initiator {
		// Initiator turn order in XX: write m1, read m2, write m3.
		// stepNoise is only called with inbound messages, so: read m2,
		// then write m3 (which completes).
		_, cs1, cs2, err = ps.hs.ReadMessage(nil, in)
		if err != nil {
			return nil, err
		}
		if cs1 != nil {
			// 2-message edge (not XX) — handled uniformly.
			return ps.finishLocked(cs1, cs2)
		}
		reply, cs1, cs2, err = ps.hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, err
		}
		if cs1 != nil {
			if _, ferr := ps.finishLocked(cs1, cs2); ferr != nil {
				return nil, ferr
			}
			return reply, nil
		}
		return reply, nil
	}
	// Responder turn order in XX: read m1, write m2, read m3.
	// stepNoise is called with m1 first (no transport yet, reply m2),
	// then with m3 (completes, no reply).
	_, cs1, cs2, err = ps.hs.ReadMessage(nil, in)
	if err != nil {
		return nil, err
	}
	if cs1 != nil {
		return ps.finishLocked(cs1, cs2)
	}
	reply, cs1, cs2, err = ps.hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if cs1 != nil {
		if _, ferr := ps.finishLocked(cs1, cs2); ferr != nil {
			return nil, ferr
		}
		return reply, nil
	}
	return reply, nil
}

// finishLocked installs transports. flynn/noise returns (initiatorSend,
// initiatorRecv): the initiator encrypts with cs1, the responder with cs2.
func (ps *peerSession) finishLocked(cs1, cs2 *noise.CipherState) ([]byte, error) {
	rs := ps.hs.PeerStatic()
	ps.remoteStatic = append([]byte{}, rs...)
	if ps.initiator {
		ps.send, ps.recv = cs1, cs2
	} else {
		ps.send, ps.recv = cs2, cs1
	}
	ps.ready = true
	ps.hs = nil
	return nil, nil
}

// encrypt seals one post-handshake message.
func (ps *peerSession) encrypt(plaintext []byte) ([]byte, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if !ps.ready {
		return nil, fmt.Errorf("channel to %s not ready", ps.peer)
	}
	out, err := ps.send.Encrypt(nil, nil, plaintext)
	return out, err
}

// decrypt opens one post-handshake message. Tampered bytes fail here.
func (ps *peerSession) decrypt(ciphertext []byte) ([]byte, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if !ps.ready {
		return nil, fmt.Errorf("channel to %s not ready", ps.peer)
	}
	out, err := ps.recv.Decrypt(nil, nil, ciphertext)
	return out, err
}

func (ps *peerSession) isReady() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.ready
}

// remoteKey returns the peer's authenticated static key — compare against
// the roster-advertised key (mismatch = key-swap attack) and feed both
// into safetyCode for the user-visible check.
func (ps *peerSession) remoteKey() []byte {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]byte{}, ps.remoteStatic...)
}
