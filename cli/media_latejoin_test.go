package main

import (
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Field regression: alice publishes audio to the room; carol
// joins LATE over the real signal path (fake server, real 2s polls).
// Asserts carol ends up with a ready session AND actually hears.
// The mic feeds continuously (like production); a finite feed would
// end before carol's multi-round-trip handshake completes.
func TestLateJoinerThreePartySignal(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	sid, err := sigA.createRoom("alice", base64.StdEncoding.EncodeToString(ida.publicKey()), "")
	if err != nil {
		t.Fatal(err)
	}
	// bob present from the start so alice's scope is non-empty
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid}
	if _, _, err := sigB.joinRoom("bob", base64.StdEncoding.EncodeToString(idb.publicKey()), ""); err != nil {
		t.Fatal(err)
	}

	feedA := make(chan []int16, 64)
	ma := newMediaManager("alice", ida,
		func(to, typ, payload string) error { return sigA.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} },
		mediaUICallbacks{onInfo: func(i string) { fmt.Println("A:", i) }})
	ma.dialIP = "127.0.0.1"
	ma.micSrc = func() (<-chan []int16, func(), error) { return feedA, func() {}, nil }
	ea := newEngineWithStun("alice", ida, sigA, engineCallbacks{
		onSignalNote: func(n signalNote) { ma.onSignalNote(n) },
	}, []string{})
	ea.start()
	defer ea.stop()
	// bob's engine keeps his signal queue drained (no media manager needed)
	eb := newEngineWithStun("bob", idb, sigB, engineCallbacks{}, []string{})
	eb.start()
	defer eb.stop()

	if err := ma.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	// Continuous audio (like a real mic): carol must catch the
	// running stream whenever her session completes.
	stopFeed := make(chan struct{})
	defer close(stopFeed)
	go func() {
		tone := make([]int16, voiceFrameLen)
		for i := range tone {
			tone[i] = 5000
		}
		for {
			select {
			case <-stopFeed:
				return
			default:
			}
			select {
			case feedA <- append([]int16(nil), tone...):
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(3 * time.Second)

	// CAROL JOINS LATE. Publisher learns her via roster tick simulation:
	// emulate what chat_tui does (PublishTo with the new roster).
	idc, _ := generateIdentity()
	sigC := &signalClient{serverURL: srv.URL, me: "carol", key: sid}
	if _, _, err := sigC.joinRoom("carol", base64.StdEncoding.EncodeToString(idc.publicKey()), ""); err != nil {
		t.Fatal(err)
	}
	var heardMu sync.Mutex
	heard := 0
	mc := newMediaManager("carol", idc,
		func(to, typ, payload string) error { return sigC.signalSend(to, typ, payload) },
		func() map[string][]byte {
			return map[string][]byte{
				"alice": ida.publicKey(),
				"bob":   idb.publicKey(),
			}
		},
		mediaUICallbacks{
			onInfo: func(i string) { fmt.Println("C:", i) },
		})
	mc.playSink = func() (*speaker, error) {
		// File speaker, not the default device sink: CI runners have no
		// ffplay/paplay, and the assertion is playout delivery (counted
		// frames), not audible sound.
		sp, err := openFileSpeaker(t.TempDir() + "/carol.pcm")
		if err != nil {
			return nil, err
		}
		sp.onPlay = func(pcm []int16) {
			heardMu.Lock()
			heard++
			heardMu.Unlock()
		}
		return sp, nil
	}
	mc.dialIP = "127.0.0.1"
	ec := newEngineWithStun("carol", idc, sigC, engineCallbacks{
		onSignalNote: func(n signalNote) { mc.onSignalNote(n) },
	}, []string{})
	ec.start()
	defer ec.stop()

	// Publisher's roster tick sees carol (and bob).
	ma.PublishTo([]string{"bob", "carol"})
	// Also refresh alice's roster view of carol for handshake verify.
	ma.SetRoster(func() map[string][]byte {
		return map[string][]byte{"bob": idb.publicKey(), "carol": idc.publicKey()}
	})

	deadline := time.Now().Add(60 * time.Second)
	for {
		mc.mu.Lock()
		w := len(mc.pubAudio) > 0
		mc.mu.Unlock()
		if w {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("carol never learned the running publisher")
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("CAROL LEARNED PUBLISHER")
	waitMediaPeer(t, ma, "carol")
	waitMediaPeer(t, mc, "alice")
	fmt.Println("SESSION READY BOTH SIDES")
	deadline = time.Now().Add(20 * time.Second)
	for {
		heardMu.Lock()
		n := heard
		heardMu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("carol never heard audio: heard=%d", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("CAROL HEARD AUDIO")
}
