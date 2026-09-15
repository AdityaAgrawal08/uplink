package main

import (
	"sync"
	"testing"
	"time"
)

// loopbackPair wires two media transports with direct method-call notes
// (no server): deterministic handshake drive over real UDP sockets.
func loopbackPair(t *testing.T) (ma, mb *mediaTransport, ida, idb *identityKey, readyA, readyB chan string, audioA, audioB chan audioPacket, errs chan error) {
	t.Helper()
	rosterFor := func(a, b *identityKey) (map[string][]byte, map[string][]byte) {
		return map[string][]byte{"bob": b.publicKey()}, map[string][]byte{"alice": a.publicKey()}
	}
	var a, b *identityKey
	var err error
	if a, err = generateIdentity(); err != nil {
		t.Fatal(err)
	}
	if b, err = generateIdentity(); err != nil {
		t.Fatal(err)
	}
	ra, rb := rosterFor(a, b)
	return loopbackPairWith(t, a, b, [2]map[string][]byte{ra, rb})
}

func loopbackPairWith(t *testing.T, ida, idb *identityKey, rosters [2]map[string][]byte) (ma, mb *mediaTransport, outA, outB *identityKey, readyA, readyB chan string, audioA, audioB chan audioPacket, errs chan error) {
	t.Helper()
	readyA = make(chan string, 4)
	readyB = make(chan string, 4)
	audioA = make(chan audioPacket, 8)
	audioB = make(chan audioPacket, 8)
	errs = make(chan error, 8)
	var err error
	ma, err = newMediaTransport("alice", ida,
		func(to, typ, payload string) error { mb.onHandshakeNote("alice", typ, payload); return nil },
		func() map[string][]byte { return rosters[0] },
		mediaCallbacks{
			onReady: func(u, _ string) { readyA <- u },
			onError: func(e error) { errs <- e },
			onAudio: func(_ string, pkt audioPacket) { audioA <- pkt },
		})
	if err != nil {
		t.Fatal(err)
	}
	mb, err = newMediaTransport("bob", idb,
		func(to, typ, payload string) error { ma.onHandshakeNote("bob", typ, payload); return nil },
		func() map[string][]byte { return rosters[1] },
		mediaCallbacks{
			onReady: func(u, _ string) { readyB <- u },
			onError: func(e error) { errs <- e },
			onAudio: func(_ string, pkt audioPacket) { audioB <- pkt },
		})
	if err != nil {
		ma.stop()
		t.Fatal(err)
	}
	pa, pb := ma.localAddr(), mb.localAddr()
	if err := ma.dialPeer("bob", "127.0.0.1", pb.Port); err != nil {
		t.Fatal(err)
	}
	if err := mb.dialPeer("alice", "127.0.0.1", pa.Port); err != nil {
		t.Fatal(err)
	}
	ma.start()
	mb.start()
	t.Cleanup(func() { ma.stop(); mb.stop() })
	return ma, mb, ida, idb, readyA, readyB, audioA, audioB, errs
}

func waitMediaReady(t *testing.T, ch chan string, peer string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != peer {
			t.Fatalf("ready for %q; want %q", got, peer)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("media handshake to %s never completed", peer)
	}
}

func TestMediaHandshakeLoopback(t *testing.T) {
	ma, mb, _, _, readyA, readyB, audioA, audioB, _ := loopbackPair(t)
	ma.beginHandshake("bob")
	waitMediaReady(t, readyA, "bob")
	waitMediaReady(t, readyB, "alice")

	if err := ma.sendMedia("bob", mediaKindAudio, encodeAudioPacket(1, 960, []byte("ping"))); err != nil {
		t.Fatal(err)
	}
	if err := mb.sendMedia("alice", mediaKindAudio, encodeAudioPacket(2, 960, []byte("pong"))); err != nil {
		t.Fatal(err)
	}
	select {
	case pkt := <-audioA:
		if string(pkt.Opus) != "pong" || pkt.Seq != 2 {
			t.Fatalf("a got %+v", pkt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("alice never got bob's audio")
	}
	select {
	case pkt := <-audioB:
		if string(pkt.Opus) != "ping" || pkt.Seq != 1 {
			t.Fatalf("b got %+v", pkt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bob never got alice's audio")
	}
}

func TestMediaKeySwapRejected(t *testing.T) {
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	wrong := make([]byte, 32)
	for i := range wrong {
		wrong[i] = byte(i + 1)
	}
	ma, _, _, _, readyA, _, _, _, errs := loopbackPairWith(t, ida, idb,
		[2]map[string][]byte{{"bob": wrong}, {"alice": ida.publicKey()}})
	ma.beginHandshake("bob")
	select {
	case err := <-errs:
		if got := err.Error(); len(got) < 10 || got[:9] != "KEY SWAP " {
			t.Fatalf("wrong error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("key-swap was never flagged")
	}
	select {
	case got := <-readyA:
		t.Fatalf("session went live despite swapped key: %s", got)
	case <-time.After(2 * time.Second):
	}
}

func TestDgramCipherReplay(t *testing.T) {
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	psA, m1, err := beginNoise(ida, "bob", true)
	if err != nil {
		t.Fatal(err)
	}
	psB, _, err := beginNoise(idb, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := psB.stepNoise(m1)
	if err != nil {
		t.Fatal(err)
	}
	m3, err := psA.stepNoise(m2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := psB.stepNoise(m3); err != nil {
		t.Fatal(err)
	}
	snd := &dgramCipher{c: psA.send.Cipher()}
	rcv := &dgramCipher{c: psB.recv.Cipher()}
	nonce, ct, err := snd.seal([]byte("hello media"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := rcv.open(nonce, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "hello media" {
		t.Fatalf("roundtrip wrong: %q", pt)
	}
	if _, err := rcv.open(nonce, ct); err == nil {
		t.Fatal("replay must be rejected")
	}
	if _, err := rcv.open(nonce+1, ct); err == nil {
		t.Fatal("wrong-nonce decrypt must fail")
	}
}

func TestAudioVideoCodecRoundtrip(t *testing.T) {
	ap := encodeAudioPacket(7, 123456, []byte{1, 2, 3})
	got, err := decodeAudioPacket(ap)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 7 || got.Ts != 123456 || len(got.Opus) != 3 {
		t.Fatalf("audio codec wrong: %+v", got)
	}
	if _, err := decodeAudioPacket([]byte{1}); err == nil {
		t.Fatal("short audio must fail")
	}
	vf := encodeVideoFrag(9, 2, 5, 777, []byte{4, 5})
	gotV, err := decodeVideoFrag(vf)
	if err != nil {
		t.Fatal(err)
	}
	if gotV.Seq != 9 || gotV.FragIdx != 2 || gotV.FragTotal != 5 || gotV.Ts != 777 {
		t.Fatalf("video codec wrong: %+v", gotV)
	}
	if _, err := decodeVideoFrag([]byte{1, 2}); err == nil {
		t.Fatal("short frag must fail")
	}
}

func TestNominateSkipsUnroutable(t *testing.T) {
	mkT := func() *mediaTransport {
		ida, _ := generateIdentity()
		m, err := newMediaTransport("x", ida,
			func(to, typ, payload string) error { return nil },
			func() map[string][]byte { return map[string][]byte{} },
			mediaCallbacks{})
		if err != nil {
			t.Fatal(err)
		}
		m.start()
		t.Cleanup(m.stop)
		return m
	}
	prober, peer := mkT(), mkT()
	peerPort := peer.localAddr().Port
	// TEST-NET-1 is unroutable; loopback answers. Must pick loopback.
	winner := prober.nominate([]string{"192.0.2.1", "127.0.0.1"}, peerPort)
	if winner == nil || !winner.IP.IsLoopback() {
		t.Fatalf("nomination picked %v; want loopback", winner)
	}
	// Nothing listening anywhere: nil, not a guess.
	if got := prober.nominate([]string{"192.0.2.1"}, 9); got != nil {
		t.Fatalf("dead candidates must yield nil, got %v", got)
	}
	_ = peer
}

func TestHandshakeRetransmitAndPurge(t *testing.T) {
	ida, _ := generateIdentity()
	var sent []string
	var mu sync.Mutex
	m, err := newMediaTransport("alice", ida,
		func(to, typ, payload string) error {
			mu.Lock()
			sent = append(sent, typ)
			mu.Unlock()
			return nil
		},
		func() map[string][]byte { return map[string][]byte{} },
		mediaCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.stop()
	ps, m1, err := beginNoise(ida, "bob", true)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.peers["bob"] = &mediaPeer{username: "bob", hs: ps, hsAt: time.Now().Add(-10 * time.Second), epoch: 11, hsM1: m1}
	m.hsEpoch["bob"] = 11
	m.mu.Unlock()
	m.beatOnce()
	mu.Lock()
	n := len(sent)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("stalled handshake re-sent %d notes; want 1", n)
	}
	// Aged past TTL: purged, never retried again.
	m.mu.Lock()
	m.peers["bob"].hsAt = time.Now().Add(-61 * time.Second)
	m.mu.Unlock()
	m.beatOnce()
	m.mu.Lock()
	stillHs := m.peers["bob"].hs != nil
	m.mu.Unlock()
	if stillHs {
		t.Fatal("stuck handshake not purged past TTL")
	}
	mu.Lock()
	n2 := len(sent)
	mu.Unlock()
	if n2 != 1 {
		t.Fatalf("purged handshake re-sent: %d total notes", n2)
	}
}

// Screenshot scenario: larger-side responder drops verification on a stale
// roster (silently, no alert), asks for restart; smaller side re-initiates;
// once beats learn the peer, the parked attempt completes. Both live.
func TestVerifyUnknownRecoversViaRestart(t *testing.T) {
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	rosterA := map[string][]byte{"bob": idb.publicKey()}
	rosterB := map[string][]byte{} // stale: beats haven't learned alice
	var mu sync.Mutex
	var restarts int
	var ma, mb *mediaTransport
	var err error
	ma, err = newMediaTransport("alice", ida,
		func(to, typ, payload string) error { mb.onHandshakeNote("alice", typ, payload); return nil },
		func() map[string][]byte { return rosterA },
		mediaCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	mb, err = newMediaTransport("bob", idb,
		func(to, typ, payload string) error {
			if typ == mediaHSRestart {
				mu.Lock()
				restarts++
				mu.Unlock()
			}
			ma.onHandshakeNote("bob", typ, payload)
			return nil
		},
		func() map[string][]byte { return rosterB },
		mediaCallbacks{})
	if err != nil {
		ma.stop()
		t.Fatal(err)
	}
	pa, pb := ma.localAddr(), mb.localAddr()
	if err := ma.dialPeer("bob", "127.0.0.1", pb.Port); err != nil {
		t.Fatal(err)
	}
	if err := mb.dialPeer("alice", "127.0.0.1", pa.Port); err != nil {
		t.Fatal(err)
	}
	ma.start()
	mb.start()
	defer ma.stop()
	defer mb.stop()

	ma.beginHandshake("bob")
	// Alice (smaller, roster complete) goes live; Bob parks unknown.
	deadline := time.Now().Add(10 * time.Second)
	for {
		ma.mu.Lock()
		_, alive := ma.peers["bob"]
		live := alive && ma.peers["bob"].ready
		ma.mu.Unlock()
		if live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alice never went live")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Bob asked for exactly one restart (throttled, not a storm).
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	n := restarts
	mu.Unlock()
	if n != 1 {
		t.Fatalf("restart requests = %d; want exactly 1", n)
	}
	// Beats learn alice; Bob's parked attempt completes without new notes.
	rosterB["alice"] = ida.publicKey()
	mb.beatOnce()
	deadline = time.Now().Add(10 * time.Second)
	for {
		mb.mu.Lock()
		var bread bool
		if p, ok := mb.peers["alice"]; ok {
			bread = p.ready
		}
		mb.mu.Unlock()
		if bread {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bob never went live after roster learned alice")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Media flows both ways now.
	if err := ma.sendMedia("bob", mediaKindAudio, encodeAudioPacket(1, 1, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := mb.sendMedia("alice", mediaKindAudio, encodeAudioPacket(2, 2, []byte("y"))); err != nil {
		t.Fatal(err)
	}
}
