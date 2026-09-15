package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
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

	mediaHS1 = "mnoise1"
	mediaHS2 = "mnoise2"
	mediaHS3 = "mnoise3"

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
	username  string
	addr      *net.UDPAddr
	send      *dgramCipher
	recv      *dgramCipher
	ready     bool
	announced bool
	hs        *peerSession
	hsAt      time.Time
	epoch     int64
	lastRx    time.Time
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
	hsCount        map[string]int // pending handshakes per source IP (DoS throttle)
	closed         bool
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

type mediaCallbacks struct {
	onAudio  func(peer string, pkt audioPacket)
	onVideo  func(peer string, frag videoFrag)
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
		completedEpoch: map[string]int64{}, hsCount: map[string]int{},
		stopCh: make(chan struct{}),
	}, nil
}

func (m *mediaTransport) localAddr() *net.UDPAddr {
	return m.conn.LocalAddr().(*net.UDPAddr)
}

func (m *mediaTransport) start() {
	m.wg.Add(1)
	go m.readLoop()
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
	p.hs, p.hsAt, p.epoch = ps, time.Now(), epoch
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
		m.dropHandshake(peer)
		return
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
	kind := raw[1]
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
