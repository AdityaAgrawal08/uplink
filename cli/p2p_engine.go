package main

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ─── P2P engine (the conductor) ─────────────────────────────────────────────
//
// Owns one mesh (WebRTC transport), one Noise session per peer, the pairwise
// box fallback, dedup, file reassembly, and both poll loops. Routing rule:
//
//   fast path:  frame -> Noise transport -> data channel (live peer)
//   fallback:   frame -> pairwise box -> server inbox (peer down/offline)
//
// Receivers can't tell which path a frame took — and don't need to. Inbox
// senders seal the whole frame as one box (kind "p2p"); data-channel frames
// arrive Noise-decrypted. Fail-closed everywhere: unknown keys, bad frames,
// and safety mismatches drop data, never display it.

const (
	engineSignalEvery = 2 * time.Second
	engineInboxEvery  = 5 * time.Second
	engineBeatEvery   = 15 * time.Second
	inboxBoxKind = "p2p"
	// fallbackFileMax caps single-box files on the inbox path. Math: the
	// server caps box payloads at 256KB; base64 inflates raw bytes 4/3 plus
	// JSON overhead, so ~160KB raw fits with margin. Larger files need the
	// direct line (P2P streams in 64KB chunks with no total cap).
	fallbackFileMax = 160 * 1024
	assemblyTTL     = 15 * time.Minute
)

type engineChat struct {
	MsgId, From, To, Text string
}

type engineFile struct {
	MsgId, From, To, Filename, Path string
	Size                            int64
}

type engineCallbacks struct {
	onChat      func(engineChat)
	onDelivered func(msgId string)
	onFile      func(engineFile)
	onFileErr   func(msgId, from, reason string)
	onTyping    func(from, to string, active bool)
	onPeerReady func(username, safetyCode string)
	onPeerLost  func(username string)
	onError     func(err error)
}

type fileAssembly struct {
	meta     frame
	chunks   map[int][]byte
	received int
	started  time.Time
}

type engine struct {
	me        string
	id        *identityKey
	sig       *signalClient
	mesh      *mesh
	cb        engineCallbacks
	mu        sync.Mutex
	roster    map[string][]byte // username -> static pubkey
	noise     map[string]*peerSession
	hs        map[string]*peerSession // handshakes in progress
	seen      *seenSet
	files     map[string]*fileAssembly
	announced map[string]bool // safety codes already shown
	presence  []rosterMember  // last heartbeat roster (presence truth for UI)
	// lastRosterAt stamps the freshest roster snapshot. Sends refresh it
	// on demand when stale (rosterFreshTTL), so a join is never missed
	// for longer than this — without putting a Redis read on every send.
	lastRosterAt time.Time
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// rosterFreshTTL bounds how stale the send path's roster may be. The 15s
// beat loop keeps it fresh in the background; this only fires a synchronous
// refresh when a send would otherwise address a stale world.
const rosterFreshTTL = 5 * time.Second

func newEngine(me string, id *identityKey, sig *signalClient, cb engineCallbacks) *engine {
	return newEngineWithStun(me, id, sig, cb, nil)
}

// newEngineWithStun is newEngine with explicit STUN servers; tests pass an
// empty list for pure-loopback (offline-safe) operation.
func newEngineWithStun(me string, id *identityKey, sig *signalClient, cb engineCallbacks, stun []string) *engine {
	e := &engine{
		me:        me,
		id:        id,
		sig:       sig,
		cb:        cb,
		roster:    map[string][]byte{},
		noise:     map[string]*peerSession{},
		hs:        map[string]*peerSession{},
		seen:      newSeenSet(2000),
		files:     map[string]*fileAssembly{},
		announced: map[string]bool{},
		stopCh:    make(chan struct{}),
	}
	e.mesh = newMesh(me, sig, stun, meshCallbacks{
		onBytes:    e.onMeshBytes,
		onPeerUp:   e.onMeshUp,
		onPeerDown: e.onMeshDown,
	})
	return e
}

// loadOrCreateIdentity keeps a stable device key across launches (stable
// safety codes). 0600 permissions; a corrupt file is replaced, never half
// read — a fresh key just looks like a new device to peers.
func loadOrCreateIdentity() (*identityKey, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return generateIdentity()
	}
	path := filepath.Join(home, ".uplink", "identity")
	if raw, err := os.ReadFile(path); err == nil && len(raw) == 32 {
		if priv, err := ecdh.X25519().NewPrivateKey(raw); err == nil {
			return &identityKey{priv: priv, pub: priv.PublicKey().Bytes()}, nil
		}
	}
	id, err := generateIdentity()
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, id.priv.Bytes(), 0o600)
	return id, nil
}

// peers returns a snapshot of the last known roster for UI rendering.
// Presence freshness is maintained by the beat loop; this never blocks.
func (e *engine) peers() []rosterMember {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]rosterMember, len(e.presence))
	copy(out, e.presence)
	return out
}

// ─── lifecycle ──────────────────────────────────────────────────────────────

func (e *engine) start() {
	e.wg.Add(3)
	go e.signalLoop()
	go e.inboxLoop()
	go e.beatLoop()
}

func (e *engine) stop() {
	select {
	case <-e.stopCh:
		return
	default:
		close(e.stopCh)
	}
	e.wg.Wait()
	e.mesh.close()
}

func (e *engine) stopped() bool {
	select {
	case <-e.stopCh:
		return true
	default:
		return false
	}
}

// setRoster replaces the known member set: learns pubkeys (validated),
// opens setup for newcomers, tears down the departed.
func (e *engine) setRoster(members []rosterMember) {
	e.mu.Lock()
	e.lastRosterAt = time.Now()
	next := map[string][]byte{}
	for _, m := range members {
		if m.Username == "" || m.Username == e.me {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(m.Pubkey)
		if err != nil || len(raw) != 32 {
			continue // fail closed on server-supplied garbage keys
		}
		next[m.Username] = raw
	}
	var added, removed []string
	for u := range next {
		if _, ok := e.roster[u]; !ok {
			added = append(added, u)
		}
	}
	for u := range e.roster {
		if _, ok := next[u]; !ok {
			removed = append(removed, u)
		}
	}
	e.roster = next
	e.mu.Unlock()

	for _, u := range added {
		e.mesh.ensurePeer(u)
	}
	for _, u := range removed {
		e.mesh.dropPeer(u)
		e.dropNoise(u)
	}
}

func (e *engine) dropNoise(peer string) {
	e.mu.Lock()
	delete(e.noise, peer)
	delete(e.hs, peer)
	e.mu.Unlock()
}

// beatLoop owns presence: heartbeat, roster refresh, re-ensure.
func (e *engine) beatLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(engineBeatEvery)
	defer ticker.Stop()
	// immediate first beat so the roster seeds without waiting
	e.beatOnce()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.beatOnce()
		}
	}
}

func (e *engine) beatOnce() {
	roster, err := e.sig.heartbeat("", nil)
	if err != nil {
		return // transient; next tick retries
	}
	e.mu.Lock()
	e.presence = roster
	e.mu.Unlock()
	e.setRoster(roster)
}

// ─── signaling poll (the ONLY signal drain) ────────────────────────────────

func (e *engine) signalLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(engineSignalEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			notes, err := e.sig.signalPoll()
			if err != nil {
				continue
			}
			for _, n := range notes {
				e.onNote(n)
			}
		}
	}
}

func (e *engine) onNote(n signalNote) {
	switch n.Type {
	case "offer", "answer":
		e.mesh.deliver(n)
	case noiseSig1, noiseSig2, noiseSig3:
		e.onHandshakeNote(n)
	}
}

// ─── Noise handshake drive ─────────────────────────────────────────────────
// Deterministic initiator: the lexicographically smaller username ALWAYS
// initiates. A smaller side receiving noise1 ignores it (it will initiate);
// this makes glare structurally impossible.

func (e *engine) onMeshUp(peer string) {
	if e.me < peer {
		e.mu.Lock()
		_, hasNoise := e.noise[peer]
		_, hasHs := e.hs[peer]
		e.mu.Unlock()
		if hasNoise || hasHs {
			return
		}
		ps, m1, err := beginNoise(e.id, peer, true)
		if err != nil {
			e.emitErr(err)
			return
		}
		e.mu.Lock()
		e.hs[peer] = ps
		e.mu.Unlock()
		if err := e.sig.signalSend(peer, noiseSig1, base64.StdEncoding.EncodeToString(m1)); err != nil {
			e.emitErr(err)
		}
	}
	// larger side waits for noise1; nothing to do on channel open
}

func (e *engine) onHandshakeNote(n signalNote) {
	raw, err := base64.StdEncoding.DecodeString(n.Payload)
	if err != nil {
		return
	}
	switch n.Type {
	case noiseSig1:
		if e.me < n.From {
			return // I initiate; ignore their attempt (glare rule)
		}
		ps, _, err := beginNoise(e.id, n.From, false)
		if err != nil {
			return
		}
		e.mu.Lock()
		e.hs[n.From] = ps
		e.mu.Unlock()
		m2, err := ps.stepNoise(raw)
		if err != nil || m2 == nil {
			e.dropNoise(n.From)
			return
		}
		_ = e.sig.signalSend(n.From, noiseSig2, base64.StdEncoding.EncodeToString(m2))
	case noiseSig2:
		e.mu.Lock()
		ps, ok := e.hs[n.From]
		e.mu.Unlock()
		if !ok {
			return
		}
		m3, err := ps.stepNoise(raw)
		if err != nil {
			e.dropNoise(n.From)
			return
		}
		if m3 != nil {
			_ = e.sig.signalSend(n.From, noiseSig3, base64.StdEncoding.EncodeToString(m3))
		}
		e.verifyReady(n.From, ps)
	case noiseSig3:
		e.mu.Lock()
		ps, ok := e.hs[n.From]
		e.mu.Unlock()
		if !ok {
			return
		}
		if _, err := ps.stepNoise(raw); err != nil {
			e.dropNoise(n.From)
			return
		}
		e.verifyReady(n.From, ps)
	}
}

// verifyReady promotes a completed handshake to a live session after
// checking the authenticated key against the roster. Mismatch = the server
// (or a MITM) swapped keys: drop everything and alert loudly.
func (e *engine) verifyReady(peer string, ps *peerSession) {
	if !ps.isReady() {
		e.dropNoise(peer)
		return
	}
	e.mu.Lock()
	want, ok := e.roster[peer]
	if !ok || !equalBytes(want, ps.remoteKey()) {
		e.mu.Unlock()
		e.mesh.dropPeer(peer)
		e.dropNoise(peer)
		e.emitErr(fmt.Errorf("KEY SWAP ALERT for %s: handshake key does not match roster — possible attack, channel dropped", peer))
		return
	}
	e.noise[peer] = ps
	delete(e.hs, peer)
	announced := e.announced[peer]
	if !announced {
		e.announced[peer] = true
	}
	e.mu.Unlock()
	if !announced && e.cb.onPeerReady != nil {
		e.cb.onPeerReady(peer, safetyCode(e.id.publicKey(), ps.remoteKey()))
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ─── inbound data-channel bytes ─────────────────────────────────────────────

func (e *engine) onMeshBytes(peer string, raw []byte) {
	e.mu.Lock()
	ps, ok := e.noise[peer]
	e.mu.Unlock()
	if !ok || !ps.isReady() {
		return // no live session: drop (fail closed)
	}
	pt, err := ps.decrypt(raw)
	if err != nil {
		// Fail-closed per Noise semantics: the channel is dead. Tear down;
		// the next roster beat re-runs setup with a fresh handshake.
		e.mesh.dropPeer(peer)
		e.dropNoise(peer)
		if e.cb.onPeerLost != nil {
			e.cb.onPeerLost(peer)
		}
		return
	}
	f, err := decodeFrame(pt)
	if err != nil {
		return
	}
	e.dispatch(f)
}

// ─── inbox poll (fallback + offline path) ───────────────────────────────────

func (e *engine) inboxLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(engineInboxEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.inboxOnce()
		}
	}
}

func (e *engine) inboxOnce() {
	boxes, err := e.sig.inboxFetch()
	if err != nil {
		return
	}
	if len(boxes) == 0 {
		return
	}
	var ack []string
	for _, b := range boxes {
		if e.seen.seen("inbox:" + b.MsgId) {
			ack = append(ack, b.MsgId) // already processed: just collect the delete
			continue
		}
		e.mu.Lock()
		senderKey, known := e.roster[b.From]
		e.mu.Unlock()
		if !known {
			// Sender not in MY roster snapshot (typically it is stale and
			// a beat refresh is pending). Leave the box UNACKED so a later
			// poll — after the roster refreshes — can still deliver it.
			// Purging here ate legitimate mail whenever rosters lagged.
			// Stragglers are bounded by the 1h server TTL + per-user cap.
			continue
		}
		pt, err := openBox(e.id, senderKey, b.Payload)
		if err != nil {
			ack = append(ack, b.MsgId) // undecryptable: purge, never display
			continue
		}
		f, err := decodeFrame(pt)
		if err != nil {
			ack = append(ack, b.MsgId)
			continue
		}
		e.dispatch(f)
		ack = append(ack, b.MsgId)
	}
	if len(ack) > 0 {
		_, _ = e.sig.inboxAck(ack)
	}
}

// ─── dispatch ───────────────────────────────────────────────────────────────

func (e *engine) dispatch(f frame) {
	switch f.Type {
	case frameChat:
		if e.cb.onChat != nil {
			e.cb.onChat(engineChat{MsgId: f.MsgId, From: f.From, To: f.To, Text: f.Data})
		}
	case frameAck:
		if e.cb.onDelivered != nil {
			e.cb.onDelivered(f.MsgId)
		}
	case frameTyping:
		if e.cb.onTyping != nil {
			e.cb.onTyping(f.From, f.To, f.Active)
		}
	case frameFileMeta, frameFileChunk, frameFileComplete, frameFile:
		e.onFileFrame(f)
	}
}

// ─── outbound ───────────────────────────────────────────────────────────────

// sendFrame routes one frame: Noise transport when live, pairwise box via
// inbox otherwise. Broadcast (to=="") fans out per peer; returns nil if at
// least one path succeeded. Typing is live-only: stale typing indicators
// delivered minutes later from an inbox would be wrong, so they drop.
func (e *engine) sendFrame(to string, f frame) error {
	if f.Type == frameTyping && !e.peerLive(to) {
		return nil
	}
	raw, err := encodeFrame(f)
	if err != nil {
		return err
	}
	if to != "" {
		return e.sendOne(to, f, raw)
	}
	e.mu.Lock()
	var peers []string
	for u := range e.roster {
		peers = append(peers, u)
	}
	e.mu.Unlock()
	if len(peers) == 0 {
		// Fail loudly, not silently: a broadcast with no known recipients
		// (typically a roster not yet refreshed after a join) must surface
		// instead of returning success having sent nothing.
		return fmt.Errorf("no recipients in roster yet — retry in a few seconds")
	}
	var firstErr error
	sent := 0
	for _, u := range peers {
		if err := e.sendOne(u, f, raw); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent++
		}
	}
	if sent == 0 && firstErr != nil {
		return firstErr
	}
	return nil
}

func (e *engine) sendOne(peer string, f frame, raw []byte) error {
	e.mu.Lock()
	ps, ok := e.noise[peer]
	key, known := e.roster[peer]
	e.mu.Unlock()
	// fast path: live Noise session + open channel
	if ok && ps.isReady() {
		if ct, err := ps.encrypt(raw); err == nil {
			if err := e.mesh.send(peer, ct); err == nil {
				return nil
			}
		}
		// channel dead: fall through to inbox (mesh will report down)
	}
	if !known {
		return fmt.Errorf("unknown peer %s", peer)
	}
	box, err := sealBox(e.id, key, raw)
	if err != nil {
		return err
	}
	return e.sig.inboxSend(peer, f.MsgId, inboxBoxKind, box)
}

func (e *engine) sendChat(to, text string) (string, error) {
	id, err := newMsgId()
	if err != nil {
		return "", err
	}
	e.ensureFreshRoster()
	f := newFrame(frameChat, id, e.me, to)
	f.Data = text
	if err := e.sendFrame(to, f); err != nil {
		return "", err
	}
	return id, nil
}

// ensureFreshRoster refreshes the roster synchronously when the snapshot is
// older than rosterFreshTTL. Sends then never address a world older than a
// few seconds, while the hot path (fresh cache) costs nothing extra.
// Failures keep the stale snapshot (fail-open for sends).
func (e *engine) ensureFreshRoster() {
	e.mu.Lock()
	stale := time.Since(e.lastRosterAt) > rosterFreshTTL
	e.mu.Unlock()
	if !stale {
		return
	}
	roster, err := e.sig.heartbeat("", nil)
	if err != nil {
		return
	}
	e.setRoster(roster)
}

func (e *engine) sendAck(to, msgId string) error {
	id, err := newMsgId()
	if err != nil {
		return err
	}
	return e.sendFrame(to, newFrame(frameAck, id, e.me, to))
}

func (e *engine) emitErr(err error) {
	if e.cb.onError != nil {
		e.cb.onError(err)
	}
}

// ─── files ──────────────────────────────────────────────────────────────────

func (e *engine) sendFile(to, path, display string, prog chan<- uploadProgressMsg) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	sumHex := hex.EncodeToString(sum[:])
	id, err := newMsgId()
	if err != nil {
		return "", 0, err
	}
	e.ensureFreshRoster()
	// Streaming (meta/chunks/complete sharing one msgId) requires the live
	// path for every recipient: inbox boxes key by msgId and would collide.
	// When anyone is unreachable, small files go as ONE self-contained box
	// and larger ones fail fast with an actionable error.
	if e.allLive(to) {
		_, _, serr := e.sendFileStream(to, id, display, data, sumHex, prog)
		if serr != nil {
			return "", 0, serr
		}
		return display, int64(len(data)), nil
	}
	if int64(len(data)) > fallbackFileMax {
		return "", 0, fmt.Errorf("peer unreachable for large file (%s) — wait for a direct connection", humanSize(int64(len(data))))
	}
	whole := newFrame(frameFile, id, e.me, to)
	whole.Filename = display
	whole.Size = int64(len(data))
	whole.SHA256 = sumHex
	whole.Data = base64.StdEncoding.EncodeToString(data)
	if err := e.sendFrame(to, whole); err != nil {
		return "", 0, err
	}
	if prog != nil {
		select {
		case prog <- uploadProgressMsg{done: int64(len(data)), total: int64(len(data))}:
		default:
		}
	}
	return display, int64(len(data)), nil
}

// allLive reports whether every recipient currently has a live E2E channel
// (DM: the one peer; broadcast: the whole roster).
func (e *engine) allLive(to string) bool {
	if to != "" {
		return e.peerLive(to)
	}
	e.mu.Lock()
	peers := make([]string, 0, len(e.roster))
	for u := range e.roster {
		peers = append(peers, u)
	}
	e.mu.Unlock()
	for _, u := range peers {
		if !e.peerLive(u) {
			return false
		}
	}
	return true
}

// sendFileStream streams meta/chunks/complete down the live path.
func (e *engine) sendFileStream(to, id, display string, data []byte, sumHex string, prog chan<- uploadProgressMsg) (string, int64, error) {
	chunks := splitChunks(data)
	meta := newFrame(frameFileMeta, id, e.me, to)
	meta.Filename = display
	meta.Size = int64(len(data))
	meta.SHA256 = sumHex
	meta.Chunks = len(chunks)
	if err := e.sendFrame(to, meta); err != nil {
		return "", 0, err
	}
	for i, c := range chunks {
		cf := newFrame(frameFileChunk, id, e.me, to)
		cf.ChunkIndex = i
		cf.Data = base64.StdEncoding.EncodeToString(c)
		if err := e.sendFrame(to, cf); err != nil {
			return "", 0, err
		}
		if prog != nil {
			select {
			case prog <- uploadProgressMsg{done: int64(i+1) * int64(frameChunkSize), total: int64(len(data))}:
			default:
			}
		}
	}
	done := newFrame(frameFileComplete, id, e.me, to)
	if err := e.sendFrame(to, done); err != nil {
		return "", 0, err
	}
	return display, int64(len(data)), nil
}

func (e *engine) peerLive(to string) bool {
	if to != "" {
		e.mu.Lock()
		ps, ok := e.noise[to]
		e.mu.Unlock()
		return ok && ps.isReady()
	}
	return false
}

func (e *engine) onFileFrame(f frame) {
	// Single-box files (inbox path) skip reassembly entirely.
	if f.Type == frameFile {
		blob, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil || int64(len(blob)) != f.Size || f.Size <= 0 || f.Size > fallbackFileMax {
			return // corrupt or absurd: drop (fail closed)
		}
		e.saveVerifiedFile(f.MsgId, f.From, f.To, f.Filename, f.Size, f.SHA256, blob)
		return
	}
	e.sweepAssemblies()
	e.mu.Lock()
	a, ok := e.files[f.MsgId]
	if !ok {
		if f.Type != frameFileMeta {
			e.mu.Unlock()
			return // chunk before meta: drop (sender always sends meta first)
		}
		if f.Size <= 0 || f.Size > uploadMaxBytes || f.Chunks <= 0 || f.Chunks > 4096 {
			e.mu.Unlock()
			return // absurd sizes fail closed
		}
		a = &fileAssembly{meta: f, chunks: map[int][]byte{}, started: time.Now()}
		e.files[f.MsgId] = a
	}
	switch f.Type {
	case frameFileChunk:
		if f.ChunkIndex < 0 || f.ChunkIndex >= a.meta.Chunks {
			break
		}
		if _, dup := a.chunks[f.ChunkIndex]; !dup {
			raw, err := base64.StdEncoding.DecodeString(f.Data)
			if err == nil && len(raw) <= frameChunkSize {
				a.chunks[f.ChunkIndex] = raw
				a.received++
			}
		}
	}
	complete := f.Type == frameFileComplete || a.received == a.meta.Chunks
	meta := a.meta
	var blob []byte
	if complete {
		blob = make([]byte, 0, meta.Size)
		for i := 0; i < meta.Chunks; i++ {
			c, ok := a.chunks[i]
			if !ok {
				complete = false
				break
			}
			blob = append(blob, c...)
		}
		if complete {
			delete(e.files, f.MsgId)
		}
	}
	e.mu.Unlock()
	if !complete {
		return
	}
	e.saveVerifiedFile(meta.MsgId, meta.From, meta.To, meta.Filename, meta.Size, meta.SHA256, blob)
}

// saveVerifiedFile checks the SHA-256 fingerprint and atomically saves the
// blob to ~/Downloads, then reports it. Shared by streamed and single-box
// receives; integrity failure discards, never displays.
func (e *engine) saveVerifiedFile(msgId, from, to, filename string, size int64, shaHex string, blob []byte) {
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != shaHex {
		if e.cb.onFileErr != nil {
			e.cb.onFileErr(msgId, from, "SHA-256 mismatch — file discarded")
		}
		return
	}
	dir, err := downloadsDir()
	if err != nil {
		if e.cb.onFileErr != nil {
			e.cb.onFileErr(msgId, from, err.Error())
		}
		return
	}
	safe := filepath.Base(filename)
	if safe == "" || safe == "." {
		safe = "file"
	}
	dest := uniquePath(dir, safe)
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		if e.cb.onFileErr != nil {
			e.cb.onFileErr(msgId, from, err.Error())
		}
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		if e.cb.onFileErr != nil {
			e.cb.onFileErr(msgId, from, err.Error())
		}
		return
	}
	if e.cb.onFile != nil {
		e.cb.onFile(engineFile{MsgId: msgId, From: from, To: to, Filename: filename, Path: dest, Size: size})
	}
}

func (e *engine) sweepAssemblies() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for id, a := range e.files {
		if now.Sub(a.started) > assemblyTTL {
			delete(e.files, id)
		}
	}
}

// ─── mesh callbacks ─────────────────────────────────────────────────────────
// (onMeshUp lives in the handshake-drive section above; initiator starts the
// Noise handshake on channel open.)

func (e *engine) onMeshDown(peer string) {
	e.dropNoise(peer)
	if e.cb.onPeerLost != nil {
		e.cb.onPeerLost(peer)
	}
}
