package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

	aliceID := mustTestIdentity(t)
	joiner := &signalClient{serverURL: srv.URL, key: "123456", me: "alice", id: aliceID}
	if _, _, err := joiner.joinRoom("alice", base64.StdEncoding.EncodeToString(aliceID.publicKey()), ""); err != nil {
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

	leaver := &signalClient{serverURL: srv.URL, key: "123456", me: "alice", id: aliceID}
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

	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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
	boxes, _, err := sigB.inboxFetch()
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

// Engine dispatch is at-least-once by design: every copy reaches the
// consumer (which dedups by msgId and acks every copy). Swallowing retries
// here used to permanently lose frames the consumer never consumed.
func TestEnginePassesRetriesThrough(t *testing.T) {
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
	if n != 2 {
		t.Fatalf("consumer got %d copies; want 2 (dedup lives consumer-side)", n)
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
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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
	boxes, _, err := sigB.inboxFetch()
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
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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
	boxes, _, err := sigB.inboxFetch()
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
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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
	badSig := &signalClient{serverURL: "http://127.0.0.1:1", me: "alice", id: dead}
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
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	ea := newEngineWithStun("alice", ida, sigA, engineCallbacks{}, []string{})
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})
	ea.start()
	defer ea.stop()

	// Bob joins a second later; nobody sends anything, ever.
	time.Sleep(1100 * time.Millisecond)
	sigB := &signalClient{serverURL: srv.URL, key: sigA.key, me: "bob", id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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

// Mid-session desync (issue #1 "Yeah" loss): the sender's session stays
// live while the receiver's generation is gone, so a mesh-sent frame is
// dropped on decrypt. The ack backstop must still deliver it — exactly
// once — via the durable inbox path.
func TestMidSessionDesyncRecoversViaBackstop(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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

	// Bob's session generation silently dies (sender still believes live).
	eb.dropNoise("alice")

	if _, err := ea.sendChat("bob", "yeah"); err != nil {
		t.Fatalf("sendChat: %v", err)
	}
	// Fast-forward the backstop: the 3s ack timeout need not gate the test.
	ea.mu.Lock()
	for _, p := range ea.unacked {
		p.sent = time.Now().Add(-time.Hour)
	}
	ea.mu.Unlock()
	// Production-faithful acking: the consumer acks every receipt within
	// ~100ms (like the TUI handler), long before any second retry is due.
	// A manual ack after waitChat would race the 3s retry clock under load.
	stopAck := make(chan struct{})
	defer close(stopAck)
	go func() {
		acked := map[string]bool{}
		for {
			select {
			case <-stopAck:
				return
			default:
			}
			pb.mu.Lock()
			var ids []string
			for _, c := range pb.chats {
				if c.Text == "yeah" && !acked[c.MsgId] {
					acked[c.MsgId] = true
					ids = append(ids, c.MsgId)
				}
			}
			pb.mu.Unlock()
			for _, id := range ids {
				_ = eb.sendAck("alice", id)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	got := waitChat(t, pb, "yeah")
	if got.From != "alice" {
		t.Fatalf("wrong envelope: %+v", got)
	}
	// Settle past a full retry window. A second retry may legitimately be
	// in flight already (worst-case ack round trip spans both 2s inbox
	// polls, exceeding the 3s backstop): production correctness is that
	// the consumer collapses copies by msgId, and the sender eventually
	// graduates when an ack lands — both asserted below.
	time.Sleep(5 * time.Second)
	pb.mu.Lock()
	unique := map[string]bool{}
	for _, c := range pb.chats {
		if c.Text == "yeah" {
			unique[c.MsgId] = true
		}
	}
	pb.mu.Unlock()
	if len(unique) != 1 {
		t.Fatalf("consumer-visible ids = %d; want exactly 1 (deduped display)", len(unique))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ea.mu.Lock()
		pending := len(ea.unacked)
		ea.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backstop never graduated — ack loop not terminating")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestSendMuPerPeer(t *testing.T) {
	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()
	if e.sendMuFor("bob") != e.sendMuFor("bob") {
		t.Fatal("same peer must share one serializer")
	}
	if e.sendMuFor("bob") == e.sendMuFor("carol") {
		t.Fatal("different peers must not share a serializer")
	}
	// Departure sheds serializers with the rest of the residue.
	e.mu.Lock()
	e.roster["bob"] = []byte{1, 2, 3}
	e.mu.Unlock()
	e.setRoster([]rosterMember{})
	e.mu.Lock()
	_, left := e.sendMu["bob"]
	e.mu.Unlock()
	if left {
		t.Fatal("departed peer serializer leaked")
	}
}

func TestBackpressureNoPeer(t *testing.T) {
	a, _ := generateIdentity()
	e := newEngine("alice", a, &signalClient{}, engineCallbacks{})
	defer e.stop()
	if n := e.mesh.bufferedAmount("ghost"); n != 0 {
		t.Fatalf("unknown peer buffered = %d; want 0", n)
	}
	if err := e.awaitDrain(context.Background(), "ghost"); err != nil {
		t.Fatalf("drain with no backlog must pass: %v", err)
	}
	if err := e.awaitDrain(context.Background(), ""); err != nil {
		t.Fatalf("broadcast drain with empty roster must pass: %v", err)
	}
}

func TestTarballDirSizeCap(t *testing.T) {
	dir := t.TempDir()
	// ~30MB of incompressible bytes: must abort during packing, before
	// the 25MB downstream check could ever see it.
	big := make([]byte, 30<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := tarballDir(dir)
	if err == nil {
		os.Remove(out)
		t.Fatal("oversize folder must fail packing, not pack-then-reject")
	}
}

// A retried chat (mesh copy + backstop retry, or a TUI queue redelivery)
// paints exactly once but is acked per copy: the sender retries until it
// hears back, and display stays single.
func TestTuiDedupsRetriedChatButAcksBoth(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	wireTestEngine(t, c, srv, "bob", "alice")
	// Production wiring: ack synchronously at receipt, push lossily.
	c.eng.cb = engineCallbacks{
		onChat: func(ec engineChat) {
			_ = c.eng.sendAck(ec.From, ec.MsgId)
			select {
			case c.netCh <- netChatMsg{chat: ec}:
			default:
			}
		},
	}

	dup := newFrame(frameChat, "dup-9", "alice", "bob")
	dup.Data = "hi again"
	c.eng.dispatch(dup)
	c.eng.dispatch(dup) // same frame twice: retry/redelivery
	// Both copies acked synchronously at receipt (distinct ack ids), so a
	// dropped queue slot still graduates the sender.
	if n := inboxDeposits(fs, "alice"); n != 2 {
		t.Fatalf("ack deposits for alice = %d; want 2 (one per copy)", n)
	}
	for i := 0; i < 2; i++ {
		msg := c.drainNetCmd()()
		nm, _ := c.Update(msg)
		*c = nm.(chatScreen)
	}

	rows := 0
	for _, h := range c.history {
		if h.MsgId == "dup-9" {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("retried chat painted %d rows; want exactly 1", rows)
	}
}

// A retried single-box file must not double-card nor double-list.
func TestTuiDedupsRetriedFileCard(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(40, 10)
	f := netFileMsg{file: engineFile{MsgId: "file-1", From: "alice", Filename: "a.txt", Size: 3, Path: "/tmp/a.txt"}}
	c1, _ := step(c, f)
	c2, _ := step(c1, f)
	cards := 0
	for _, l := range c2.localLines {
		if l.kind == lineFileCard {
			cards++
		}
	}
	if cards != 1 {
		t.Fatalf("retried file carded %d times; want exactly 1", cards)
	}
	if len(c2.received) != 1 {
		t.Fatalf("received drawer has %d entries; want 1", len(c2.received))
	}
}

// Delivery receipts: a confirmed own send dims until the peer ack lands,
// then restores. Stale unacked sends raise the status warning.
func TestDeliveryReceiptDimAndRestore(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)

	sc.settleSend(sendDoneMsg{text: "hi", to: "", seq: 99, msgId: "r1", code: 201})
	if len(sc.unackedUI) != 1 {
		t.Fatalf("confirmed send not tracked: %v", sc.unackedUI)
	}
	own := chatMessage{Seq: 99, MsgId: "r1", Username: "bob", Kind: "chat", Text: "hi", ConvID: generalConv, CreatedAt: "2026-09-15T19:21:00Z"}
	_ = sc.renderedLine(own) // prime the bubble cache
	if _, ok := sc.renderCache[99]; !ok {
		t.Fatal("bubble was not cached")
	}
	// Eviction itself is unit-covered (rebuildView re-caches right after,
	// so absence can never hold post-Update — by design, not a leak).
	sc.evictRenderCache("r1")
	if _, ok := sc.renderCache[99]; ok {
		t.Fatal("evictRenderCache left the dimmed paint cached")
	}
	m2, _ := sc.Update(netDeliveredMsg{msgId: "r1"})
	sc = m2.(chatScreen)
	if len(sc.unackedUI) != 0 {
		t.Fatal("ack did not graduate the receipt")
	}
	// NOTE: the dim itself is asserted structurally (map + eviction): with
	// no TTY attached lipgloss strips the faint SGR, so byte comparison
	// cannot see it here, but bright terminals render Faint distinctly.
}

// Stale unacked sends warn via the status line; ancient ones expire quiet.
func TestUnconfirmedStatusSweep(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	wireTestEngine(t, c, srv, "bob")
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)

	sc.unackedUI = map[string]time.Time{"old": time.Now().Add(-time.Hour)}
	m2, _ := sc.Update(rosterTickMsg{})
	sc = m2.(chatScreen)
	if len(sc.unackedUI) != 0 {
		t.Fatal("hour-old receipt should expire, not nag forever")
	}
	if sc.status != "" {
		t.Fatalf("status should clear with no stale entries, got %q", sc.status)
	}

	sc.unackedUI = map[string]time.Time{"slow": time.Now().Add(-time.Minute)}
	m3, _ := sc.Update(rosterTickMsg{})
	sc = m3.(chatScreen)
	if !strings.Contains(sc.status, "unconfirmed") {
		t.Fatalf("stale receipt must warn, status = %q", sc.status)
	}
}

// Room broadcasts arriving in a DM thread count up instead of vanishing
// silently; returning to the room clears the counter.
func TestRoomUnreadInThread(t *testing.T) {
	c := newFilterScreen("bob", "alice")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	sc := m.(chatScreen)
	if sc.activeConv() == generalConv {
		t.Fatal("fixture must start inside the DM thread")
	}
	sc.handleNewMessage(chatMessage{Seq: 7, MsgId: "b1", Username: "alice", Kind: "chat", Text: "room ping", ConvID: generalConv, CreatedAt: "2026-09-15T19:21:00Z"})
	if sc.roomUnread != 1 {
		t.Fatalf("roomUnread = %d; want 1", sc.roomUnread)
	}
	if h := sc.roomHeaderView(80); !strings.Contains(h, "1 new in room") {
		t.Fatalf("room header must advertise the room backlog: %q", h)
	}
	sc.exitPrivate()
	if sc.roomUnread != 0 {
		t.Fatal("returning to the room must clear the counter")
	}
}

// The room header stays clean: no version, key, or member counts —
// just the room name and kind.
func TestHeaderStaysClean(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	sc := m.(chatScreen)
	h := sc.roomHeaderView(100)
	if !strings.Contains(h, "General") || !strings.Contains(h, "Public Room") {
		t.Fatalf("room header must name the room: %q", h)
	}
	for _, banned := range []string{"v" + normVersion(version), "members", "in call"} {
		if strings.Contains(h, banned) {
			t.Fatalf("room header must not show %q: %q", banned, h)
		}
	}
	// The room code (invite key) must always be visible while in the room.
	sc.key = "ABC123"
	if h := sc.roomHeaderView(100); !strings.Contains(h, "ABC123") {
		t.Fatalf("room header must show the room code: %q", h)
	}
	if h := sc.roomHeaderCompact(100); !strings.Contains(h, "ABC123") {
		t.Fatalf("compact header must show the room code: %q", h)
	}
}

// Fast join-chat race: responder learns the newcomer from live traffic
// (not just beats), with no false KEY SWAP alarm.
func TestUnknownHandshakeNoteRefreshesSilently(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sigA.key, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}

	var errs []string
	var mu sync.Mutex
	ea := newEngine("alice", ida, sigA, engineCallbacks{
		onError: func(err error) {
			mu.Lock()
			errs = append(errs, err.Error())
			mu.Unlock()
		},
	})
	defer ea.stop()
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})

	// noise1 from a peer my snapshot never learned.
	psB, m1, err := beginNoise(idb, "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = psB
	ea.onHandshakeNote(signalNote{From: "bob", Type: noiseSig1, Payload: wrapHs(7, m1)})
	ea.mu.Lock()
	_, known := ea.roster["bob"]
	ea.mu.Unlock()
	if !known {
		t.Fatal("live handshake traffic must refresh a stale roster immediately")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range errs {
		if strings.Contains(e, "KEY SWAP") {
			t.Fatalf("false attack alarm on a routine join: %s", e)
		}
	}
}

// verifyReady with a valid handshake for a not-yet-known peer refreshes
// silently instead of crying attack.
func TestVerifyReadyUnknownPeerNoAlert(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sigA.key, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}

	var errs []string
	var mu sync.Mutex
	ea := newEngine("alice", ida, sigA, engineCallbacks{
		onError: func(err error) {
			mu.Lock()
			errs = append(errs, err.Error())
			mu.Unlock()
		},
	})
	defer ea.stop()
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})

	// Locally completed handshake against the real key, roster still stale.
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
	ea.trackHs("bob", psA, 42)
	ea.verifyReady("bob", psA)
	mu.Lock()
	defer mu.Unlock()
	for _, e := range errs {
		if strings.Contains(e, "KEY SWAP") {
			t.Fatalf("false attack alarm: %s", e)
		}
	}
	ea.mu.Lock()
	_, known := ea.roster["bob"]
	_, penalized := ea.lastFail["bob"]
	ea.mu.Unlock()
	if !known {
		t.Fatal("verifyReady should have refreshed the stale roster")
	}
	if penalized {
		t.Fatal("unknown peer must not carry a retry penalty")
	}
	// The refresh learned bob through setRoster-added, which must have
	// re-initiated the dropped handshake (transport was never torn down).
	ea.mu.Lock()
	_, hs := ea.hs["bob"]
	ea.mu.Unlock()
	if !hs {
		t.Fatal("no re-handshake started after the roster learned bob")
	}
}

// Unknown-sender inbox boxes trigger a refresh and deliver at once.
func TestUnknownInboxSenderRefreshesAndDelivers(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sigA.key, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}
	p := newEngineProbe()
	ea := newEngine("alice", ida, sigA, p.callbacks())
	defer ea.stop()
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})

	f := newFrame(frameChat, "first-1", "bob", "alice")
	f.Data = "hey first"
	raw, _ := encodeFrame(f)
	rawPubB, _ := base64.StdEncoding.DecodeString(pubB)
	_ = rawPubB
	box, err := sealBox(idb, ida.publicKey(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := sigB.inboxSend("alice", "first-1", "p2p", box); err != nil {
		t.Fatal(err)
	}
	ea.inboxOnce()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.chats) != 1 || p.chats[0].Text != "hey first" {
		t.Fatalf("first message from unknown sender not delivered: %+v", p.chats)
	}
}

func TestSignalPayloadTooBig(t *testing.T) {
	if signalPayloadTooBig(strings.Repeat("x", 16*1024)) {
		t.Fatal("exactly at cap must pass")
	}
	if !signalPayloadTooBig(strings.Repeat("x", 16*1024+1)) {
		t.Fatal("over cap must trip")
	}
}

// Peer learned after its channel is already open must still handshake:
// setRoster-added re-initiates instead of waiting for a flap.
func TestRosterLearnedAfterOpenStillHandshakes(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sigA.key, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
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

	// Alice's snapshot is stale (beat predates Bob); transport setup runs
	// ahead of roster knowledge, exactly the fast join-chat race.
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})
	eb.setRoster(members)
	ea.start()
	eb.start()
	ea.mesh.ensurePeer("bob")

	// The beat learns Bob; the handshake must start now, not on next flap.
	deadline := time.Now().Add(15 * time.Second)
	learned := false
	for time.Now().Before(deadline) {
		ea.beatOnce()
		ea.mu.Lock()
		_, hs := ea.hs["bob"]
		_, live := ea.noise["bob"]
		ea.mu.Unlock()
		if hs || live {
			learned = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !learned {
		t.Fatal("no handshake started after the roster learned bob")
	}
	waitReady(t, pa, "bob")
	waitReady(t, pb, "alice")
}

// Faithful screenshot reproduction: two live TUI screens, rapid alternating
// sends with no pacing (exercises pending/outbox/settle paths), real engines
// over loopback. Every sent text must land exactly once in the peer's
// history.
func TestTuiRapidConversationCrossDelivery(t *testing.T) {
	fs := newFakeSignalServer()
	srv := httptest.NewServer(fs)
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice", id: ida}
	sid, err := sigA.createRoom("alice", pubA, "")
	if err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sid, id: idb}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}

	mkScreen := func(me, key string, id *identityKey) *chatScreen {
		sc := newChatScreen(srv.URL, key, me, id, "")
		sc.vp = *viewportPtr(60, 20)
		m, _ := sc.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		out := m.(chatScreen)
		if roster, _, err := out.sig.heartbeat("", nil); err == nil {
			out.eng.setRoster(roster)
			out.users = onlineNames(roster, me)
		}
		out.eng.start()
		return &out
	}
	sa := mkScreen("alice", sid, ida)
	sb := mkScreen("bob", sid, idb)
	defer sa.eng.stop()
	defer sb.eng.stop()

	type send struct {
		to   *chatScreen
		text string
	}
	script := []send{
		{sa, "Hey"}, {sa, "Hello"}, {sb, "hi"}, {sa, "yo"},
		{sb, "Nice to meet you"}, {sa, "how are you"},
	}
	// Fire with no pacing, following only sendDoneMsg chains (promotion).
	// Drain re-arms are endless by design and must NOT be followed here.
	follow := func(sc **chatScreen, cmd tea.Cmd) {
		for depth := 0; cmd != nil && depth < 8; depth++ {
			msg := cmd()
			if msg == nil {
				return
			}
			var out tea.Model
			var next tea.Cmd
			out, next = (*sc).Update(msg)
			**sc = out.(chatScreen)
			if _, ok := msg.(sendDoneMsg); !ok {
				return
			}
			cmd = next
		}
	}
	for _, s := range script {
		follow(&s.to, s.to.submitLine(s.text))
	}
	// Pump both drains until every text is cross-visible exactly once.
	countText := func(sc *chatScreen, text string) int {
		n := 0
		for _, h := range sc.history {
			if h.Text == text {
				n++
			}
		}
		return n
	}
	done := func() bool {
		for _, want := range []string{"Hey", "Hello", "yo", "how are you"} {
			if countText(sb, want) != 1 {
				return false
			}
		}
		for _, want := range []string{"hi", "Nice to meet you"} {
			if countText(sa, want) != 1 {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(40 * time.Second)
	for !done() && time.Now().Before(deadline) {
		for _, sc := range []*chatScreen{sa, sb} {
			for {
				select {
				case m := <-sc.netCh:
					var out tea.Model
					var cmd tea.Cmd
					out, cmd = sc.Update(m)
					*sc = out.(chatScreen)
					follow(&sc, cmd)
				default:
					goto drained
				}
			}
		drained:
		}
		time.Sleep(200 * time.Millisecond)
	}
	dump := func(tag string, sc *chatScreen, peer string) {
		var hs []string
		for _, h := range sc.history {
			hs = append(hs, h.Username+":"+h.Text)
		}
		sc.eng.mu.Lock()
		unacked := len(sc.eng.unacked)
		_, live := sc.eng.noise[peer]
		sc.eng.mu.Unlock()
		t.Logf("%s history=%v outbox=%d pending=%v locals=%d unacked=%d live=%v", tag, hs, len(sc.outbox), sc.pending != nil, len(sc.localLines), unacked, live)
	}
	for _, want := range []string{"Hey", "Hello", "yo", "how are you"} {
		if n := countText(sb, want); n != 1 {
			dump("bob", sb, "alice")
			dump("alice", sa, "bob")
			fs.mu.Lock()
			var boxes []string
			for k, m := range fs.boxes {
				for id := range m {
					boxes = append(boxes, k+"/"+id)
				}
			}
			fs.mu.Unlock()
			t.Logf("server boxes residue: %v", boxes)
			t.Fatalf("bob shows %q %d times; want exactly 1", want, n)
		}
	}
	for _, want := range []string{"hi", "Nice to meet you"} {
		if n := countText(sa, want); n != 1 {
			t.Errorf("alice shows %q %d times; want exactly 1", want, n)
		}
	}
}
