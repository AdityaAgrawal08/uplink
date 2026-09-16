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

// ─── Media publishing (/video + /audio, no calls) ───────────────────────────
//
// One model: publish. /video and /audio toggle what THIS machine sends to
// its current scope (the DM peer, or every online room member); receivers
// dial the publisher back over per-peer Noise_XX UDP sessions and render
// or play. There is no ringing, no accepting, no busy: announces are
// idempotent, watchers reply with their own coords so the publisher can
// dial back, and handshake roles follow the username glare rule (the same
// rule that made calls converge, so concurrent publishers never deadlock).
//
// Signaling (rides the existing signal queue, tiny + rare):
//   media-live {ip, ips, port, video, audio} — "I am sending X to you"
//   media-stop {video, audio}                — "I stopped sending X"
// A Live=false announce is a watcher's join reply (coords for the
// publisher's fan-out; never rendered, never re-replied).
//
// One video feed is shown and one speaker heard at a time (pin model):
// interleaved frames or mixed voice from two publishers cannot be
// un-corrupted, so the pin holds until 5s (video) / 2s (audio) of silence.
//
// Path healing: every 2s the manager checks every expected peer (we send
// to them or they send to us); a session silent >10s is re-probed at most
// once per 30s, with one honest verdict line per outcome.

const (
	mediaLive = "media-live"
	mediaStop = "media-stop"

	callWatchdogAfter = 10 * time.Second
	healCheckEvery    = 2 * time.Second
	healEvery         = 30 * time.Second
	videoPinTimeout   = 5 * time.Second
	audioPinTimeout   = 2 * time.Second
	// videoRenderMinInterval bounds paint cost: decode runs full-rate,
	// the TUI sheds (~14fps at this interval is smooth for ASCII).
	videoRenderMinInterval = 70 * time.Millisecond
)

type mediaAnnouncePayload struct {
	IP    string   `json:"ip"`
	Ips   []string `json:"ips,omitempty"`
	Port  int      `json:"port"`
	Live  bool     `json:"live"`
	Video bool     `json:"video"`
	Audio bool     `json:"audio"`
}

// mediaUICallbacks is the manager → UI surface (never called from the
// network path without a snapshot).
type mediaUICallbacks struct {
	onInfo       func(info string)    // one-line status/errors for the transcript
	onLevel      func(level float64)  // mic loudness 0..1 (throttled)
	onVideoFrame func(lines []string) // decoded remote ASCII frame
	onSelfFrame  func(lines []string) // local camera preview
}

type mediaManager struct {
	me       string
	id       *identityKey
	sendNote func(to, noteType, payload string) error
	roster   func() map[string][]byte
	micSrc   func() (<-chan []int16, func(), error)
	playSink func() (func([]int16), func(), error)
	cb       mediaUICallbacks
	lanIPs   func() []string
	dialIP   string // test hook: pins advertised coords to loopback

	mu sync.Mutex
	// Publish state: what we send, and to whom (scope locked at toggle;
	// roster ticks top up fresh joiners and prune leavers).
	videoOn bool
	audioOn bool
	videoTo map[string]bool
	audioTo map[string]bool
	// Watch state: who sends to us.
	pubVideo map[string]bool
	pubAudio map[string]bool
	replied  map[string]bool // announced/replied peers (no note storms)

	transport *mediaTransport
	started   bool
	healStop  chan struct{}
	wg        sync.WaitGroup

	// camera live-cycle (nil unless streaming)
	cameraStop    chan struct{}
	stopCameraSrc func()
	previewQ      chan vidFrame // self-view queue (bounded, drop-oldest)
	videoOnFlag   bool          // camera loop running (videoOn is the toggle)
	videoAsm      *fragAssembler
	rxFramesQ     chan vidFrame // remote decoded frames (bounded, drop-oldest)
	rxDone        chan struct{}
	rxPump        bool
	rxOnFlag      bool
	videoSrcFn    func() (<-chan vidFrame, func(), error)
	videoSeq      uint16
	videoTs       uint32

	// audio live-cycle
	audioStop   chan struct{}
	audioRxStop chan struct{}
	stopMic     func()
	stopPlayFn  func()
	txVoice     *opusVoice
	rxVoice     *opusVoice
	rxJbs       map[string]*jitterBuffer
	audioPinned string
	audioPinAt  time.Time

	// render geometry + throttle + counters (diagnostics on toggle lines)
	rxPinned       string
	rxPinAt        time.Time
	lastRemoteShow time.Time
	lastSelfShow   time.Time
	txFrames       uint64
	rxFrames       uint64
	remoteShows    uint64
	selfShows      uint64
	renderCols     int
	renderRows     int

	// path healing
	lastHeal  map[string]time.Time
	healTries int
	// Session establishment with retries: one lost probe or one lost
	// announce must never permanently strand a peer (the old join ran
	// exactly once — intermittent "works for some users" came from this).
	peerAnn     map[string]mediaAnnouncePayload // last announce per publisher (re-join coords)
	joinTries   map[string]int
	lastJoinAt  map[string]time.Time
	joinVerdict map[string]bool // honest "unreachable" line already shown
}

func newMediaManager(me string, id *identityKey, sendNote func(to, noteType, payload string) error, roster func() map[string][]byte, cb mediaUICallbacks) *mediaManager {
	return &mediaManager{
		me: me, id: id, sendNote: sendNote, roster: roster, cb: cb,
		lanIPs:      localLANIPs,
		micSrc:      openMicFrames,
		playSink:    openPlaySink,
		videoTo:     map[string]bool{},
		audioTo:     map[string]bool{},
		pubVideo:    map[string]bool{},
		pubAudio:    map[string]bool{},
		replied:     map[string]bool{},
		rxJbs:       map[string]*jitterBuffer{},
		lastHeal:    map[string]time.Time{},
		peerAnn:     map[string]mediaAnnouncePayload{},
		joinTries:   map[string]int{},
		lastJoinAt:  map[string]time.Time{},
		joinVerdict: map[string]bool{},
		renderCols:  videoPaneDefaultCols,
		renderRows:  videoPaneDefaultRows,
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

func openPlaySink() (func([]int16), func(), error) {
	sp, err := openSpeaker()
	if err != nil {
		return nil, nil, err
	}
	return sp.play, sp.close, nil
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

func (m *mediaManager) VideoOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.videoOn
}

func (m *mediaManager) AudioOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audioOn
}

func (m *mediaManager) VideoPublishers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.pubVideo)
}

func (m *mediaManager) AudioPublishers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedKeys(m.pubAudio)
}

// VideoScope describes who we show ("bob" / "the room (3 people)") or "".
// videoStatsLine summarizes the last publish in one line (frames sent,
// received, painted on both sides) — diagnostics without a command.
func (m *mediaManager) videoStatsLine() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("sent=%d recv=%d remotePaints=%d selfPaints=%d",
		m.txFrames, m.rxFrames, m.remoteShows, m.selfShows)
}

func (m *mediaManager) VideoScope() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.videoOn {
		return ""
	}
	return describeScope(m.videoTo)
}

func (m *mediaManager) AudioScope() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.audioOn {
		return ""
	}
	return describeScope(m.audioTo)
}

// Watching reports whether any remote video feed is being rendered.
func (m *mediaManager) Watching() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pubVideo) > 0 || m.rxOnFlag
}

// Hearing reports whether any remote audio is being played.
func (m *mediaManager) Hearing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pubAudio) > 0
}

// RxOn reports whether remote video has arrived at least once.
func (m *mediaManager) RxOn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rxOnFlag
}

// MediaActive reports whether any media state exists (exit cleanup gate).
func (m *mediaManager) MediaActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.videoOn || m.audioOn || len(m.pubVideo) > 0 || len(m.pubAudio) > 0 ||
		m.transport != nil
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

// ToggleVideo turns the camera on (announce to scope) or off (media-stop).
// Toggling on again while on re-announces to fresh scope members only.
func (m *mediaManager) ToggleVideo(scope []string) error {
	m.mu.Lock()
	if m.videoOn {
		m.mu.Unlock()
		m.stopVideoPublish()
		m.emit(m.videoStatsLine() + " · camera off")
		return nil
	}
	for _, p := range scope {
		if p != "" && p != m.me {
			m.videoTo[p] = true
		}
	}
	if len(m.videoTo) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("nobody to show (alone here)")
	}
	desc := describeScope(m.videoTo)
	m.videoOn = true
	m.mu.Unlock()
	if _, err := m.ensureTransport(); err != nil {
		m.mu.Lock()
		m.videoOn, m.videoTo = false, map[string]bool{}
		m.mu.Unlock()
		return err
	}
	m.emit("camera on — showing " + desc)
	if err := m.publishAnnounce(); err != nil {
		return err
	}
	return m.startCameraTx()
}

// ToggleAudio turns the mic on (announce to scope) or off (media-stop).
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
	if len(m.audioTo) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("nobody to talk to (alone here)")
	}
	desc := describeScope(m.audioTo)
	m.audioOn = true
	m.mu.Unlock()
	if _, err := m.ensureTransport(); err != nil {
		m.mu.Lock()
		m.audioOn, m.audioTo = false, map[string]bool{}
		m.mu.Unlock()
		return err
	}
	m.emit("mic live — talking to " + desc)
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
	for p := range m.videoTo {
		if !m.replied[p] {
			targets = append(targets, p)
		}
	}
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
// prunes leavers. Streams whose scope empties stop themselves.
func (m *mediaManager) PublishTo(scope []string) {
	m.mu.Lock()
	if !m.videoOn && !m.audioOn {
		m.mu.Unlock()
		return
	}
	live := map[string]bool{}
	for _, p := range scope {
		if p != "" && p != m.me {
			live[p] = true
		}
	}
	for p := range m.videoTo {
		if !live[p] {
			delete(m.videoTo, p)
			delete(m.replied, p)
			m.maybeForgetJoinLocked(p)
		}
	}
	for p := range m.audioTo {
		if !live[p] {
			delete(m.audioTo, p)
			delete(m.replied, p)
			m.maybeForgetJoinLocked(p)
		}
	}
	var fresh []string
	for p := range live {
		if !m.replied[p] {
			fresh = append(fresh, p)
		}
	}
	videoOn, audioOn := len(m.videoTo) > 0, len(m.audioTo) > 0
	m.mu.Unlock()
	if m.videoOn && !videoOn {
		m.stopVideoPublish()
	}
	if m.audioOn && !audioOn {
		m.stopAudioPublish()
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

// maybeForgetJoinLocked drops join-retry state when a peer is no longer
// either side of a media relationship. Runs under m.mu.
func (m *mediaManager) maybeForgetJoinLocked(p string) {
	if m.pubVideo[p] || m.pubAudio[p] {
		return
	}
	delete(m.peerAnn, p)
	delete(m.joinTries, p)
	delete(m.lastJoinAt, p)
	delete(m.joinVerdict, p)
}

// sendAnnounce posts our UDP coords + mask to one peer.
func (m *mediaManager) sendAnnounce(to string, live bool) error {
	m.mu.Lock()
	t := m.transport
	video, audio := m.videoOn, m.audioOn
	m.mu.Unlock()
	if t == nil {
		return fmt.Errorf("no media socket")
	}
	addr := t.localAddr()
	if addr == nil {
		return fmt.Errorf("no media socket")
	}
	ip, ips := m.advertiseAddrs()
	raw, _ := json.Marshal(mediaAnnouncePayload{IP: ip, Ips: ips, Port: addr.Port, Live: live, Video: video, Audio: audio})
	return m.sendNote(to, mediaLive, string(raw))
}

// sendStop tells one peer we stopped a kind.
func (m *mediaManager) sendStop(to string, video, audio bool) {
	raw, _ := json.Marshal(mediaAnnouncePayload{Video: video, Audio: audio})
	_ = m.sendNote(to, mediaStop, string(raw))
}

// ensureTransport returns the live media socket, creating + starting it on
// first use (publish needs media without any call).
func (m *mediaManager) ensureTransport() (*mediaTransport, error) {
	m.mu.Lock()
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
		t.onHandshakeNote(n.From, n.Type, n.Payload)

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
		if ann.Live && ann.Video && !m.pubVideo[n.From] {
			m.pubVideo[n.From] = true
			changed = describePublisherChange(n.From, ann)
		}
		if ann.Live && ann.Audio && !m.pubAudio[n.From] {
			m.pubAudio[n.From] = true
			if changed == "" {
				changed = describePublisherChange(n.From, ann)
			}
		}
		needReply := ann.Live && !m.replied[n.From] && !m.videoTo[n.From] && !m.audioTo[n.From]
		if needReply {
			m.replied[n.From] = true
		}
		// Fresh announce coordinates + retry budget reset: re-announces
		// are the recovery signal for both sides.
		if ann.Live && (ann.Video || ann.Audio) {
			m.peerAnn[n.From] = ann
			m.joinTries[n.From] = 0
			m.joinVerdict[n.From] = false
			delete(m.lastJoinAt, n.From)
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
		var st mediaAnnouncePayload
		_ = json.Unmarshal([]byte(n.Payload), &st) // bare {} = stop all
		m.mu.Lock()
		wasVideo := m.pubVideo[n.From]
		wasAudio := m.pubAudio[n.From]
		if st.Video || (!st.Video && !st.Audio) {
			delete(m.pubVideo, n.From)
		}
		if st.Audio || (!st.Video && !st.Audio) {
			delete(m.pubAudio, n.From)
		}
		nowVideo := m.pubVideo[n.From]
		nowAudio := m.pubAudio[n.From]
		if !nowVideo && !nowAudio {
			delete(m.peerAnn, n.From)
			delete(m.joinTries, n.From)
			delete(m.lastJoinAt, n.From)
			delete(m.joinVerdict, n.From)
		}
		if !nowVideo && m.rxPinned == n.From {
			m.rxPinned = ""
		}
		if !nowAudio {
			delete(m.rxJbs, n.From)
			if m.audioPinned == n.From {
				m.audioPinned = ""
			}
		}
		watching := len(m.pubVideo) > 0
		hearing := len(m.pubAudio) > 0
		var rdone chan struct{}
		if !watching && !m.videoOn {
			rdone = m.rxDone
			m.rxDone, m.rxPump, m.rxOnFlag = nil, false, false
			m.videoAsm = nil
		}
		stopPlayout := !hearing && !m.audioOn && m.audioRxStop != nil
		m.mu.Unlock()
		if rdone != nil {
			close(rdone)
		}
		if stopPlayout {
			m.stopAudioPlayout()
		}
		if wasVideo && !nowVideo {
			m.emit(n.From + " stopped video")
		}
		if wasAudio && !nowAudio {
			m.emit(n.From + " stopped audio")
		}
	}
}

func describePublisherChange(from string, ann mediaAnnouncePayload) string {
	switch {
	case ann.Video && ann.Audio:
		return from + " is sharing video + audio"
	case ann.Video:
		return from + " is sharing video"
	default:
		return from + " is sharing audio"
	}
}

// ─── Transport dispatch ─────────────────────────────────────────────────────

// mediaCB adapts manager handlers to the transport's callback table.
func (m *mediaManager) mediaCB() mediaCallbacks {
	return mediaCallbacks{
		onAudio: m.onRemoteAudio,
		onVideo: m.onVideoFrag,
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

// onTransportReady anchors healing (session live ⇒ silence after this is
// a path problem, not handshake timing).
func (m *mediaManager) onTransportReady(peer string) {
	m.mu.Lock()
	m.lastHeal[peer] = time.Now() // fresh session: give it a full window
	m.mu.Unlock()
}

// ─── Camera TX ──────────────────────────────────────────────────────────────

// startCameraTx is the single-flight camera loop (cameraStop guards).
func (m *mediaManager) startCameraTx() error {
	m.mu.Lock()
	if m.cameraStop != nil {
		m.mu.Unlock()
		return nil
	}
	srcFn := m.videoSrcFn
	if srcFn == nil {
		srcFn = cameraFrames
	}
	m.mu.Unlock()

	frames, stopSrc, err := srcFn()
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	m.mu.Lock()
	m.cameraStop = stop
	m.stopCameraSrc = stopSrc
	if m.previewQ == nil {
		m.previewQ = make(chan vidFrame, 4)
	}
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			m.videoOnFlag = false
			m.mu.Unlock()
		}()
		var n uint64
		for {
			select {
			case <-stop:
				return
			case f, ok := <-frames:
				if !ok {
					return
				}
				n++
				// Self-view at half rate (drop-if-full queue): halves
				// local render cost, zero network effect.
				if n%2 == 0 {
					select {
					case m.previewQ <- f:
					default:
					}
				}
				payloads, err := fragVideoFrame(f.Jpeg)
				if err != nil {
					continue
				}
				m.mu.Lock()
				t := m.transport
				m.videoTs++
				ts := m.videoTs
				m.txFrames++
				m.mu.Unlock()
				if t == nil {
					return
				}
				for i, p := range payloads {
					m.mu.Lock()
					m.videoSeq++
					seq := m.videoSeq
					m.mu.Unlock()
					pkt := encodeVideoFrag(seq, uint16(i), uint16(len(payloads)), ts, p)
					for _, peer := range t.readyPeers() {
						m.mu.Lock()
						inScope := m.videoTo[peer]
						m.mu.Unlock()
						if !inScope {
							continue
						}
						_ = t.sendMedia(peer, mediaKindVideo, pkt)
					}
				}
			}
		}
	}()
	m.mu.Lock()
	m.videoOnFlag = true
	if m.rxDone == nil {
		m.rxDone = make(chan struct{})
	}
	if !m.rxPump {
		m.rxPump = true
		m.startRenderPump()
	}
	m.mu.Unlock()
	return nil
}

// ─── Video RX ───────────────────────────────────────────────────────────────

// ensureVideoRxLocked builds the receive pipeline on first expected video.
// Runs under m.mu; returns true once the pipeline exists.
func (m *mediaManager) ensureVideoRxLocked() bool {
	if m.rxFramesQ != nil {
		return true
	}
	m.rxFramesQ = make(chan vidFrame, 4)
	if m.rxDone == nil {
		m.rxDone = make(chan struct{})
	}
	if !m.rxPump {
		m.rxPump = true
		m.startRenderPump()
	}
	return true
}

// teardownVideoRxLocked drops the remote pipeline when nobody publishes
// and our own camera is off. Runs under m.mu; returns the closeables.
func (m *mediaManager) teardownVideoRxLocked() (rdone chan struct{}) {
	if m.rxDone == nil {
		return nil
	}
	rdone = m.rxDone
	m.rxDone, m.rxPump, m.rxOnFlag = nil, false, false
	m.videoAsm = nil
	return rdone
}

// onVideoFrag reassembles + decodes + queues one remote JPEG frame. Frags
// pin to one sender (5s silence re-pins): interleaved timestamps from two
// publishers would corrupt every frame otherwise.
func (m *mediaManager) onVideoFrag(peer string, frag videoFrag) {
	m.mu.Lock()
	if len(m.pubVideo) == 0 {
		m.mu.Unlock()
		return // nobody we know is publishing: ignore strays
	}
	now := time.Now()
	if m.rxPinned == "" || now.Sub(m.rxPinAt) > videoPinTimeout {
		if peer != m.rxPinned {
			m.videoAsm = newFragAssembler()
		}
		m.rxPinned, m.rxPinAt = peer, now
	} else if peer != m.rxPinned {
		m.mu.Unlock()
		return
	} else {
		m.rxPinAt = now
	}
	asm := m.videoAsm
	q := m.rxFramesQ
	if asm == nil {
		asm = newFragAssembler()
		m.videoAsm = asm
	}
	if q == nil {
		if m.rxDone == nil {
			m.rxDone = make(chan struct{})
		}
		q = make(chan vidFrame, 4)
		m.rxFramesQ = q
		if !m.rxPump {
			m.rxPump = true
			m.startRenderPump()
		}
	}
	m.mu.Unlock()

	complete := asm.push(frag)
	if complete == nil {
		return
	}
	frame, err := jpegToRGB(complete)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.rxFrames++
	m.mu.Unlock()
	select {
	case q <- frame:
	default: // drop-if-full: decode keeps pace or sheds load
	}
}

// startRenderPump relays decoded frames (remote + self-view) to ASCII
// painting. Queues snapshot per iteration: nil channels block forever in
// select, so missing sides simply idle. Paint is throttled per side.
func (m *mediaManager) startRenderPump() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			m.mu.Lock()
			var remote, preview <-chan vidFrame
			if m.rxFramesQ != nil {
				remote = m.rxFramesQ
			}
			if m.previewQ != nil {
				preview = m.previewQ
			}
			done := m.rxDone
			cols, rows := m.renderCols, m.renderRows
			m.mu.Unlock()
			if done == nil {
				return
			}
			select {
			case <-done:
				return
			case f, ok := <-remote:
				if !ok {
					return
				}
				now := time.Now()
				m.mu.Lock()
				m.rxOnFlag = true
				due := now.Sub(m.lastRemoteShow) >= videoRenderMinInterval
				if due {
					m.lastRemoteShow = now
					m.remoteShows++
				}
				cols, rows = m.renderCols, m.renderRows
				m.mu.Unlock()
				if due {
					if cb := m.callbacks(); cb.onVideoFrame != nil {
						cb.onVideoFrame(asciiFrame(f.RGB, f.Width, f.Height, cols, rows, videoStyle()))
					}
				}
			case f, ok := <-preview:
				if !ok {
					return
				}
				now := time.Now()
				m.mu.Lock()
				due := now.Sub(m.lastSelfShow) >= videoRenderMinInterval
				if due {
					m.lastSelfShow = now
					m.selfShows++
				}
				cols, rows = m.renderCols, m.renderRows
				m.mu.Unlock()
				if due {
					if cb := m.callbacks(); cb.onSelfFrame != nil {
						cb.onSelfFrame(asciiFrame(f.RGB, f.Width, f.Height, cols, rows, videoStyle()))
					}
				}
			}
		}
	}()
}

// SetVideoSize snapshots the true pane geometry for the render pump
// (kills the fixed-width crop: frames render at the pane's real width).
func (m *mediaManager) SetVideoSize(cols, rows int) {
	if cols < 10 || rows < 3 {
		return // degenerate pane: keep the last sane geometry
	}
	m.mu.Lock()
	m.renderCols, m.renderRows = cols, rows
	m.mu.Unlock()
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
		m.audioOn = false
		m.audioTo = map[string]bool{}
		m.mu.Unlock()
		m.emit("mic unavailable: " + err.Error())
		return
	}
	txVoice, err := newOpusVoice()
	if err != nil {
		stopMic()
		m.audioOn = false
		m.audioTo = map[string]bool{}
		m.mu.Unlock()
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
// stops. seq/ts continue across source restarts (RTP continuity).
func (m *mediaManager) micRun(stop chan struct{}, txVoice *opusVoice, seq *uint16, ts *uint32, frames *<-chan []int16) micEnd {
	for {
		select {
		case <-stop:
			return micStopped
		case f, ok := <-*frames:
			if !ok {
				return micSourceDied
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
			for _, peer := range t.readyPeers() {
				m.mu.Lock()
				inScope := m.audioTo[peer]
				m.mu.Unlock()
				if !inScope {
					continue
				}
				_ = t.sendMedia(peer, mediaKindAudio, ap)
			}
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
	play, stopPlay, err := m.playSink()
	if err != nil {
		m.mu.Unlock()
		m.emit("speaker unavailable: " + err.Error())
		return
	}
	rxVoice, err := newOpusVoice()
	if err != nil {
		stopPlay()
		m.mu.Unlock()
		m.emit("opus init failed: " + err.Error())
		return
	}
	m.rxVoice = rxVoice
	if m.rxJbs == nil {
		m.rxJbs = map[string]*jitterBuffer{}
	}
	m.stopPlayFn = stopPlay
	stop := make(chan struct{})
	m.audioRxStop = stop
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			m.rxVoice = nil
			m.rxJbs = map[string]*jitterBuffer{}
			m.audioPinned = ""
			m.mu.Unlock()
		}()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var last []byte
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				pinned := m.audioPinned
				jb := m.rxJbs[pinned]
				anyPublisher := len(m.pubAudio) > 0
				rxV := m.rxVoice
				m.mu.Unlock()
				if !anyPublisher || jb == nil || rxV == nil {
					continue
				}
				pkt, gap := jb.pop()
				if gap {
					if last != nil {
						if fec, err := rxV.decodeFEC(last); err == nil {
							play(fec)
						}
					}
				} else {
					last = pkt.Opus
					if pcm, err := rxV.decode(pkt.Opus); err == nil {
						play(pcm)
					}
				}
			}
		}
	}()
}

// onRemoteAudio routes a peer's audio into their jitter buffer and elects
// the audible sender (2s-silence re-pin: no voice mixing).
func (m *mediaManager) onRemoteAudio(peer string, pkt audioPacket) {
	m.mu.Lock()
	if len(m.pubAudio) == 0 {
		m.mu.Unlock()
		return
	}
	if m.rxJbs == nil {
		m.rxJbs = map[string]*jitterBuffer{}
	}
	jb := m.rxJbs[peer]
	if jb == nil {
		jb = newJitterBuffer()
		m.rxJbs[peer] = jb
	}
	now := time.Now()
	if m.audioPinned == "" || now.Sub(m.audioPinAt) > audioPinTimeout {
		m.audioPinned, m.audioPinAt = peer, now
	} else if peer == m.audioPinned {
		m.audioPinAt = now
	}
	m.mu.Unlock()
	jb.push(pkt)
}

// ─── Stops ──────────────────────────────────────────────────────────────────

// stopVideoPublish: camera off — media-stop to scope; keep watching others.
func (m *mediaManager) stopVideoPublish() {
	m.mu.Lock()
	scope := sortedKeys(m.videoTo)
	m.videoTo = map[string]bool{}
	m.videoOn = false
	m.mu.Unlock()
	for _, to := range scope {
		m.sendStop(to, true, false)
	}
	m.stopVideoStream()
}

// stopVideoStream stops the camera loop; remote RX survives if watching.
func (m *mediaManager) stopVideoStream() {
	m.mu.Lock()
	stop := m.cameraStop
	srcStop := m.stopCameraSrc
	m.cameraStop, m.stopCameraSrc = nil, nil
	m.videoOnFlag = false
	keepRx := len(m.pubVideo) > 0
	var rdone chan struct{}
	if !keepRx {
		rdone = m.teardownVideoRxLocked()
	}
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if rdone != nil {
		close(rdone)
	}
	if srcStop != nil {
		srcStop()
	}
}

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
		m.sendStop(to, false, true)
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
	m.rxVoice = nil
	m.rxJbs = map[string]*jitterBuffer{}
	m.audioPinned = ""
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
	cameraStop := m.cameraStop
	audioStop := m.audioStop
	audioRxStop := m.audioRxStop
	rdone := m.rxDone
	stopMic := m.stopMic
	stopPlay := m.stopPlayFn
	m.cameraStop, m.stopCameraSrc = nil, nil
	m.audioStop, m.audioRxStop, m.stopMic, m.stopPlayFn = nil, nil, nil, nil
	m.videoOn, m.audioOn = false, false
	m.videoTo, m.audioTo = map[string]bool{}, map[string]bool{}
	m.pubVideo, m.pubAudio, m.replied = map[string]bool{}, map[string]bool{}, map[string]bool{}
	m.rxPinned, m.audioPinned = "", ""
	m.rxDone, m.rxPump, m.rxOnFlag = nil, false, false
	m.peerAnn, m.joinTries, m.lastJoinAt, m.joinVerdict = map[string]mediaAnnouncePayload{}, map[string]int{}, map[string]time.Time{}, map[string]bool{}
	m.started = false
	scope := sortedKeys(m.videoTo)
	audioScope := sortedKeys(m.audioTo)
	m.mu.Unlock()
	// Best-effort stop notes so watchers do not freeze on our last frame.
	for _, to := range scope {
		m.sendStop(to, true, false)
	}
	for _, to := range audioScope {
		m.sendStop(to, false, true)
	}
	if stop != nil {
		close(stop)
	}
	if cameraStop != nil {
		close(cameraStop)
	}
	if audioStop != nil {
		close(audioStop)
	}
	if audioRxStop != nil {
		close(audioRxStop)
	}
	if rdone != nil {
		close(rdone)
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
	for p := range m.videoTo {
		add(p)
	}
	for p := range m.audioTo {
		add(p)
	}
	for p := range m.pubVideo {
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
		weSend := m.videoTo[p] || m.audioTo[p]
		theySend := m.pubVideo[p] || m.pubAudio[p]
		ann, haveAnn := m.peerAnn[p]
		if !weSend && !theySend {
			delete(m.peerAnn, p)
			delete(m.joinTries, p)
			delete(m.lastJoinAt, p)
			delete(m.joinVerdict, p)
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
			m.mu.Unlock()
			if weSend {
				// Re-announce: fresh coords, watcher re-joins us.
				_ = m.sendAnnounce(p, true)
			}
			if theySend && haveAnn {
				// Re-join from the stored announce + re-reply so the
				// publisher can also re-dial us.
				m.joinPeer(p, ann)
				_ = m.sendAnnounce(p, false)
			}
		case d.LastRxAge > callWatchdogAfter:
			// Live session gone quiet: heal the path (bounded).
			m.mu.Lock()
			if time.Since(m.lastHeal[p]) < healEvery {
				m.mu.Unlock()
				continue
			}
			m.lastHeal[p] = now
			m.healTries++
			m.mu.Unlock()
			m.healPeer(p)
		}
	}
}

// emitJoinInfo is intentionally unused: retries are silent, outcomes are
// loud ("media secured with X" / the unreachable verdict line).

// healPeer re-checks a silent peer's route from BOTH sides: restart the
// handshake and, when the route may have moved, exchange fresh announce
// coords (publisher re-announces; watcher re-joins from stored coords).
func (m *mediaManager) healPeer(peer string) {
	m.mu.Lock()
	t := m.transport
	weSend := m.videoTo[peer] || m.audioTo[peer]
	ann, haveAnn := m.peerAnn[peer]
	m.mu.Unlock()
	if t == nil {
		return
	}
	if d := t.diagPeer(peer); d.Addr == "" {
		return // no route yet; the join retry handles first contact
	}
	if t.lastRxAt(peer).After(time.Now().Add(-callWatchdogAfter)) {
		return // traffic resumed while we scheduled
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
