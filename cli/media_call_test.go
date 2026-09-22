package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// testSpeaker builds a file-backed speaker (writer goroutine drains to a
// temp file, never blocks playout) with an optional play counter.
func testSpeaker(t *testing.T, mu *sync.Mutex, heard *int) (*speaker, error) {
	t.Helper()
	sp, err := openFileSpeaker(t.TempDir() + "/speaker.pcm")
	if err != nil {
		return nil, err
	}
	if mu != nil && heard != nil {
		sp.onPlay = func([]int16) { mu.Lock(); *heard++; mu.Unlock() }
	}
	t.Cleanup(sp.close)
	return sp, nil
}

// publishPair wires two managers note-to-note (no server, no hardware):
// synthetic mic + camera, counted playback, drained state/info lines.
type publishPair struct {
	a, b       *mediaManager
	feedA      chan []int16
	heardB     func() int
	states     chan string
	cameraA    chan vidFrame
	stopCamera chan struct{}
}

func newPublishPair(t *testing.T) *publishPair {
	t.Helper()
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	states := make(chan string, 64)
	p := &publishPair{states: states}
	p.feedA = make(chan []int16, 64)
	p.cameraA = make(chan vidFrame, 16)
	p.stopCamera = make(chan struct{})

	var ma, mb *mediaManager
	var heard int
	var mu sync.Mutex
	ma = newMediaManager("alice", ida,
		func(to, typ, payload string) error {
			mb.onSignalNote(signalNote{From: "alice", Type: typ, Payload: payload})
			return nil
		},
		func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} },
		mediaUICallbacks{
			onInfo: func(info string) {
				select {
				case states <- "a:" + info:
				default:
				}
			},
		})
	mb = newMediaManager("bob", idb,
		func(to, typ, payload string) error {
			ma.onSignalNote(signalNote{From: "bob", Type: typ, Payload: payload})
			return nil
		},
		func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} },
		mediaUICallbacks{
			onInfo: func(info string) {
				select {
				case states <- "b:" + info:
				default:
				}
			},
		})
	ma.dialIP, mb.dialIP = "127.0.0.1", "127.0.0.1"
	ma.micSrc = func() (<-chan []int16, func(), error) { return p.feedA, func() {}, nil }
	mb.micSrc = func() (<-chan []int16, func(), error) { return nil, func() {}, nil }
	ma.playSink = func() (*speaker, error) { return testSpeaker(t, nil, nil) }
	mb.playSink = func() (*speaker, error) { return testSpeaker(t, &mu, &heard) }
	ma.videoSrcFn = func() (<-chan vidFrame, func(), error) { return p.cameraA, func() {}, nil }
	ma.SetRoster(func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} })
	mb.SetRoster(func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} })
	p.a, p.b, p.heardB = ma, mb, func() int { mu.Lock(); defer mu.Unlock(); return heard }
	t.Cleanup(func() {
		ma.stopAll()
		mb.stopAll()
	})
	return p
}

// jpegFrame encodes a solid-color 320x240 JPEG (deterministic test input).
func jpegFrame(t *testing.T, r, g, b byte) vidFrame {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, videoWidth, videoHeight))
	for y := 0; y < videoHeight; y++ {
		for x := 0; x < videoWidth; x++ {
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	jf := append([]byte(nil), buf.Bytes()...)
	f, err := jpegToRGB(jf)
	if err != nil {
		t.Fatalf("synthetic jpeg undecodable: %v", err)
	}
	return f
}

func TestVideoPublishLoopback(t *testing.T) {
	p := newPublishPair(t)
	// Continuous camera feed (early frames drop pre-handshake, later flow).
	go func() {
		f := jpegFrame(t, 200, 10, 10)
		for {
			select {
			case <-p.stopCamera:
				return
			case p.cameraA <- f:
				time.Sleep(66 * time.Millisecond)
			}
		}
	}()
	rendered := make(chan []string, 8)
	selfed := make(chan []string, 8)
	p.b.cb.onVideoFrame = func(lines []string) { rendered <- lines }
	p.a.cb.onSelfFrame = func(lines []string) { selfed <- lines }
	if err := p.a.ToggleVideo([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if !p.a.VideoOn() {
		t.Fatal("ToggleVideo must flip videoOn")
	}
	waitMediaPeer(t, p.a, "bob")
	waitMediaPeer(t, p.b, "alice")
	select {
	case lines := <-rendered:
		if len(lines) == 0 {
			t.Fatal("empty remote render")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no published frame rendered")
	}
	select {
	case lines := <-selfed:
		if len(lines) == 0 {
			t.Fatal("empty self-view")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no self-view rendered")
	}
	if !p.b.Watching() {
		t.Fatal("receiver must be watching")
	}
	// Toggle off: receiver stops watching, announce stops.
	if err := p.a.ToggleVideo([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if p.a.VideoOn() {
		t.Fatal("second toggle must stop the stream")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.b.mu.Lock()
		w := len(p.b.pubVideo) > 0
		p.b.mu.Unlock()
		if !w {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("media-stop never landed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestAudioPublishLoopback(t *testing.T) {
	p := newPublishPair(t)
	if err := p.a.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	waitMediaPeer(t, p.a, "bob")
	waitMediaPeer(t, p.b, "alice")
	tone := make([]int16, voiceFrameLen)
	for i := range tone {
		tone[i] = 8000
	}
	for i := 0; i < 30; i++ {
		p.feedA <- append([]int16(nil), tone...)
		time.Sleep(20 * time.Millisecond)
	}
	deadline := time.Now().Add(15 * time.Second)
	for p.heardB() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("audio not flowing: heard=%d", p.heardB())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Toggle off = mute: playback stops growing.
	before := p.heardB()
	if err := p.a.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if p.a.AudioOn() {
		t.Fatal("second toggle must stop the mic")
	}
	time.Sleep(300 * time.Millisecond)
	if after := p.heardB(); after > before+5 {
		t.Fatalf("audio kept flowing after stop: %d -> %d", before, after)
	}
}

func TestToggleVideoSoloPreview(t *testing.T) {
	p := newPublishPair(t)
	// Solo (nil scope): the camera starts in preview-only mode instead of
	// failing — self-view works with nobody else around.
	if err := p.a.ToggleVideo(nil); err != nil {
		t.Fatalf("solo ToggleVideo must succeed: %v", err)
	}
	if !p.a.VideoOn() {
		t.Fatal("solo toggle must flip videoOn")
	}
	// Empty-scope roster ticks while alone must not stop the preview.
	p.a.PublishTo(nil)
	p.a.PublishTo([]string{})
	if !p.a.VideoOn() {
		t.Fatal("solo preview must survive empty-scope ticks")
	}
	// Toggle off stops the preview.
	if err := p.a.ToggleVideo(nil); err != nil {
		t.Fatalf("solo toggle-off must succeed: %v", err)
	}
	if p.a.VideoOn() {
		t.Fatal("second solo toggle must stop the stream")
	}
}

func TestToggleAudioSoloPreview(t *testing.T) {
	p := newPublishPair(t)
	if err := p.a.ToggleAudio(nil); err != nil {
		t.Fatalf("solo ToggleAudio must succeed: %v", err)
	}
	if !p.a.AudioOn() {
		t.Fatal("solo toggle must flip audioOn")
	}
	p.a.PublishTo(nil)
	p.a.PublishTo([]string{})
	if !p.a.AudioOn() {
		t.Fatal("solo mic must survive empty-scope ticks")
	}
	if err := p.a.ToggleAudio(nil); err != nil {
		t.Fatalf("solo toggle-off must succeed: %v", err)
	}
	if p.a.AudioOn() {
		t.Fatal("second solo toggle must stop the mic")
	}
}

func TestPublishTopUpDedupes(t *testing.T) {
	ida, _ := generateIdentity()
	var mu sync.Mutex
	announces := map[string]int{}
	m := newMediaManager("alice", ida,
		func(to, typ, payload string) error {
			if typ == mediaLive {
				mu.Lock()
				announces[to]++
				mu.Unlock()
			}
			return nil
		},
		nil, mediaUICallbacks{})
	m.dialIP = "127.0.0.1"
	frames := make(chan vidFrame, 4)
	m.videoSrcFn = func() (<-chan vidFrame, func(), error) { return frames, func() {}, nil }
	if err := m.ToggleVideo([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	// Fresh peer gets exactly one announce; repeats send none.
	m.PublishTo([]string{"bob", "carol"})
	m.PublishTo([]string{"bob", "carol"})
	mu.Lock()
	defer mu.Unlock()
	if announces["bob"] != 1 {
		t.Fatalf("bob announced %d times; want 1", announces["bob"])
	}
	if announces["carol"] != 1 {
		t.Fatalf("carol announced %d times; want 1", announces["carol"])
	}
}

func mkVideoFrags(t *testing.T, f vidFrame, ts uint32) []videoFrag {
	t.Helper()
	payloads, err := fragVideoFrame(f.Jpeg)
	if err != nil {
		t.Fatal(err)
	}
	var out []videoFrag
	for i, pl := range payloads {
		out = append(out, videoFrag{
			Seq: uint16(ts), FragIdx: uint16(i), FragTotal: uint16(len(payloads)),
			Ts: ts, Data: pl,
		})
	}
	return out
}

func TestVideoTwoPublishersBothPaint(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	rendered := make(chan []string, 8)
	m.cb.onVideoFrame = func(lines []string) { rendered <- lines }
	m.mu.Lock()
	m.pubVideo["bob"] = true
	m.pubVideo["carol"] = true
	m.mu.Unlock()
	fb := jpegFrame(t, 10, 200, 10)
	fc := jpegFrame(t, 200, 10, 10)
	for _, fr := range mkVideoFrags(t, fb, 1001) {
		m.onVideoFrag("bob", fr)
	}
	for _, fr := range mkVideoFrags(t, fc, 2002) {
		m.onVideoFrag("carol", fr)
	}
	// Both feeds must hold independent RX state (no shared pin/assembler).
	m.mu.Lock()
	_, hasB := m.videoFeeds["bob"]
	_, hasC := m.videoFeeds["carol"]
	m.mu.Unlock()
	if !hasB || !hasC {
		t.Fatal("both publishers must hold independent feeds")
	}
	// The grid must paint BOTH peers' tiles (not just one pinned feed).
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case lines := <-rendered:
			joined := strings.Join(lines, "\n")
			if strings.Contains(joined, "bob") && strings.Contains(joined, "carol") {
				return // both tiles painted
			}
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("grid never painted both publishers' tiles")
		}
	}
}

func TestVideoStopOneKeepsSurvivor(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	rendered := make(chan []string, 8)
	m.cb.onVideoFrame = func(lines []string) { rendered <- lines }
	m.mu.Lock()
	m.pubVideo["bob"] = true
	m.pubVideo["carol"] = true
	m.mu.Unlock()
	f := jpegFrame(t, 10, 200, 10)
	for _, fr := range mkVideoFrags(t, f, 1001) {
		m.onVideoFrag("bob", fr)
	}
	for _, fr := range mkVideoFrags(t, f, 2002) {
		m.onVideoFrag("carol", fr)
	}
	// Drain until both paint, then stop bob: carol must survive without
	// shared-pipeline teardown.
	deadline := time.Now().Add(2 * time.Second)
both:
	for {
		select {
		case lines := <-rendered:
			joined := strings.Join(lines, "\n")
			if strings.Contains(joined, "bob") && strings.Contains(joined, "carol") {
				break both
			}
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("grid never painted both tiles before stop")
		}
	}
	m.onSignalNote(signalNote{From: "bob", Type: mediaStop, Payload: `{"video":true}`})
	m.mu.Lock()
	_, hasB := m.videoFeeds["bob"]
	_, hasC := m.videoFeeds["carol"]
	m.mu.Unlock()
	if hasB {
		t.Fatal("stopped publisher's feed must be dropped")
	}
	if !hasC {
		t.Fatal("survivor's feed must persist across another's stop")
	}
	// Carol alone must still paint.
	deadline = time.Now().Add(2 * time.Second)
	for {
		select {
		case lines := <-rendered:
			if strings.Contains(strings.Join(lines, "\n"), "carol") {
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("survivor stopped painting after peer stop")
		}
	}
}

func TestRenderGridLayout(t *testing.T) {
	f := jpegFrame(t, 10, 200, 10)
	one := renderGrid([]namedFrame{{"bob", f}}, 40, 12)
	if len(one) == 0 {
		t.Fatal("single tile must render")
	}
	if !strings.Contains(one[0], "bob") {
		t.Fatalf("first row must label the peer; got %q", one[0])
	}
	if got := ansiWidth(stripANSIGrid(one[0])); got != 40 {
		t.Fatalf("tile row width %d; want 40", got)
	}
	two := renderGrid([]namedFrame{{"a", f}, {"b", f}, {"c", f}, {"d", f}}, 40, 16)
	if len(two) == 0 {
		t.Fatal("2x2 grid must render")
	}
	joined := strings.Join(two, "\n")
	for _, p := range []string{"a", "b", "c", "d"} {
		if !strings.Contains(joined, p) {
			t.Fatalf("grid missing tile %q", p)
		}
	}
	if got := renderGrid(nil, 40, 12); got != nil {
		t.Fatal("empty grid must be nil")
	}
}

func stripANSIGrid(s string) string {
	var sb strings.Builder
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func TestAudioBothTalkersDecoded(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	m.mu.Lock()
	m.pubAudio["bob"] = true
	m.pubAudio["carol"] = true
	m.mu.Unlock()
	pkt := audioPacket{Opus: []byte{0x01}}
	m.onRemoteAudio("bob", pkt)
	m.onRemoteAudio("carol", pkt)
	// Each talker owns a decoder: no shared state, no pin.
	m.mu.Lock()
	_, hasB := m.rxDecs["bob"]
	_, hasC := m.rxDecs["carol"]
	_, hasJB := m.rxJbs["bob"]
	m.mu.Unlock()
	if !hasB || !hasC || !hasJB {
		t.Fatal("both talkers need decoders + jitter buffers")
	}
}

func TestSlashRegistryExact(t *testing.T) {
	want := []string{"/help", "/upload", "/download", "/video", "/audio", "/kick", "/admin", "/unadmin"}
	got := make([]string, 0, len(slashCommands))
	for _, cmd := range slashCommands {
		got = append(got, cmd.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("slash registry = %v; want exactly %v", got, want)
	}
}

func TestHiddenVideoAndChatMapping(t *testing.T) {
	c := newFilterScreen("bob", "", "carol")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	sc := m.(chatScreen)
	sc.call = &mediaManager{videoOn: true, renderCols: videoPaneDefaultCols, renderRows: videoPaneDefaultRows}
	lines := make([]string, 0, videoPaneDefaultRows)
	for i := 0; i < videoPaneDefaultRows; i++ {
		lines = append(lines, "row")
	}
	m2, _ := sc.Update(netVideoMsg{lines: lines})
	sc = m2.(chatScreen)
	l := sc.layoutFor()
	if l.videoRows != 0 || l.vidPanelW != 0 || l.camRows != 0 {
		t.Fatal("video surfaces must not claim UI space")
	}
	if len(sc.videoLines) != len(lines) || !sc.call.VideoOn() {
		t.Fatal("hiding video UI must preserve received frames and call state")
	}
	// Items: General at +0/+1, carol at +2/+3 (two rows per item).
	if got := sc.peerAtY(l.rosterY0+2, l); got != "carol" {
		t.Fatalf("chat row mapped to %q; want carol", got)
	}
	// Wheel over the camera strip never opens a thread (falls to chat).
	frameOff, headOff := 0, 0
	if l.frameOn {
		frameOff = 1
	}
	if l.showHeader {
		headOff = 1
	}
	stripY := frameOff + headOff + l.headRows + l.callRows + l.vpHeight + 2 + 1
	m3, _ := sc.Update(tea.MouseMsg{X: 70, Y: stripY, Type: tea.MouseWheelDown})
	sc = m3.(chatScreen)
	if sc.targetUser != "" {
		t.Fatal("wheel over camera strip must not open a thread")
	}
	if got := sc.View(); strings.Contains(got, "Live Cameras") {
		t.Fatal("view must not show the Live Cameras strip")
	}
	// Tiny terminal collapses the strip cleanly (rows return to chat).
	m4, _ := sc.Update(tea.WindowSizeMsg{Width: 100, Height: 18})
	sc = m4.(chatScreen)
	if l2 := sc.layoutFor(); l2.camRows != 0 || l2.vidPanelW != 0 {
		t.Fatal("video UI must collapse on short terminals")
	}
}

func TestKeepaliveOnStalledMic(t *testing.T) {
	p := newPublishPair(t)
	// Mic source that never yields (stalled capture): the keepalive
	// ticker must still hold the session open.
	stalled := make(chan []int16)
	p.a.micSrc = func() (<-chan []int16, func(), error) { return stalled, func() {}, nil }
	if err := p.a.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	waitMediaPeer(t, p.a, "bob")
	waitMediaPeer(t, p.b, "alice")
	// 3s of total mic silence: keepalive pings (1/s) must keep lastRx
	// fresh, so no heal verdict fires and the session stays Ready.
	time.Sleep(3 * time.Second)
	if d := p.b.transport.diagPeer("alice"); d.LastRxAge > 3*time.Second {
		t.Fatalf("keepalive failed: lastRx age %v", d.LastRxAge)
	}
	if !p.a.transport.peerReady("bob") || !p.b.transport.peerReady("alice") {
		t.Fatal("session must survive a stalled mic")
	}
	select {
	case s := <-p.states:
		if strings.Contains(s, "silent") || strings.Contains(s, "re-probing") {
			t.Fatalf("no heal verdict may fire on a ping-held path: %q", s)
		}
	default:
	}
}

func TestProbeMicPeakGatesSilence(t *testing.T) {
	quiet := make(chan []int16, 4)
	loud := make(chan []int16, 4)
	silent := make([]int16, voiceFrameLen) // exact digital zeros
	tone := make([]int16, voiceFrameLen)
	for i := range tone {
		tone[i] = 4000
	}
	quiet <- silent
	loud <- tone
	if peak, ok := probeMicPeak(quiet); !ok || peak >= micSilenceFloor {
		t.Fatalf("silent input must probe below floor: peak=%d ok=%v", peak, ok)
	}
	if peak, ok := probeMicPeak(loud); !ok || peak < micSilenceFloor {
		t.Fatalf("live input must clear the floor: peak=%d ok=%v", peak, ok)
	}
	closed := make(chan []int16)
	close(closed)
	if _, ok := probeMicPeak(closed); ok {
		t.Fatal("closed channel must report not-ok")
	}
}

func TestLateJoinerSeesRunningPublisher(t *testing.T) {
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	var mbRef atomic.Value // *mediaManager, set when bob joins late
	sendToBob := func(to, typ, payload string) error {
		if to != "bob" {
			return nil
		}
		if mb, ok := mbRef.Load().(*mediaManager); ok && mb != nil {
			mb.onSignalNote(signalNote{From: "alice", Type: typ, Payload: payload})
		}
		return nil // bob absent: note lost (the late-joiner hole)
	}
	ma := newMediaManager("alice", ida, sendToBob,
		func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} },
		mediaUICallbacks{})
	ma.dialIP = "127.0.0.1"
	frames := make(chan vidFrame, 8)
	ma.videoSrcFn = func() (<-chan vidFrame, func(), error) { return frames, func() {}, nil }
	feed := make(chan []int16, 64)
	ma.micSrc = func() (<-chan []int16, func(), error) { return feed, func() {}, nil }
	t.Cleanup(ma.stopAll)
	if err := ma.ToggleVideo([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if err := ma.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	// Bob joins LATE, after both streams run: no announce is in flight.
	mb := newMediaManager("bob", idb,
		func(to, typ, payload string) error {
			ma.onSignalNote(signalNote{From: "bob", Type: typ, Payload: payload})
			return nil
		},
		func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} },
		mediaUICallbacks{})
	mb.dialIP = "127.0.0.1"
	mbRef.Store(mb)
	t.Cleanup(mb.stopAll)
	rendered := make(chan []string, 8)
	mb.cb.onVideoFrame = func(lines []string) { rendered <- lines }
	// Force the scope-wide re-announce (normally every 6s via healthCheck).
	ma.mu.Lock()
	ma.lastRenotify = time.Now().Add(-10 * time.Second)
	ma.mu.Unlock()
	ma.healthCheck()
	// Bob must learn, join, and render without any toggle from alice.
	deadline := time.Now().Add(20 * time.Second)
	for {
		mb.mu.Lock()
		watching := len(mb.pubVideo) > 0
		mb.mu.Unlock()
		if watching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late joiner never learned the running publisher")
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitMediaPeer(t, ma, "bob")
	waitMediaPeer(t, mb, "alice")
	go func() {
		f := jpegFrame(t, 10, 200, 10)
		for {
			select {
			case frames <- f:
				time.Sleep(66 * time.Millisecond)
			case <-time.After(20 * time.Second):
				return
			}
		}
	}()
	select {
	case lines := <-rendered:
		if len(lines) == 0 {
			t.Fatal("empty late-join render")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("late joiner never rendered the running stream")
	}
}
