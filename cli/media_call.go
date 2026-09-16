package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp/codecs"
)

// ─── Voice calls (LAN, 1:1) ─────────────────────────────────────────────────
//
// Call control rides the existing signal queue (offer/accept/decline/end —
// tiny, rare, no budget impact); media flows direct UDP via mediaTransport.
// One call at a time: a second incoming offer is auto-declined (busy).

const (
	callOffer   = "call-offer"
	callAccept  = "call-accept"
	callDecline = "call-decline"
	callEnd     = "call-end"

	callOfferTTL      = 30 * time.Second
	callWatchdogAfter = 10 * time.Second
)

// Room video publish rides the same signal queue + Noise UDP sessions as
// calls, but needs no call: video-live announces camera coords to a scope
// (DM peer or room members), video-stop withdraws. Receivers dial back and
// render; a Live=false announce is a watcher's join reply (coords for the
// publisher's fan-out, never rendered, never re-replied).
const (
	videoLive = "video-live"
	videoStop = "video-stop"

	// videoRenderMinInterval bounds UI cost: decode runs full-rate, paint
	// sheds (8fps/side is plenty for a 56x16 ASCII pane).
	videoRenderMinInterval = 125 * time.Millisecond
	// videoPinTimeout re-pins the single remote renderer after silence
	// (one decoder, so one sender at a time; see onVideoFrag).
	videoPinTimeout = 5 * time.Second
	// healEvery bounds watchdog re-probes of a blackholed UDP path.
	healEvery = 30 * time.Second
)

type videoAnnouncePayload struct {
	IP   string   `json:"ip"`
	Ips  []string `json:"ips,omitempty"`
	Port int      `json:"port"`
	Live bool     `json:"live"`
}

type callOfferPayload struct {
	IP   string   `json:"ip"`
	Ips  []string `json:"ips,omitempty"`
	Port int      `json:"port"`
	Ts   int64    `json:"ts"`
}

type callAnswerPayload struct {
	IP     string   `json:"ip"`
	Ips    []string `json:"ips,omitempty"`
	Port   int      `json:"port"`
	Accept bool     `json:"accept"`
}

type callState int

const (
	callIdle callState = iota
	callOutgoing
	callRinging
	callLive
)

func (s callState) String() string {
	switch s {
	case callOutgoing:
		return "calling"
	case callRinging:
		return "ringing"
	case callLive:
		return "live"
	default:
		return "idle"
	}
}

type callCallbacks struct {
	onRinging    func(peer string)                               // incoming offer worth showing
	onState      func(state callState, peer string, info string) // transitions + errors
	onLevel      func(level float64)                             // mic loudness 0..1 (throttled)
	onVideoFrame func(lines []string)                            // decoded remote ASCII frame
	onSelfFrame  func(lines []string)                            // local camera preview
}

type callManager struct {
	me       string
	id       *identityKey
	sendNote func(to, noteType, payload string) error
	roster   func() map[string][]byte
	micSrc   func() (<-chan []int16, func(), error)
	playSink func() (func([]int16), func(), error)
	cb       callCallbacks

	mu        sync.Mutex
	state     callState
	peer      string
	transport *mediaTransport
	// audio live-cycle
	audioStop     chan struct{}
	audioWg       sync.WaitGroup
	stopMic       func()
	stopPlay      func()
	rxJb          *jitterBuffer
	muted         atomic.Bool
	offer         callOfferPayload
	offerFrom     string
	warnedNoMedia bool
	rxOn          bool
	// mediaReadyAt anchors the no-media watchdog: silence before the Noise
	// session exists is handshake timing (~3 polls), not a fault.
	mediaReadyAt time.Time
	// dialIP overrides localLANIP (tests pin loopback).
	dialIP string
	// lanIPs lists advertised addresses (tests pin loopback).
	lanIPs func() []string
	// video live-cycle (nil unless streaming).
	videoStop    chan struct{}
	stopVideoSrc func()
	previewDec   frameDecoder // local monitor decode (self-view, TX side)
	videoAsm     *fragAssembler
	videoDec     frameDecoder
	rxDone       chan struct{}
	rxPump       bool
	videoSrc     func() (<-chan []byte, func(), error)
	videoDecFn   func() (frameDecoder, error)
	videoOn      bool
	videoSeq     uint16
	videoTs      uint32
	// Room publish (no call needed): TX fans out to every ready session.
	publishing bool
	pubTargets map[string]bool // announced scope (for stop notes + top-up)
	pubLive    map[string]bool // peers currently publishing (watch set)
	pubMuted   map[string]bool // sent video-stop: keep session, skip fan-out
	pubReplied map[string]bool // watcher-reply already sent (no storms)
	watching   bool
	peerAns    callAnswerPayload // caller-side coords for watchdog re-probe
	// Single remote renderer: pinned sender + render throttle + counters.
	rxPinned       string
	rxPinAt        time.Time
	lastRemoteShow time.Time
	lastSelfShow   time.Time
	txFrames       uint64
	rxFrames       uint64
	remoteShows    uint64
	selfShows      uint64
	// Watchdog path healing (call mode): re-nominate a blackholed route.
	lastHeal  time.Time
	healTries int
}

func newCallManager(me string, id *identityKey, sendNote func(to, noteType, payload string) error, roster func() map[string][]byte, cb callCallbacks) *callManager {
	return &callManager{
		me: me, id: id, sendNote: sendNote, roster: roster, cb: cb,
		lanIPs:   localLANIPs,
		micSrc:   openMicFrames,
		playSink: openPlaySink,
	}
}

func openMicFrames() (<-chan []int16, func(), error) {
	mc, err := openMic()
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
			case f, ok := <-mc.frames:
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
	return out, func() { close(done); mc.stop() }, nil
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
// blackholes all media with perfect signaling — so offers carry the whole
// list and the peer nominates by provable reachability.
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

// localLANIP returns the best single guess (first of the ranked list).
func localLANIP() (string, error) {
	ips := localLANIPs()
	if len(ips) == 0 {
		return "", fmt.Errorf("no LAN IPv4 found")
	}
	return ips[0], nil
}

// callbacks snapshots the callback table: tests rewire callbacks mid-run
// while transport goroutines invoke them, so every read takes the lock.
func (c *callManager) callbacks() callCallbacks {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cb
}

func (c *callManager) emitState(info string) {
	c.mu.Lock()
	st, peer := c.state, c.peer
	c.mu.Unlock()
	if cb := c.callbacks().onState; cb != nil {
		cb(st, peer, info)
	}
}

// Call starts an outgoing call: fresh transport (ephemeral port), offer sent.
func (c *callManager) Call(peer string) error {
	c.mu.Lock()
	if c.state != callIdle {
		st := c.state
		c.mu.Unlock()
		return fmt.Errorf("already in a call (%s)", st)
	}
	c.mu.Unlock()
	t, err := newMediaTransport(c.me, c.id, c.sendNote, c.roster, c.mediaCB())
	if err != nil {
		return err
	}
	ips := localLANIPs()
	if c.lanIPs != nil {
		ips = c.lanIPs()
	}
	ip := ""
	if len(ips) > 0 {
		ip = ips[0]
	}
	if c.dialIP != "" {
		ip, ips = c.dialIP, []string{c.dialIP}
	}
	if ip == "" {
		return fmt.Errorf("no LAN IPv4 found")
	}
	raw, _ := json.Marshal(callOfferPayload{IP: ip, Ips: ips, Port: t.localAddr().Port, Ts: time.Now().Unix()})
	c.mu.Lock()
	c.transport, c.state, c.peer = t, callOutgoing, peer
	c.mu.Unlock()
	t.start()
	if err := c.sendNote(peer, callOffer, string(raw)); err != nil {
		c.teardownLocked("offer failed")
		return err
	}
	c.emitState("ringing " + peer)
	return nil
}

// Accept answers a ringing offer and starts media.
func (c *callManager) Accept() error {
	c.mu.Lock()
	if c.state != callRinging {
		st := c.state
		c.mu.Unlock()
		return fmt.Errorf("no incoming call (state %s)", st)
	}
	peer, offer := c.peer, c.offer
	c.mu.Unlock()
	t, err := newMediaTransport(c.me, c.id, c.sendNote, c.roster, c.mediaCB())
	if err != nil {
		return err
	}
	// Start the socket loop BEFORE nominating: pong replies are processed
	// by readLoop, and probing a deaf socket always yields nil.
	t.start()
	ips := localLANIPs()
	if c.lanIPs != nil {
		ips = c.lanIPs()
	}
	ip := ""
	if len(ips) > 0 {
		ip = ips[0]
	}
	if c.dialIP != "" {
		ip, ips = c.dialIP, []string{c.dialIP}
	}
	if ip == "" {
		return fmt.Errorf("no LAN IPv4 found")
	}
	// Nominate a provably reachable address from the peer's candidates
	// (plus their primary) instead of blindly trusting one IP.
	cands := append(append([]string{}, offer.Ips...), offer.IP)
	winner := t.nominate(cands, offer.Port)
	if winner == nil {
		return fmt.Errorf("no reachable address for %s (tried %d candidate(s))", peer, len(cands))
	}
	if err := t.dialPeer(peer, winner.IP.String(), winner.Port); err != nil {
		return err
	}
	raw, _ := json.Marshal(callAnswerPayload{IP: ip, Ips: ips, Port: t.localAddr().Port, Accept: true})
	c.mu.Lock()
	c.transport = t
	c.mu.Unlock()
	if err := c.sendNote(peer, callAccept, string(raw)); err != nil {
		c.teardownLocked("accept failed")
		return err
	}
	t.beginHandshake(peer)
	c.setLiveLocked(peer)
	return nil
}

// Decline refuses a ringing offer.
func (c *callManager) Decline() error {
	c.mu.Lock()
	if c.state != callRinging {
		c.mu.Unlock()
		return fmt.Errorf("no incoming call")
	}
	peer := c.peer
	c.state, c.peer = callIdle, ""
	c.mu.Unlock()
	raw, _ := json.Marshal(callAnswerPayload{Accept: false})
	_ = c.sendNote(peer, callAccept, string(raw))
	c.emitState("declined " + peer)
	return nil
}

// Hangup ends the live call (notifies the peer best-effort). With no call
// it still leaves room video / watch state ("not in a call" only when
// there is truly no media activity at all).
func (c *callManager) Hangup() error {
	c.mu.Lock()
	inCall := c.state == callLive || c.state == callOutgoing
	mediaActive := c.videoOn || c.publishing || c.watching || c.transport != nil
	peer := c.peer
	c.mu.Unlock()
	if !inCall && !mediaActive {
		return fmt.Errorf("not in a call")
	}
	if inCall {
		_ = c.sendNote(peer, callEnd, "{}")
	}
	c.teardownLocked("hung up")
	return nil
}

func (c *callManager) Mute(muted bool) {
	c.muted.Store(muted)
}

func (c *callManager) setLiveLocked(peer string) {
	c.mu.Lock()
	c.state = callLive
	c.mu.Unlock()
	c.emitState("live with " + peer)
	// Audio starts on transport ready, not here: the media handshake still
	// needs ~3 signal polls, and starting early only burns mic frames and
	// trips the no-media watchdog before any datagram could exist.
	c.maybeStartAudio(peer)
}

// onTransportReady fires when the Noise session goes live: anchor the
// no-media watchdog and start audio if the call is already live (otherwise
// setLiveLocked picks it up — handshake and call-state race either order
// depending on note timing).
func (c *callManager) onTransportReady(peer string) {
	c.mu.Lock()
	c.mediaReadyAt = time.Now()
	c.mu.Unlock()
	c.maybeStartAudio(peer)
	c.emitState("media secured with " + peer)
}

// maybeStartAudio starts the audio pipeline once the call is live AND the
// transport is ready, whichever comes last. Safe to call from both paths.
func (c *callManager) maybeStartAudio(peer string) {
	c.mu.Lock()
	if c.state != callLive || c.audioStop != nil {
		c.mu.Unlock()
		return
	}
	var ready bool
	if c.transport != nil {
		ready = c.transport.peerReady(peer)
	}
	c.mu.Unlock()
	if ready {
		c.startAudio(peer)
	}
}

// maybeHeal re-probes a blackholed UDP route (bounded: healEvery). Runs
// off the audio watchdog so the 20ms playout tick never blocks on the
// 600ms nomination probe.
func (c *callManager) maybeHeal(peer string) {
	c.mu.Lock()
	if c.state != callLive || time.Since(c.lastHeal) < healEvery {
		c.mu.Unlock()
		return
	}
	c.lastHeal = time.Now()
	c.healTries++
	c.mu.Unlock()
	go c.healPath(peer)
}

// healPath re-nominates the peer's stored candidates and re-dials when the
// route moved; prompts a peer-side fresh handshake too. Every outcome gets
// one honest state line (healed / moved / still blocked).
func (c *callManager) healPath(peer string) {
	c.mu.Lock()
	if c.state != callLive || c.peer != peer {
		c.mu.Unlock()
		return
	}
	t := c.transport
	var cands []string
	var port int
	if c.offerFrom == peer {
		cands, port = append(append([]string{}, c.offer.Ips...), c.offer.IP), c.offer.Port
	} else {
		cands, port = append(append([]string{}, c.peerAns.Ips...), c.peerAns.IP), c.peerAns.Port
	}
	var cur string
	if t != nil {
		cur = t.diagPeer(peer).Addr
		lastRx := t.lastRxAt(peer)
		if time.Since(lastRx) <= callWatchdogAfter {
			c.mu.Unlock()
			return // traffic resumed while we scheduled
		}
	}
	c.mu.Unlock()
	if t == nil || len(cands) == 0 || port == 0 {
		return
	}
	winner := t.nominate(cands, port)
	if winner == nil {
		c.emitState("path re-probe: no reachable address — AP isolation or firewall likely")
		t.sendRestart(peer)
		return
	}
	if winner.String() != cur {
		_ = t.dialPeer(peer, winner.IP.String(), winner.Port)
		c.emitState("path re-nominated to " + winner.String() + " — healing")
		t.sendRestart(peer)
		return
	}
	c.emitState("path re-probe: same route, still no Rx — remote silent or blocked inbound")
	t.sendRestart(peer)
}

func (c *callManager) teardownLocked(info string) {
	c.mu.Lock()
	t := c.transport
	c.transport = nil
	c.state, c.peer = callIdle, ""
	c.warnedNoMedia = false
	stop := c.audioStop
	c.audioStop = nil
	stopMic, stopPlay := c.stopMic, c.stopPlay
	c.stopMic, c.stopPlay = nil, nil
	c.rxJb = nil
	vstop, vdec := c.videoStop, c.videoDec
	vsrcStop := c.stopVideoSrc
	pdec := c.previewDec
	rdone := c.rxDone
	wasPublishing := c.publishing
	var pubTargets []string
	for to := range c.pubTargets {
		pubTargets = append(pubTargets, to)
	}
	c.videoStop, c.videoAsm, c.videoDec, c.videoOn, c.rxOn, c.stopVideoSrc = nil, nil, nil, false, false, nil
	c.previewDec = nil
	c.publishing = false
	c.pubTargets, c.pubLive, c.pubMuted, c.pubReplied = nil, nil, nil, nil
	c.watching = false
	c.rxPinned = ""
	c.rxDone, c.rxPump = nil, false
	c.mu.Unlock()
	if wasPublishing {
		for _, to := range pubTargets {
			_ = c.sendNote(to, videoStop, "{}")
		}
	}
	if stop != nil {
		close(stop)
	}
	if vstop != nil {
		close(vstop)
	}
	if rdone != nil {
		close(rdone)
	}
	c.audioWg.Wait()
	if vsrcStop != nil {
		vsrcStop()
	}
	if pdec != nil {
		pdec.close()
	}
	if vdec != nil {
		vdec.close()
	}
	if stopMic != nil {
		stopMic()
	}
	if stopPlay != nil {
		stopPlay()
	}
	if t != nil {
		t.stop()
	}
	c.emitState(info)
}

// onSignalNote handles call control + media handshake notes.
func (c *callManager) onSignalNote(n signalNote) {
	switch n.Type {
	case mediaHS1, mediaHS2, mediaHS3, mediaHSRestart:
		c.mu.Lock()
		t := c.transport
		c.mu.Unlock()
		if t == nil {
			return
		}
		var env hsEnvelope
		if err := json.Unmarshal([]byte(n.Payload), &env); err != nil {
			return
		}
		_ = env
		t.onHandshakeNote(n.From, n.Type, n.Payload)
	case callOffer:
		var offer callOfferPayload
		if err := json.Unmarshal([]byte(n.Payload), &offer); err != nil {
			return
		}
		if time.Since(time.Unix(offer.Ts, 0)) > callOfferTTL {
			return // stale ring from a dead attempt
		}
		c.mu.Lock()
		busy := c.state != callIdle
		if !busy {
			c.state, c.peer, c.offer, c.offerFrom = callRinging, n.From, offer, n.From
		}
		c.mu.Unlock()
		if busy {
			raw, _ := json.Marshal(callAnswerPayload{Accept: false})
			_ = c.sendNote(n.From, callAccept, string(raw))
			return
		}
		if cb := c.callbacks().onRinging; cb != nil {
			cb(n.From)
		}
		c.emitState("incoming call from " + n.From)
	case callAccept:
		var ans callAnswerPayload
		if err := json.Unmarshal([]byte(n.Payload), &ans); err != nil {
			return
		}
		c.mu.Lock()
		outgoing := c.state == callOutgoing && c.peer == n.From
		t := c.transport
		c.mu.Unlock()
		if !outgoing || t == nil {
			return
		}
		if !ans.Accept {
			c.teardownLocked(n.From + " declined")
			return
		}
		c.mu.Lock()
		c.peerAns = ans // watchdog re-probe coords (path healing)
		c.mu.Unlock()
		cands := append(append([]string{}, ans.Ips...), ans.IP)
		winner := t.nominate(cands, ans.Port)
		if winner == nil {
			c.teardownLocked("no reachable address for " + n.From)
			return
		}
		if err := t.dialPeer(n.From, winner.IP.String(), winner.Port); err != nil {
			c.teardownLocked("bad accept address")
			return
		}
		t.beginHandshake(n.From)
		c.setLiveLocked(n.From)
	case callDecline:
		c.mu.Lock()
		ringing := c.state == callRinging && c.peer == n.From
		c.mu.Unlock()
		if ringing {
			c.teardownLocked(n.From + " cancelled")
		}
	case callEnd:
		c.mu.Lock()
		inCall := (c.state == callLive || c.state == callRinging) && c.peer == n.From
		c.mu.Unlock()
		if inCall {
			c.teardownLocked(n.From + " hung up")
		}
	case videoLive:
		var ann videoAnnouncePayload
		if err := json.Unmarshal([]byte(n.Payload), &ann); err != nil {
			return
		}
		if n.From == c.me {
			return
		}
		// Socket first: the watcher join-reply below needs our coords,
		// and joinVideo needs the read loop running.
		if _, err := c.ensureTransport(); err != nil {
			return
		}
		c.mu.Lock()
		if c.pubLive == nil {
			c.pubLive = map[string]bool{}
		}
		if c.pubReplied == nil {
			c.pubReplied = map[string]bool{}
		}
		// A re-announce clears a stale mute (watcher is back).
		delete(c.pubMuted, n.From)
		needReply := ann.Live && !c.publishing && !c.pubReplied[n.From]
		if ann.Live {
			c.pubLive[n.From] = true
			c.watching = true
		}
		if needReply {
			c.pubReplied[n.From] = true
		}
		c.mu.Unlock()
		// Watcher join-reply carries coords for our fan-out (never
		// rendered, never re-replied); publisher announces render.
		if needReply {
			_ = c.sendVideoAnnounce(n.From, false)
		}
		if ann.Live {
			c.emitState(n.From + " is sharing video")
		}
		go c.joinVideo(n.From, ann)
	case videoStop:
		c.mu.Lock()
		delete(c.pubLive, n.From)
		delete(c.pubMuted, n.From)
		if c.rxPinned == n.From {
			c.rxPinned = ""
		}
		dropRx := false
		if len(c.pubLive) == 0 {
			c.watching = false
			// Keep the shared pump while our own camera runs (self-view
			// still needs it); drop the remote pipeline otherwise.
			dropRx = !c.videoOn
		}
		var dec frameDecoder
		var rdone chan struct{}
		if dropRx {
			dec = c.videoDec
			rdone = c.rxDone
			c.videoAsm, c.videoDec, c.rxOn = nil, nil, false
			c.rxDone, c.rxPump = nil, false
		}
		c.mu.Unlock()
		if rdone != nil {
			close(rdone)
		}
		if dec != nil {
			dec.close()
		}
		c.emitState(n.From + " stopped video")
	}
}

// ensureTransport returns the live media socket, creating + starting it on
// first use (room publish needs media without any call).
func (c *callManager) ensureTransport() (*mediaTransport, error) {
	c.mu.Lock()
	if c.transport != nil {
		t := c.transport
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	t, err := newMediaTransport(c.me, c.id, c.sendNote, c.roster, c.mediaCB())
	if err != nil {
		return nil, err
	}
	t.start()
	c.mu.Lock()
	if c.transport != nil {
		existing := c.transport
		c.mu.Unlock()
		t.stop()
		return existing, nil
	}
	c.transport = t
	c.mu.Unlock()
	return t, nil
}

// advertiseAddrs snapshots the coords peers should dial (test overrides
// honored exactly like the call offer/answer path).
func (c *callManager) advertiseAddrs() (ip string, ips []string) {
	ips = localLANIPs()
	if c.lanIPs != nil {
		ips = c.lanIPs()
	}
	if len(ips) > 0 {
		ip = ips[0]
	}
	if c.dialIP != "" {
		ip, ips = c.dialIP, []string{c.dialIP}
	}
	return ip, ips
}

// sendVideoAnnounce posts our UDP coords (Live=true publisher, Live=false
// watcher join-reply) to one peer.
func (c *callManager) sendVideoAnnounce(to string, live bool) error {
	c.mu.Lock()
	t := c.transport
	c.mu.Unlock()
	if t == nil {
		return fmt.Errorf("no media socket")
	}
	addr := t.localAddr()
	if addr == nil {
		return fmt.Errorf("no media socket")
	}
	ip, ips := c.advertiseAddrs()
	raw, _ := json.Marshal(videoAnnouncePayload{IP: ip, Ips: ips, Port: addr.Port, Live: live})
	return c.sendNote(to, videoLive, string(raw))
}

// StartPublish shares the camera with a scope (DM peer or room members)
// with no call required. Announces coords, dials back on replies, and fans
// out every frame to all ready sessions. Idempotent per scope via PublishTo.
func (c *callManager) StartPublish(targets []string) error {
	// Already streaming from a call: just widen the scope (the TX loop
	// fans out to every ready session, so the call peer is covered).
	c.mu.Lock()
	attached := c.videoOn && !c.publishing && c.state == callLive
	c.mu.Unlock()
	if attached {
		if err := c.PublishTo(targets); err != nil {
			return err
		}
		c.mu.Lock()
		c.publishing = true
		c.mu.Unlock()
		c.emitState("publishing video to the room")
		return nil
	}
	c.mu.Lock()
	if c.videoOn {
		on := c.videoOn
		_ = on
		c.mu.Unlock()
		return fmt.Errorf("video already on")
	}
	c.mu.Unlock()
	t, err := c.ensureTransport()
	if err != nil {
		return err
	}
	_ = t
	seen := map[string]bool{}
	var scope []string
	for _, p := range targets {
		if p == "" || p == c.me || seen[p] {
			continue
		}
		seen[p] = true
		scope = append(scope, p)
	}
	for _, to := range scope {
		if err := c.sendVideoAnnounce(to, true); err != nil {
			continue // best-effort: roster tick tops up misses
		}
		c.mu.Lock()
		if c.pubTargets == nil {
			c.pubTargets = map[string]bool{}
		}
		c.pubTargets[to] = true
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.publishing = true
	if c.pubTargets == nil {
		c.pubTargets = map[string]bool{}
	}
	c.mu.Unlock()
	if err := c.startCameraTx("publishing video"); err != nil {
		return err
	}
	return nil
}

// PublishTo announces to scope members missed earlier (late joiners).
// No-op unless publishing; never restarts the camera.
func (c *callManager) PublishTo(targets []string) error {
	c.mu.Lock()
	if !c.publishing {
		c.mu.Unlock()
		return nil
	}
	var fresh []string
	seen := map[string]bool{}
	for _, p := range targets {
		if p == "" || p == c.me || seen[p] {
			continue
		}
		seen[p] = true
		if c.pubTargets == nil || !c.pubTargets[p] {
			fresh = append(fresh, p)
		}
	}
	c.mu.Unlock()
	for _, to := range fresh {
		if err := c.sendVideoAnnounce(to, true); err != nil {
			continue
		}
		c.mu.Lock()
		if c.pubTargets == nil {
			c.pubTargets = map[string]bool{}
		}
		c.pubTargets[to] = true
		c.mu.Unlock()
	}
	return nil
}

// joinVideo dials a video announcer (probe → dial → handshake; roles follow
// the username glare rule, so concurrent publishers converge with no extra
// round). Idempotent: live sessions are left alone.
func (c *callManager) joinVideo(from string, ann videoAnnouncePayload) {
	c.mu.Lock()
	t := c.transport
	ready := t != nil && t.peerReady(from)
	c.mu.Unlock()
	if ready {
		return
	}
	if t == nil {
		var err error
		if t, err = c.ensureTransport(); err != nil {
			c.emitState("video from " + from + " unreachable (no socket)")
			return
		}
	}
	cands := append(append([]string{}, ann.Ips...), ann.IP)
	winner := t.nominate(cands, ann.Port)
	if winner == nil {
		c.emitState("video from " + from + " unreachable (no route — AP isolation?)")
		return
	}
	if err := t.dialPeer(from, winner.IP.String(), winner.Port); err != nil {
		return
	}
	t.beginHandshake(from)
}

// previewDecoder builds the self-view decoder (nil-safe: remote streaming
// never depends on local preview).
func (c *callManager) previewDecoder() (frameDecoder, error) {
	c.mu.Lock()
	decFn := c.videoDecFn
	c.mu.Unlock()
	if decFn == nil {
		decFn = func() (frameDecoder, error) { return newFFmpegDecoder() }
	}
	return decFn()
}

func (c *callManager) mediaCB() mediaCallbacks {
	return mediaCallbacks{
		onAudio: c.onRemoteAudio,
		onVideo: func(peer string, frag videoFrag) { c.onVideoFrag(peer, frag) },
		onReady: func(peer, code string) { c.onTransportReady(peer) },
		onLost:  func(peer string) { c.emitState("media line lost — will rejoin on traffic") },
		onError: func(err error) { c.emitState("media: " + err.Error()) },
	}
}

func (c *callManager) onRemoteAudio(_ string, pkt audioPacket) {
	c.mu.Lock()
	jb := c.rxJb
	c.mu.Unlock()
	if jb == nil {
		return
	}
	jb.push(pkt)
}

// instances per direction: the codec is stateful and not safe for sharing.
func (c *callManager) startAudio(peer string) {
	frames, stopMic, err := c.micSrc()
	if err != nil {
		c.emitState("mic unavailable: " + err.Error())
		return
	}
	play, stopPlay, err := c.playSink()
	if err != nil {
		stopMic()
		c.emitState("speaker unavailable: " + err.Error())
		return
	}
	txVoice, err := newOpusVoice()
	if err != nil {
		stopMic()
		stopPlay()
		c.emitState("opus init failed: " + err.Error())
		return
	}
	rxVoice, err := newOpusVoice()
	if err != nil {
		stopMic()
		stopPlay()
		c.emitState("opus init failed: " + err.Error())
		return
	}
	jb := newJitterBuffer()
	stop := make(chan struct{})
	c.mu.Lock()
	c.audioStop = stop
	c.stopMic, c.stopPlay = stopMic, stopPlay
	c.rxJb = jb
	c.mu.Unlock()
	var seq uint16
	var ts uint32
	// TX: mic → encode → send.
	c.audioWg.Add(1)
	go func() {
		defer c.audioWg.Done()
		tick := 0
		for {
			select {
			case <-stop:
				return
			case f, ok := <-frames:
				if !ok {
					return
				}
				if c.muted.Load() {
					// Muted still pings (~every 2s): keeps NAT bindings warm
					// and tells the peer's watchdog we're alive, not gone.
					tick++
					if tick%100 == 0 {
						c.mu.Lock()
						t := c.transport
						c.mu.Unlock()
						if t != nil {
							_ = t.sendMedia(peer, mediaKindPing, nil)
						}
					}
					continue
				}
				if tick%5 == 0 {
					if onLevel := c.callbacks().onLevel; onLevel != nil {
						onLevel(rmsLevel(f))
					}
				}
				tick++
				pkt, err := txVoice.encode(f)
				if err != nil {
					continue
				}
				seq++
				ts += voiceFrameLen
				c.mu.Lock()
				t := c.transport
				c.mu.Unlock()
				if t == nil {
					return
				}
				_ = t.sendMedia(peer, mediaKindAudio, encodeAudioPacket(seq, ts, pkt))
			}
		}
	}()
	// RX: 20ms playout tick; gaps repaired via in-band FEC, else skipped
	// (the clock must advance — see jitterBuffer). Also the no-path
	// watchdog: signaling alive but zero datagrams means a blocked direct
	// path (guest-WiFi isolation), not a quiet peer — say so once.
	c.audioWg.Add(1)
	go func() {
		defer c.audioWg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var last []byte
		watchTick := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				pkt, gap := jb.pop()
				if gap {
					if last != nil {
						if fec, err := rxVoice.decodeFEC(last); err == nil {
							play(fec)
						}
					}
				} else {
					last = pkt.Opus
					pcm, err := rxVoice.decode(pkt.Opus)
					if err == nil {
						play(pcm)
					}
				}
				if watchTick++; watchTick >= 100 {
					watchTick = 0
					c.mu.Lock()
					t := c.transport
					readyAt := c.mediaReadyAt
					warned := c.warnedNoMedia
					c.mu.Unlock()
					var lastRx time.Time
					if t != nil {
						lastRx = t.lastRxAt(peer)
					}
					// Both clocks must agree: session live 10s+ with zero
					// datagrams in 10s. Before the session exists (or right
					// after it forms) silence is normal handshake timing.
					if t != nil && !readyAt.IsZero() &&
						time.Since(readyAt) > callWatchdogAfter &&
						time.Since(lastRx) > callWatchdogAfter {
						if !warned {
							c.mu.Lock()
							c.warnedNoMedia = true
							c.mu.Unlock()
							c.emitState("no media received — direct path may be blocked (guest WiFi isolation?)")
						}
						// Healing, not just warning: a stale nomination
						// (DHCP renew, AP roam, wrong candidate) recovers by
						// re-probing; hard isolation gets an honest verdict.
						c.maybeHeal(peer)
					}
				}
			}
		}
	}()
}

// diagLines renders call diagnostics for /mediastats: everything needed
// to tell signaling failure apart from UDP blackholes.
func (c *callManager) diagLines() []string {
	st, peer := c.State()
	lines := []string{
		fmt.Sprintf("call state=%s peer=%q muted=%v", st, peer, c.Muted()),
		fmt.Sprintf("offered IPs: %v", localLANIPs()),
	}
	c.mu.Lock()
	videoInfo := fmt.Sprintf("video on=%v publishing=%v watching=%v rxPinned=%q txFrames=%d rxFrames=%d remoteShows=%d selfShows=%d",
		c.videoOn, c.publishing, c.watching, c.rxPinned, c.txFrames, c.rxFrames, c.remoteShows, c.selfShows)
	healInfo := fmt.Sprintf("heal tries=%d", c.healTries)
	var pubList []string
	for to := range c.pubLive {
		pubList = append(pubList, to)
	}
	c.mu.Unlock()
	lines = append(lines, videoInfo, "publishers live: "+fmt.Sprintf("%v", pubList), healInfo)
	if peer == "" {
		return lines
	}
	c.mu.Lock()
	t := c.transport
	known := false
	if _, ok := c.roster()[peer]; ok {
		known = true
	}
	c.mu.Unlock()
	lines = append(lines, fmt.Sprintf("roster knows peer: %v", known))
	if t == nil {
		return append(lines, "transport: none")
	}
	d := t.diagPeer(peer)
	lines = append(lines,
		fmt.Sprintf("transport local=%s peer=%s", d.LocalAddr, d.Addr),
		fmt.Sprintf("session ready=%v hs=%v hsAge=%v tries=%d verifyPending=%v", d.Ready, d.HasHs, d.HsAge, d.HsTries, d.VerifyPending),
		fmt.Sprintf("datagrams sent=%d recv=%d lastRx=%v ago", d.Sent, d.Recv, d.LastRxAge),
	)
	return lines
}

// State reports the call state snapshot for UI rendering.
func (c *callManager) State() (callState, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.peer
}

func (c *callManager) Muted() bool { return c.muted.Load() }

// declinePayload is the wire bytes for refusing a call (shared with the
// headless auto-decline path, which has no manager).
func declinePayload() string {
	raw, _ := json.Marshal(callAnswerPayload{Accept: false})
	return string(raw)
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

// ─── Video live-cycle ───────────────────────────────────────────────────────

// StartVideo begins camera send + remote render. Live call required.
func (c *callManager) StartVideo() error {
	c.mu.Lock()
	if c.state != callLive {
		st := c.state
		c.mu.Unlock()
		return fmt.Errorf("no live call (state %s)", st)
	}
	if c.videoOn {
		c.mu.Unlock()
		return fmt.Errorf("video already on")
	}
	peer := c.peer
	c.mu.Unlock()
	return c.startCameraTx("video on with " + peer)
}

// startCameraTx is the single-flight camera loop shared by call-attached
// video and room publish (one TX loop ever: videoOn guards).
func (c *callManager) startCameraTx(info string) error {
	c.mu.Lock()
	srcFn := c.videoSrc
	if srcFn == nil {
		srcFn = defaultVideoSrc
	}
	c.mu.Unlock()

	frames, stopSrc, err := srcFn()
	if err != nil {
		return err
	}
	// Local monitor decode for self-view (best-effort: remote streaming
	// must not depend on it).
	preview, previewErr := c.previewDecoder()
	stop := make(chan struct{})
	c.mu.Lock()
	c.videoStop, c.videoOn = stop, true
	c.stopVideoSrc = stopSrc
	c.previewDec = preview
	// Self-view is TX-side: the pump must run even when this side never
	// receives remote fragments (sender-only never hits ensureRxLocked).
	if c.rxDone == nil {
		c.rxDone = make(chan struct{})
	}
	if !c.rxPump {
		c.rxPump = true
		c.startRenderPump()
	}
	c.mu.Unlock()
	if previewErr != nil {
		c.emitState("self-view unavailable: " + previewErr.Error())
	}
	c.emitState(info)
	depay := &codecs.VP8Packet{}
	// TX: camera frames → frag → fan-out to every ready session (call peer
	// and/or room publish targets share one loop; per-peer Noise sessions
	// keep it E2E). Preview subsampled: self-view at half rate halves local
	// decode CPU for zero network effect.
	c.audioWg.Add(1)
	go func() {
		defer c.audioWg.Done()
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
				if preview != nil && n%2 == 0 {
					preview.submit(f) // drop-if-full; send path never waits
				}
				payloads, err := fragVP8(f)
				if err != nil {
					continue
				}
				c.mu.Lock()
				t := c.transport
				c.videoTs++
				ts := c.videoTs
				c.txFrames++
				c.mu.Unlock()
				if t == nil {
					return
				}
				for i, p := range payloads {
					c.mu.Lock()
					c.videoSeq++
					seq := c.videoSeq
					muted := c.pubMuted
					c.mu.Unlock()
					pkt := encodeVideoFrag(seq, uint16(i), uint16(len(payloads)), ts, p)
					for _, peer := range t.readyPeers() {
						if muted != nil && muted[peer] {
							continue // sent video-stop: keep session, skip frames
						}
						_ = t.sendMedia(peer, mediaKindVideo, pkt)
					}
				}
			}
		}
	}()
	_ = depay
	return nil
}

// ensureRxLocked builds the receive pipeline on first inbound video (a
// peer may publish while we only watch). Runs under c.mu.
func (c *callManager) ensureRxLocked() bool {
	if c.videoAsm != nil && c.videoDec != nil {
		return true
	}
	// A call, our own publish, or a watched publisher all legitimize
	// inbound video (stray frags from anyone else have no session anyway).
	if c.state != callLive && !c.publishing && !c.watching {
		return false
	}
	decFn := c.videoDecFn
	if decFn == nil {
		decFn = func() (frameDecoder, error) { return newFFmpegDecoder() }
	}
	dec, err := decFn()
	if err != nil {
		return false
	}
	c.videoAsm = newFragAssembler()
	c.videoDec = dec
	if c.rxDone == nil {
		c.rxDone = make(chan struct{})
	}
	if !c.rxPump {
		c.rxPump = true
		c.startRenderPump()
	}
	return true
}

// onVideoFrag reassembles, decodes, and renders one remote fragment.
// Depacketize first (FU-A descriptors are transport, not picture data),
// then assemble by timestamp; only complete frames decode. One decoder
// serves one sender at a time: frags pin to the first active sender and
// anyone else is ignored until 5s of silence (interleaved timestamps from
// two publishers would otherwise corrupt every frame).
func (c *callManager) onVideoFrag(peer string, frag videoFrag) {
	c.mu.Lock()
	if !c.ensureRxLocked() {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	if c.rxPinned == "" {
		c.rxPinned, c.rxPinAt = peer, now
	} else if peer != c.rxPinned {
		if now.Sub(c.rxPinAt) > videoPinTimeout {
			c.rxPinned, c.rxPinAt = peer, now
			c.videoAsm = newFragAssembler()
		} else {
			c.mu.Unlock()
			return
		}
	} else {
		c.rxPinAt = now
	}
	asm := c.videoAsm
	c.mu.Unlock()
	payload, err := func() ([]byte, error) {
		depay := &codecs.VP8Packet{}
		return depay.Unmarshal(frag.Data)
	}()
	if err != nil {
		return
	}
	complete := asm.push(videoFrag{
		Seq: frag.Seq, FragIdx: frag.FragIdx, FragTotal: frag.FragTotal,
		Ts: frag.Ts, Data: payload,
	})
	if complete == nil {
		return
	}
	c.mu.Lock()
	dec := c.videoDec
	if dec != nil {
		c.rxFrames++
	}
	c.mu.Unlock()
	if dec == nil {
		return
	}
	dec.submit(complete) // drop-if-full: decode keeps pace or sheds load
}

// startRenderPump relays decoded RGB (remote and self-view) to ASCII
// rendering. Decoders are snapshotted per iteration: nil channels block
// forever in select, so missing sides simply idle. Paint is throttled per
// side (videoRenderMinInterval): decode runs full-rate, the TUI sheds.
func (c *callManager) startRenderPump() {
	c.audioWg.Add(1)
	go func() {
		defer c.audioWg.Done()
		for {
			c.mu.Lock()
			var remote, preview <-chan []byte
			if c.videoDec != nil {
				remote = c.videoDec.results()
			}
			if c.previewDec != nil {
				preview = c.previewDec.results()
			}
			done := c.rxDone
			c.mu.Unlock()
			if done == nil {
				return
			}
			select {
			case <-done:
				return
			case rgb, ok := <-remote:
				if !ok {
					return
				}
				now := time.Now()
				c.mu.Lock()
				c.rxOn = true
				due := now.Sub(c.lastRemoteShow) >= videoRenderMinInterval
				if due {
					c.lastRemoteShow = now
					c.remoteShows++
				}
				c.mu.Unlock()
				if due {
					if cb := c.callbacks().onVideoFrame; cb != nil {
						cb(asciiFrame(rgb, videoWidth, videoHeight, videoPaneCols, videoPaneRows))
					}
				}
			case rgb, ok := <-preview:
				if !ok {
					return
				}
				now := time.Now()
				c.mu.Lock()
				due := now.Sub(c.lastSelfShow) >= videoRenderMinInterval
				if due {
					c.lastSelfShow = now
					c.selfShows++
				}
				c.mu.Unlock()
				if due {
					if cb := c.callbacks().onSelfFrame; cb != nil {
						cb(asciiFrame(rgb, videoWidth, videoHeight, videoPaneCols, videoPaneRows))
					}
				}
			}
		}
	}()
}

// rxDoneChan returns the receive-path done channel, creating it with the
// pipeline. Render pump and decoder die with the call, never the frame.
func (c *callManager) rxDoneChan() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rxDone == nil {
		c.rxDone = make(chan struct{})
	}
	return c.rxDone
}

// StopVideo ends camera send + remote render, keeping audio up. In publish
// mode it also withdraws the room announce (watchers keep their last frame
// until our video-stop lands); watching others continues on the shared
// pipeline.
func (c *callManager) StopVideo() {
	c.mu.Lock()
	stop := c.videoStop
	dec := c.videoDec
	srcStop := c.stopVideoSrc
	pdec := c.previewDec
	wasPublishing := c.publishing
	var targets []string
	for to := range c.pubTargets {
		targets = append(targets, to)
	}
	c.videoStop, c.videoOn, c.stopVideoSrc = nil, false, nil
	c.previewDec = nil
	c.publishing = false
	c.pubTargets = nil
	c.pubMuted = nil
	keepRx := c.watching
	var rdone chan struct{}
	if !keepRx {
		rdone = c.rxDone
		dec = c.videoDec
		c.videoAsm, c.videoDec, c.rxOn = nil, nil, false
		c.rxDone, c.rxPump = nil, false
	} else {
		dec = nil
	}
	c.mu.Unlock()
	if wasPublishing {
		for _, to := range targets {
			_ = c.sendNote(to, videoStop, "{}")
		}
	}
	if stop != nil {
		close(stop)
	}
	if rdone != nil {
		close(rdone)
	}
	if srcStop != nil {
		srcStop()
	}
	if pdec != nil {
		pdec.close()
	}
	if dec != nil {
		dec.close()
	}
}

// Publishing reports whether room video (no call) is streaming.
func (c *callManager) Publishing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.publishing
}

// Watching reports whether a room publisher is being watched.
func (c *callManager) Watching() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watching
}

// VideoOn reports whether video is streaming.
func (c *callManager) VideoOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.videoOn
}

// RxOn reports whether remote video has arrived at least once.
func (c *callManager) RxOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rxOn
}

func defaultVideoSrc() (<-chan []byte, func(), error) {
	enc, err := startVideoEncoder()
	if err != nil {
		return nil, nil, err
	}
	out := make(chan []byte, 8)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer enc.stop()
		d := newOggDemux(enc.out)
		// Skip the 2 stream-header packets by position (never by content:
		// video bytes could theoretically match the magic).
		for skipped := 0; skipped < 2; skipped++ {
			if _, err := d.packet(); err != nil {
				return
			}
		}
		for {
			select {
			case <-done:
				return
			default:
			}
			pkt, err := d.packet()
			if err != nil {
				return
			}
			select {
			case out <- pkt:
			case <-done:
				return
			}
		}
	}()
	return out, func() { close(done) }, nil
}
