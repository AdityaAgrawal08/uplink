package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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
	ma.playSink = func() (func([]int16), func(), error) { return func([]int16) {}, func() {}, nil }
	mb.playSink = func() (func([]int16), func(), error) {
		return func([]int16) { mu.Lock(); heard++; mu.Unlock() }, func() {}, nil
	}
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

func TestToggleVideoRequiresScope(t *testing.T) {
	p := newPublishPair(t)
	if err := p.a.ToggleVideo(nil); err == nil {
		t.Fatal("publishing to nobody must fail")
	}
	if p.a.VideoOn() {
		t.Fatal("failed toggle must not flip state")
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

func TestVideoPinSecondSender(t *testing.T) {
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
	m.mu.Lock()
	pinned := m.rxPinned
	m.mu.Unlock()
	if pinned != "bob" {
		t.Fatalf("first sender must pin; pinned=%q", pinned)
	}
	select {
	case lines := <-rendered:
		if len(lines) == 0 {
			t.Fatal("empty render")
		}
	case <-time.After(time.Second):
		t.Fatal("pinned frame never rendered")
	}
	// Let the paint throttle window pass before the re-pin assertions.
	time.Sleep(videoRenderMinInterval + 40*time.Millisecond)
	// Second sender while pinned: ignored entirely.
	for _, fr := range mkVideoFrags(t, f, 2002) {
		m.onVideoFrag("carol", fr)
	}
	select {
	case lines := <-rendered:
		t.Fatalf("second sender must not render while pinned (%d lines)", len(lines))
	default:
	}
	// After silence, the pin yields to the new sender.
	m.mu.Lock()
	m.rxPinAt = time.Now().Add(-videoPinTimeout - time.Second)
	m.mu.Unlock()
	for _, fr := range mkVideoFrags(t, f, 3003) {
		m.onVideoFrag("carol", fr)
	}
	m.mu.Lock()
	pinned = m.rxPinned
	m.mu.Unlock()
	if pinned != "carol" {
		t.Fatalf("pin must yield after silence; pinned=%q", pinned)
	}
	// The paint throttle (70ms) may shed this frame; wait past it.
	deadline := time.Now().Add(time.Second)
	for len(rendered) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-rendered:
	case <-time.After(time.Second):
		t.Fatal("unpinned frame never rendered")
	}
}

func TestAudioPinSwitchesSpeakers(t *testing.T) {
	m := newMediaManager("alice", nil, nil, nil, mediaUICallbacks{})
	m.mu.Lock()
	m.pubAudio["bob"] = true
	m.pubAudio["carol"] = true
	m.mu.Unlock()
	pkt := audioPacket{}
	for i := 0; i < 5; i++ {
		m.onRemoteAudio("bob", pkt)
	}
	m.mu.Lock()
	pinned := m.audioPinned
	m.mu.Unlock()
	if pinned != "bob" {
		t.Fatalf("first talker must pin; pinned=%q", pinned)
	}
	// Carol talks while bob still active: bob keeps the speaker.
	m.onRemoteAudio("carol", pkt)
	m.mu.Lock()
	pinned = m.audioPinned
	m.mu.Unlock()
	if pinned != "bob" {
		t.Fatalf("active talker must hold the pin; pinned=%q", pinned)
	}
	// After 2s of silence, carol takes over.
	m.mu.Lock()
	m.audioPinAt = time.Now().Add(-audioPinTimeout - time.Second)
	m.mu.Unlock()
	m.onRemoteAudio("carol", pkt)
	m.mu.Lock()
	pinned = m.audioPinned
	m.mu.Unlock()
	if pinned != "carol" {
		t.Fatalf("pin must switch after silence; pinned=%q", pinned)
	}
}

func TestSlashRegistryExact(t *testing.T) {
	want := []string{"/help", "/upload", "/download", "/video", "/audio"}
	got := make([]string, 0, len(slashCommands))
	for _, cmd := range slashCommands {
		got = append(got, cmd.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("slash registry = %v; want exactly %v", got, want)
	}
}

func TestVideoSidebarSplitAndScroll(t *testing.T) {
	c := newFilterScreen("bob", "", "carol")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	sc.call = &mediaManager{videoOn: true, renderCols: videoPaneDefaultCols, renderRows: videoPaneDefaultRows}
	lines := make([]string, 0, videoPaneDefaultRows)
	for i := 0; i < videoPaneDefaultRows; i++ {
		lines = append(lines, "row")
	}
	m2, _ := sc.Update(netVideoMsg{lines: lines})
	sc = m2.(chatScreen)
	l := sc.layoutFor()
	if l.videoRows == 0 {
		t.Fatal("video box must claim rows while streaming")
	}
	if got := sc.peerAtY(l.rosterY0+1, l); got != "carol" {
		t.Fatalf("roster row below the video box mapped to %q; want carol", got)
	}
	// Wheel over the video box is swallowed (scrolls video), never selects.
	m3, _ := sc.Update(tea.MouseMsg{X: l.rosterX + 1, Y: l.rosterY0 - 1, Type: tea.MouseWheelDown})
	sc = m3.(chatScreen)
	if sc.targetUser != "" {
		t.Fatal("wheel over video must not open a thread")
	}
	// Video box present above the roster.
	if got := sc.View(); !strings.Contains(got, "VIDEO") || !strings.Contains(got, "ONLINE") {
		t.Fatal("sidebar must show VIDEO above ONLINE")
	}
	// Video off collapses the split cleanly.
	sc.call = &mediaManager{}
	l2 := sc.layoutFor()
	if l2.videoRows != 0 || l2.rosterY0 >= l.rosterY0 {
		t.Fatal("video collapse must return rows to the roster")
	}
}
