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
	onVideoFrame func(lines []string)                            // decoded ASCII video frame
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
	videoAsm     *fragAssembler
	videoDec     frameDecoder
	videoSrc     func() (<-chan []byte, func(), error)
	videoDecFn   func() (frameDecoder, error)
	videoOn      bool
	videoSeq     uint16
	videoTs      uint32
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

func (c *callManager) emitState(info string) {
	c.mu.Lock()
	st, peer := c.state, c.peer
	c.mu.Unlock()
	if c.cb.onState != nil {
		c.cb.onState(st, peer, info)
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

// Hangup ends the live call (notifies the peer best-effort).
func (c *callManager) Hangup() error {
	c.mu.Lock()
	if c.state != callLive && c.state != callOutgoing {
		c.mu.Unlock()
		return fmt.Errorf("not in a call")
	}
	peer := c.peer
	c.mu.Unlock()
	_ = c.sendNote(peer, callEnd, "{}")
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
	c.videoStop, c.videoAsm, c.videoDec, c.videoOn, c.rxOn, c.stopVideoSrc = nil, nil, nil, false, false, nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if vstop != nil {
		close(vstop)
	}
	c.audioWg.Wait()
	if vsrcStop != nil {
		vsrcStop()
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
		if c.cb.onRinging != nil {
			c.cb.onRinging(n.From)
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
	}
}

func (c *callManager) mediaCB() mediaCallbacks {
	return mediaCallbacks{
		onAudio: c.onRemoteAudio,
		onVideo: func(_ string, frag videoFrag) { c.onVideoFrag(frag) },
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
				if tick%5 == 0 && c.cb.onLevel != nil {
					c.cb.onLevel(rmsLevel(f))
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
					if t != nil && !warned && !readyAt.IsZero() &&
						time.Since(readyAt) > callWatchdogAfter &&
						time.Since(lastRx) > callWatchdogAfter {
						c.mu.Lock()
						c.warnedNoMedia = true
						c.mu.Unlock()
						c.emitState("no media received — direct path may be blocked (guest WiFi isolation?)")
					}
				}
			}
		}
	}()
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
	srcFn := c.videoSrc
	if srcFn == nil {
		srcFn = defaultVideoSrc
	}
	c.mu.Unlock()

	frames, stopSrc, err := srcFn()
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	c.mu.Lock()
	c.videoStop, c.videoOn = stop, true
	c.stopVideoSrc = stopSrc
	c.mu.Unlock()
	c.emitState("video on with " + peer)
	depay := &codecs.VP8Packet{}
	// TX: camera frames → frag → send.
	c.audioWg.Add(1)
	go func() {
		defer c.audioWg.Done()
		for {
			select {
			case <-stop:
				return
			case f, ok := <-frames:
				if !ok {
					return
				}
				payloads, err := fragVP8(f)
				if err != nil {
					continue
				}
				c.mu.Lock()
				t := c.transport
				c.videoTs++
				ts := c.videoTs
				c.mu.Unlock()
				if t == nil {
					return
				}
				for i, p := range payloads {
					c.mu.Lock()
					c.videoSeq++
					seq := c.videoSeq
					c.mu.Unlock()
					_ = t.sendMedia(peer, mediaKindVideo, encodeVideoFrag(seq, uint16(i), uint16(len(payloads)), ts, p))
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
	if c.state != callLive {
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
	return true
}

// onVideoFrag reassembles, decodes, and renders one remote fragment.
// Depacketize first (FU-A descriptors are transport, not picture data),
// then assemble by timestamp; only complete frames decode.
func (c *callManager) onVideoFrag(frag videoFrag) {
	c.mu.Lock()
	if !c.ensureRxLocked() {
		c.mu.Unlock()
		return
	}
	asm, dec := c.videoAsm, c.videoDec
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
	rgb, err := dec.decode(complete)
	if err != nil {
		return
	}
	c.mu.Lock()
	c.rxOn = true
	onFrame := c.cb.onVideoFrame
	c.mu.Unlock()
	if onFrame != nil {
		onFrame(asciiFrame(rgb, videoWidth, videoHeight, videoPaneCols, videoPaneRows))
	}
}

// StopVideo ends camera send + remote render, keeping audio up.
func (c *callManager) StopVideo() {
	c.mu.Lock()
	stop := c.videoStop
	dec := c.videoDec
	srcStop := c.stopVideoSrc
	c.videoStop, c.videoAsm, c.videoDec, c.videoOn, c.rxOn, c.stopVideoSrc = nil, nil, nil, false, false, nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if srcStop != nil {
		srcStop()
	}
	if dec != nil {
		dec.close()
	}
}

// VideoOn reports whether video is streaming.
func (c *callManager) VideoOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.videoOn
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
		r := newIvfReader(enc.out)
		for {
			select {
			case <-done:
				return
			default:
			}
			f, err := r.frame()
			if err != nil {
				return
			}
			select {
			case out <- f:
			case <-done:
				return
			}
		}
	}()
	return out, func() { close(done) }, nil
}
