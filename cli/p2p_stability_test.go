package main

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHsEnvelopeRoundtrip(t *testing.T) {
	epoch := time.Now().UnixNano()
	s := wrapHs(epoch, []byte("handshake-bytes"))
	gotEpoch, gotMsg, err := unwrapHs(s)
	if err != nil {
		t.Fatal(err)
	}
	if gotEpoch != epoch || string(gotMsg) != "handshake-bytes" {
		t.Fatalf("roundtrip mismatch: %d %q", gotEpoch, gotMsg)
	}
	if _, _, err := unwrapHs("not-json"); err == nil {
		t.Fatal("corrupt envelope must fail")
	}
	if _, _, err := unwrapHs(`{"epoch":1,"data":"%%%"}`); err == nil {
		t.Fatal("bad base64 must fail")
	}
}

// A duplicate noise1 (same epoch, e.g. signal-queue redelivery) arriving
// after handshake completion must not touch the live session.
func TestDuplicateNoise1IgnoredAfterCompletion(t *testing.T) {
	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()

	// fake a completed session + epoch
	ps, _, _ := beginNoise(a, "bob", true)
	e.mu.Lock()
	e.noise["bob"] = ps // not ready, but present...
	e.completedEpoch["bob"] = 100
	e.mu.Unlock()

	// duplicate of the completed epoch with garbage payload: must be
	// ignored BEFORE any crypto (no panic, no state change)
	e.onHandshakeNote(signalNote{From: "bob", Type: noiseSig1, Payload: wrapHs(100, []byte("junk"))})
	e.mu.Lock()
	_, stillThere := e.noise["bob"]
	_, hsCreated := e.hs["bob"]
	e.mu.Unlock()
	if !stillThere {
		t.Fatal("live session torn down by duplicate note")
	}
	if hsCreated {
		t.Fatal("duplicate note spawned a competing handshake")
	}
}

// A stale noise2 (older epoch than the tracked attempt) must be ignored.
func TestStaleNoise2Ignored(t *testing.T) {
	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()

	b, _ := generateIdentity()
	_ = b
	peer := "bob"
	ps, _, err := beginNoise(a, peer, true)
	if err != nil {
		t.Fatal(err)
	}
	e.trackHs(peer, ps, 200)
	// stale epoch for the same peer: must not disturb the tracked attempt
	e.onHandshakeNote(signalNote{From: peer, Type: noiseSig2, Payload: wrapHs(199, []byte("junk"))})
	e.mu.Lock()
	_, stillTracked := e.hs[peer]
	epoch := e.hsEpoch[peer]
	e.mu.Unlock()
	if !stillTracked || epoch != 200 {
		t.Fatal("stale note disturbed the tracked handshake")
	}
	e.dropNoise(peer)
}

func TestPeerDownExactlyOnce(t *testing.T) {
	fires := 0
	m := newMesh("me", &signalClient{}, []string{}, meshCallbacks{
		onPeerDown: func(string) { fires++ },
	})
	defer m.close()
	mp := &meshPeer{username: "x"}
	m.mu.Lock()
	m.peers["x"] = mp
	m.mu.Unlock()
	m.peerDown(mp)
	m.peerDown(mp) // stale repeat: must be silent
	if fires != 1 {
		t.Fatalf("onPeerDown fired %d times; want exactly 1", fires)
	}
}

func TestSendGateNoChannel(t *testing.T) {
	m := newMesh("me", &signalClient{}, []string{}, meshCallbacks{})
	defer m.close()
	if err := m.send("ghost", []byte("x")); err == nil {
		t.Fatal("send with no entry must fail fast (fallback trigger)")
	}
	m.mu.Lock()
	m.peers["half"] = &meshPeer{username: "half"} // entry, no pc/dc yet
	m.mu.Unlock()
	if err := m.send("half", []byte("x")); err == nil {
		t.Fatal("send during setup must fail fast (fallback trigger)")
	}
}

func TestSetupBackoffGating(t *testing.T) {
	old := setupRetryBackoff
	setupRetryBackoff = time.Hour // force the gate shut for the test
	defer func() { setupRetryBackoff = old }()

	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()

	// First drop retries on the next beat (transient-friendly): a lone
	// recent lastFail with no flap history must NOT gate.
	e.mu.Lock()
	e.lastFail["bob"] = time.Now()
	e.mu.Unlock()
	e.reconcilePeers([]rosterMember{{Username: "bob"}})
	if !e.mesh.hasPeer("bob") {
		t.Fatal("first failure must retry immediately, not sit out backoff")
	}
	e.mesh.dropPeer("bob")

	// Repeated quick failures throttle: failCount>=2 inside the window gates.
	e.mu.Lock()
	e.lastFail["bob"] = time.Now()
	e.failCount["bob"] = 2
	e.mu.Unlock()
	e.reconcilePeers([]rosterMember{{Username: "bob"}})
	if e.mesh.hasPeer("bob") {
		t.Fatal("reconcile must not re-attempt inside the backoff window")
	}

	e.mu.Lock()
	e.lastFail["bob"] = time.Now().Add(-2 * time.Hour)
	e.mu.Unlock()
	e.reconcilePeers([]rosterMember{{Username: "bob"}})
	if !e.mesh.hasPeer("bob") {
		t.Fatal("reconcile must attempt after backoff expiry")
	}
	e.mesh.dropPeer("bob")
}

func TestFlapCounting(t *testing.T) {
	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()

	e.onMeshDown("bob") // unknown peer: still records the drop
	e.mu.Lock()
	c1 := e.failCount["bob"]
	e.mu.Unlock()
	if c1 != 1 {
		t.Fatalf("first drop failCount = %d; want 1", c1)
	}
	e.onMeshDown("bob")
	e.mu.Lock()
	c2 := e.failCount["bob"]
	e.mu.Unlock()
	if c2 != 2 {
		t.Fatalf("second quick drop failCount = %d; want 2", c2)
	}
	// Quiet for a minute resets the counter to 1 (no throttle debt across
	// unrelated incidents).
	e.mu.Lock()
	e.lastFail["bob"] = time.Now().Add(-2 * time.Minute)
	e.mu.Unlock()
	e.onMeshDown("bob")
	e.mu.Lock()
	c3 := e.failCount["bob"]
	e.mu.Unlock()
	if c3 != 1 {
		t.Fatalf("drop after quiet failCount = %d; want 1", c3)
	}
}

func TestDepartedUserCleanup(t *testing.T) {
	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()
	e.mu.Lock()
	e.completedEpoch["bob"] = 123
	e.lastFail["bob"] = time.Now()
	e.failCount["bob"] = 2
	e.announced["bob"] = true
	e.roster["bob"] = []byte{1, 2, 3}
	e.mu.Unlock()
	// bob vanishes from the server roster (pruned): all residue goes.
	e.setRoster([]rosterMember{})
	e.mu.Lock()
	_, ce := e.completedEpoch["bob"]
	_, lf := e.lastFail["bob"]
	_, fc := e.failCount["bob"]
	_, an := e.announced["bob"]
	e.mu.Unlock()
	if ce || lf || fc || an {
		t.Fatal("departed user must leave no epoch/penalty/announcement residue")
	}
}

func TestResolveStunURLs(t *testing.T) {
	if got := resolveStunURLs([]string{}); len(got) != 0 {
		t.Fatalf("explicit empty must stay empty (loopback), got %v", got)
	}
	if got := resolveStunURLs(nil); len(got) != 1 || got[0] != stunServer {
		t.Fatalf("nil without env must default, got %v", got)
	}
	t.Setenv("UPLINK_STUN", "stun:a:3478, stun:b:3478 ,,")
	if got := resolveStunURLs(nil); len(got) != 2 || got[0] != "stun:a:3478" || got[1] != "stun:b:3478" {
		t.Fatalf("env parse wrong: %v", got)
	}
	t.Setenv("UPLINK_STUN", "   , ,")
	if got := resolveStunURLs(nil); len(got) != 1 || got[0] != stunServer {
		t.Fatalf("blank env must fall back to default, got %v", got)
	}
}

// Roster ticks announce membership deltas as transcript system lines and
// repaint immediately (appendLocal rebuilds). Regression: the tick used to
// update c.users with no rebuild, so the sidebar visibly refreshed only
// when the next message triggered a repaint.
func TestRosterTickAnnouncesJoinsLeaves(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob")

	joiner := &signalClient{serverURL: srv.URL, key: "123456", me: "alice"}
	if _, err := joiner.joinRoom("alice", base64.StdEncoding.EncodeToString(make([]byte, 32)), ""); err != nil {
		t.Fatal(err)
	}
	c.eng.beatOnce()
	before := len(c.localLines)
	m, _ := c.Update(rosterTickMsg{})
	*c = m.(chatScreen)
	if len(c.users) != 2 {
		t.Fatalf("roster after join = %v; want [bob alice]", c.users)
	}
	found := false
	for _, l := range c.localLines[before:] {
		if l.text == "* alice joined" {
			found = true
		}
	}
	if !found {
		t.Fatal("join produced no '* alice joined' system line")
	}

	leaver := &signalClient{serverURL: srv.URL, key: "123456", me: "alice"}
	if err := leaver.leaveRoom(); err != nil {
		t.Fatal(err)
	}
	c.eng.beatOnce()
	before = len(c.localLines)
	m, _ = c.Update(rosterTickMsg{})
	*c = m.(chatScreen)
	if len(c.users) != 1 {
		t.Fatalf("roster after leave = %v; want [bob]", c.users)
	}
	found = false
	for _, l := range c.localLines[before:] {
		if l.text == "* alice left" {
			found = true
		}
	}
	if !found {
		t.Fatal("leave produced no '* alice left' system line")
	}

	// Steady state: no membership change, no announcement spam.
	before = len(c.localLines)
	m, _ = c.Update(rosterTickMsg{})
	*c = m.(chatScreen)
	if len(c.localLines) != before {
		t.Fatal("steady-state tick must not append system lines")
	}
}

// A peer restarting with the same identity (app relaunch) must reconverge:
// the survivor drops the dead transport, re-handshakes at a newer epoch,
// and chat flows again — over a live Noise session, not just inbox fallback.
func TestEnginePeerRestartConverges(t *testing.T) {
	oldGrace := disconnectGraceTimeout
	disconnectGraceTimeout = 200 * time.Millisecond // shrink flap detection; restored after
	defer func() { disconnectGraceTimeout = oldGrace }()

	srv := httptest.NewServer(newFakeSignalServer())
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

	members := []rosterMember{
		{Username: "alice", Pubkey: pubA, Online: true},
		{Username: "bob", Pubkey: pubB, Online: true},
	}
	pa, pb := newEngineProbe(), newEngineProbe()
	ea := newEngineWithStun("alice", ida, sigA, pa.callbacks(), []string{})
	eb := newEngineWithStun("bob", idb, sigB, pb.callbacks(), []string{})
	defer ea.stop()
	defer eb.stop()
	ea.setRoster(members)
	eb.setRoster(members)
	ea.start()
	eb.start()
	waitReady(t, pa, "bob")
	waitReady(t, pb, "alice")

	// Bob relaunches: same identity, fresh engine.
	eb.stop()
	pb2 := newEngineProbe()
	eb2 := newEngineWithStun("bob", idb, sigB, pb2.callbacks(), []string{})
	defer eb2.stop()
	eb2.setRoster(members)
	eb2.start()

	// Responder side completes the new handshake...
	waitReady(t, pb2, "alice")

	// ...and the survivor holds a live session again (proves the newer
	// epoch replaced the dead generation instead of flapping).
	deadline := time.Now().Add(90 * time.Second)
	for {
		ea.mu.Lock()
		ps, ok := ea.noise["bob"]
		live := ok && ps.isReady()
		ea.mu.Unlock()
		if live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("alice never re-established a live session to restarted bob")
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Chat flows both directions after the restart.
	if _, err := ea.sendChat("bob", "after restart"); err != nil {
		t.Fatalf("sendChat after restart: %v", err)
	}
	if got := waitChat(t, pb2, "after restart"); got.From != "alice" {
		t.Fatalf("wrong envelope after restart: %+v", got)
	}
	if _, err := eb2.sendChat("alice", "bob is back"); err != nil {
		t.Fatalf("reply after restart: %v", err)
	}
	if got := waitChat(t, pa, "bob is back"); got.From != "bob" {
		t.Fatalf("wrong reply envelope: %+v", got)
	}
}
