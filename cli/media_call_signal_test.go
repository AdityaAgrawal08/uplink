package main

import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// Full publish stack over the REAL signal path (fake server, real 2s
// polls, real timing): media-live announces → Noise handshake notes →
// media ready. No Call/Accept anywhere — publish is the whole model.
func waitMediaPeer(t *testing.T, m *mediaManager, peer string) {
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

func TestPublishOverSignalPath(t *testing.T) {
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

	var ma, mb *mediaManager
	var muB sync.Mutex
	feedA := make(chan []int16, 64)
	var gotB int
	ma = newMediaManager("alice", ida,
		func(to, typ, payload string) error { return sigA.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} },
		mediaUICallbacks{onInfo: func(i string) { t.Log("A:", i) }})
	mb = newMediaManager("bob", idb,
		func(to, typ, payload string) error { return sigB.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} },
		mediaUICallbacks{onInfo: func(i string) { t.Log("B:", i) }})
	for _, m := range []*mediaManager{ma, mb} {
		m.dialIP = "127.0.0.1"
	}
	ma.micSrc = func() (<-chan []int16, func(), error) { return feedA, func() {}, nil }
	mb.playSink = func() (func([]int16), func(), error) {
		return func([]int16) { muB.Lock(); gotB++; muB.Unlock() }, func() {}, nil
	}

	// Engines drive the signal queue polls (real path, fake server).
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

	// Audio publish only (heavier camera pipeline is covered by the
	// method-loopback + ffmpeg tests): alice publishes, bob hears.
	if err := ma.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	waitMediaPeer(t, ma, "bob")
	waitMediaPeer(t, mb, "alice")

	tone := make([]int16, voiceFrameLen)
	for i := range tone {
		tone[i] = 5000
	}
	for i := 0; i < 40; i++ {
		feedA <- append([]int16(nil), tone...)
	}
	deadline := time.Now().Add(25 * time.Second)
	for {
		muB.Lock()
		n := gotB
		muB.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audio not flowing: heard=%d", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !mb.Hearing() {
		t.Fatal("receiver must be in hearing state")
	}
	if !ma.AudioOn() {
		t.Fatal("publisher must report audio on")
	}
	// Stop publishes: watcher stops hearing.
	if err := ma.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		mb.mu.Lock()
		stopped := len(mb.pubAudio) == 0
		mb.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("media-stop never landed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	ma.stopAll()
	mb.stopAll()
}

// TestAudioEndToEndWithTestMic proves the whole audio chain with real
// capture and real playout: UPLINK_MIC=test (synthetic sine → real
// frameChunker → real Opus) on the publisher, UPLINK_SPEAKER_OUT (raw
// PCM file sink replacing ffplay) on the receiver. Asserts the receiver's
// file actually contains the tone (energy > 0) — the definitive fix proof
// for "audio does not work at all".
func TestAudioEndToEndWithTestMic(t *testing.T) {
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
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid}
	if _, err := sigB.joinRoom("bob", base64.StdEncoding.EncodeToString(idb.publicKey()), ""); err != nil {
		t.Fatal(err)
	}

	// Synthetic capture: the sine source feeds the real pipeline.
	testA := newMediaManager("alice", ida,
		func(to, typ, payload string) error { return sigA.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} },
		mediaUICallbacks{})
	speakerFile := t.TempDir() + "/bob.pcm"
	testB := newMediaManager("bob", idb,
		func(to, typ, payload string) error { return sigB.signalSend(to, typ, payload) },
		func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} },
		mediaUICallbacks{})
	for _, m := range []*mediaManager{testA, testB} {
		m.dialIP = "127.0.0.1"
	}
	testA.micSrc = func() (<-chan []int16, func(), error) {
		mc, err := openTestMic()
		if err != nil {
			return nil, nil, err
		}
		return mc.frames, mc.stop, nil
	}
	var sp *speaker
	testB.playSink = func() (func([]int16), func(), error) {
		s, err := openFileSpeaker(speakerFile)
		if err != nil {
			t.Fatalf("file speaker: %v", err)
		}
		sp = s
		return s.play, s.close, nil
	}

	ea := newEngineWithStun("alice", ida, sigA, engineCallbacks{
		onSignalNote: func(n signalNote) { testA.onSignalNote(n) },
	}, []string{})
	eb := newEngineWithStun("bob", idb, sigB, engineCallbacks{
		onSignalNote: func(n signalNote) { testB.onSignalNote(n) },
	}, []string{})
	ea.start()
	eb.start()
	defer ea.stop()
	defer eb.stop()

	if err := testA.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	waitMediaPeer(t, testA, "bob")
	waitMediaPeer(t, testB, "alice")

	// The sine stream runs by itself; wait for the playout to land in the
	// file (jitter buffer priming + opus decode + writer goroutine).
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := os.Stat(speakerFile)
		if err == nil && info.Size() > 24000 { // ≥ 0.25s of 48k s16le mono
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no playout landed in the speaker file (size=%v)", info)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Energy check: a sine must dominate silence.
	raw, err := os.ReadFile(speakerFile)
	if err != nil {
		t.Fatal(err)
	}
	energy := 0
	for i := 0; i+1 < len(raw); i += 2 {
		v := int(int16(uint16(raw[i]) | uint16(raw[i+1])<<8))
		energy += v * v / 65536
	}
	if energy < 1000 {
		t.Fatalf("playout file has no signal (energy=%d)", energy)
	}
	// Stop: the file speaker closes cleanly (no hang).
	if err := testA.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	testA.stopAll()
	testB.stopAll()
	_ = sp
}
