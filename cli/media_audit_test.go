package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- C2: FEC streak cap ----------------------------------------------------

// One real Opus packet followed by permanent silence must not hum forever:
// after ~8 PLC ticks the talker contributes silence.
func TestFECStreakCapsHum(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	m.mu.Lock()
	m.pubAudio["bob"] = true
	m.mu.Unlock()
	path := t.TempDir() + "/fec.pcm"
	m.playSink = func() (*speaker, error) { return openFileSpeaker(path) }

	// Three distinct packets prime the jitter buffer, then eternal
	// silence: only the capped FEC tail may play after that.
	pkts := encodeSine(t, 440, 8000, 3)
	feedStream(t, m, "bob", pkts, 7)
	m.ensureAudioRx()
	time.Sleep(1200 * time.Millisecond) // 60 ticks; uncapped hum would fill it all
	m.stopAudioPlayout()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frames := len(raw) / (voiceFrameLen * 2)
	// ≤8 FEC tail frames + slack for tick races. Uncapped code emits ~60.
	if frames > 14 {
		t.Fatalf("FEC hum never capped: %d frames played after stream end", frames)
	}
	if frames == 0 {
		t.Fatal("nothing played at all — playout broken")
	}
}

// encodeSine produces nFrames of DISTINCT Opus packets from one encoder
// over a phase-continuous sine. Reusing a single packet's bytes for every
// sequence number is pathological: the stateful decoder resonates and
// blows up (a test-only artifact, never production traffic).
func encodeSine(t *testing.T, freqHz float64, level int16, nFrames int) [][]byte {
	t.Helper()
	enc, err := newOpusVoice()
	if err != nil {
		t.Fatal(err)
	}
	out := make([][]byte, 0, nFrames)
	for f := 0; f < nFrames; f++ {
		tone := make([]int16, voiceFrameLen)
		for j := range tone {
			ph := 2 * math.Pi * freqHz * float64(f*voiceFrameLen+j) / voiceRate
			tone[j] = int16(float64(level) * math.Sin(ph))
		}
		pkt, err := enc.encode(tone)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, pkt)
	}
	return out
}

func feedStream(t *testing.T, m *mediaManager, peer string, pkts [][]byte, startSeq uint16) {
	t.Helper()
	for i, pkt := range pkts {
		m.onRemoteAudio(peer, audioPacket{Seq: startSeq + uint16(i), Ts: uint32(startSeq+uint16(i)) * voiceFrameLen, Opus: pkt})
	}
}

// ---- H3: normalize-on-clip mixer -------------------------------------------

func TestMixerSingleTalkerPassthrough(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	m.mu.Lock()
	m.pubAudio["bob"] = true
	m.mu.Unlock()
	path := t.TempDir() + "/mix1.pcm"
	m.playSink = func() (*speaker, error) { return openFileSpeaker(path) }

	pkts := encodeSine(t, 440, 9000, 12)
	feedStream(t, m, "bob", pkts, 0)
	m.ensureAudioRx()
	// Keep feeding distinct packets so the talker never gaps out.
	done := make(chan struct{})
	defer close(done)
	go func() {
		enc, _ := newOpusVoice()
		var s uint16 = 12
		var n int
		for {
			select {
			case <-done:
				return
			default:
			}
			tone := make([]int16, voiceFrameLen)
			for j := range tone {
				ph := 2 * math.Pi * 440 * float64((12+n)*voiceFrameLen+j) / voiceRate
				tone[j] = int16(9000 * math.Sin(ph))
			}
			pkt, _ := enc.encode(tone)
			m.onRemoteAudio("bob", audioPacket{Seq: s, Ts: uint32(s) * voiceFrameLen, Opus: pkt})
			s++
			n++
			time.Sleep(15 * time.Millisecond)
		}
	}()
	time.Sleep(600 * time.Millisecond)
	m.stopAudioPlayout()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < voiceFrameLen*2*5 {
		t.Fatalf("single talker starved: %d bytes", len(raw))
	}
	// Sine RMS ≈ level/√2 ≈ 6364; passthrough must preserve loudness
	// within codec tolerance (old soft curve nearly halved lone peaks).
	rmsOut := rmsFile(t, raw)
	if rmsOut < 3500 || rmsOut > 9500 {
		t.Fatalf("single-talker loudness wrong: out=%.0f want≈6364", rmsOut)
	}
}

func TestMixerTwoTalkersNoClip(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	m.mu.Lock()
	m.pubAudio["bob"] = true
	m.pubAudio["carol"] = true
	m.mu.Unlock()
	path := t.TempDir() + "/mix2.pcm"
	m.playSink = func() (*speaker, error) { return openFileSpeaker(path) }

	// Two LOUD talkers on distinct streams: naive sum would hard-clip
	// at int16 extremes; both must stay audible and bounded.
	pktsB := encodeSine(t, 440, 20000, 400)
	pktsC := encodeSine(t, 550, -20000, 400)
	done := make(chan struct{})
	defer close(done)
	go func() {
		var s uint16
		for i := 0; i < 400; i++ {
			select {
			case <-done:
				return
			default:
			}
			m.onRemoteAudio("bob", audioPacket{Seq: s, Ts: uint32(s) * voiceFrameLen, Opus: pktsB[i]})
			m.onRemoteAudio("carol", audioPacket{Seq: s, Ts: uint32(s) * voiceFrameLen, Opus: pktsC[i]})
			s++
			time.Sleep(15 * time.Millisecond)
		}
	}()
	m.ensureAudioRx()
	time.Sleep(600 * time.Millisecond)
	m.stopAudioPlayout()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < voiceFrameLen*2*5 {
		t.Fatalf("mixed talkers starved: %d bytes", len(raw))
	}
	// Both present: output energy must clearly exceed either alone, and
	// stay bounded (normalize, never wrap).
	if e := rmsFile(t, raw); e < 3000 {
		t.Fatalf("mix sounds empty: rms=%.0f", e)
	}
}

func rmsOf(pcm []int16) float64 {
	var sum float64
	for _, v := range pcm {
		f := float64(v) / 32768
		sum += f * f
	}
	if len(pcm) == 0 {
		return 0
	}
	return 32768 * math.Sqrt(sum/float64(len(pcm)))
}

func rmsFile(t *testing.T, raw []byte) float64 {
	t.Helper()
	n := len(raw) / 2
	if n == 0 {
		t.Fatal("empty playout file")
	}
	var sum float64
	for i := 0; i+1 < len(raw); i += 2 {
		v := int16(uint16(raw[i]) | uint16(raw[i+1])<<8)
		f := float64(v) / 32768
		sum += f * f
	}
	return 32768 * math.Sqrt(sum/float64(n))
}

// ---- H1: prune grace -------------------------------------------------------

func TestPruneGraceKeepsFlappingPeer(t *testing.T) {
	m := newMediaManager("alice", nil, func(string, string, string) error { return nil }, nil, mediaUICallbacks{})
	m.mu.Lock()
	m.videoOn = true
	m.videoTo = map[string]bool{"bob": true}
	m.mu.Unlock()
	// One missed tick must NOT prune (heartbeat every 5s, tick every 2s).
	m.PublishTo([]string{})
	m.mu.Lock()
	_, kept1 := m.videoTo["bob"]
	m.mu.Unlock()
	if !kept1 {
		t.Fatal("first missed tick pruned a live peer")
	}
	m.PublishTo([]string{})
	m.mu.Lock()
	_, kept2 := m.videoTo["bob"]
	m.mu.Unlock()
	if !kept2 {
		t.Fatal("second missed tick pruned a live peer")
	}
	// Third consecutive absence: genuinely gone, prune + stop.
	m.PublishTo([]string{})
	m.mu.Lock()
	_, kept3 := m.videoTo["bob"]
	on := m.videoOn
	m.mu.Unlock()
	if kept3 {
		t.Fatal("third consecutive absence must prune the leaver")
	}
	if on {
		t.Fatal("emptied scope must stop the stream")
	}
	// Presence clears the count: rejoin, flap once, still kept.
	m.mu.Lock()
	m.videoOn = true
	m.videoTo = map[string]bool{"bob": true}
	m.mu.Unlock()
	m.PublishTo([]string{})
	m.PublishTo([]string{"bob"})
	m.PublishTo([]string{})
	m.mu.Lock()
	_, kept := m.videoTo["bob"]
	m.mu.Unlock()
	if !kept {
		t.Fatal("presence must reset the absence count")
	}
}

// ---- H2: failed start withdraws watchers -----------------------------------

func TestFailedStartWithdrawsWatchers(t *testing.T) {
	var mu sync.Mutex
	var stops []string
	m := newMediaManager("alice", nil,
		func(to, typ, payload string) error {
			if typ == mediaStop {
				mu.Lock()
				stops = append(stops, to)
				mu.Unlock()
			}
			return nil
		}, nil, mediaUICallbacks{})
	m.micSrc = func() (<-chan []int16, func(), error) {
		return nil, nil, fmt.Errorf("no such device")
	}
	_ = m.ToggleAudio([]string{"bob"})
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, to := range stops {
		if to == "bob" {
			found = true
		}
	}
	if !found {
		t.Fatal("mic failure after announce must send media-stop to the scope")
	}
	if m.AudioOn() {
		t.Fatal("failed toggle must leave audio off")
	}
}

func TestFailedCameraStartWithdrawsWatchers(t *testing.T) {
	var mu sync.Mutex
	var stops []string
	m := newMediaManager("alice", nil,
		func(to, typ, payload string) error {
			if typ == mediaStop {
				mu.Lock()
				stops = append(stops, to)
				mu.Unlock()
			}
			return nil
		}, nil, mediaUICallbacks{})
	m.videoSrcFn = func() (<-chan vidFrame, func(), error) {
		return nil, nil, fmt.Errorf("no such camera")
	}
	if err := m.ToggleVideo([]string{"bob"}); err == nil {
		t.Fatal("camera failure must surface an error")
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, to := range stops {
		if to == "bob" {
			found = true
		}
	}
	if !found {
		t.Fatal("camera failure after announce must send media-stop to the scope")
	}
}

// ---- H4/M2: rapid toggles, single live loop --------------------------------

func TestRapidToggleSingleTxLoop(t *testing.T) {
	m := newMediaManager("alice", nil, func(string, string, string) error { return nil }, nil, mediaUICallbacks{})
	m.dialIP = "127.0.0.1"
	src1 := make(chan vidFrame, 8)
	src2 := make(chan vidFrame, 8)
	calls := 0
	m.videoSrcFn = func() (<-chan vidFrame, func(), error) {
		calls++
		if calls == 1 {
			return src1, func() {}, nil
		}
		return src2, func() {}, nil
	}
	if err := m.ToggleVideo([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if err := m.ToggleVideo([]string{"bob"}); err != nil { // off
		t.Fatal(err)
	}
	if err := m.ToggleVideo([]string{"bob"}); err != nil { // on again
		t.Fatal(err)
	}
	f := jpegFrame(t, 10, 200, 10)
	before := m.txFramesOf()
	src1 <- f // stale loop must ignore this
	time.Sleep(150 * time.Millisecond)
	if got := m.txFramesOf(); got != before {
		t.Fatalf("stale TX loop sent %d frames after toggle", got-before)
	}
	src2 <- f
	deadline := time.Now().Add(2 * time.Second)
	for m.txFramesOf() == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.txFramesOf() == before {
		t.Fatal("fresh TX loop never sent")
	}
	m.mu.Lock()
	gen := m.cameraGen
	m.mu.Unlock()
	if gen != 2 {
		t.Fatalf("camera generation = %d; want 2 (one per successful start)", gen)
	}
}

// ---- M1: speaker death surfaces --------------------------------------------

func TestSpeakerDeathEmits(t *testing.T) {
	var mu sync.Mutex
	var infos []string
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{
		onInfo: func(s string) {
			mu.Lock()
			infos = append(infos, s)
			mu.Unlock()
		},
	})
	path := t.TempDir() + "/dead.pcm"
	sp, err := openFileSpeaker(path)
	if err != nil {
		t.Fatal(err)
	}
	// Kill the sink underneath the writer: next write fails.
	sp.in.(*os.File).Close()
	m.playSink = func() (*speaker, error) { return sp, nil }
	m.mu.Lock()
	m.pubAudio["bob"] = true
	m.mu.Unlock()
	v, err := newOpusVoice()
	if err != nil {
		t.Fatal(err)
	}
	tone := make([]int16, voiceFrameLen)
	for i := range tone {
		tone[i] = 7000
	}
	pkt, err := v.encode(tone)
	if err != nil {
		t.Fatal(err)
	}
	for s := uint16(0); s < 6; s++ {
		m.onRemoteAudio("bob", audioPacket{Seq: s, Ts: uint32(s) * voiceFrameLen, Opus: pkt})
	}
	m.ensureAudioRx()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, s := range infos {
			if strings.Contains(s, "speaker output failed") {
				found = true
			}
		}
		mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("speaker death never surfaced")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *mediaManager) txFramesOf() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.txFrames
}
