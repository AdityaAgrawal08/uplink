package main

import (
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type engineProbe struct {
	mu       sync.Mutex
	chats    []engineChat
	files    []engineFile
	ready    map[string]string // peer -> safety code
	lost     []string
	chatCh   chan engineChat
	readyCh  chan string
	fileCh   chan engineFile
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
	roster, err := sigB.joinRoom("bob", pubB, "")
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
	disp, size, err := ea.sendFile("bob", src, "note.txt", nil)
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
