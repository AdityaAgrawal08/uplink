package main

import (
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
	a, b   *mediaManager
	feedA  chan []int16
	heardB func() int
	states chan string
}

func newPublishPair(t *testing.T) *publishPair {
	t.Helper()
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	states := make(chan string, 64)
	p := &publishPair{states: states}
	p.feedA = make(chan []int16, 64)

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
	ma.SetRoster(func() map[string][]byte { return map[string][]byte{"bob": idb.publicKey()} })
	mb.SetRoster(func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} })
	p.a, p.b, p.heardB = ma, mb, func() int { mu.Lock(); defer mu.Unlock(); return heard }
	t.Cleanup(func() {
		ma.stopAll()
		mb.stopAll()
	})
	return p
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
	m.micSrc = func() (<-chan []int16, func(), error) {
		ch := make(chan []int16, 4)
		return ch, func() {}, nil
	}
	m.playSink = func() (*speaker, error) { return testSpeaker(t, nil, nil) }
	if err := m.ToggleAudio([]string{"bob"}); err != nil {
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
	want := []string{"/help", "/reply", "/upload", "/download", "/audio", "/kick", "/admin", "/unadmin"}
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
	sc.call = &mediaManager{audioOn: true}
	l := sc.layoutFor()
	if !sc.call.AudioOn() {
		t.Fatal("hidden video UI must preserve call state")
	}
	// Items: General at +0/+1, carol at +2/+3 (two rows per item).
	if got := sc.peerAtY(l.rosterY0+2, l); got != "carol" {
		t.Fatalf("chat row mapped to %q; want carol", got)
	}
	// Wheel over the transcript never opens a thread.
	m3, _ := sc.Update(tea.MouseMsg{X: 70, Y: 10, Type: tea.MouseWheelDown})
	sc = m3.(chatScreen)
	if sc.targetUser != "" {
		t.Fatal("wheel over transcript must not open a thread")
	}
	if got := sc.View(); strings.Contains(got, "Live Cameras") {
		t.Fatal("view must not show the Live Cameras strip")
	}
	// Tiny terminal still paints exactly its rows.
	m4, _ := sc.Update(tea.WindowSizeMsg{Width: 100, Height: 18})
	sc = m4.(chatScreen)
	if rows := strings.Count(sc.View(), "\n") + 1; rows != 18 {
		t.Fatalf("tiny frame painted %d rows; want exactly 18", rows)
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
	feed := make(chan []int16, 64)
	ma.micSrc = func() (<-chan []int16, func(), error) { return feed, func() {}, nil }
	t.Cleanup(ma.stopAll)
	if err := ma.ToggleAudio([]string{"bob"}); err != nil {
		t.Fatal(err)
	}
	// Bob joins LATE, after the audio stream runs: no announce is in flight.
	var mu sync.Mutex
	heard := 0
	mb := newMediaManager("bob", idb,
		func(to, typ, payload string) error {
			ma.onSignalNote(signalNote{From: "bob", Type: typ, Payload: payload})
			return nil
		},
		func() map[string][]byte { return map[string][]byte{"alice": ida.publicKey()} },
		mediaUICallbacks{})
	mb.dialIP = "127.0.0.1"
	mb.playSink = func() (*speaker, error) { return testSpeaker(t, &mu, &heard) }
	mbRef.Store(mb)
	t.Cleanup(mb.stopAll)
	// Force the scope-wide re-announce (normally every 6s via healthCheck).
	ma.mu.Lock()
	ma.lastRenotify = time.Now().Add(-10 * time.Second)
	ma.mu.Unlock()
	ma.healthCheck()
	// Bob must learn and join without any toggle from alice.
	deadline := time.Now().Add(20 * time.Second)
	for {
		mb.mu.Lock()
		hearing := len(mb.pubAudio) > 0
		mb.mu.Unlock()
		if hearing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late joiner never learned the running publisher")
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitMediaPeer(t, ma, "bob")
	waitMediaPeer(t, mb, "alice")
	tone := make([]int16, voiceFrameLen)
	for i := range tone {
		tone[i] = 8000
	}
	for i := 0; i < 30; i++ {
		feed <- append([]int16(nil), tone...)
		time.Sleep(20 * time.Millisecond)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		h := heard
		mu.Unlock()
		if h >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("late joiner never heard the running stream: heard=%d", h)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStopAllNotifiesScope(t *testing.T) {
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
	m.mu.Lock()
	m.audioOn = true
	m.audioTo = map[string]bool{"bob": true, "carol": true}
	m.mu.Unlock()
	m.stopAll()
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"bob", "carol"} {
		found := false
		for _, to := range stops {
			if to == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("stopAll must notify %q, got %v", want, stops)
		}
	}
	if m.AudioOn() {
		t.Fatal("stopAll must switch audio off")
	}
}
