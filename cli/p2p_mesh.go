package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// ─── WebRTC mesh transport ──────────────────────────────────────────────────
//
// One PeerConnection per peer, one ordered-reliable data channel ("uplink")
// each. This layer moves opaque bytes; Noise encryption, framing, and
// fallback live above it (the engine). Design choices:
//
//   - Deterministic roles kill glare: the lexicographically SMALLER username
//     always offers, the larger always answers. Neither side ever races.
//   - Non-trickle ICE: offer/answer carry all candidates (one signal note
//     each). Slightly slower setup (~1-2s for STUN), vastly simpler and
//     immune to candidate-queue races.
//   - Public STUN only (no self-hosted TURN per frozen constraints).
//     Symmetric-NAT pairs fail setup here and the engine falls back to the
//     serverless inbox relay — degraded but connected.
//   - Setup is one-shot per peer with a hard timeout; drops mid-session
//     surface as onPeerDown and the engine re-runs setup (fresh PC + fresh
//     Noise handshake, which also bounds forward-secrecy windows).

const (
	meshLabel        = "uplink"
	meshSetupTimeout = 30 * time.Second
	meshPollEvery    = 1500 * time.Millisecond
	stunServer       = "stun:stun.l.google.com:19302"
)

// disconnectGraceTimeout is how long a Disconnected PC may flap before we
// declare it dead. Disconnected is TRANSIENT (NAT rebinding, brief radio
// loss) — the old code treated it as terminal, tearing down healthy
// channels and churning full re-handshakes. Overridable in tests.
var disconnectGraceTimeout = 10 * time.Second

// resolveStunURLs picks ICE servers: explicit list wins (tests pass an
// empty slice for pure loopback), then UPLINK_STUN (comma-separated),
// then the public Google default. Production-grade override without
// recompiling: UPLINK_STUN="stun:host1:3478,stun:host2:3478".
func resolveStunURLs(explicit []string) []string {
	if explicit != nil {
		return explicit
	}
	if env, ok := os.LookupEnv("UPLINK_STUN"); ok {
		var out []string
		for _, s := range strings.Split(env, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{stunServer}
}

type meshCallbacks struct {
	onBytes    func(peer string, raw []byte) // inbound data-channel payload
	onPeerUp   func(peer string)             // channel open, ready to send
	onPeerDown func(peer string)             // failed/closed/timed-out
}

type meshPeer struct {
	username string
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
}

type mesh struct {
	me       string
	sig      *signalClient
	stunURLs []string // override for tests (empty = host candidates only)
	cb       meshCallbacks
	mu       sync.Mutex
	peers    map[string]*meshPeer
	// noteQs routes server-drained signaling notes to the setup goroutine
	// waiting for them. The engine owns the ONLY signal poll loop (drain
	// clears the server queue, so two pollers would steal each other's
	// notes); it forwards offer/answer notes here.
	noteQs map[string]chan signalNote
	closed bool
}

func newMesh(me string, sig *signalClient, stun []string, cb meshCallbacks) *mesh {
	return &mesh{
		me:       me,
		sig:      sig,
		stunURLs: resolveStunURLs(stun),
		cb:       cb,
		peers:    map[string]*meshPeer{},
		noteQs:   map[string]chan signalNote{},
	}
}

// iOffer reports whether I am the offerer for this pair (deterministic).
func (m *mesh) iOffer(peer string) bool {
	return m.me < peer
}

// ensurePeer starts setup for a peer exactly once; concurrent and repeat
// calls are no-ops while setup is in flight or the channel is up.
func (m *mesh) ensurePeer(username string) {
	if username == m.me {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if _, ok := m.peers[username]; ok {
		m.mu.Unlock()
		return
	}
	mp := &meshPeer{username: username}
	m.peers[username] = mp
	m.mu.Unlock()
	go m.setupPeer(mp)
}

// send transmits bytes to an OPEN channel. Fails fast otherwise — the
// engine treats failure as "use inbox fallback", never blocks here.
// The connection-state gate is load-bearing: without it, bytes written
// into a half-open (dead but not yet reaped) channel vanish silently with
// no fallback, because Send itself may still succeed into kernel buffers.
func (m *mesh) send(peer string, raw []byte) error {
	m.mu.Lock()
	mp, ok := m.peers[peer]
	var dc *webrtc.DataChannel
	var pc *webrtc.PeerConnection
	if ok {
		dc, pc = mp.dc, mp.pc
	}
	m.mu.Unlock()
	if !ok || dc == nil || pc == nil {
		return fmt.Errorf("no open channel to %s", peer)
	}
	if dc.ReadyState() != webrtc.DataChannelStateOpen ||
		pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return fmt.Errorf("channel to %s not connected", peer)
	}
	return dc.Send(raw)
}

// deliver routes a drained offer/answer note to the setup goroutine for
// that peer. Only the engine calls this. Unknown or finished peers are
// ignored; a full queue drops (setup timeout covers the loss).
func (m *mesh) deliver(n signalNote) {
	if n.Type != "offer" && n.Type != "answer" {
		return
	}
	m.mu.Lock()
	q, ok := m.noteQs[n.From]
	if !ok {
		if _, setup := m.peers[n.From]; !setup {
			m.mu.Unlock()
			return
		}
		q = make(chan signalNote, 16)
		m.noteQs[n.From] = q
	}
	m.mu.Unlock()
	select {
	case q <- n:
	default:
	}
}

// dropQueue discards a peer's note queue (setup finished or failed).
func (m *mesh) dropQueue(peer string) {
	m.mu.Lock()
	delete(m.noteQs, peer)
	m.mu.Unlock()
}

// queueFor returns (creating) the routed-note queue for a peer in setup.
func (m *mesh) queueFor(peer string) chan signalNote {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.noteQs[peer]
	if !ok {
		q = make(chan signalNote, 16)
		m.noteQs[peer] = q
	}
	return q
}

// hasPeer reports whether a peer has a live setup entry (in flight or up).
// The engine uses it to avoid churning setups that are still working.
func (m *mesh) hasPeer(username string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.peers[username]
	return ok
}

// dropPeer tears down a peer (leave, ban, or re-setup).
func (m *mesh) dropPeer(username string) {
	m.mu.Lock()
	mp, ok := m.peers[username]
	if ok {
		delete(m.peers, username)
		delete(m.noteQs, username)
	}
	m.mu.Unlock()
	if ok {
		m.closePeer(mp)
	}
}

func (m *mesh) closePeer(mp *meshPeer) {
	// Closing the PC wakes a blocked setupPeer via the connection-state
	// callback; no separate done channel (single ownership, no double-close).
	// Fields are copied under lock: writers (setup goroutine, Pion threads)
	// and readers (close paths) otherwise race.
	m.mu.Lock()
	dc, pc := mp.dc, mp.pc
	m.mu.Unlock()
	if dc != nil {
		_ = dc.Close()
	}
	if pc != nil {
		_ = pc.Close()
	}
}

// setPC stores the PeerConnection under lock.
func (m *mesh) setPC(mp *meshPeer, pc *webrtc.PeerConnection) {
	m.mu.Lock()
	mp.pc = pc
	m.mu.Unlock()
}

// setDC stores the data channel under lock (wired from Pion threads).
func (m *mesh) setDC(mp *meshPeer, dc *webrtc.DataChannel) {
	m.mu.Lock()
	mp.dc = dc
	m.mu.Unlock()
}

func (m *mesh) close() {
	m.mu.Lock()
	m.closed = true
	peers := m.peers
	m.peers = map[string]*meshPeer{}
	m.noteQs = map[string]chan signalNote{}
	m.mu.Unlock()
	for _, mp := range peers {
		m.closePeer(mp)
	}
}

// peerDown tears down one peer connection: pointer-guarded so stale
// callbacks (superseded setups, timers firing after close) are no-ops
// instead of double-firing onPeerDown. Exactly-once notification.
func (m *mesh) peerDown(mp *meshPeer) {
	m.mu.Lock()
	cur, ok := m.peers[mp.username]
	if !ok || cur != mp {
		m.mu.Unlock()
		return
	}
	delete(m.peers, mp.username)
	delete(m.noteQs, mp.username)
	m.mu.Unlock()
	m.closePeer(mp)
	if m.cb.onPeerDown != nil {
		m.cb.onPeerDown(mp.username)
	}
}

func (m *mesh) newPC() (*webrtc.PeerConnection, error) {
	var iceServers []webrtc.ICEServer
	if len(m.stunURLs) > 0 {
		iceServers = []webrtc.ICEServer{{URLs: m.stunURLs}}
	}
	return webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
}

// waitGatheringComplete resolves when ICE gathering finishes or times out.
// Non-trickle: the offer/answer then carries every candidate at once.
func waitGatheringComplete(pc *webrtc.PeerConnection, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ICE gathering timed out")
	}
}

// setupPeer runs the full one-shot setup for one peer.
func (m *mesh) setupPeer(mp *meshPeer) {
	peer := mp.username
	ctx, cancel := context.WithTimeout(context.Background(), meshSetupTimeout)
	defer cancel()

	pc, err := m.newPC()
	if err != nil {
		m.peerDown(mp)
		return
	}
	m.setPC(mp, pc)

	// Terminal states fail the attempt; transient Disconnected gets a grace
	// window to recover (NAT rebinding, brief radio loss) before we tear
	// down. onPeerDown fires at most once per peerDown-guarded teardown.
	failed := make(chan struct{}, 1)
	signalFailed := func() {
		select {
		case failed <- struct{}{}:
		default:
		}
	}
	var graceMu sync.Mutex
	var graceTimer *time.Timer
	stopGrace := func() {
		graceMu.Lock()
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer = nil
		}
		graceMu.Unlock()
	}
	defer stopGrace()
	opened := make(chan struct{})
	var openedOnce sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			stopGrace()
			m.peerDown(mp)
			signalFailed()
		case webrtc.PeerConnectionStateDisconnected:
			graceMu.Lock()
			if graceTimer == nil {
				graceTimer = time.AfterFunc(disconnectGraceTimeout, func() {
					m.peerDown(mp)
					signalFailed()
				})
			}
			graceMu.Unlock()
		case webrtc.PeerConnectionStateConnected:
			stopGrace()
		}
	})

	wireChannel := func(dc *webrtc.DataChannel) {
		m.setDC(mp, dc)
		dc.OnOpen(func() {
			openedOnce.Do(func() { close(opened) })
			if m.cb.onPeerUp != nil {
				m.cb.onPeerUp(peer)
			}
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if m.cb.onBytes != nil {
				m.cb.onBytes(peer, msg.Data)
			}
		})
	}

	if m.iOffer(peer) {
		if err := m.setupOfferer(ctx, mp, pc, failed, wireChannel); err != nil {
			_ = pc.Close()
			m.peerDown(mp)
			return
		}
	} else {
		if err := m.setupAnswerer(ctx, mp, pc, failed, wireChannel); err != nil {
			_ = pc.Close()
			m.peerDown(mp)
			return
		}
	}

	// Wait for the channel to open (or failure/timeout). closePeer
	// surfaces here through the connection-state callback. On success we
	// RETURN with the connection live — the old code fell through to the
	// timeout branch and killed every healthy peer 30s after setup,
	// flapping the whole mesh on a fixed schedule.
	select {
	case <-ctx.Done():
		_ = pc.Close()
		m.peerDown(mp)
	case <-failed:
		_ = pc.Close()
		m.peerDown(mp)
	case <-opened:
	}
}

// setupOfferer: create channel+offer, send it, await answer, install it.
func (m *mesh) setupOfferer(ctx context.Context, mp *meshPeer, pc *webrtc.PeerConnection, failed chan struct{}, wire func(*webrtc.DataChannel)) error {
	peer := mp.username
	ordered := true
	dc, err := pc.CreateDataChannel(meshLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	wire(dc)

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	if err := waitGatheringComplete(pc, 10*time.Second); err != nil {
		return err
	}
	local := pc.LocalDescription()
	if local == nil {
		return fmt.Errorf("no local description")
	}
	if err := m.sig.signalSend(peer, "offer", local.SDP); err != nil {
		return err
	}

	// Await the answer on our routed note queue (the engine is the sole
	// signal poller; notes arrive via deliver).
	q := m.queueFor(peer)
	defer m.dropQueue(peer)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("answer wait timed out")
		case <-failed:
			return fmt.Errorf("connection failed during setup")
		case n := <-q:
			if n.From == peer && n.Type == "answer" {
				return pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer,
					SDP:  n.Payload,
				})
			}
		}
	}
}

// setupAnswerer: await offer, install it, answer back.
func (m *mesh) setupAnswerer(ctx context.Context, mp *meshPeer, pc *webrtc.PeerConnection, failed chan struct{}, wire func(*webrtc.DataChannel)) error {
	peer := mp.username
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == meshLabel {
			wire(dc)
		}
	})

	q := m.queueFor(peer)
	defer m.dropQueue(peer)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("offer wait timed out")
		case <-failed:
			return fmt.Errorf("connection failed during setup")
		case n := <-q:
			if n.From != peer || n.Type != "offer" {
				continue
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  n.Payload,
			}); err != nil {
				return err
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return err
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				return err
			}
			if err := waitGatheringComplete(pc, 10*time.Second); err != nil {
				return err
			}
			local := pc.LocalDescription()
			if local == nil {
				return fmt.Errorf("no local description")
			}
			return m.sig.signalSend(peer, "answer", local.SDP)
		}
	}
}

// signalPayloadTooBig guards SDP sizes against the server cap without
// importing server constants here (16KB, must stay in sync).
func signalPayloadTooBig(sdp string) bool {
	return len(sdp) > 16*1024
}
