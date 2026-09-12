package main

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type meshProbe struct {
	mu      sync.Mutex
	recv    map[string][][]byte
	up      map[string]bool
	down    []string
	upCh    chan string
	bytesCh chan struct{}
}

func newMeshProbe() *meshProbe {
	return &meshProbe{
		recv:    map[string][][]byte{},
		up:      map[string]bool{},
		upCh:    make(chan string, 8),
		bytesCh: make(chan struct{}, 64),
	}
}

func (p *meshProbe) callbacks() meshCallbacks {
	return meshCallbacks{
		onBytes: func(peer string, raw []byte) {
			p.mu.Lock()
			p.recv[peer] = append(p.recv[peer], raw)
			p.mu.Unlock()
			select {
			case p.bytesCh <- struct{}{}:
			default:
			}
		},
		onPeerUp: func(peer string) {
			p.mu.Lock()
			p.up[peer] = true
			p.mu.Unlock()
			select {
			case p.upCh <- peer:
			default:
			}
		},
		onPeerDown: func(peer string) {
			p.mu.Lock()
			p.down = append(p.down, peer)
			p.mu.Unlock()
		},
	}
}

// pumpNotes simulates the engine's single signal poll loop: drain my queue,
// forward offer/answer notes into the mesh. Runs until stop closes.
func pumpNotes(sig *signalClient, m *mesh, stop <-chan struct{}, t *testing.T) {
	t.Helper()
	go func() {
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				notes, err := sig.signalPoll()
				if err != nil {
					continue
				}
				for _, n := range notes {
					m.deliver(n)
				}
			}
		}
	}()
}

func waitUp(t *testing.T, p *meshProbe, peer string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		ok := p.up[peer]
		p.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("peer %s never came up", peer)
}

func waitBytes(t *testing.T, p *meshProbe, peer string, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := len(p.recv[peer])
		p.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("only got bytes for %s", peer)
}

// TestMeshLoopback connects two in-process peers through the fake signaling
// server using host candidates only (works fully offline).
func TestMeshLoopback(t *testing.T) {
	srv := httptest.NewServer(newFakeSignalServer())
	defer srv.Close()

	// seed the fake room
	seed := &signalClient{serverURL: srv.URL, me: "seed"}
	sid, err := seed.createRoom("seed", "pub", "")
	if err != nil {
		t.Fatal(err)
	}
	pa := newMeshProbe()
	pb := newMeshProbe()
	ma := newMesh("alice", &signalClient{serverURL: srv.URL, me: "alice", key: sid}, []string{}, pa.callbacks())
	mb := newMesh("bob", &signalClient{serverURL: srv.URL, me: "bob", key: sid}, []string{}, pb.callbacks())
	defer ma.close()
	defer mb.close()

	// register both members (fake server has no pubkey requirement)
	sa := &signalClient{serverURL: srv.URL, me: "alice", key: sid}
	sb := &signalClient{serverURL: srv.URL, me: "bob", key: sid}
	if _, err := sa.joinRoom("alice", "pub-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.joinRoom("bob", "pub-b", ""); err != nil {
		t.Fatal(err)
	}

	ma.ensurePeer("bob")
	mb.ensurePeer("alice")

	stop := make(chan struct{})
	defer close(stop)
	pumpNotes(sa, ma, stop, t)
	pumpNotes(sb, mb, stop, t)

	waitUp(t, pa, "bob")
	waitUp(t, pb, "alice")

	if err := ma.send("bob", []byte("hello bob")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitBytes(t, pb, "alice", 1)

	if err := mb.send("alice", []byte("hello alice")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitBytes(t, pa, "bob", 1)

	pb.mu.Lock()
	got := string(pb.recv["alice"][0])
	pb.mu.Unlock()
	if got != "hello bob" {
		t.Fatalf("wrong payload: %q", got)
	}

	if err := ma.send("ghost", []byte("x")); err == nil {
		t.Fatal("send to unknown peer must fail")
	}

	// ensurePeer is idempotent while up
	ma.ensurePeer("bob")
}

func TestMeshTieBreak(t *testing.T) {
	m := newMesh("bob", nil, nil, meshCallbacks{})
	if m.iOffer("alice") {
		t.Fatal("bob must not offer to alice (alice < bob)")
	}
	if !m.iOffer("carol") {
		t.Fatal("bob must offer to carol (bob < carol)")
	}
	if (&mesh{me: "alice"}).iOffer("alice") {
		t.Fatal("must never offer to self (guarded by ensurePeer anyway)")
	}
}
