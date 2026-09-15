package main

import (
	"encoding/base64"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Full call stack over the REAL signal path (fake server, real 2s polls,
// real timing): offer -> accept -> Noise handshake notes -> media ready.
// The method-call loopback tests bypass all of this.
func waitMediaPeer(t *testing.T, m *callManager, peer string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		m.mu.Lock()
		var ready bool
		if m.transport != nil {
			ready = m.transport.peerReady(peer)
		}
		m.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("media session to %s never went live", peer)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestCallOverSignalPath(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid}
	if _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}

	feedA := make(chan []int16, 64)
	feedB := make(chan []int16, 64)
	var muA, muB sync.Mutex
	var gotA, gotB int

	var ma, mb *callManager
	ma = newCallManager("alice", ida,
		func(to, typ, payload string) error { return sigA.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} },
		callCallbacks{
			onRinging: func(peer string) {},
			onState:   func(st callState, peer, info string) { t.Logf("a %s %s %s", st, peer, info) },
		})
	mb = newCallManager("bob", idb,
		func(to, typ, payload string) error { return sigB.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} },
		callCallbacks{
			onRinging: func(peer string) {
				if err := mb.Accept(); err != nil {
					t.Logf("auto-accept failed: %v", err)
				}
			},
			onState: func(st callState, peer, info string) { t.Logf("b %s %s %s", st, peer, info) },
		})
	for _, m := range []*callManager{ma, mb} {
		m.dialIP = "127.0.0.1"
	}
	ma.micSrc = func() (<-chan []int16, func(), error) { return feedA, func() {}, nil }
	mb.micSrc = func() (<-chan []int16, func(), error) { return feedB, func() {}, nil }
	ma.playSink = func() (func([]int16), func(), error) {
		return func(pcm []int16) { muA.Lock(); gotA++; muA.Unlock() }, func() {}, nil
	}
	mb.playSink = func() (func([]int16), func(), error) {
		return func(pcm []int16) { muB.Lock(); gotB++; muB.Unlock() }, func() {}, nil
	}

	ea := newEngineWithStun("alice", ida, sigA, engineCallbacks{
		onSignalNote: func(n signalNote) { ma.onSignalNote(n) },
	}, []string{})
	eb := newEngineWithStun("bob", idb, sigB, engineCallbacks{
		onSignalNote: func(n signalNote) { mb.onSignalNote(n) },
	}, []string{})
	ea.start()
	eb.start()
	defer ea.stop()
	defer eb.stop()

	if err := ma.Call("bob"); err != nil {
		t.Fatal(err)
	}
	waitMediaPeer(t, ma, "bob")
	waitMediaPeer(t, mb, "alice")

	tone := make([]int16, voiceFrameLen)
	for i := range tone {
		tone[i] = 5000
	}
	for i := 0; i < 10; i++ {
		feedA <- append([]int16(nil), tone...)
		feedB <- append([]int16(nil), tone...)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		muA.Lock()
		a := gotA
		muA.Unlock()
		muB.Lock()
		b := gotB
		muB.Unlock()
		if a >= 3 && b >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audio not flowing both ways: a=%d b=%d", a, b)
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = ma.Hangup()
	_ = mb.Hangup()
}
