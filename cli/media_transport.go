package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
)

// ─── Media datagram transport (LAN calls) ───────────────────────────────────
//
// Direct UDP between call peers. Handshake notes ride the existing signal
// queue (authenticated room membership, no LAN spoofing surface); only
// media bytes fly direct. Crypto reuses the device identity keys: one
// Noise_XX handshake per call peer, then datagrams sealed with explicit
// random nonces (loss/reorder-safe, unlike the sequential transport
// nonces). Replay cache bounds duplicate processing.

const (
	mediaVer        = 1
	mediaKindAudio  = 0x01
	mediaKindVideo  = 0x02
	mediaKindKeyReq = 0x03
	mediaKindPing   = 0x04
	mediaKindPong   = 0x05
	// Pre-handshake path probes (unencrypted, cookie-matched). Used to
	// nominate a reachable address before any session exists.
	mediaKindPingPre = 0x06
	mediaKindPongPre = 0x07

	mediaHS1 = "mnoise1"
	mediaHS2 = "mnoise2"
	mediaHS3 = "mnoise3"
	// Restart requests let the non-initiating side recover a dead handshake:
	// only the smaller username may initiate, so a larger side that drops
	// verification can never restart by itself — it asks the smaller side to.
	mediaHSRestart = "mnoise-restart"

	restartThrottle = 10 * time.Second

	mediaNonceSize   = 8
	mediaHeaderLen   = 1 + 1 + mediaNonceSize
	mediaMaxDatagram = 1400 // stay under typical MTU; VP8 fragments smaller

	replayCacheSize = 2048
	hsAttemptTTL    = 60 * time.Second
	maxHsPerIP      = 5
)

// dgramCipher is one direction of a media session (explicit nonces).
type dgramCipher struct {
	mu sync.Mutex
	c  noise.Cipher
	// seen bounds replay processing; evicts oldest first.
	seen  map[uint64]struct{}
	order []uint64
}

func (d *dgramCipher) seal(plaintext []byte) (nonce uint64, ct []byte, err error) {
	var nb [8]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return 0, nil, err
	}
	nonce = binary.BigEndian.Uint64(nb[:])
	d.mu.Lock()
	defer d.mu.Unlock()
	return nonce, d.c.Encrypt(nil, nonce, nil, plaintext), nil
}

func (d *dgramCipher) open(nonce uint64, ct []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.seen[nonce]; dup {
		return nil, fmt.Errorf("replay")
	}
	pt, err := d.c.Decrypt(nil, nonce, nil, ct)
	if err != nil {
		return nil, err
	}
	if d.seen == nil {
		d.seen = map[uint64]struct{}{}
	}
	d.seen[nonce] = struct{}{}
	d.order = append(d.order, nonce)
	for len(d.order) > replayCacheSize {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return pt, nil
}

// mediaPeer tracks one call peer: handshake state, then live ciphers.
type mediaPeer struct {
	username      string
	addr          *net.UDPAddr
	send          *dgramCipher
	recv          *dgramCipher
	ready         bool
	announced     bool
	hs            *peerSession
	hsAt          time.Time
	epoch         int64
	hsM1          []byte // initiator first message (retransmits on stall)
	hsTries       int
	hsNudged      int64 // epoch already nudge-recovered (once per attempt)
	verifyPending bool  // roster-unknown at verify; beat loop retries boundedly
	lastRx        time.Time
}

// mediaTransport owns the UDP socket and per-peer media sessions.
// Callbacks mirror the engine shape; TUI/headless consume them.
type mediaTransport struct {
	me   string
	id   *identityKey
	conn *net.UDPConn
	// sendNote delivers handshake notes via the signal queue.
	sendNote func(to, noteType, payload string) error
	// roster returns username -> static pubkey for key verification.
	roster func() map[string][]byte
	cb     mediaCallbacks

	mu             sync.Mutex
	peers          map[string]*mediaPeer
	hsEpoch        map[string]int64
	completedEpoch map[string]int64
	prePongs       map[uint64]chan *net.UDPAddr
	restartedAt    map[string]time.Time
	sentDgrams     atomic.Uint64 // all outbound datagrams (any kind/peer)
	recvDgrams     atomic.Uint64 // all inbound, pre-session probes included
	closed         bool
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

type mediaCallbacks struct {
	onAudio  func(peer string, pkt audioPacket)
	onKeyReq func(peer string)
	onReady  func(peer string, safetyCode string)
	onLost   func(peer string)
	onError  func(err error)
}

func newMediaTransport(me string, id *identityKey, sendNote func(to, noteType, payload string) error, roster func() map[string][]byte, cb mediaCallbacks) (*mediaTransport, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	return &mediaTransport{
		me: me, id: id, conn: conn, sendNote: sendNote, roster: roster, cb: cb,
		peers: map[string]*mediaPeer{}, hsEpoch: map[string]int64{},
		completedEpoch: map[string]int64{},
		prePongs:       map[uint64]chan *net.UDPAddr{},
		restartedAt:    map[string]time.Time{},
		stopCh:         make(chan struct{}),
	}, nil
}

func (m *mediaTransport) localAddr() *net.UDPAddr {
	return m.conn.LocalAddr().(*net.UDPAddr)
}

func (m *mediaTransport) start() {
	m.wg.Add(2)
	go m.readLoop()
	go m.beatLoop()
}

// beatLoop retransmits stalled handshakes and purges dead ones. A lost
// mnoise note must not strand a call forever with zero diagnostics.
func (m *mediaTransport) beatLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.beatOnce()
		}
	}
}

func (m *mediaTransport) beatOnce() {
	now := time.Now()
	m.mu.Lock()
	type retry struct {
		peer  string
		epoch int64
		m1    []byte
	}
	var retries []retry
	type reverify struct {
		peer string
		hs   *peerSession
	}
	var reverifies []reverify
	var nudges []string
	for username, p := range m.peers {
		if p.ready {
			continue
		}
		if p.verifyPending && p.hs != nil {
			// Roster may have learned the peer since: bounded retries.
			// Late joiners routinely miss the first roster snapshots, so
			// the window is generous — a completed handshake is expensive
			// to throw away and the manager re-joins anyway.
			if p.hsTries >= 6 {
				p.hs = nil
				p.verifyPending = false
				delete(m.hsEpoch, username)
				continue
			}
			p.hsTries++
			reverifies = append(reverifies, reverify{peer: username, hs: p.hs})
			continue
		}
		if p.hs == nil {
			continue
		}
		age := now.Sub(p.hsAt)
		if age > hsAttemptTTL {
			p.hs = nil
			delete(m.hsEpoch, username)
			continue
		}
		if age > 8*time.Second && p.hsTries < 3 && len(p.hsM1) > 0 {
			p.hsTries++
			retries = append(retries, retry{peer: username, epoch: p.epoch, m1: p.hsM1})
			continue
		}
		// Responder-side stall: our handshake waits on a note that may
		// never come (lost in transit) while the peer believes it is done.
		// Nudge once per attempt — smaller side restarts directly, larger
		// side asks (only smaller may initiate).
		if age > 15*time.Second && p.hsNudged != p.epoch {
			p.hsNudged = p.epoch
			nudges = append(nudges, username)
			continue
		}
	}
	m.mu.Unlock()
	for _, r := range retries {
		if len(r.m1) == 0 {
			continue
		}
		_ = m.sendNote(r.peer, mediaHS1, wrapHs(r.epoch, r.m1))
	}
	for _, r := range reverifies {
		m.verifyReady(r.peer, r.hs)
	}
	for _, peer := range nudges {
		if m.me < peer {
			// Smaller side restarts directly (fresh hs + epoch).
			m.dropHandshake(peer)
			m.beginHandshake(peer)
		} else {
			m.sendRestart(peer)
		}
	}
}

// sendRestart asks the smaller peer to begin a fresh handshake (throttled:
// a hostile or confused peer must not turn this into churn).
func (m *mediaTransport) sendRestart(peer string) {
	m.mu.Lock()
	if time.Since(m.restartedAt[peer]) < restartThrottle {
		m.mu.Unlock()
		return
	}
	m.restartedAt[peer] = time.Now()
	m.mu.Unlock()
	_ = m.sendNote(peer, mediaHSRestart, wrapHs(time.Now().UnixNano(), []byte("restart")))
}

// answers (600ms deadline). Unanswered candidates are unroutable from here
// (wrong subnet, AP isolation, firewall) — never silently chosen.
func (m *mediaTransport) nominate(ips []string, port int) *net.UDPAddr {
	var targets []*net.UDPAddr
	seen := map[string]bool{}
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
		if err != nil {
			continue
		}
		targets = append(targets, addr)
	}
	if len(targets) == 0 {
		return nil
	}
	// No single-candidate fast path: an untested single IP is exactly how
	// calls blackhole with perfect signaling. 600ms worst case, once per
	// setup; LAN pongs land in single-digit ms.
	var cookieB [8]byte
	if _, err := rand.Read(cookieB[:]); err != nil {
		return targets[0]
	}
	cookie := binary.BigEndian.Uint64(cookieB[:])
	ch := make(chan *net.UDPAddr, len(targets))
	m.mu.Lock()
	m.prePongs[cookie] = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.prePongs, cookie)
		m.mu.Unlock()
	}()
	pkt := append([]byte{mediaVer, mediaKindPingPre}, cookieB[:]...)
	for _, t := range targets {
		if _, err := m.conn.WriteToUDP(pkt, t); err == nil {
			m.sentDgrams.Add(1)
		}
	}
	select {
	case winner := <-ch:
		return winner
	case <-time.After(600 * time.Millisecond):
		return nil
	}
}

func (m *mediaTransport) stop() {
	select {
	case <-m.stopCh:
		return
	default:
		close(m.stopCh)
	}
	m.conn.Close()
	m.wg.Wait()
}

// dialPeer records where a peer's datagrams go (learned from call-accept).
func (m *mediaTransport) dialPeer(username, ip string, port int) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[username]
	if !ok {
		p = &mediaPeer{username: username}
		m.peers[username] = p
	}
	p.addr = addr
	return nil
}

// beginHandshake starts (smaller username) or prepares a Noise_XX session.
// Mirrors the chat glare rule so both sides agree on roles with no extra round.
func (m *mediaTransport) beginHandshake(peer string) {
	if m.me >= peer {
		return
	}
	m.mu.Lock()
	p, ok := m.peers[peer]
	if !ok {
		p = &mediaPeer{username: peer}
		m.peers[peer] = p
	}
	if p.ready || p.hs != nil {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	ps, m1, err := beginNoise(m.id, peer, true)
	if err != nil {
		m.emitErr(err)
		return
	}
	epoch := time.Now().UnixNano()
	m.mu.Lock()
	p.hs, p.hsAt, p.epoch, p.hsM1, p.hsTries = ps, time.Now(), epoch, m1, 1
	m.hsEpoch[peer] = epoch
	m.mu.Unlock()
	if err := m.sendNote(peer, mediaHS1, wrapHs(epoch, m1)); err != nil {
		m.emitErr(err)
	}
}

// onHandshakeNote drives the responder/continuation side.
func (m *mediaTransport) onHandshakeNote(from string, noteType, payload string) {
	epoch, raw, err := unwrapHs(payload)
	if err != nil {
		return
	}
	switch noteType {
	case mediaHS1:
		if m.me < from {
			return
		}
		m.mu.Lock()
		completed, done := m.completedEpoch[from]
		m.mu.Unlock()
		if done && epoch <= completed {
			return
		}
		ps, _, err := beginNoise(m.id, from, false)
		if err != nil {
			return
		}
		m.mu.Lock()
		p, ok := m.peers[from]
		if !ok {
			p = &mediaPeer{username: from}
			m.peers[from] = p
		}
		p.hs, p.hsAt, p.epoch = ps, time.Now(), epoch
		m.hsEpoch[from] = epoch
		m.mu.Unlock()
		m2, err := ps.stepNoise(raw)
		if err != nil || m2 == nil {
			m.dropHandshake(from)
			return
		}
		_ = m.sendNote(from, mediaHS2, wrapHs(epoch, m2))
	case mediaHSRestart:
		m.onRestartNote(from, epoch)
	case mediaHS2, mediaHS3:
		m.mu.Lock()
		p, ok := m.peers[from]
		var hs *peerSession
		var wantEpoch int64
		tracked := false
		if ok {
			hs = p.hs
			wantEpoch, tracked = m.hsEpoch[from]
		}
		m.mu.Unlock()
		if !ok || hs == nil || !tracked || wantEpoch != epoch {
			return
		}
		var reply []byte
		if noteType == mediaHS2 {
			var err error
			reply, err = hs.stepNoise(raw)
			if err != nil {
				m.dropHandshake(from)
				return
			}
			if reply != nil {
				_ = m.sendNote(from, mediaHS3, wrapHs(epoch, reply))
			}
		} else {
			if _, err := hs.stepNoise(raw); err != nil {
				m.dropHandshake(from)
				return
			}
		}
		m.verifyReady(from, hs)
	}
}

func (m *mediaTransport) dropHandshake(peer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[peer]; ok {
		p.hs = nil
		p.verifyPending = false
	}
	delete(m.hsEpoch, peer)
}

// onRestartNote handles a peer's request to begin a fresh handshake.
// Only the smaller username may initiate (glare rule mirrored); anyone
// else asking is ignored. Throttled like sends.
func (m *mediaTransport) onRestartNote(from string, epoch int64) {
	if m.me > from {
		return
	}
	m.mu.Lock()
	if completed, done := m.completedEpoch[from]; done && epoch <= completed {
		m.mu.Unlock()
		return
	}
	if time.Since(m.restartedAt[from]) < restartThrottle {
		m.mu.Unlock()
		return
	}
	m.restartedAt[from] = time.Now()
	m.mu.Unlock()
	m.dropNoise(from)
	m.beginHandshake(from)
}

// dropNoise clears live + in-progress session state (transport kept).
func (m *mediaTransport) dropNoise(peer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[peer]; ok {
		p.send, p.recv, p.ready, p.hs = nil, nil, false, nil
	}
	delete(m.hsEpoch, peer)
}

// verifyReady promotes a completed handshake after roster key check.
func (m *mediaTransport) verifyReady(peer string, ps *peerSession) {
	if !ps.isReady() {
		m.dropHandshake(peer)
		return
	}
	keys := m.roster()
	want, ok := keys[peer]
	if !ok {
		// Stale snapshot, not an attack: re-read once (beats may have
		// landed mid-handshake). Still unknown → park the attempt and let
		// the beat loop re-verify (bounded); larger sides additionally ask
		// the peer to restart, since only the smaller may initiate.
		if want2, ok2 := m.roster()[peer]; ok2 {
			want, ok = want2, true
		} else {
			m.mu.Lock()
			if p, found := m.peers[peer]; found && p.hs != nil {
				p.verifyPending = true
			}
			m.mu.Unlock()
			if m.me > peer {
				m.sendRestart(peer)
			}
			return
		}
	}
	if !equalBytes(want, ps.remoteKey()) {
		m.dropHandshake(peer)
		m.emitErr(fmt.Errorf("KEY SWAP ALERT for %s: media key does not match roster", peer))
		return
	}
	m.mu.Lock()
	p, found := m.peers[peer]
	if !found {
		p = &mediaPeer{username: peer}
		m.peers[peer] = p
	}
	p.send = &dgramCipher{c: ps.send.Cipher()}
	p.recv = &dgramCipher{c: ps.recv.Cipher()}
	p.ready = true
	p.hs = nil
	p.verifyPending = false
	p.lastRx = time.Now()
	if epoch, has := m.hsEpoch[peer]; has {
		m.completedEpoch[peer] = epoch
		delete(m.hsEpoch, peer)
	}
	announced := p.announced
	p.announced = true
	m.mu.Unlock()
	if !announced && m.cb.onReady != nil {
		m.cb.onReady(peer, safetyCode(m.id.publicKey(), ps.remoteKey()))
	}
}

// peerDiag is a lock-free snapshot for path diagnostics.
type peerDiag struct {
	HasEntry      bool
	Ready         bool
	HasHs         bool
	HsAge         time.Duration
	HsTries       int
	VerifyPending bool
	Addr          string
	LastRxAge     time.Duration
	Sent          uint64
	Recv          uint64
	LocalAddr     string
}

func (m *mediaTransport) diagPeer(peer string) peerDiag {
	d := peerDiag{Sent: m.sentDgrams.Load(), Recv: m.recvDgrams.Load()}
	if a := m.localAddr(); a != nil {
		d.LocalAddr = a.String()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[peer]; ok {
		d.HasEntry = true
		d.Ready = p.ready
		d.HasHs = p.hs != nil
		if p.hs != nil && !p.hsAt.IsZero() {
			d.HsAge = time.Since(p.hsAt).Round(time.Second)
		}
		d.HsTries = p.hsTries
		d.VerifyPending = p.verifyPending
		if p.addr != nil {
			d.Addr = p.addr.String()
		}
		if !p.lastRx.IsZero() {
			d.LastRxAge = time.Since(p.lastRx).Round(time.Second)
		}
	}
	return d
}
func (m *mediaTransport) peerReady(peer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.peers[peer]
	return ok && p.ready
}

// readyPeers lists usernames with a live session (room video fan-out set).
func (m *mediaTransport) readyPeers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for name, p := range m.peers {
		if p.ready && p.addr != nil && p.send != nil {
			out = append(out, name)
		}
	}
	return out
}

// lastRxAt reports when a peer's datagrams last arrived (watchdog input).
func (m *mediaTransport) lastRxAt(peer string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[peer]; ok {
		return p.lastRx
	}
	return time.Time{}
}

// sendMedia seals one media payload as a datagram. Fails fast without a
// live session — callers fall back or report, never block.
func (m *mediaTransport) sendMedia(peer string, kind byte, payload []byte) error {
	m.mu.Lock()
	p, ok := m.peers[peer]
	var send *dgramCipher
	var addr *net.UDPAddr
	if ok && p.ready {
		send, addr = p.send, p.addr
	}
	m.mu.Unlock()
	if !ok || send == nil || addr == nil {
		return fmt.Errorf("no live media session to %s", peer)
	}
	nonce, ct, err := send.seal(payload)
	if err != nil {
		return err
	}
	raw := make([]byte, 0, mediaHeaderLen+len(ct))
	raw = append(raw, mediaVer, kind)
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], nonce)
	raw = append(raw, nb[:]...)
	raw = append(raw, ct...)
	_, err = m.conn.WriteToUDP(raw, addr)
	if err == nil {
		m.sentDgrams.Add(1)
	}
	return err
}

func (m *mediaTransport) readLoop() {
	defer m.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, src, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-m.stopCh:
				return
			default:
				continue
			}
		}
		m.onDatagram(src, append([]byte(nil), buf[:n]...))
	}
}

func (m *mediaTransport) onDatagram(src *net.UDPAddr, raw []byte) {
	if len(raw) < mediaHeaderLen || raw[0] != mediaVer {
		return
	}
	m.recvDgrams.Add(1)
	kind := raw[1]
	// Pre-handshake path probes (unencrypted, cookie-matched): nominate a
	// reachable address before any session exists.
	if kind == mediaKindPingPre && len(raw) >= mediaHeaderLen {
		reply := append([]byte{mediaVer, mediaKindPongPre}, raw[2:10]...)
		_, _ = m.conn.WriteToUDP(reply, src)
		return
	}
	if kind == mediaKindPongPre && len(raw) >= mediaHeaderLen {
		cookie := binary.BigEndian.Uint64(raw[2:10])
		m.mu.Lock()
		ch, ok := m.prePongs[cookie]
		m.mu.Unlock()
		if ok {
			cp := *src
			cp.IP = append(net.IP(nil), src.IP...)
			select {
			case ch <- &cp:
			default:
			}
		}
		return
	}
	nonce := binary.BigEndian.Uint64(raw[2:10])
	ct := raw[10:]
	m.mu.Lock()
	var peer *mediaPeer
	for _, p := range m.peers {
		if p.addr != nil && p.addr.IP.Equal(src.IP) && p.addr.Port == src.Port && p.ready {
			peer = p
			break
		}
	}
	m.mu.Unlock()
	if peer == nil {
		return
	}
	pt, err := peer.recv.open(nonce, ct)
	if err != nil {
		return
	}
	m.mu.Lock()
	peer.lastRx = time.Now()
	m.mu.Unlock()
	m.dispatchMedia(peer.username, kind, pt)
}

func (m *mediaTransport) emitErr(err error) {
	if m.cb.onError != nil {
		m.cb.onError(err)
	}
}
