package main

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type engineProbe struct {
	mu      sync.Mutex
	chats   []engineChat
	files   []engineFile
	ready   map[string]string // peer -> safety code
	lost    []string
	chatCh  chan engineChat
	readyCh chan string
	fileCh  chan engineFile
}

func newEngineProbe() *engineProbe {
	return &engineProbe{
		ready:   map[string]string{},
		chatCh:  make(chan engineChat, 64),
		readyCh: make(chan string, 8),
		fileCh:  make(chan engineFile, 8),
	}
}

func (p *engineProbe) callbacks() engineCallbacks {
	return engineCallbacks{
		onChat: func(c engineChat) {
			p.mu.Lock()
			p.chats = append(p.chats, c)
			p.mu.Unlock()
			select {
			case p.chatCh <- c:
			default:
			}
		},
		onFile: func(f engineFile) {
			p.mu.Lock()
			p.files = append(p.files, f)
			p.mu.Unlock()
			select {
			case p.fileCh <- f:
			default:
			}
		},
		onPeerReady: func(u, code string) {
			p.mu.Lock()
			p.ready[u] = code
			p.mu.Unlock()
			select {
			case p.readyCh <- u:
			default:
			}
		},
		onPeerLost: func(u string) {
			p.mu.Lock()
			p.lost = append(p.lost, u)
			p.mu.Unlock()
		},
		onError: func(err error) {},
	}
}

func waitReady(t *testing.T, p *engineProbe, peer string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, ok := p.ready[peer]
		p.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("E2E channel to %s never became ready", peer)
}

func waitChat(t *testing.T, p *engineProbe, text string) engineChat {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case c := <-p.chatCh:
			if c.Text == text {
				return c
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("never received chat %q", text)
	return engineChat{}
}

// TestEngineEndToEnd runs two full engines (P2P + Noise + fallback) against
// the fake signaling server over loopback WebRTC.
func TestEngineEndToEnd(t *testing.T) {
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
	roster, _, err := sigB.joinRoom("bob", pubB, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(roster) != 2 {
		t.Fatalf("expected 2 in roster, got %d", len(roster))
	}

	pa, pb := newEngineProbe(), newEngineProbe()
	ea := newEngineWithStun("alice", ida, sigA, pa.callbacks(), []string{})
	eb := newEngineWithStun("bob", idb, sigB, pb.callbacks(), []string{})
	defer ea.stop()
	defer eb.stop()

	members := []rosterMember{
		{Username: "alice", Pubkey: pubA, Online: true},
		{Username: "bob", Pubkey: pubB, Online: true},
	}
	ea.setRoster(members)
	eb.setRoster(members)
	ea.start()
	eb.start()

	// E2E channels both directions (Noise over signal notes, media over DC)
	waitReady(t, pa, "bob")
	waitReady(t, pb, "alice")

	pa.mu.Lock()
	ca := pa.ready["bob"]
	pa.mu.Unlock()
	pb.mu.Lock()
	cb := pb.ready["alice"]
	pb.mu.Unlock()
	if ca == "" || ca != cb {
		t.Fatalf("safety codes differ: %q vs %q", ca, cb)
	}

	// P2P chat roundtrip (ciphertext over the data channel)
	if _, err := ea.sendChat("bob", "hello over p2p"); err != nil {
		t.Fatalf("sendChat: %v", err)
	}
	got := waitChat(t, pb, "hello over p2p")
	if got.From != "alice" || got.To != "bob" {
		t.Fatalf("wrong envelope: %+v", got)
	}

	// P2P file roundtrip with hash verification
	dir := t.TempDir()
	src := filepath.Join(dir, "note.txt")
	content := []byte("engine file payload 123456789")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	// point downloads at temp dir via HOME override is complex; instead
	// assert via callback path existence under real ~/Downloads and clean up
	disp, size, err := ea.sendFile(context.Background(), "bob", src, "note.txt", nil)
	if err != nil {
		t.Fatalf("sendFile: %v", err)
	}
	if disp != "note.txt" || size != int64(len(content)) {
		t.Fatalf("sendFile meta wrong: %q %d", disp, size)
	}
	select {
	case f := <-pb.fileCh:
		if f.Filename != "note.txt" || f.From != "alice" {
			t.Fatalf("wrong file envelope: %+v", f)
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(content) {
			t.Fatal("file content mismatch after reassembly")
		}
		os.Remove(f.Path) // keep ~/Downloads clean
	case <-time.After(60 * time.Second):
		t.Fatal("file never arrived")
	}

	// Kill both data channels: next message must take the inbox fallback
	// (pairwise box, server can't read) and still arrive exactly once.
	ea.mesh.dropPeer("bob")
	eb.mesh.dropPeer("alice")
	time.Sleep(500 * time.Millisecond)
	if _, err := ea.sendChat("bob", "hello over fallback"); err != nil {
		t.Fatalf("fallback sendChat: %v", err)
	}
	got = waitChat(t, pb, "hello over fallback")
	if got.From != "alice" {
		t.Fatalf("wrong fallback envelope: %+v", got)
	}
}

// A file too big for one box with no live peer must fail fast with an
// actionable error — never silently wedge or spray oversized boxes.
func TestEngineBigFileFallbackRefused(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())

	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	if _, err := sigA.createRoom("alice", base64.StdEncoding.EncodeToString(ida.publicKey()), ""); err != nil {
		t.Fatal(err)
	}
	pa := newEngineProbe()
	ea := newEngineWithStun("alice", ida, sigA, pa.callbacks(), []string{})
	defer ea.stop()
	ea.setRoster([]rosterMember{{Username: "bob", Pubkey: pubB, Online: true}})

	big := make([]byte, fallbackFileMax+1)
	src := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(src, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ea.sendFile(context.Background(), "bob", src, "big.bin", nil); err == nil {
		t.Fatal("oversize fallback file must fail fast")
	}
}

// Empty text never reaches the wire (TUI guards too; defense in depth).
func TestEngineSendChatRejectsEmpty(t *testing.T) {
	ida, _ := generateIdentity()
	e := newEngine("alice", ida, &signalClient{}, engineCallbacks{})
	defer e.stop()
	if _, err := e.sendChat("bob", ""); err == nil {
		t.Fatal("empty text must fail")
	}
	if _, err := e.sendChat("", ""); err == nil {
		t.Fatal("empty broadcast must fail")
	}
}

// Pruned while asleep (missed beats): the next beat rejoins automatically
// with the same identity instead of rotting at 403 forever.
func TestEngineAutoRejoinAfterPrune(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	// Bob stays behind so the room survives alice's prune.
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sigA.key}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}
	pa := newEngineProbe()
	ea := newEngineWithStun("alice", ida, sigA, pa.callbacks(), []string{})
	defer ea.stop()
	ea.setRoster([]rosterMember{{Username: "alice", Pubkey: pubA, Online: true}})

	// Simulate a prune: someone (or the sweeper) drops alice server-side.
	kicker := &signalClient{serverURL: srv.URL, me: "alice", key: sigA.key}
	if err := kicker.leaveRoom(); err != nil {
		t.Fatal(err)
	}

	// Next beat must rejoin transparently.
	ea.beatOnce()
	roster, _, err := sigA.heartbeat("", nil)
	if err != nil {
		t.Fatalf("still out after rejoin: %v", err)
	}
	found := false
	for _, m := range roster {
		if m.Username == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatal("engine did not rejoin after prune")
	}
}

func TestLoneUserBroadcastSucceeds(t *testing.T) {
	e := &engine{me: "bob", roster: map[string][]byte{}}
	f := newFrame(frameChat, "id1", "bob", "")
	f.Data = "hello alone"
	// Never synced: empty roster means we haven't learned the room yet —
	// fail loudly so the sender retries instead of dropping silently.
	if err := e.sendFrame("", f); err == nil {
		t.Fatal("unsynced engine must fail loudly on empty roster")
	}
	// Synced but alone: chatting is allowed — the local echo stands,
	// there is simply nobody to fan out to.
	e.setRoster(nil)
	if err := e.sendFrame("", f); err != nil {
		t.Fatalf("lone synced user must chat freely, got %v", err)
	}
}

func TestEpochChangeTriggersRosterRefresh(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	idc, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	pubB := base64.StdEncoding.EncodeToString(idb.publicKey())
	pubC := base64.StdEncoding.EncodeToString(idc.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	sigB := &signalClient{serverURL: srv.URL, me: "bob", key: sigA.key}
	if _, _, err := sigB.joinRoom("bob", pubB, ""); err != nil {
		t.Fatal(err)
	}
	pa := newEngineProbe()
	ea := newEngineWithStun("alice", ida, sigA, pa.callbacks(), []string{})
	defer ea.stop()
	ea.beatOnce() // seeds roster + epoch (bob's join bumped it to 1)
	names := func() map[string]bool {
		out := map[string]bool{}
		for _, m := range ea.peers() {
			out[m.Username] = true
		}
		return out
	}
	if got := names(); len(got) != 2 || !got["bob"] {
		t.Fatalf("seeded presence = %v; want alice+bob", got)
	}
	// Carol joins elsewhere; alice's next inbox poll (2s cadence) sees the
	// epoch move and refreshes immediately — no waiting out the 5s beat.
	sigC := &signalClient{serverURL: srv.URL, me: "carol", key: sigA.key}
	if _, _, err := sigC.joinRoom("carol", pubC, ""); err != nil {
		t.Fatal(err)
	}
	ea.inboxOnce()
	if got := names(); !got["carol"] {
		t.Fatalf("epoch-triggered refresh missed carol: %v", got)
	}
	ea.mu.Lock()
	epoch := ea.lastEpoch
	ea.mu.Unlock()
	if epoch != 2 {
		t.Fatalf("lastEpoch = %d; want 2", epoch)
	}
}

func TestBeatRecordsAndClearsFailures(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()
	ida, _ := generateIdentity()
	pubA := base64.StdEncoding.EncodeToString(ida.publicKey())
	sigA := &signalClient{serverURL: srv.URL, me: "alice"}
	if _, err := sigA.createRoom("alice", pubA, ""); err != nil {
		t.Fatal(err)
	}
	e := newEngine("alice", ida, sigA, engineCallbacks{})
	// Healthy beat records nothing.
	e.beatOnce()
	if err := e.beatErr(); err != nil {
		t.Fatalf("healthy beat must clear failures, got %v", err)
	}
	// Dead server records a down-classified failure.
	e.sig.serverURL = "http://127.0.0.1:1"
	e.beatOnce()
	berr := e.beatErr()
	if berr == nil || !isServerDown(berr) {
		t.Fatalf("dead beat must record a down error, got %v", berr)
	}
	// Recovery clears it.
	e.sig.serverURL = srv.URL
	e.beatOnce()
	if err := e.beatErr(); err != nil {
		t.Fatalf("recovered beat must clear failures, got %v", err)
	}
}
