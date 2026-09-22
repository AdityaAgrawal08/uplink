package main

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	// engineInboxEvery sets fallback/offline delivery latency. 2s (was 5s):
	// the inbox is the durability backstop for every mesh-path loss, so a
	// faster poll directly shrinks worst-case delivery time. Cost is one
	// cheap fetch per cycle; the per-user read budget (600/5min) still has
	// headroom over steady-state use (~330/5min across all three loops).
	engineInboxEvery = 2 * time.Second
	engineBeatEvery  = 5 * time.Second
	inboxBoxKind     = "p2p"
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
	// onRoster fires when a beat learns membership moved (join/leave).
	// The sidebar refreshes off this (~100ms) instead of waiting for the
	// next render tick. Nil-tolerant: headless consumers ignore it.
	onRoster func()
	onError  func(err error)
	// onSignalNote receives non-chat signal types (call control, media
	// handshake). The engine owns the only signal drain; consumers of
	// other note types subscribe here instead of polling.
	onSignalNote func(note signalNote)
}

type fileAssembly struct {
	meta     frame
	chunks   map[int][]byte
	received int
	started  time.Time
}

type engine struct {
	me     string
	id     *identityKey
	sig    *signalClient
	mesh   *mesh
	cb     engineCallbacks
	mu     sync.Mutex
	roster map[string][]byte // username -> static pubkey
	noise  map[string]*peerSession
	hs     map[string]*peerSession // handshakes in progress
	hsAt   map[string]time.Time    // handshake start times (stuck-hs expiry)
	// hsEpoch tracks the epoch of each in-progress handshake; completedEpoch
	// tracks the epoch of the live session. A re-handshake always carries a
	// newer epoch, so duplicate/redelivered notes (same or older epoch) are
	// ignored instead of nuking a healthy session.
	hsEpoch        map[string]int64
	completedEpoch map[string]int64
	// lastFail throttles setup retries per peer (setupRetryBackoff) so a
	// persistently failing peer can't churn PC+handshake storms.
	// failCount distinguishes a first drop (retry on next beat — likely
	// transient, and the common rejoin case) from repeated quick failures
	// (throttled). Success clears both; departure clears both.
	lastFail  map[string]time.Time
	failCount map[string]int
	// unacked tracks mesh-sent chat/file frames awaiting a delivery ack,
	// keyed msgId+"|"+peer (hex ids, alphanumeric usernames — unambiguous).
	// Broadcasts fan out per peer and each recipient acks. Entries graduate
	// on ack and expire after maxInboxRetries inbox re-sends — the backstop
	// for frames the mesh ate (sent into a dead or cross-generation
	// session). Inbox-first sends are durable by construction (1h server
	// TTL + redelivery) and are never tracked.
	unacked map[string]*pendingAck
	// sendMu serializes the mesh fast path per peer so encrypt order ==
	// wire order. Noise nonces are sequential: concurrent encrypt+send
	// from the chat and file-upload goroutines could otherwise swap two
	// frames on the wire, and the receiver would fail-decrypt the first
	// arrival and tear down a healthy session. Guarded by e.mu.
	sendMu    map[string]*sync.Mutex
	files     map[string]*fileAssembly
	announced map[string]bool // safety codes already shown
	presence  []rosterMember  // last heartbeat roster (presence truth for UI)
	// joinPassword lets the engine rejoin by itself after being pruned for
	// missed heartbeats (e.g. laptop sleep). Memory-only, never logged.
	joinPassword string
	// lastRosterAt stamps the freshest roster snapshot. Sends refresh it
	// on demand when stale (rosterFreshTTL), so a join is never missed
	// for longer than this — without putting a Redis read on every send.
	lastRosterAt time.Time
	// lastEpoch is the roster generation from the last heartbeat/inbox
	// response. A changed epoch proves membership moved, so the engine
	// refreshes immediately instead of waiting out the beat cadence.
	lastEpoch int64
	// rosterSynced flips on the first successful roster sync. A broadcast
	// with zero peers after that means the user is genuinely alone (chat
	// freely: the echo stands, nothing to fan out); before that it means
	// we haven't learned the room yet (fail loudly, retry soon).
	rosterSynced bool
	// lastTrigger throttles traffic-triggered refreshes (see triggerRefresh).
	lastTrigger time.Time
	// endedNotified/lastRejoinErr quiet the beat failure paths: a destroyed
	// room reports once, a failing rejoin backs off to 60s.
	endedNotified bool
	lastRejoinErr time.Time
	// lastBeatErr is the latest heartbeat failure (nil after any success).
	// The TUI polls it on its render tick to hold the server-down alert.
	lastBeatErr error
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

// rosterFreshTTL bounds how stale the send path's roster may be. 2s keeps
// the join-then-send blind window human-imperceptible; the beat loop and
// traffic-triggered refreshes cover the rest.
const rosterFreshTTL = 2 * time.Second

// setupRetryBackoff is the minimum gap between setup attempts for one peer
// after a failure. Var (not const) so tests can shrink it.
var setupRetryBackoff = 30 * time.Second

const (
	// ackTimeout is how long a mesh-sent frame waits for its delivery ack
	// before the first inbox re-send. Mesh RTT is milliseconds; 3s covers
	// scheduling jitter without stalling the backstop behind a dead peer.
	ackTimeout = 3 * time.Second
	// maxInboxRetries bounds the backstop: initial mesh attempt + this many
	// durable inbox re-sends, then the entry is dropped. A longer-gone peer
	// is offline; its mail is either already durable (inbox-first sends
	// persist 1h server-side) or stops deserving retries. Unbounded retry
	// would leak memory and spam a dead peer's queue.
	maxInboxRetries = 3
	// retryEvery sets the backstop sweep cadence (piggybacks the 2s class).
	retryEvery = 2 * time.Second
)

// pendingAck is one mesh-sent frame awaiting its delivery ack.
type pendingAck struct {
	to    string
	f     frame
	sent  time.Time
	tries int // inbox re-sends so far
}

func unackedKey(msgId, peer string) string { return msgId + "|" + peer }

// hsEnvelope wraps one Noise handshake message with the initiator's epoch
// (unix nanos at handshake start). Epochs order handshakes per peer pair:
// a note carrying an epoch at or below the completed one is a duplicate or
// a stale retransmit and must be ignored — never allowed to tear down the
// live session. Without this, signal-queue redelivery nukes healthy E2E
// channels and both sides flap forever.
type hsEnvelope struct {
	Epoch int64  `json:"epoch"`
	Data  string `json:"data"`
}

func wrapHs(epoch int64, msg []byte) string {
	raw, _ := json.Marshal(hsEnvelope{Epoch: epoch, Data: base64.StdEncoding.EncodeToString(msg)})
	return string(raw)
}

func unwrapHs(payload string) (int64, []byte, error) {
	var env hsEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return 0, nil, err
	}
	msg, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return 0, nil, err
	}
	return env.Epoch, msg, nil
}

func newEngine(me string, id *identityKey, sig *signalClient, cb engineCallbacks) *engine {
	return newEngineWithStun(me, id, sig, cb, nil)
}

// newEngineWithStun is newEngine with explicit STUN servers; tests pass an
// empty list for pure-loopback (offline-safe) operation.
func newEngineWithStun(me string, id *identityKey, sig *signalClient, cb engineCallbacks, stun []string) *engine {
	e := &engine{
		me:             me,
		id:             id,
		sig:            sig,
		cb:             cb,
		roster:         map[string][]byte{},
		noise:          map[string]*peerSession{},
		hs:             map[string]*peerSession{},
		hsAt:           map[string]time.Time{},
		hsEpoch:        map[string]int64{},
		completedEpoch: map[string]int64{},
		lastFail:       map[string]time.Time{},
		failCount:      map[string]int{},
		unacked:        map[string]*pendingAck{},
		sendMu:         map[string]*sync.Mutex{},
		files:          map[string]*fileAssembly{},
		announced:      map[string]bool{},
		stopCh:         make(chan struct{}),
	}
	e.mesh = newMesh(me, sig, stun, meshCallbacks{
		onBytes:    e.onMeshBytes,
		onPeerUp:   e.onMeshUp,
		onPeerDown: e.onMeshDown,
		onError:    e.emitErr,
	})
	return e
}

// loadOrCreateIdentity keeps a stable device key across launches (stable
// safety codes). 0600 permissions; a corrupt file is replaced, never half
// read — a fresh key just looks like a new device to peers.
func loadOrCreateIdentity() (*identityKey, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("cannot determine home directory for device identity: %v", err)
	}
	path := filepath.Join(home, ".uplink", "identity")
	if raw, rerr := os.ReadFile(path); rerr == nil {
		if len(raw) != 32 {
			return nil, fmt.Errorf("device identity at %s is corrupt (%d bytes) — move it aside to regenerate", path, len(raw))
		}
		if priv, kerr := ecdh.X25519().NewPrivateKey(raw); kerr == nil {
			return &identityKey{priv: priv, pub: priv.PublicKey().Bytes()}, nil
		} else {
			return nil, fmt.Errorf("device identity at %s is corrupt — move it aside to regenerate: %v", path, kerr)
		}
	} else if !os.IsNotExist(rerr) {
		return nil, fmt.Errorf("cannot read device identity at %s: %v", path, rerr)
	}
	id, err := generateIdentity()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, id.priv.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("cannot persist device identity at %s: %v", path, err)
	}
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

// beatErr reports the latest heartbeat failure (nil after any success).
// The TUI holds the server-down alert while this classifies as down.
func (e *engine) beatErr() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastBeatErr
}

// ─── lifecycle ──────────────────────────────────────────────────────────────

func (e *engine) start() {
	e.wg.Add(4)
	go e.signalLoop()
	go e.inboxLoop()
	go e.beatLoop()
	go e.retryLoop()
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

// applyPushedRoster installs a server-fresh roster from a moderation
// response (kick/grant): presence for the UI, keys for the mesh, epoch for
// the change tracker — one locked handoff, no waiting for the next beat.
func (e *engine) applyPushedRoster(roster []rosterMember, epoch int64) {
	e.mu.Lock()
	e.presence = roster
	e.lastEpoch = epoch
	e.mu.Unlock()
	e.setRoster(roster)
}

// setRoster replaces the known member set: learns pubkeys (validated),
// opens setup for newcomers, tears down the departed.
func (e *engine) setRoster(members []rosterMember) {
	e.mu.Lock()
	e.lastRosterAt = time.Now()
	e.rosterSynced = true
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
		e.maybeInitiate(u) // channel may predate the roster entry
	}
	for _, u := range removed {
		e.mesh.dropPeer(u)
		e.dropNoise(u)
		// Departed users leave no residue: stale epochs/penalties must not
		// mute a future rejoin, and a rejoining user (possibly a new device
		// key) must re-announce its safety code instead of inheriting trust.
		e.mu.Lock()
		delete(e.completedEpoch, u)
		delete(e.lastFail, u)
		delete(e.failCount, u)
		delete(e.announced, u)
		delete(e.sendMu, u)
		e.mu.Unlock()
	}
}

func (e *engine) dropNoise(peer string) {
	e.mu.Lock()
	delete(e.noise, peer)
	delete(e.hs, peer)
	delete(e.hsAt, peer)
	delete(e.hsEpoch, peer)
	// NOTE: completedEpoch and lastFail intentionally survive — the former
	// rejects duplicate notes after teardown, the latter throttles retries.
	e.mu.Unlock()
}

// trackHs records a handshake start for stuck-handshake expiry.
func (e *engine) trackHs(peer string, ps *peerSession, epoch int64) {
	e.mu.Lock()
	e.hs[peer] = ps
	e.hsAt[peer] = time.Now()
	e.hsEpoch[peer] = epoch
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
	roster, epoch, err := e.sig.heartbeat("", nil)
	if err != nil {
		// Pruned while asleep (missed beats): rejoin with the same identity
		// instead of rotting at 403 forever. A 409 here means someone took
		// our name meanwhile — surface it, don't loop.
		if isNotMember(err) {
			e.mu.Lock()
			recentErr := time.Since(e.lastRejoinErr) < 60*time.Second
			e.mu.Unlock()
			if _, _, jerr := e.sig.joinRoom(e.me, base64.StdEncoding.EncodeToString(e.id.publicKey()), e.joinPassword); jerr != nil {
				e.mu.Lock()
				e.lastRejoinErr = time.Now()
				e.lastBeatErr = jerr
				e.mu.Unlock()
				if !recentErr {
					e.emitErr(fmt.Errorf("rejoin failed (%v) — rejoin manually", jerr))
				}
				return
			}
			e.mu.Lock()
			e.lastRejoinErr = time.Time{}
			e.mu.Unlock()
			roster, epoch, err = e.sig.heartbeat("", nil)
			if err != nil {
				e.mu.Lock()
				e.lastBeatErr = err
				e.mu.Unlock()
				return
			}
		} else if apiStatusCode(err) == 404 {
			// Room destroyed under us (last one out ends it): nothing to
			// rejoin — say so once instead of rotting silently.
			e.mu.Lock()
			notified := e.endedNotified
			e.endedNotified = true
			e.lastBeatErr = err
			e.mu.Unlock()
			if !notified {
				e.emitErr(fmt.Errorf("session ended — rooms vanish when emptied; create or join a new one"))
			}
			return
		} else {
			e.mu.Lock()
			e.lastBeatErr = err
			e.mu.Unlock()
			return // transient; next tick retries
		}
	}
	e.mu.Lock()
	e.presence = roster
	e.lastEpoch = epoch
	e.lastBeatErr = nil
	before := make(map[string]bool, len(e.roster))
	for u := range e.roster {
		before[u] = true
	}
	e.mu.Unlock()
	e.setRoster(roster)
	e.reconcilePeers(roster)
	// Membership moved: wake the UI now (~100ms drain) instead of letting
	// it sit stale until the next render tick.
	e.mu.Lock()
	changed := len(e.roster) != len(before)
	if !changed {
		for u := range e.roster {
			if !before[u] {
				changed = true
				break
			}
		}
	}
	e.mu.Unlock()
	if changed && e.cb.onRoster != nil {
		e.cb.onRoster()
	}
}

// isNotMember reports the server's "you are not in this session" rejection.
func isNotMember(err error) bool {
	return apiStatusCode(err) == 403
}

// stuckHsTTL bounds a handshake with no progress. Past it the attempt is
// torn down and retried fresh (signal notes can be lost to TTL expiry,
// leaving an otherwise-healthy peer pair stalled forever).
const stuckHsTTL = 60 * time.Second

// reconcilePeers closes the retry gap: failed mesh setups leave no trace
// (teardown removes the entry) and stuck handshakes leave only an hs entry,
// so without this a transient failure would strand a peer until the roster
// itself changed. In-flight setups and fresh handshakes are left alone.
func (e *engine) reconcilePeers(roster []rosterMember) {
	now := time.Now()
	for _, m := range roster {
		if m.Username == "" || m.Username == e.me {
			continue
		}
		e.mu.Lock()
		_, live := e.noise[m.Username]
		_, hs := e.hs[m.Username]
		started := e.hsAt[m.Username]
		backoff := e.lastFail[m.Username]
		flaps := e.failCount[m.Username]
		e.mu.Unlock()
		if live {
			continue
		}
		if hs {
			if now.Sub(started) < stuckHsTTL {
				continue // handshake in progress; leave it alone
			}
			// stuck: full restart (fresh PC + fresh Noise = fresh nonces).
			// The 60s stall already served as the throttle — clear any
			// retry penalty so the replacement attempt starts now.
			e.mesh.dropPeer(m.Username)
			e.dropNoise(m.Username)
			e.mu.Lock()
			delete(e.lastFail, m.Username)
			delete(e.failCount, m.Username)
			e.mu.Unlock()
		} else if e.mesh.hasPeer(m.Username) {
			continue // mesh setup in flight; leave it alone
		}
		// Throttle restarts: a repeatedly flapping peer re-runs full setup
		// at most every setupRetryBackoff. A first (or long-quiet) failure
		// retries on the next beat instead — transient drops and rejoins
		// recover in seconds, not backoff windows.
		if flaps >= 2 && !backoff.IsZero() && now.Sub(backoff) < setupRetryBackoff {
			continue
		}
		e.mesh.ensurePeer(m.Username)
	}
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
	default:
		if e.cb.onSignalNote != nil {
			e.cb.onSignalNote(n)
		}
	}
}

// ─── Noise handshake drive ─────────────────────────────────────────────────
// Deterministic initiator: the lexicographically smaller username ALWAYS
// initiates. A smaller side receiving noise1 ignores it (it will initiate);
// this makes glare structurally impossible.

func (e *engine) onMeshUp(peer string) {
	e.maybeInitiate(peer)
	// larger side waits for noise1; nothing else to do on channel open
}

// maybeInitiate starts the Noise handshake as the deterministic initiator
// (smaller username). Called from onMeshUp AND when a beat learns a peer
// whose channel is already open — without the second call site, peers
// learned after their channel opened would never handshake until a flap.
func (e *engine) maybeInitiate(peer string) {
	if e.me >= peer {
		return
	}
	e.mu.Lock()
	_, live := e.noise[peer]
	_, hs := e.hs[peer]
	e.mu.Unlock()
	if live || hs {
		return
	}
	ps, m1, err := beginNoise(e.id, peer, true)
	if err != nil {
		e.emitErr(err)
		return
	}
	epoch := time.Now().UnixNano()
	e.trackHs(peer, ps, epoch)
	if err := e.sig.signalSend(peer, noiseSig1, wrapHs(epoch, m1)); err != nil {
		e.emitErr(err)
	}
}

// triggerRefresh refreshes the roster outside the beat cadence when live
// traffic proves it stale (notes/boxes from unknown senders). Throttled
// to one per 2s; the beat loop owns the steady state.
func (e *engine) triggerRefresh() {
	e.mu.Lock()
	if time.Since(e.lastTrigger) < 2*time.Second {
		e.mu.Unlock()
		return
	}
	e.lastTrigger = time.Now()
	e.mu.Unlock()
	e.beatOnce()
}

func (e *engine) onHandshakeNote(n signalNote) {
	epoch, raw, err := unwrapHs(n.Payload)
	if err != nil {
		return
	}
	e.mu.Lock()
	_, known := e.roster[n.From]
	e.mu.Unlock()
	if !known {
		e.triggerRefresh()
	}
	switch n.Type {
	case noiseSig1:
		if e.me < n.From {
			return // I initiate; ignore their attempt (glare rule)
		}
		e.mu.Lock()
		completed, done := e.completedEpoch[n.From]
		e.mu.Unlock()
		if done && epoch <= completed {
			return // duplicate/redelivered note: must never touch the live session
		}
		if done {
			// Genuine restart (newer epoch after a completed session):
			// drop stale transport + session FIRST so both sides converge
			// on this handshake instead of straddling two generations
			// (cross-encrypting = mutual decrypt fails). First contact
			// (no completed session) drops nothing — the transport the
			// handshake itself needs stays up.
			e.mesh.dropPeer(n.From)
			e.dropNoise(n.From)
			// Deliberate restart, not failure: clear any retry penalty so
			// the transport re-establishes immediately instead of sitting
			// out the 30s flap backoff on inbox fallback.
			e.mu.Lock()
			delete(e.lastFail, n.From)
			delete(e.failCount, n.From)
			e.mu.Unlock()
		}
		ps, _, err := beginNoise(e.id, n.From, false)
		if err != nil {
			return
		}
		e.trackHs(n.From, ps, epoch)
		m2, err := ps.stepNoise(raw)
		if err != nil || m2 == nil {
			e.dropNoise(n.From)
			return
		}
		_ = e.sig.signalSend(n.From, noiseSig2, wrapHs(epoch, m2))
	case noiseSig2:
		e.mu.Lock()
		ps, ok := e.hs[n.From]
		wantEpoch, tracked := e.hsEpoch[n.From]
		e.mu.Unlock()
		if !ok || !tracked || wantEpoch != epoch {
			return // stale retransmit for a superseded attempt
		}
		m3, err := ps.stepNoise(raw)
		if err != nil {
			e.dropNoise(n.From)
			return
		}
		if m3 != nil {
			_ = e.sig.signalSend(n.From, noiseSig3, wrapHs(epoch, m3))
		}
		e.verifyReady(n.From, ps)
	case noiseSig3:
		e.mu.Lock()
		ps, ok := e.hs[n.From]
		wantEpoch, tracked := e.hsEpoch[n.From]
		e.mu.Unlock()
		if !ok || !tracked || wantEpoch != epoch {
			return // stale retransmit for a superseded attempt
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
	if !ok {
		// Roster hasn't learned this peer yet (fast join-chat race), not
		// an attack: drop the attempt FIRST so the refresh below sees a
		// clean slate, then refresh now. The roster-added path restarts
		// the handshake with the key known.
		e.mu.Unlock()
		e.dropNoise(peer)
		e.triggerRefresh()
		return
	}
	if !equalBytes(want, ps.remoteKey()) {
		// Back off before any retry: under active key-swap attack this
		// path would otherwise handshake-storm every beat.
		e.lastFail[peer] = time.Now()
		e.failCount[peer] = 2
		e.mu.Unlock()
		e.mesh.dropPeer(peer)
		e.dropNoise(peer)
		e.emitErr(fmt.Errorf("KEY SWAP ALERT for %s: handshake key does not match roster — possible attack, channel dropped", peer))
		return
	}
	e.noise[peer] = ps
	delete(e.hs, peer)
	delete(e.hsAt, peer)
	// Handshake epoch graduates: future notes at/below this epoch are
	// duplicates and must never touch the live session again.
	if epoch, ok := e.hsEpoch[peer]; ok {
		e.completedEpoch[peer] = epoch
		delete(e.hsEpoch, peer)
	}
	// Healthy peers carry no retry penalty.
	delete(e.lastFail, peer)
	delete(e.failCount, peer)
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
	boxes, epoch, err := e.sig.inboxFetch()
	if err != nil {
		return
	}
	// Roster generation moved (join/leave/prune elsewhere): refresh the
	// roster NOW on the 2s inbox cadence instead of waiting out the 5s
	// beat. Costs nothing extra — the epoch rides the fetch we already
	// made — and triggerRefresh throttles stampedes to one per 2s.
	e.mu.Lock()
	changed := epoch != e.lastEpoch
	e.lastEpoch = epoch
	e.mu.Unlock()
	if changed {
		e.triggerRefresh()
	}
	if len(boxes) == 0 {
		return
	}
	var ack []string
	for _, b := range boxes {
		// NOTE: no seen-prefilter here. Inbound dedup lives in the
		// consumers (TUI/headless ack every copy they receive and display
		// only the first), so redelivered boxes re-dispatch harmlessly and
		// are acked below. Engine-side marking used to swallow retries for
		// frames the consumer never consumed — a permanent silent loss.
		e.mu.Lock()
		senderKey, known := e.roster[b.From]
		e.mu.Unlock()
		if !known {
			// Sender not in MY roster snapshot (typically it is stale and
			// a beat refresh is pending). Refresh now — the sender is
			// provably live — and deliver immediately if learned.
			// Otherwise leave the box UNACKED for a later poll. Purging
			// here ate legitimate mail whenever rosters lagged.
			// Stragglers are bounded by the 1h server TTL + per-user cap.
			e.triggerRefresh()
			e.mu.Lock()
			senderKey, known = e.roster[b.From]
			e.mu.Unlock()
			if !known {
				continue
			}
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
	// NO inbound dedup here by design. Consumers (TUI, headless) dedup by
	// msgId themselves AND ack every copy they receive. The old
	// engine-side marking swallowed backstop retries for frames the
	// consumer never consumed (e.g. a dropped TUI queue slot under burst):
	// the retry arrived, dispatch dropped it as "seen", no ack ever went
	// out, the sender retried into the void 3x and gave up — one
	// permanently vanished message with no error anywhere. At-least-once
	// delivery plus consumer-side exactly-once display is the correct
	// split. Stream frames (shared msgId) were never marked and still
	// aren't; only chat/single-box frames need consumer dedup.
	if (f.Type == frameChat || f.Type == frameFile) && f.MsgId == "" {
		return // protocol garbage: displayable frames need ids
	}
	switch f.Type {
	case frameChat:
		if e.cb.onChat != nil {
			e.cb.onChat(engineChat{MsgId: f.MsgId, From: f.From, To: f.To, Text: f.Data})
		}
	case frameAck:
		// The ack names the ORIGINAL message in Data (MsgId is the ack's
		// own fresh id). Graduate the sender's backstop entry, if any.
		orig := f.Data
		if orig == "" {
			orig = f.MsgId
		}
		e.ackReceived(f.From, orig)
		if e.cb.onDelivered != nil {
			e.cb.onDelivered(orig)
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
		e.mu.Lock()
		synced := e.rosterSynced
		e.mu.Unlock()
		if synced {
			// Genuinely alone in the room: chatting is allowed — the
			// local echo stands, there is simply nobody to fan out to.
			return nil
		}
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
	// fast path: live Noise session + open channel. Serialized per peer so
	// concurrent chat and file-upload goroutines can't swap two frames on
	// the wire out of Noise nonce order (the receiver would fail-decrypt
	// the first arrival and tear down a healthy session).
	if ok && ps.isReady() {
		sm := e.sendMuFor(peer)
		sm.Lock()
		ct, encErr := ps.encrypt(raw)
		var sendErr error
		if encErr == nil {
			sendErr = e.mesh.send(peer, ct)
		}
		sm.Unlock()
		if encErr == nil && sendErr == nil {
			// Mesh delivery is NOT assumed: the frame may land in a
			// dead or cross-generation session and be dropped on
			// decrypt. Track chat/file payloads for the ack backstop
			// (streams and control frames are excluded — see below).
			if f.Type == frameChat || f.Type == frameFile {
				e.trackUnacked(peer, f)
			}
			return nil
		}
		if encErr == nil {
			// Encrypted into a dead channel: the send nonce advanced but
			// the peer never got the ciphertext, so the session's nonce
			// streams are now desynced — every FUTURE mesh frame would
			// fail decrypt too. Tear down to force a clean re-handshake
			// instead of limping with a poisoned session, then fall
			// through to the durable inbox path.
			e.mesh.dropPeer(peer)
			e.dropNoise(peer)
		}
		// channel dead: fall through to inbox (mesh will report down)
	}
	if !known {
		return fmt.Errorf("unknown peer %s", peer)
	}
	// Stream frames (meta/chunks/complete share one msgId) must NEVER take
	// the inbox path: the server keys boxes by msgId, so a chunk would
	// overwrite its siblings and the receiver's assembly could never
	// complete — a silent file loss. Fail loud instead; the sender retries
	// once the direct line is back.
	switch f.Type {
	case frameFileMeta, frameFileChunk, frameFileComplete:
		return fmt.Errorf("direct line to %s dropped mid-stream — retry the file", peer)
	}
	if err := e.sendInbox(peer, key, f, raw); err != nil {
		// Durable path failed too (transient server/rate-limit errors):
		// arm the ack backstop so the frame is retried instead of lost.
		// Bounded and deduped on receipt, so a false failure (the box
		// actually landed — HSET is idempotent on msgId) just overwrites.
		if f.Type == frameChat || f.Type == frameFile {
			e.trackUnacked(peer, f)
		}
		return err
	}
	return nil
}

// sendInbox seals one frame as a durable pairwise box. Re-sealed per call
// (fresh ephemeral key), so retries never replay identical bytes.
func (e *engine) sendInbox(peer string, key []byte, f frame, raw []byte) error {
	box, err := sealBox(e.id, key, raw)
	if err != nil {
		return err
	}
	return e.sig.inboxSend(peer, f.MsgId, inboxBoxKind, box)
}

// sendMuFor returns the per-peer mesh fast-path serializer, creating it.
// Callers hold it across encrypt+send so frame order on the wire matches
// Noise nonce order for that peer.
func (e *engine) sendMuFor(peer string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	sm, ok := e.sendMu[peer]
	if !ok {
		sm = &sync.Mutex{}
		e.sendMu[peer] = sm
	}
	return sm
}

// trackUnacked records a mesh-sent frame for the ack backstop.
func (e *engine) trackUnacked(peer string, f frame) {
	e.mu.Lock()
	e.unacked[unackedKey(f.MsgId, peer)] = &pendingAck{to: peer, f: f, sent: time.Now()}
	e.mu.Unlock()
}

// ackReceived graduates a tracked frame: the peer confirmed receipt.
func (e *engine) ackReceived(peer, msgId string) {
	if msgId == "" {
		return
	}
	e.mu.Lock()
	delete(e.unacked, unackedKey(msgId, peer))
	e.mu.Unlock()
}

// ─── ack backstop (retry loop) ──────────────────────────────────────────────
// Mesh-sent frames whose ack doesn't arrive within ackTimeout are re-sent
// via the durable inbox path (never via mesh — replaying into a suspect
// session risks duplicates AND nonce churn). Bounded by maxInboxRetries,
// then dropped; the receiver dedups by msgId so a late mesh delivery plus
// an inbox retry can never double-display.

func (e *engine) retryLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(retryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.retryOnce()
		}
	}
}

func (e *engine) retryOnce() {
	now := time.Now()
	e.mu.Lock()
	var due []*pendingAck
	for k, p := range e.unacked {
		if now.Sub(p.sent) < ackTimeout {
			continue
		}
		if _, known := e.roster[p.to]; !known {
			delete(e.unacked, k) // peer left: nothing to retry to
			continue
		}
		if p.tries >= maxInboxRetries {
			delete(e.unacked, k)
			continue
		}
		p.tries++
		p.sent = now
		due = append(due, p)
	}
	e.mu.Unlock()
	for _, p := range due {
		raw, err := encodeFrame(p.f)
		if err != nil {
			continue
		}
		e.mu.Lock()
		key := e.roster[p.to]
		e.mu.Unlock()
		_ = e.sendInbox(p.to, key, p.f, raw)
	}
}

func (e *engine) sendChat(to, text string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("empty message")
	}
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
	roster, epoch, err := e.sig.heartbeat("", nil)
	if err != nil {
		return
	}
	e.mu.Lock()
	e.presence = roster
	e.lastEpoch = epoch
	e.mu.Unlock()
	e.setRoster(roster)
}

func (e *engine) sendAck(to, msgId string) error {
	id, err := newMsgId()
	if err != nil {
		return err
	}
	// Data names the ORIGINAL message: the ack's own MsgId is fresh per
	// ack, so without this the sender could never correlate delivery.
	ack := newFrame(frameAck, id, e.me, to)
	ack.Data = msgId
	return e.sendFrame(to, ack)
}

func (e *engine) emitErr(err error) {
	if e.cb.onError != nil {
		e.cb.onError(err)
	}
}

// ─── files ──────────────────────────────────────────────────────────────────

func (e *engine) sendFile(ctx context.Context, to, path, display string, prog chan<- uploadProgressMsg) (string, int64, error) {
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
		_, _, serr := e.sendFileStream(ctx, to, id, display, data, sumHex, prog)
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
const (
	// fileBackpressureHigh caps SCTP bytes in flight per file stream.
	// Without it a 25MB transfer fills kernel buffers and chat frames
	// queue behind megabytes on the same ordered channel (head-of-line
	// blocking = chat latency + apparent loss). 512KB bounds the chat
	// delay behind bulk bytes while keeping enough window for healthy
	// throughput on high-RTT links.
	fileBackpressureHigh = 512 * 1024
	// fileBackpressureStall aborts a stream whose buffers never drain
	// (dead link the PC state hasn't noticed yet) instead of wedging the
	// single upload-queue slot forever. Loud error; the user retries.
	fileBackpressureStall = 60 * time.Second
	// fileBackpressurePoll sets the drain-check cadence inside a stream.
	fileBackpressurePoll = 10 * time.Millisecond
)

// awaitDrain blocks until every recipient's queued bytes fit under the
// backpressure window (or the stall deadline hits). Broadcasts wait on
// the slowest peer — one wedged receiver must not wedge the sender, so
// the deadline converts that into a loud error instead.
func (e *engine) awaitDrain(ctx context.Context, to string) error {
	deadline := time.Now().Add(fileBackpressureStall)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var max uint64
		if to != "" {
			max = e.mesh.bufferedAmount(to)
		} else {
			e.mu.Lock()
			for u := range e.roster {
				if n := e.mesh.bufferedAmount(u); n > max {
					max = n
				}
			}
			e.mu.Unlock()
		}
		if max <= fileBackpressureHigh {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("direct line stalled under backpressure — retry the file")
		}
		time.Sleep(fileBackpressurePoll)
	}
}

func (e *engine) sendFileStream(ctx context.Context, to, id, display string, data []byte, sumHex string, prog chan<- uploadProgressMsg) (string, int64, error) {
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
		if err := e.awaitDrain(ctx, to); err != nil {
			return "", 0, err
		}
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
	e.mu.Lock()
	announced := e.announced[peer]
	// Flap counting: a first drop retries on the next beat (likely
	// transient — the common rejoin case). Only repeated drops in quick
	// succession arm the setupRetryBackoff throttle, so one flapping peer
	// can't churn PC+handshake storms at beat frequency.
	prev := e.lastFail[peer]
	if !prev.IsZero() && time.Since(prev) < 60*time.Second {
		e.failCount[peer]++
	} else {
		e.failCount[peer] = 1
	}
	e.lastFail[peer] = time.Now()
	e.mu.Unlock()
	// Notify the UI only for peers that once had verified E2E. Setup-time
	// failures (never ready) retry quietly via reconcile — previously every
	// failed setup printed "lost direct line", which was both wrong (no
	// line ever existed) and spammy.
	if announced && e.cb.onPeerLost != nil {
		e.cb.onPeerLost(peer)
	}
}
