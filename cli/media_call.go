package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// ─── Media publishing (/audio voice calls) ────────────────────────────────
//
// One model: publish. /audio toggles what THIS machine sends to its current
// scope (the DM peer, or every online room member); receivers dial the
// publisher back over per-peer Noise_XX UDP sessions and play. There is no
// ringing, no accepting, no busy: announces are idempotent, watchers reply
// with their own coords so the publisher can dial back, and handshake roles
// follow the username glare rule (concurrent publishers never deadlock).
//
// Signaling (rides the existing signal queue, tiny + rare):
//   media-live {ip, ips, port, audio} — "I am sending audio to you"
//   media-stop {audio}                 — "I stopped sending audio"
// A Live=false announce is a watcher's join reply (coords for the
// publisher's fan-out; never rendered, never re-replied).
//
// One speaker heard at a time (pin model): mixed voice from two publishers
// cannot be un-corrupted, so the pin holds until 2s of silence.
//
// Path healing: every 2s the manager checks every expected peer (we send
// to them or they send to us); a session silent >10s is re-probed at most
// once per 30s, with one honest verdict line per outcome.

const (
	mediaLive = "media-live"
	mediaStop = "media-stop"
	// mediaWant asks a peer to (re-)announce: a watcher with a stale or
	// missing announce and no session prompts the publisher instead of
	// blindly probing. Bounded like restart notes.
	mediaWant = "media-want"

	callWatchdogAfter = 10 * time.Second
	healCheckEvery    = 2 * time.Second
	healEvery         = 30 * time.Second
)

type mediaAnnouncePayload struct {
	IP    string   `json:"ip"`
	Ips   []string `json:"ips,omitempty"`
	Port  int      `json:"port"`
	Live  bool     `json:"live"`
	Audio bool     `json:"audio"`
}

// mediaUICallbacks is the manager → UI surface (never called from the
// network path without a snapshot).
type mediaUICallbacks struct {
	onInfo  func(info string)   // one-line status/errors for the status line
	onLevel func(level float64) // mic loudness 0..1 (throttled)
}

type mediaManager struct {
	me       string
	id       *identityKey
	sendNote func(to, noteType, payload string) error
	roster   func() map[string][]byte
	micSrc   func() (<-chan []int16, func(), error)
	playSink func() (*speaker, error)
	cb       mediaUICallbacks
	lanIPs   func() []string
	dialIP   string // test hook: pins advertised coords to loopback

	mu sync.Mutex
	// Publish state: what we send, and to whom (scope locked at toggle;
	// roster ticks top up fresh joiners and prune leavers).
	audioOn bool
	audioTo map[string]bool
	// Watch state: who sends to us.
	pubAudio map[string]bool
	replied  map[string]bool // announced/replied peers (no note storms)

	transport *mediaTransport
	started   bool
	healStop  chan struct{}
	stopped   bool // set by stopAll: ensureTransport must not allocate after teardown
	wg        sync.WaitGroup

	// audio live-cycle
	audioStop   chan struct{}
	audioRxStop chan struct{}
	stopMic     func()
	stopPlayFn  func()
	txVoice     *opusVoice
	rxVoice     *opusVoice // kept for compat; decode is per-peer (rxDecs)
	rxJbs       map[string]*jitterBuffer
	rxDecs      map[string]*opusVoice
	playoutGen  int // generation: exiting playout only clears current gen

	// render counters (diagnostics on toggle lines)
	txFrames    uint64
	rxFrames    uint64
	remoteShows uint64
	selfShows   uint64

	// path healing
	lastHeal  map[string]time.Time
	healTries int
	// healPingAt tracks the last probe ping per peer for two-phase heal:
	// ping first, restart the session only if it stays silent.
	healPingAt map[string]time.Time
	// Session establishment with retries: one lost probe or one lost
	// announce must never permanently strand a peer (the old join ran
	// exactly once — intermittent "works for some users" came from this).
	// join-retry + heal state per peer
	peerAnn     map[string]mediaAnnouncePayload // last announce per publisher (re-join coords)
	joinTries   map[string]int
	lastJoinAt  map[string]time.Time
	joinVerdict map[string]bool // honest "unreachable" line already shown
	// announce timing per peer (re-announce + want-note throttles)
	annRxAt      map[string]time.Time // last mediaLive received
	lastWantSent map[string]time.Time // last media-want sent
	lastRenotify time.Time            // last scope-wide re-announce
	// absentTicks counts consecutive roster ticks a scope member has been
	// missing (prune grace against heartbeat flaps; see pruneGraceTicks).
	absentTicks map[string]int
}

func newMediaManager(me string, id *identityKey, sendNote func(to, noteType, payload string) error, roster func() map[string][]byte, cb mediaUICallbacks) *mediaManager {
	return &mediaManager{
		me: me, id: id, sendNote: sendNote, roster: roster, cb: cb,
		lanIPs:       localLANIPs,
		micSrc:       openMicFrames,
		playSink:     openPlaySink,
		audioTo:      map[string]bool{},
		pubAudio:     map[string]bool{},
		replied:      map[string]bool{},
		rxJbs:        map[string]*jitterBuffer{},
		rxDecs:       map[string]*opusVoice{},
		lastHeal:     map[string]time.Time{},
		healPingAt:   map[string]time.Time{},
		peerAnn:      map[string]mediaAnnouncePayload{},
		joinTries:    map[string]int{},
		lastJoinAt:   map[string]time.Time{},
		joinVerdict:  map[string]bool{},
		annRxAt:      map[string]time.Time{},
		lastWantSent: map[string]time.Time{},
		absentTicks:  map[string]int{},
	}
}

// openMicFrames resolves the capture chain (fallback candidates + status
// name) and adapts it to the manager's micSrc signature.
func openMicFrames() (<-chan []int16, func(), error) {
	frames, stop, err := micSource()
	if err != nil {
		return nil, nil, err
	}
	out := make(chan []int16, 50)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case f, ok := <-frames:
				if !ok {
					return
				}
				select {
				case out <- f:
				case <-done:
					return
				}
			}
		}
	}()
	return out, func() { close(done); stop() }, nil
}

func openPlaySink() (*speaker, error) {
	return openSpeaker()
}

// localLANIPs returns usable local IPv4s, best first: skip virtual/docker/
// VPN interfaces, prefer RFC1918. A single wrong pick (docker bridge, VPN)
// blackholes all media with perfect signaling — so announces carry the
// whole list and the peer nominates by provable reachability.
func localLANIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var preferred, fallback []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := iface.Name
		virtual := strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "tun") ||
			strings.HasPrefix(name, "tap") || strings.HasPrefix(name, "tailscale") ||
			strings.HasPrefix(name, "utun")
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			v4 := ip.To4()
			if v4 == nil || ip.IsLoopback() {
				continue
			}
			if virtual {
				fallback = append(fallback, v4.String())
			} else {
				preferred = append(preferred, v4.String())
			}
		}
	}
	return append(preferred, fallback...)
}

// rosterMap converts engine roster snapshots to verify keys (skips garbage
// instead of failing the whole map).
func rosterMap(peers []rosterMember) map[string][]byte {
	out := map[string][]byte{}
	for _, m := range peers {
		if m.Username == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(m.Pubkey)
		if err != nil || len(raw) != 32 {
			continue
		}
		out[m.Username] = raw
	}
	return out
}

// ─── Status accessors (TUI chips + roster badges) ───────────────────────────

func (m *mediaManager) AudioOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audioOn
}

func (m *mediaManager) AudioPublishers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.pubAudio)
}

func (m *mediaManager) AudioScope() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.audioOn {
		return ""
	}
	return describeScope(m.audioTo)
}

// Hearing reports whether any remote audio is being played.
func (m *mediaManager) Hearing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pubAudio) > 0
}

// MediaActive reports whether any media state exists (exit cleanup gate).
func (m *mediaManager) MediaActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audioOn || len(m.pubAudio) > 0 || m.transport != nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (m *mediaManager) callbacks() mediaUICallbacks {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cb
}

func (m *mediaManager) emit(info string) {
	if cb := m.callbacks(); cb.onInfo != nil {
		cb.onInfo(info)
	}
}

// ─── Scope + toggles (the two user commands) ────────────────────────────────

// scopeFor resolves "wherever the user is": their DM peer, or the room.
func (m *mediaManager) scopeFor(targetUser string, roomUsers []string) []string {
	if targetUser != "" {
		return []string{targetUser}
	}
	out := make([]string, 0, len(roomUsers))
	for _, u := range roomUsers {
		if u != "" && u != m.me {
			out = append(out, u)
		}
	}
	return out
}

func describeScope(set map[string]bool) string {
	switch len(set) {
	case 0:
		return "nobody (alone here)"
	case 1:
		for p := range set {
			return p
		}
	}
	return fmt.Sprintf("the room (%d people)", len(set))
}

// ToggleAudio turns the mic on (announce to scope) or off (media-stop).
// Solo use is fully supported: with nobody else around the mic starts
// locally (level meter live, no announce goes out) and late joiners are
// admitted automatically via PublishTo.
func (m *mediaManager) ToggleAudio(scope []string) error {
	m.mu.Lock()
	if m.audioOn {
		m.mu.Unlock()
		m.stopAudioPublish()
		m.emit("mic off")
		return nil
	}
	for _, p := range scope {
		if p != "" && p != m.me {
			m.audioTo[p] = true
		}
	}
	solo := len(m.audioTo) == 0
	desc := describeScope(m.audioTo)
	m.audioOn = true
	m.mu.Unlock()
	if _, err := m.ensureTransport(); err != nil {
		m.mu.Lock()
		m.audioOn, m.audioTo = false, map[string]bool{}
		m.mu.Unlock()
		return err
	}
	if solo {
		m.emit("mic live — preview only (alone here; peers join automatically)")
	} else {
		m.emit("mic live — talking to " + desc)
	}
	if err := m.publishAnnounce(); err != nil {
		return err
	}
	m.startMicTx()
	return nil
}

// publishAnnounce posts our coords + mask to every scope member we have
// not contacted yet (fresh joiners), via the signal queue.
func (m *mediaManager) publishAnnounce() error {
	m.mu.Lock()
	var targets []string
	for p := range m.audioTo {
		if !m.replied[p] {
			targets = append(targets, p)
		}
	}
	m.mu.Unlock()
	for _, to := range targets {
		if err := m.sendAnnounce(to, true); err != nil {
			continue // the roster tick retries via PublishTo
		}
		m.mu.Lock()
		m.replied[to] = true
		m.mu.Unlock()
	}
	return nil
}

// PublishTo announces to scope members missed earlier (late joiners) and
// prunes leavers. Live scope members join the send scope while audio is
// on: announcing without admitting them would complete the handshake yet
// never send them a frame (late joiners saw "media secured" and then
// silence forever). A send set pruned down from remotes to nobody stops
// its stream; a preview-only solo session (empty from the start) keeps
// running until the user toggles off or hangs up, and joiners are admitted
// the moment they appear.
func (m *mediaManager) PublishTo(scope []string) {
	m.mu.Lock()
	if !m.audioOn {
		m.mu.Unlock()
		return
	}
	live := map[string]bool{}
	for _, p := range scope {
		if p != "" && p != m.me {
			live[p] = true
		}
	}
	// Snapshot before pruning: a send set that HAD remotes and loses its
	// last one stops its stream (leavers). A set that was already empty
	// (preview-only solo session) keeps running — only an explicit toggle
	// or hangup stops it.
	hadAudioRemote := len(m.audioTo) > 0
	// Prune grace: a scope member missing from one tick is usually a
	// flapped heartbeat, not a leaver. Count consecutive absences and
	// prune only past the grace window; presence clears the count.
	// Without this, one missed 5s beat on a 2s tick tore down live
	// streams (mic loops, session respawns).
	for p := range m.audioTo {
		if live[p] {
			delete(m.absentTicks, p)
			continue
		}
		m.absentTicks[p]++
		if m.absentTicks[p] < pruneGraceTicks {
			continue
		}
		delete(m.absentTicks, p)
		delete(m.audioTo, p)
		delete(m.replied, p)
		m.maybeForgetJoinLocked(p)
	}
	if m.audioOn {
		for p := range live {
			m.audioTo[p] = true
		}
	}
	var fresh []string
	for p := range live {
		if !m.replied[p] {
			fresh = append(fresh, p)
		}
	}
	audioScopeEmpty := len(m.audioTo) == 0
	stopA := m.audioOn && hadAudioRemote && audioScopeEmpty
	m.mu.Unlock()
	if stopA {
		m.stopAudioPublish()
	}
	// Status lines only on real transitions (joiners admitted, last leaver
	// stopped a stream) — never a per-tick heartbeat, so preview-only solo
	// sessions stay quiet.
	if stopA {
		m.emit("sharing stopped — nobody left in scope")
	}
	if len(fresh) > 0 {
		m.emit("sharing now covers: " + describeScope(m.currentSendScope()))
	}
	for _, to := range fresh {
		if err := m.sendAnnounce(to, true); err != nil {
			continue
		}
		m.mu.Lock()
		m.replied[to] = true
		m.mu.Unlock()
	}
}

// currentSendScope returns the audio send scope for status lines.
func (m *mediaManager) currentSendScope() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for p := range m.audioTo {
		out[p] = true
	}
	return out
}

// pruneGraceTicks is how many consecutive 2s roster ticks a scope member
// may be absent before PublishTo prunes them. Heartbeats run every 5s, so
// a single missed beat must never kill a live stream (flapping roster =
// stop/restart churn, torn-down mic loops, session respawns).
const pruneGraceTicks = 3

// maybeForgetJoinLocked drops join-retry state when a peer is no longer
// either side of a media relationship. Runs under m.mu.
func (m *mediaManager) maybeForgetJoinLocked(p string) {
	if m.pubAudio[p] {
		return
	}
	delete(m.peerAnn, p)
	delete(m.joinTries, p)
	delete(m.lastJoinAt, p)
	delete(m.joinVerdict, p)
	delete(m.healPingAt, p)
	delete(m.annRxAt, p)
	delete(m.lastWantSent, p)
}

// sendAnnounce posts our UDP coords + mask to one peer.
func (m *mediaManager) sendAnnounce(to string, live bool) error {
	m.mu.Lock()
	t := m.transport
	audio := m.audioOn
	m.mu.Unlock()
	if t == nil {
		return fmt.Errorf("no media socket")
	}
	addr := t.localAddr()
	if addr == nil {
		return fmt.Errorf("no media socket")
	}
	ip, ips := m.advertiseAddrs()
	raw, _ := json.Marshal(mediaAnnouncePayload{IP: ip, Ips: ips, Port: addr.Port, Live: live, Audio: audio})
	return m.sendNote(to, mediaLive, string(raw))
}

// sendStop tells one peer we stopped audio.
func (m *mediaManager) sendStop(to string, audio bool) {
	raw, _ := json.Marshal(mediaAnnouncePayload{Audio: audio})
	_ = m.sendNote(to, mediaStop, string(raw))
}

// ensureTransport returns the live media socket, creating + starting it on
// first use (publish needs media without any call).
func (m *mediaManager) ensureTransport() (*mediaTransport, error) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil, fmt.Errorf("media stopped")
	}
	if m.transport != nil {
		t := m.transport
		m.mu.Unlock()
		m.startLoops()
		return t, nil
	}
	m.mu.Unlock()
	t, err := newMediaTransport(m.me, m.id, m.sendNote, m.rosterSnapshot(), m.mediaCB())
	if err != nil {
		return nil, err
	}
	t.start()
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		t.stop()
		return nil, fmt.Errorf("media stopped")
	}
	if m.transport != nil {
		existing := m.transport
		m.mu.Unlock()
		t.stop()
		m.startLoops()
		return existing, nil
	}
	m.transport = t
	m.mu.Unlock()
	m.startLoops()
	return t, nil
}

func (m *mediaManager) rosterSnapshot() func() map[string][]byte {
	return func() map[string][]byte {
		m.mu.Lock()
		fn := m.roster
		m.mu.Unlock()
		if fn == nil {
			return map[string][]byte{}
		}
		return fn()
	}
}

// advertiseAddrs snapshots the coords peers should dial (test overrides
// honored exactly like the old offer/answer path).
func (m *mediaManager) advertiseAddrs() (ip string, ips []string) {
	ips = localLANIPs()
	if m.lanIPs != nil {
		ips = m.lanIPs()
	}
	if len(ips) > 0 {
		ip = ips[0]
	}
	if m.dialIP != "" {
		ip, ips = m.dialIP, []string{m.dialIP}
	}
	return ip, ips
}

// joinPeer dials an announcer (probe → dial → handshake). Handshake roles
// follow the username glare rule, so concurrent publishers converge with
// no extra round. Idempotent: live sessions are left alone.
func (m *mediaManager) joinPeer(from string, ann mediaAnnouncePayload) {
	m.mu.Lock()
	t := m.transport
	ready := t != nil && t.peerReady(from)
	m.mu.Unlock()
	if ready {
		return
	}
	if ann.IP == "" && len(ann.Ips) == 0 {
		return // stop-style payload, not an announce
	}
	if t == nil {
		var err error
		if t, err = m.ensureTransport(); err != nil {
			m.emit("media from " + from + " unreachable (no socket)")
			return
		}
	}
	cands := append(append([]string{}, ann.Ips...), ann.IP)
	winner := t.nominate(cands, ann.Port)
	if winner == nil {
		m.emit("media from " + from + " unreachable (no route — AP isolation?)")
		return
	}
	if err := t.dialPeer(from, winner.IP.String(), winner.Port); err != nil {
		return
	}
	t.beginHandshake(from)
}

// ─── Signaling notes ────────────────────────────────────────────────────────

func (m *mediaManager) onSignalNote(n signalNote) {
	switch n.Type {
	case mediaHS1, mediaHS2, mediaHS3, mediaHSRestart:
		m.mu.Lock()
		t := m.transport
		m.mu.Unlock()
		if t == nil {
			return
		}
		// Any handshake activity from this peer proves they are alive and
		// engaged: reset the join-retry budget (a fresh announce is not
		// the only sign of life).
		m.mu.Lock()
		m.joinTries[n.From] = 0
		m.joinVerdict[n.From] = false
		delete(m.lastJoinAt, n.From)
		m.mu.Unlock()
		t.onHandshakeNote(n.From, n.Type, n.Payload)

	case mediaWant:
		// A watcher asks us to (re-)announce. Answer only if we actually
		// publish; the announce itself is idempotent on their side.
		m.mu.Lock()
		publishing := m.audioOn
		m.mu.Unlock()
		if publishing {
			_ = m.sendAnnounce(n.From, true)
		}

	case mediaLive:
		var ann mediaAnnouncePayload
		if err := json.Unmarshal([]byte(n.Payload), &ann); err != nil {
			return
		}
		if n.From == m.me {
			return
		}
		// Socket first: the join-reply needs our coords, and joinPeer
		// needs the read loop running.
		if _, err := m.ensureTransport(); err != nil {
			return
		}
		m.mu.Lock()
		changed := ""
		if ann.Live && ann.Audio && !m.pubAudio[n.From] {
			m.pubAudio[n.From] = true
			changed = describePublisherChange(n.From, ann)
		}
		t2 := m.transport
		audioTo := m.audioTo[n.From]
		m.mu.Unlock()
		// Reply with our coords whenever the session isn't up: the
		// one-shot `replied` flag is not enough — if our reply or their
		// dial was lost, only a fresh reply re-opens the publisher side.
		// Replies are idempotent (coords + mask), so repeats are cheap.
		needReply := ann.Live && (t2 == nil || !t2.peerReady(n.From)) &&
			!audioTo
		m.mu.Lock()
		if needReply {
			m.replied[n.From] = true
		}
		// Fresh announce coordinates + retry budget reset: re-announces
		// are the recovery signal for both sides.
		if ann.Live && ann.Audio {
			m.peerAnn[n.From] = ann
			m.joinTries[n.From] = 0
			m.joinVerdict[n.From] = false
			delete(m.lastJoinAt, n.From)
			m.annRxAt[n.From] = time.Now()
		}
		m.mu.Unlock()
		if needReply {
			// Watcher join-reply carries our coords for the publisher's
			// fan-out (never rendered, never re-replied).
			_ = m.sendAnnounce(n.From, false)
		}
		if changed != "" {
			m.emit(changed)
		}
		if ann.Audio {
			m.ensureAudioRx()
		}
		go m.joinPeer(n.From, ann)

	case mediaStop:
		// Any stop note ends audio from this peer (local stops always
		// carry audio; the video kind is retired).
		var st mediaAnnouncePayload
		_ = json.Unmarshal([]byte(n.Payload), &st)
		m.mu.Lock()
		wasAudio := m.pubAudio[n.From]
		delete(m.pubAudio, n.From)
		nowAudio := m.pubAudio[n.From]
		if !nowAudio {
			delete(m.peerAnn, n.From)
			delete(m.joinTries, n.From)
			delete(m.lastJoinAt, n.From)
			delete(m.joinVerdict, n.From)
			delete(m.annRxAt, n.From)
			delete(m.lastWantSent, n.From)
		}
		if !nowAudio {
			delete(m.rxJbs, n.From)
			delete(m.rxDecs, n.From)
		}
		hearing := len(m.pubAudio) > 0
		stopPlayout := !hearing && !m.audioOn && m.audioRxStop != nil
		m.mu.Unlock()
		if stopPlayout {
			m.stopAudioPlayout()
		}
		if wasAudio && !nowAudio {
			m.emit(n.From + " stopped audio")
		}
	}
}

func describePublisherChange(from string, ann mediaAnnouncePayload) string {
	return from + " is sharing audio"
}

// ─── Transport dispatch ─────────────────────────────────────────────────────

// mediaCB adapts manager handlers to the transport's callback table.
func (m *mediaManager) mediaCB() mediaCallbacks {
	return mediaCallbacks{
		onAudio: m.onRemoteAudio,
		onReady: func(peer, code string) {
			m.mu.Lock()
			m.lastHeal[peer] = time.Now() // fresh session: full heal window
			delete(m.joinVerdict, peer)
			delete(m.joinTries, peer)
			delete(m.lastJoinAt, peer)
			m.mu.Unlock()
			m.emit("media secured with " + peer)
		},
		onLost:  func(peer string) { m.emit("media line lost to " + peer + " — rejoining on traffic") },
		onError: func(err error) { m.emit("media: " + err.Error()) },
	}
}

// truncateVisible clips s to width visible cells (ANSI-aware).
func truncateVisible(s string, width int) string {
	var sb strings.Builder
	w := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			sb.WriteRune(r)
			continue
		}
		if inEsc {
			sb.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if w >= width {
			continue
		}
		sb.WriteRune(r)
		w++
	}
	for w < width {
		sb.WriteByte(' ')
		w++
	}
	return sb.String()
}

// ─── Mic TX + audio RX ──────────────────────────────────────────────────────

// startMicTx runs the mic → Opus → fan-out loop while audio is on. Single
// flight (audioStop guards).
func (m *mediaManager) startMicTx() {
	m.mu.Lock()
	if m.audioStop != nil {
		m.mu.Unlock()
		return
	}
	frames, stopMic, err := m.micSrc()
	if err != nil {
		scope := sortedKeys(m.audioTo)
		m.audioOn = false
		m.audioTo = map[string]bool{}
		m.mu.Unlock()
		// Announce already went out in ToggleAudio: withdraw it, or
		// watchers handshake and wait on silence (heal churn).
		for _, to := range scope {
			m.sendStop(to, true)
		}
		m.emit("mic unavailable: " + err.Error())
		return
	}
	txVoice, err := newOpusVoice()
	if err != nil {
		stopMic()
		scope := sortedKeys(m.audioTo)
		m.audioOn = false
		m.audioTo = map[string]bool{}
		m.mu.Unlock()
		for _, to := range scope {
			m.sendStop(to, true)
		}
		m.emit("opus init failed: " + err.Error())
		return
	}
	stop := make(chan struct{})
	m.audioStop = stop
	m.stopMic = stopMic
	m.txVoice = txVoice
	m.mu.Unlock()

	source := ""
	if v, ok := lastMicSource.Load().(string); ok {
		source = v
	}
	if source != "" {
		m.emit("mic source: " + source)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		var seq uint16
		var ts uint32
		for restart := 0; ; restart++ {
			switch m.micRun(stop, txVoice, &seq, &ts, &frames) {
			case micStopped:
				return
			case micSourceDied:
				if restart >= 2 {
					m.emit("mic capture ended — restart attempts exhausted; /audio off + on to retry")
					return
				}
				m.emit(fmt.Sprintf("mic capture ended — restarting (%d/3)", restart+2))
				newFrames, newStop, err := m.micSrc()
				if err != nil {
					m.emit("mic restart failed: " + err.Error())
					return
				}
				m.mu.Lock()
				prev := m.stopMic
				m.stopMic = newStop
				m.mu.Unlock()
				if prev != nil {
					prev()
				}
				frames = newFrames
				continue
			}
		}
	}()
}

// micEnd distinguishes deliberate shutdown from a dead capture source.
type micEnd int

const (
	micStopped micEnd = iota
	micSourceDied
)

// micRun drives one capture-to-send loop until the source dies or the mic
// stops. seq/ts continue across source restarts (RTP continuity). A 1s
// keepalive ticker covers capture stalls AND speech pauses: without it a
// quiet sender looks identical to a dead path, and the watchdog would tear
// down a healthy session (the old self-inflicted "silent 10s+" loop).
func (m *mediaManager) micRun(stop chan struct{}, txVoice *opusVoice, seq *uint16, ts *uint32, frames *<-chan []int16) micEnd {
	keepalive := time.NewTicker(time.Second)
	defer keepalive.Stop()
	lastTx := time.Now()
	var nFrames uint64
	sendToScope := func(pkt []byte, kind byte) {
		m.mu.Lock()
		t := m.transport
		m.mu.Unlock()
		if t == nil {
			return
		}
		for _, peer := range t.readyPeers() {
			m.mu.Lock()
			inScope := m.audioTo[peer]
			m.mu.Unlock()
			if !inScope {
				continue
			}
			_ = t.sendMedia(peer, kind, pkt)
		}
		lastTx = time.Now()
	}
	for {
		select {
		case <-stop:
			return micStopped
		case <-keepalive.C:
			if time.Since(lastTx) < time.Second {
				continue
			}
			m.mu.Lock()
			t := m.transport
			m.mu.Unlock()
			if t == nil {
				return micSourceDied
			}
			for _, peer := range t.readyPeers() {
				m.mu.Lock()
				inScope := m.audioTo[peer]
				m.mu.Unlock()
				if !inScope {
					continue
				}
				_ = t.sendMedia(peer, mediaKindPing, nil)
			}
			lastTx = time.Now()
		case f, ok := <-*frames:
			if !ok {
				return micSourceDied
			}
			nFrames++
			// Live input meter (throttled): revives the header VU bar
			// and tells silence apart from a dead mic at toggle time.
			if nFrames%5 == 0 {
				if onLevel := m.callbacks().onLevel; onLevel != nil {
					onLevel(rmsLevel(f))
				}
			}
			pkt, err := txVoice.encode(f)
			if err != nil {
				continue
			}
			*seq++
			*ts += voiceFrameLen
			m.mu.Lock()
			t := m.transport
			m.mu.Unlock()
			if t == nil {
				return micSourceDied
			}
			ap := encodeAudioPacket(*seq, *ts, pkt)
			sendToScope(ap, mediaKindAudio)
		}
	}
}

// ensureAudioRx builds (once) the playout pipeline: 20ms tick over the
// pinned sender's jitter buffer with in-band FEC gap repair. Runs while
// any peer publishes audio — no call involved.
func (m *mediaManager) ensureAudioRx() {
	m.mu.Lock()
	if m.rxVoice != nil {
		m.mu.Unlock()
		return
	}
	sp, err := m.playSink()
	if err != nil {
		m.mu.Unlock()
		m.emit("speaker unavailable: " + err.Error())
		return
	}
	play, stopPlay := sp.play, sp.close
	rxVoice, err := newOpusVoice()
	if err != nil {
		stopPlay()
		m.mu.Unlock()
		m.emit("opus init failed: " + err.Error())
		return
	}
	m.rxVoice = rxVoice
	m.playoutGen++
	rxGen := m.playoutGen
	if m.rxJbs == nil {
		m.rxJbs = map[string]*jitterBuffer{}
	}
	if m.rxDecs == nil {
		m.rxDecs = map[string]*opusVoice{}
	}
	m.stopPlayFn = stopPlay
	stop := make(chan struct{})
	m.audioRxStop = stop
	m.mu.Unlock()

	m.wg.Add(1)
	go func(gen int, voice *opusVoice) {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			// Generation guard: do not clobber a freshly-created pipeline
			// if playout was restarted while this goroutine exited.
			if m.playoutGen == gen {
				if m.rxVoice == voice {
					m.rxVoice = nil
				}
				m.rxJbs = map[string]*jitterBuffer{}
				m.rxDecs = map[string]*opusVoice{}
			}
			m.mu.Unlock()
		}()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		speakerDead := false
		last := map[string][]byte{}
		fecStreak := map[string]int{}
		// fecMaxStreak caps PLC extrapolation per talker: Opus in-band
		// FEC is only valid for the packet immediately after the
		// reference. Repeating FEC from the same stale packet every tick
		// synthesizes a robotic hum forever; after the cap the talker
		// contributes silence until real packets resume.
		const fecMaxStreak = 8
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// Speaker death surfaces once (paplay/ffplay killed
				// externally reads as permanent silence otherwise).
				if !speakerDead && sp.Dead() {
					speakerDead = true
					m.emit("speaker output failed (device unplugged?) — toggling /audio re-arms playout")
				}
				m.mu.Lock()
				anyPublisher := len(m.pubAudio) > 0
				rxV := m.rxVoice
				// Snapshot per-peer buffers+decoders; decode+mix below
				// without holding the manager lock.
				type talker struct {
					peer string
					jb   *jitterBuffer
					dec  *opusVoice
				}
				var talkers []talker
				if anyPublisher && rxV != nil {
					for peer, jb := range m.rxJbs {
						if _, ok := m.pubAudio[peer]; !ok {
							continue
						}
						dec := m.rxDecs[peer]
						if jb == nil || dec == nil {
							continue
						}
						talkers = append(talkers, talker{peer, jb, dec})
					}
				}
				m.mu.Unlock()
				if len(talkers) == 0 {
					continue
				}
				// One playout tick across all talkers: pop each jitter
				// buffer, decode with that peer's OWN decoder (Opus
				// state is per-stream; sharing one decoder across
				// talkers produced garbage), sum with soft clipping so
				// overlap never hard-clips.
				var acc [voiceFrameLen]int32
				active := 0
				for _, tk := range talkers {
					pkt, gap := tk.jb.pop()
					var pcm []int16
					if gap {
						if ref, ok := last[tk.peer]; ok && fecStreak[tk.peer] < fecMaxStreak {
							if fec, err := tk.dec.decodeFEC(ref); err == nil {
								pcm = fec
								fecStreak[tk.peer]++
							}
						}
					} else {
						last[tk.peer] = pkt.Opus
						fecStreak[tk.peer] = 0
						if d, err := tk.dec.decode(pkt.Opus); err == nil {
							pcm = d
						}
					}
					if len(pcm) == 0 {
						continue
					}
					active++
					for i, v := range pcm {
						if i >= len(acc) {
							break
						}
						acc[i] += int32(v)
					}
				}
				if active == 0 {
					continue
				}
				mixed := make([]int16, voiceFrameLen)
				// Normalize only on actual clip: a lone talker passes
				// through bit-transparent (the old always-on soft curve
				// halved single-speaker peaks); overlap scales to fit.
				var peak int32
				for _, s := range acc {
					if s < 0 {
						s = -s
					}
					if s > peak {
						peak = s
					}
				}
				if peak <= 32767 {
					for i, s := range acc {
						mixed[i] = int16(s)
					}
				} else {
					gain := float64(32767) / float64(peak)
					for i, s := range acc {
						mixed[i] = int16(float64(s) * gain)
					}
				}
				play(mixed)
			}
		}
	}(rxGen, rxVoice)
}

// onRemoteAudio routes a peer's audio into their jitter buffer. Every
// active talker gets its OWN Opus decoder (created on first packet):
// decoder state is per-stream, and the old shared decoder turned
// overlapping talkers into garbage. The playout tick mixes all talkers.
func (m *mediaManager) onRemoteAudio(peer string, pkt audioPacket) {
	m.mu.Lock()
	// Only buffer for publishers we actually watch; otherwise any peer
	// with a live session can grow rxJbs/rxDecs unboundedly (bounded today
	// by peer count, reclaimed only on mediaStop).
	if !m.pubAudio[peer] {
		m.mu.Unlock()
		return
	}
	if m.rxJbs == nil {
		m.rxJbs = map[string]*jitterBuffer{}
	}
	if m.rxDecs == nil {
		m.rxDecs = map[string]*opusVoice{}
	}
	jb := m.rxJbs[peer]
	if jb == nil {
		jb = newJitterBuffer()
		m.rxJbs[peer] = jb
	}
	if _, ok := m.rxDecs[peer]; !ok {
		dec, err := newOpusVoice()
		if err != nil {
			m.mu.Unlock()
			return
		}
		m.rxDecs[peer] = dec
	}
	m.mu.Unlock()
	jb.push(pkt)
}

// ─── Stops ──────────────────────────────────────────────────────────────────

// stopAudioPublish: mic off — media-stop to scope; keep hearing others.
func (m *mediaManager) stopAudioPublish() {
	m.mu.Lock()
	scope := sortedKeys(m.audioTo)
	m.audioTo = map[string]bool{}
	m.audioOn = false
	stop := m.audioStop
	m.audioStop = nil
	stopMic := m.stopMic
	m.stopMic = nil
	m.txVoice = nil
	keepPlayout := len(m.pubAudio) > 0
	var stopRx chan struct{}
	if !keepPlayout && m.audioRxStop != nil {
		stopRx = m.audioRxStop
		m.audioRxStop = nil
	}
	m.mu.Unlock()
	for _, to := range scope {
		m.sendStop(to, true)
	}
	if stop != nil {
		close(stop)
	}
	if stopMic != nil {
		stopMic()
	}
	if stopRx != nil {
		close(stopRx)
	}
}

// stopAudioPlayout stops the 20ms playout loop + speaker.
func (m *mediaManager) stopAudioPlayout() {
	m.mu.Lock()
	stop := m.audioRxStop
	m.audioRxStop = nil
	closeFn := m.stopPlayFn
	m.stopPlayFn = nil
	m.playoutGen++ // invalidate exiting goroutine's deferred reset
	m.rxVoice = nil
	m.rxJbs = map[string]*jitterBuffer{}
	m.rxDecs = map[string]*opusVoice{}
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if closeFn != nil {
		closeFn()
	}
}

// stopAll tears everything down (session exit / Ctrl+C).
func (m *mediaManager) stopAll() {
	m.mu.Lock()
	stop := m.healStop
	m.healStop = nil
	audioStop := m.audioStop
	audioRxStop := m.audioRxStop
	stopMic := m.stopMic
	stopPlay := m.stopPlayFn
	m.audioStop, m.audioRxStop, m.stopMic, m.stopPlayFn = nil, nil, nil, nil
	m.playoutGen++ // invalidate any exiting playout reset
	m.stopped = true
	m.healTries = 0
	m.absentTicks = map[string]int{}
	m.lastHeal = map[string]time.Time{}
	m.audioOn = false
	// Snapshot the scope BEFORE clearing: stop notes must reach everyone
	// we were publishing to, otherwise peers wait on silence + heal churn.
	audioScope := sortedKeys(m.audioTo)
	m.audioTo = map[string]bool{}
	m.pubAudio, m.replied = map[string]bool{}, map[string]bool{}
	m.rxDecs = map[string]*opusVoice{}
	m.peerAnn, m.joinTries, m.lastJoinAt, m.joinVerdict = map[string]mediaAnnouncePayload{}, map[string]int{}, map[string]time.Time{}, map[string]bool{}
	m.annRxAt, m.lastWantSent = map[string]time.Time{}, map[string]time.Time{}
	m.healPingAt = map[string]time.Time{}
	m.started = false
	m.mu.Unlock()
	// Best-effort stop notes so listeners do not wait on silence.
	for _, to := range audioScope {
		m.sendStop(to, true)
	}
	if stop != nil {
		close(stop)
	}
	if audioStop != nil {
		close(audioStop)
	}
	if audioRxStop != nil {
		close(audioRxStop)
	}
	m.wg.Wait()
	if stopMic != nil {
		stopMic()
	}
	if stopPlay != nil {
		stopPlay()
	}
	m.mu.Lock()
	t := m.transport
	m.transport = nil
	m.mu.Unlock()
	if t != nil {
		t.stop()
	}
}

// ─── Loops + healing ────────────────────────────────────────────────────────

// startLoops launches the heal ticker once.
func (m *mediaManager) startLoops() {
	m.mu.Lock()
	if m.started || m.healStop != nil {
		m.mu.Unlock()
		return
	}
	m.started = true
	stop := make(chan struct{})
	m.healStop = stop
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(healCheckEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.healthCheck()
			}
		}
	}()
}

// expectedPeers lists everyone we exchange media with right now.
func (m *mediaManager) expectedPeers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for p := range m.audioTo {
		add(p)
	}
	for p := range m.pubAudio {
		add(p)
	}
	for p := range m.peerAnn {
		add(p)
	}
	return out
}

const (
	joinRetryEvery  = 3 * time.Second
	maxJoinTries    = 10
	publishRenotify = 6 * time.Second
)

// healthCheck is the maintenance tick: it both retries session
// establishment (a lost probe or announce must not strand a peer forever —
// the old fire-once join is why media "worked for some users") and heals
// live-but-silent sessions.
func (m *mediaManager) healthCheck() {
	expected := m.expectedPeers()
	if len(expected) == 0 {
		return
	}
	m.mu.Lock()
	t := m.transport
	m.mu.Unlock()
	if t == nil {
		return
	}
	now := time.Now()
	for _, p := range expected {
		// Still relevant? (stops and leavers clear the sets before this.)
		m.mu.Lock()
		weSend := m.audioTo[p]
		theySend := m.pubAudio[p]
		ann, haveAnn := m.peerAnn[p]
		if !weSend && !theySend {
			delete(m.peerAnn, p)
			delete(m.joinTries, p)
			delete(m.lastJoinAt, p)
			delete(m.joinVerdict, p)
			delete(m.annRxAt, p)
			delete(m.lastWantSent, p)
			m.mu.Unlock()
			continue
		}
		tries := m.joinTries[p]
		lastJoin := m.lastJoinAt[p]
		verdictShown := m.joinVerdict[p]
		m.mu.Unlock()

		d := t.diagPeer(p)
		switch {
		case !d.Ready:
			// Session never formed: retry the join, both directions.
			if time.Since(lastJoin) < joinRetryEvery || tries >= maxJoinTries {
				if tries >= maxJoinTries && !verdictShown {
					m.mu.Lock()
					m.joinVerdict[p] = true
					m.mu.Unlock()
					m.emit("media to " + p + " not reachable after " +
						fmt.Sprint(maxJoinTries) + " join attempts — AP isolation or firewall likely")
				}
				continue
			}
			m.mu.Lock()
			m.joinTries[p]++
			m.lastJoinAt[p] = now
			lastWant := m.lastWantSent[p]
			annRx := m.annRxAt[p]
			m.mu.Unlock()
			if weSend {
				// Re-announce: fresh coords, watcher re-joins us.
				_ = m.sendAnnounce(p, true)
			}
			if theySend && haveAnn {
				// Re-join from the stored announce + re-reply so the
				// publisher can also re-dial us. Async: nominate blocks
				// up to 600ms and must never stall the 2s health tick
				// (with N peers the tick would drift seconds behind).
				// Tracked on wg + stopped-guarded so stopAll cannot be
				// resurrected after teardown.
				m.mu.Lock()
				stopped := m.stopped
				if !stopped {
					m.wg.Add(1)
				}
				m.mu.Unlock()
				if !stopped {
					go func(peer string, a mediaAnnouncePayload) {
						defer m.wg.Done()
						m.joinPeer(peer, a)
					}(p, ann)
				}
				_ = m.sendAnnounce(p, false)
			}
			// They publish (or published) but their last announce is
			// stale and there is still no session: ask them to announce
			// instead of blindly probing. Bounded: one want per 10s.
			if (theySend || haveAnn) && time.Since(annRx) > 10*time.Second &&
				time.Since(lastWant) > 10*time.Second {
				_ = m.sendNote(p, mediaWant, "{}")
				m.mu.Lock()
				m.lastWantSent[p] = now
				m.mu.Unlock()
			}
		case d.LastRxAge > callWatchdogAfter:
			// Live session gone quiet: heal runs every tick; healPeer
			// itself throttles probes vs restarts (async: nominate can
			// block up to 600ms per peer). Tracked + stopped-guarded.
			m.mu.Lock()
			stopped := m.stopped
			if !stopped {
				m.wg.Add(1)
			}
			m.mu.Unlock()
			if !stopped {
				go func(peer string) {
					defer m.wg.Done()
					m.healPeer(peer)
				}(p)
			}
		}
	}
	// Scope-wide re-announce (the missing periodic path): roster ticks
	// only cover membership *changes*, so a newcomer whose announce was
	// lost — or who arrived between ticks — would otherwise wait for the
	// next join/leave. Cheap: a few small notes per peer per interval.
	m.mu.Lock()
	renotify := m.audioOn && time.Since(m.lastRenotify) >= publishRenotify
	if renotify {
		m.lastRenotify = now
	}
	var scope []string
	if renotify {
		for p := range m.audioTo {
			scope = append(scope, p)
		}
	}
	m.mu.Unlock()
	if renotify {
		// Async: signalSend is an HTTP POST per peer; the 2s tick must
		// not wait on the network.
		go func(targets []string) {
			for _, to := range targets {
				_ = m.sendAnnounce(to, true)
			}
		}(scope)
	}
}

// emitJoinInfo is intentionally unused: retries are silent, outcomes are
// loud ("media secured with X" / the unreachable verdict line).

// healProbeWait is how long a probe ping gets to answer before the path
// is declared dead and the session restarts.
const healProbeWait = 4 * time.Second

// healPeer re-checks a silent peer's route in two phases: probe ping
// first, and only if the ping goes unanswered does it restart the
// handshake and re-exchange coords. The old code tore down the Noise
// session on mere quiet (speech pauses included); a quiet-but-healthy
// path now survives with a single ping.
// Runs in its own goroutine: joinPeer nominate blocks up to 600ms.
func (m *mediaManager) healPeer(peer string) {
	m.mu.Lock()
	t := m.transport
	weSend := m.audioTo[peer]
	ann, haveAnn := m.peerAnn[peer]
	lastPing := m.healPingAt[peer]
	m.mu.Unlock()
	if t == nil {
		return
	}
	if d := t.diagPeer(peer); d.Addr == "" {
		return // no route yet; the join retry handles first contact
	}
	if t.lastRxAt(peer).After(time.Now().Add(-callWatchdogAfter)) {
		// Traffic resumed while we scheduled: if a probe was in flight,
		// the path just proved itself — say so once and stand down.
		m.mu.Lock()
		_, probed := m.healPingAt[peer]
		if probed {
			delete(m.healPingAt, peer)
		}
		m.mu.Unlock()
		if probed {
			m.emit("media path to " + peer + " recovered")
		}
		return
	}
	if lastPing.IsZero() {
		// Phase 1: probe ping only. The pong updates lastRx on arrival;
		// if traffic resumes, no verdict and no restart ever happen.
		_ = t.sendMedia(peer, mediaKindPing, nil)
		m.mu.Lock()
		m.healPingAt[peer] = time.Now()
		m.mu.Unlock()
		return
	}
	if time.Since(lastPing) < healProbeWait {
		return // probe still in flight; give the pong time
	}
	// Phase 2: probe unanswered — the path is really dead. Restart the
	// session and re-exchange coords (restarts bounded by healEvery).
	m.mu.Lock()
	if time.Since(m.lastHeal[peer]) < healEvery {
		m.healPingAt[peer] = time.Now() // re-arm probe; retry restart later
		m.mu.Unlock()
		return
	}
	m.lastHeal[peer] = time.Now()
	m.healTries++
	tries := m.healTries
	delete(m.healPingAt, peer)
	m.mu.Unlock()
	if tries > 10 {
		m.emit("media to " + peer + " unstable — giving up auto-heal, toggle /audio to retry")
		return
	}
	t.sendRestart(peer)
	if weSend {
		_ = m.sendAnnounce(peer, true)
	}
	if haveAnn {
		m.joinPeer(peer, ann)
	}
	m.emit("media to " + peer + " silent " + callWatchdogAfter.String() + "+ — re-probing path")
}

// SetRoster binds the roster closure (engine constructed after manager).
func (m *mediaManager) SetRoster(fn func() map[string][]byte) {
	m.mu.Lock()
	m.roster = fn
	m.mu.Unlock()
}
