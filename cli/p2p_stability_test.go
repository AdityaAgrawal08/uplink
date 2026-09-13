package main

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
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

// Roster ticks update the Online sidebar silently: membership lives ONLY
// in the sidebar, never as transcript lines. The tick must still repaint
// immediately on change (rebuildView), not on the next message.
func TestRosterTickSilentSidebar(t *testing.T) {
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
	for _, l := range c.localLines[before:] {
		if strings.Contains(l.text, "alice joined") {
			t.Fatal("join must not print a transcript line (sidebar only)")
		}
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
	for _, l := range c.localLines[before:] {
		if strings.Contains(l.text, "alice left") {
			t.Fatal("leave must not print a transcript line (sidebar only)")
		}
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

// The ack backstop: mesh-sent frames unacked past ackTimeout are re-sent via
// the durable inbox; acks graduate entries; exhausted/peer-gone entries die.
func TestAckBackstopRetryAndGraduate(t *testing.T) {
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

	pa := newEngineProbe()
	ea := newEngine("alice", ida, sigA, pa.callbacks())
	defer ea.stop()
	members := []rosterMember{
		{Username: "alice", Pubkey: pubA, Online: true},
		{Username: "bob", Pubkey: pubB, Online: true},
	}
	ea.setRoster(members)

	backdate := func(msgId string) {
		ea.mu.Lock()
		ea.unacked[unackedKey(msgId, "bob")].sent = time.Now().Add(-time.Hour)
		ea.mu.Unlock()
	}

	// Overdue entry is re-sent through the inbox (durable, re-sealed).
	f := newFrame(frameChat, "m1", "alice", "bob")
	f.Data = "hi"
	ea.trackUnacked("bob", f)
	backdate("m1")
	ea.retryOnce()
	boxes, err := sigB.inboxFetch()
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 {
		t.Fatalf("retry deposited %d boxes; want 1", len(boxes))
	}
	rawPubA, _ := base64.StdEncoding.DecodeString(pubA)
	pt, err := openBox(idb, rawPubA, boxes[0].Payload)
	if err != nil {
		t.Fatalf("retry box undecryptable: %v", err)
	}
	rf, err := decodeFrame(pt)
	if err != nil || rf.Type != frameChat || rf.MsgId != "m1" || rf.Data != "hi" {
		t.Fatalf("retry box wrong: %+v err=%v", rf, err)
	}
	ea.mu.Lock()
	tries := ea.unacked[unackedKey("m1", "bob")].tries
	ea.mu.Unlock()
	if tries != 1 {
		t.Fatalf("tries = %d; want 1", tries)
	}

	// Fresh entries are left alone.
	f2 := newFrame(frameChat, "m2", "alice", "bob")
	f2.Data = "fresh"
	ea.trackUnacked("bob", f2)
	ea.retryOnce()
	if n := inboxDeposits(fs, "bob"); n != 1 {
		t.Fatalf("fresh entry re-sent: %d boxes; want 1", n)
	}

	// Delivery ack graduates the entry (correlated by original id in Data).
	ack := newFrame(frameAck, "ack-1", "bob", "alice")
	ack.Data = "m1"
	ea.dispatch(ack)
	ea.mu.Lock()
	_, still := ea.unacked[unackedKey("m1", "bob")]
	ea.mu.Unlock()
	if still {
		t.Fatal("acked entry not graduated")
	}

	// Exhausted entries die quietly (no further re-send).
	ea.mu.Lock()
	ea.unacked[unackedKey("m9", "bob")] = &pendingAck{to: "bob", f: f, sent: time.Now().Add(-time.Hour), tries: maxInboxRetries}
	ea.mu.Unlock()
	ea.retryOnce()
	if n := inboxDeposits(fs, "bob"); n != 1 {
		t.Fatalf("exhausted entry re-sent: %d boxes; want 1", n)
	}
	ea.mu.Lock()
	_, ghost := ea.unacked[unackedKey("m9", "bob")]
	ea.mu.Unlock()
	if ghost {
		t.Fatal("exhausted entry not dropped")
	}

	// Peer-gone entries die quietly.
	ea.mu.Lock()
	ea.unacked[unackedKey("mx", "ghost")] = &pendingAck{to: "ghost", f: f, sent: time.Now().Add(-time.Hour)}
	ea.mu.Unlock()
	ea.retryOnce() // must not panic or send
	ea.mu.Lock()
	_, left := ea.unacked[unackedKey("mx", "ghost")]
	ea.mu.Unlock()
	if left {
		t.Fatal("peer-gone entry not dropped")
	}
}

func TestInboundChatDedup(t *testing.T) {
	a, _ := generateIdentity()
	p := newEngineProbe()
	e := newEngine("alice", a, &signalClient{}, p.callbacks())
	defer e.stop()
	f := newFrame(frameChat, "dup-1", "bob", "alice")
	f.Data = "hello"
	e.dispatch(f)
	e.dispatch(f) // mesh delivery + backstop retry of the same frame
	p.mu.Lock()
	n := len(p.chats)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("duplicate chat displayed %d times; want exactly 1", n)
	}
}

func TestSendAckNamesOriginal(t *testing.T) {
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
	ea := newEngine("alice", ida, sigA, engineCallbacks{})
	defer ea.stop()
	ea.setRoster([]rosterMember{
		{Username: "alice", Pubkey: pubA, Online: true},
		{Username: "bob", Pubkey: pubB, Online: true},
	})
	// No live session: ack travels the inbox path.
	if err := ea.sendAck("bob", "orig-99"); err != nil {
		t.Fatal(err)
	}
	boxes, err := sigB.inboxFetch()
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 {
		t.Fatalf("ack boxes = %d; want 1", len(boxes))
	}
	rawPubA, _ := base64.StdEncoding.DecodeString(pubA)
	pt, err := openBox(idb, rawPubA, boxes[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	af, err := decodeFrame(pt)
	if err != nil || af.Type != frameAck || af.Data != "orig-99" {
		t.Fatalf("ack frame wrong: %+v err=%v", af, err)
	}
}

// Mesh send failing AFTER Noise encrypt must tear the session down (the send
// nonce advanced without the peer receiving — keeping the session would
// poison every future frame) and still deliver durably via inbox.
func TestMeshFailTeardownAndInboxFallback(t *testing.T) {
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

	// Drive a local XX handshake to a READY initiator session (no network).
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
	if !psA.isReady() || !psB.isReady() {
		t.Fatal("local handshake did not reach ready")
	}

	ea := newEngine("alice", ida, sigA, engineCallbacks{})
	defer ea.stop()
	ea.setRoster([]rosterMember{
		{Username: "alice", Pubkey: pubA, Online: true},
		{Username: "bob", Pubkey: pubB, Online: true},
	})
	ea.mu.Lock()
	ea.noise["bob"] = psA
	ea.mu.Unlock()
	// Mesh entry present but with no open channel: encrypt succeeds, send fails.
	ea.mesh.mu.Lock()
	ea.mesh.peers["bob"] = &meshPeer{username: "bob"}
	ea.mesh.mu.Unlock()

	f := newFrame(frameChat, "m-teardown", "alice", "bob")
	f.Data = "via fallback"
	raw, _ := encodeFrame(f)
	if err := ea.sendOne("bob", f, raw); err != nil {
		t.Fatalf("sendOne must fall back, not fail: %v", err)
	}
	// Poisoned session torn down...
	ea.mu.Lock()
	_, live := ea.noise["bob"]
	_, tracked := ea.unacked[unackedKey("m-teardown", "bob")]
	ea.mu.Unlock()
	if live {
		t.Fatal("desynced session kept — future mesh frames would fail decrypt")
	}
	if tracked {
		t.Fatal("inbox-first delivery must not arm the ack backstop")
	}
	if ea.mesh.hasPeer("bob") {
		t.Fatal("dead transport entry kept — reconcile would stall on it")
	}
	// ...and the frame still delivered durably.
	boxes, err := sigB.inboxFetch()
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 || boxes[0].MsgId != "m-teardown" {
		t.Fatalf("fallback boxes wrong: %+v", boxes)
	}
}

// Stream frames must never take the inbox fallback: the server keys boxes
// by msgId, so a chunk would overwrite its siblings and the file could
// never complete. Loud error instead (sender retries on a fresh line).
func TestStreamFramesNeverInbox(t *testing.T) {
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
	ea := newEngine("alice", ida, sigA, engineCallbacks{})
	defer ea.stop()
	ea.setRoster([]rosterMember{
		{Username: "alice", Pubkey: pubA, Online: true},
		{Username: "bob", Pubkey: pubB, Online: true},
	})

	// No live session: stream chunk must fail, not inbox-corrupt.
	chunk := newFrame(frameFileChunk, "stream-1", "alice", "bob")
	chunk.Data = base64.StdEncoding.EncodeToString([]byte("x"))
	raw, _ := encodeFrame(chunk)
	if err := ea.sendOne("bob", chunk, raw); err == nil {
		t.Fatal("stream chunk without a live line must fail loud")
	}
	meta := newFrame(frameFileMeta, "stream-1", "alice", "bob")
	rawMeta, _ := encodeFrame(meta)
	if err := ea.sendOne("bob", meta, rawMeta); err == nil {
		t.Fatal("stream meta without a live line must fail loud")
	}
	if n := inboxDeposits(fs, "bob"); n != 0 {
		t.Fatalf("stream frames reached the inbox (%d boxes) — msgId collision", n)
	}

	// Chat still falls back fine on the same dead line.
	chat := newFrame(frameChat, "c1", "alice", "bob")
	chat.Data = "hi"
	rawChat, _ := encodeFrame(chat)
	if err := ea.sendOne("bob", chat, rawChat); err != nil {
		t.Fatalf("chat fallback must work: %v", err)
	}
	if n := inboxDeposits(fs, "bob"); n != 1 {
		t.Fatalf("chat fallback boxes = %d; want 1", n)
	}
}

// A failed inbox deposit arms the backstop instead of losing the frame.
func TestInboxFailureArmsBackstop(t *testing.T) {
	dead, _ := generateIdentity()
	// Server URL that refuses connections: every HTTP call fails.
	badSig := &signalClient{serverURL: "http://127.0.0.1:1", me: "alice"}
	e := newEngine("alice", dead, badSig, engineCallbacks{})
	defer e.stop()
	peerId, _ := generateIdentity()
	e.mu.Lock()
	e.roster["bob"] = peerId.publicKey()
	e.mu.Unlock()

	f := newFrame(frameChat, "m-fail", "alice", "bob")
	f.Data = "important"
	raw, _ := encodeFrame(f)
	if err := e.sendOne("bob", f, raw); err == nil {
		t.Fatal("dead server must surface an inbox error")
	}
	e.mu.Lock()
	_, tracked := e.unacked[unackedKey("m-fail", "bob")]
	e.mu.Unlock()
	if !tracked {
		t.Fatal("failed inbox deposit must arm the ack backstop")
	}
}

// Join visibility without anyone sending: a newcomer must appear in the
// survivor's presence through beats alone, within a few beat intervals.
func TestRosterVisibilityWithoutSends(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	ea := newEngineWithStun("alice", ida, sigA, engineCallbacks{}, []string{})
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})
	ea.start()
	defer ea.stop()

	// Bob joins a second later; nobody sends anything, ever.
	time.Sleep(1100 * time.Millisecond)
	sigB := &signalClient{serverURL: srv.URL, key: sigA.key, me: "bob"}
	if _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}
	joinedAt := time.Now()
	deadline := joinedAt.Add(15 * time.Second)
	for {
		ea.mu.Lock()
		var names []string
		for _, m := range ea.presence {
			if m.Online {
				names = append(names, m.Username)
			}
		}
		ea.mu.Unlock()
		found := false
		for _, n := range names {
			if n == "bob" {
				found = true
			}
		}
		if found {
			t.Logf("bob visible after %v", time.Since(joinedAt).Round(100*time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bob never appeared in alice's presence (had %v)", names)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
