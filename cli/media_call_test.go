package main

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// callPair wires two managers with direct note routing + synthetic audio:
// heardFrames is a mutex-guarded capture of played-back audio.
type heardFrames struct {
	mu     sync.Mutex
	frames [][]int16
}

func (h *heardFrames) add(pcm []int16) {
	h.mu.Lock()
	h.frames = append(h.frames, append([]int16(nil), pcm...))
	h.mu.Unlock()
}

func (h *heardFrames) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.frames)
}

func (h *heardFrames) energy() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var e float64
	var n int
	for _, f := range h.frames {
		for _, s := range f {
			e += float64(s) * float64(s)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return e / float64(n)
}

// full call stack, no server, no hardware.
func callPair(t *testing.T) (a, b *callManager, feedA, feedB chan []int16, heardA, heardB *heardFrames, states chan string) {
	t.Helper()
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	rosterA := map[string][]byte{"bob": idb.publicKey()}
	rosterB := map[string][]byte{"alice": ida.publicKey()}
	states = make(chan string, 16)
	var ma, mb *callManager
	mkSink := func(store *heardFrames) (func([]int16), func(), error) {
		return store.add, func() {}, nil
	}
	gotA, gotB := &heardFrames{}, &heardFrames{}
	feedA = make(chan []int16, 32)
	feedB = make(chan []int16, 32)
	ma = newCallManager("alice", ida,
		func(to, typ, payload string) error {
			mb.onSignalNote(signalNote{From: "alice", Type: typ, Payload: payload})
			return nil
		},
		func() map[string][]byte { return rosterA },
		callCallbacks{onState: func(s callState, p, info string) { states <- "a:" + s.String() + ":" + p }})
	mb = newCallManager("bob", idb,
		func(to, typ, payload string) error {
			ma.onSignalNote(signalNote{From: "bob", Type: typ, Payload: payload})
			return nil
		},
		func() map[string][]byte { return rosterB },
		callCallbacks{onState: func(s callState, p, info string) { states <- "b:" + s.String() + ":" + p }})
	ma.dialIP, mb.dialIP = "127.0.0.1", "127.0.0.1"
	ma.micSrc = func() (<-chan []int16, func(), error) { return feedA, func() {}, nil }
	mb.micSrc = func() (<-chan []int16, func(), error) { return feedB, func() {}, nil }
	ma.playSink = func() (func([]int16), func(), error) { return mkSink(gotA) }
	mb.playSink = func() (func([]int16), func(), error) { return mkSink(gotB) }
	heardA, heardB = gotA, gotB
	t.Cleanup(func() {
		_ = ma.Hangup()
		_ = mb.Hangup()
	})
	return ma, mb, feedA, feedB, heardA, heardB, states
}

func dumpState(m *callManager) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	audio := m.audioStop != nil
	return m.state.String() + "/" + m.peer + "/audio:" + map[bool]string{true: "on", false: "off"}[audio]
}

func dumpMedia(m *callManager, peer string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.transport == nil {
		return "no-transport"
	}
	if m.transport.peerReady(peer) {
		return "media-ready"
	}
	return "transport-no-session"
}

func waitCallState(t *testing.T, states chan string, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case got := <-states:
			if got == want {
				return
			}
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatalf("never saw call state %q", want)
			}
		}
	}
}

func feedTone(feed chan []int16, n int) {
	f := make([]int16, voiceFrameLen)
	for i := range f {
		if (i/24)%2 == 0 {
			f[i] = 6000
		} else {
			f[i] = -6000
		}
	}
	for i := 0; i < n; i++ {
		feed <- append([]int16(nil), f...)
	}
}

func TestCallOfferAcceptAudioBothWays(t *testing.T) {
	ma, mb, feedA, feedB, heardA, heardB, states := callPair(t)
	if err := ma.Call("bob"); err != nil {
		t.Fatal(err)
	}
	// Bob auto-accepts on ring (TUI would prompt; engine path is direct).
	if err := mb.Accept(); err != nil {
		t.Fatalf("accept: %v (state=%v)", err, func() callState { mb.mu.Lock(); defer mb.mu.Unlock(); return mb.state }())
	}
	waitCallState(t, states, "a:live:bob")
	waitCallState(t, states, "b:live:alice")
	feedTone(feedA, 8)
	feedTone(feedB, 8)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if heardA.count() >= 4 && heardB.count() >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audio not flowing both ways: a=%d b=%d; astate=%s amedia=%s; bstate=%s bmedia=%s",
				heardA.count(), heardB.count(), dumpState(ma), dumpMedia(ma, "bob"), dumpState(mb), dumpMedia(mb, "alice"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Energy check: received audio is signal, not silence.
	if heardA.energy() < 1000 || heardB.energy() < 1000 {
		t.Fatal("received audio has no energy (silence looped back?)")
	}
	if err := ma.Hangup(); err != nil {
		t.Fatal(err)
	}
	waitCallState(t, states, "b:idle:")
}

func TestCallDeclineAndBusy(t *testing.T) {
	ma, mb, _, _, _, _, states := callPair(t)
	if err := ma.Call("bob"); err != nil {
		t.Fatal(err)
	}
	waitCallState(t, states, "b:ringing:alice")
	if err := mb.Decline(); err != nil {
		t.Fatal(err)
	}
	waitCallState(t, states, "a:idle:")
}

func TestCallStaleOfferIgnored(t *testing.T) {
	ma, mb, _, _, _, _, _ := callPair(t)
	_ = ma
	mb.onSignalNote(signalNote{From: "alice", Type: callOffer, Payload: `{"ip":"127.0.0.1","port":9,"ts":1}`})
	mb.mu.Lock()
	st := mb.state
	mb.mu.Unlock()
	if st != callIdle {
		t.Fatalf("stale offer changed state to %v", st)
	}
}

func TestCallMuteStopsVoice(t *testing.T) {
	ma, mb, feedA, _, heardA, heardB, states := callPair(t)
	if err := ma.Call("bob"); err != nil {
		t.Fatal(err)
	}
	if err := mb.Accept(); err != nil {
		t.Fatal(err)
	}
	waitCallState(t, states, "a:live:bob")
	waitCallState(t, states, "b:live:alice")
	ma.Mute(true)
	n0 := heardB.count()
	feedTone(feedA, 10)
	time.Sleep(1500 * time.Millisecond)
	if n1 := heardB.count(); n1 != n0 {
		t.Fatalf("muted sender delivered %d frames", n1-n0)
	}
	_ = heardA
	_ = mb
}

func TestCallCommandsRegistered(t *testing.T) {
	want := map[string]bool{"/help": false, "/upload": false, "/download": false, "/call": false, "/accept": false, "/decline": false, "/hangup": false, "/mute": false}
	for _, c := range slashCommands {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("command %s missing from palette", name)
		}
	}
}

func TestCallRingLineAndHeader(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	sc := m.(chatScreen)
	before := len(sc.localLines)
	m2, _ := sc.Update(callRingMsg{peer: "alice"})
	sc = m2.(chatScreen)
	if len(sc.localLines) != before+1 {
		t.Fatal("ringing must announce exactly one line")
	}
	sc.callLevel = 0.5
	if !strings.Contains(sc.headerView(), normVersion(version)) {
		t.Fatal("header lost version segment")
	}
}

func TestRosterMapSkipsGarbage(t *testing.T) {
	peers := []rosterMember{
		{Username: "ok", Pubkey: base64.StdEncoding.EncodeToString(make([]byte, 32)), Online: true},
		{Username: "short", Pubkey: base64.StdEncoding.EncodeToString(make([]byte, 4)), Online: true},
		{Username: "bad", Pubkey: "%%%", Online: true},
		{Username: "", Pubkey: base64.StdEncoding.EncodeToString(make([]byte, 32)), Online: true},
	}
	m := rosterMap(peers)
	if len(m) != 1 || len(m["ok"]) != 32 {
		t.Fatalf("rosterMap wrong: %v", m)
	}
}

func TestVuBarBounds(t *testing.T) {
	if got := vuBar(-1); len(got) == 0 {
		t.Fatal("empty bar")
	}
	if got := vuBar(0); strings.Count(got, "▂") != 0 {
		t.Fatalf("silent bar lit: %q", got)
	}
	if got := vuBar(1); strings.Count(got, "▂") != 8 {
		t.Fatalf("full bar wrong: %q", got)
	}
	if got := vuBar(99); strings.Count(got, "▂") != 8 {
		t.Fatalf("clamp wrong: %q", got)
	}
}

func TestCallNoPeerHint(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	sc := m.(chatScreen)
	sc.callPeer("")
	found := false
	for _, l := range sc.localLines {
		if strings.Contains(l.text, "/call <user>") {
			found = true
		}
	}
	if !found {
		t.Fatal("peerless /call must hint usage")
	}
}

func TestVideoSyntheticLoopback(t *testing.T) {
	ma, mb, _, _, _, _, states := callPair(t)
	if err := ma.Call("bob"); err != nil {
		t.Fatal(err)
	}
	if err := mb.Accept(); err != nil {
		t.Fatal(err)
	}
	waitCallState(t, states, "a:live:bob")
	waitCallState(t, states, "b:live:alice")

	// Synthetic camera: two arbitrary frames (payloader fragments bytes).
	frames := make(chan []byte, 4)
	f1 := make([]byte, 3000)
	for i := range f1 {
		f1[i] = byte(i)
	}
	f2 := make([]byte, 500)
	for i := range f2 {
		f2[i] = byte(255 - i)
	}
	frames <- append([]byte(nil), f1...)
	frames <- append([]byte(nil), f2...)
	ma.mu.Lock()
	ma.videoSrc = func() (<-chan []byte, func(), error) { return frames, func() {}, nil }
	ma.mu.Unlock()
	mb.mu.Lock()
	mb.videoDecFn = func() (frameDecoder, error) {
		return &identityDecoder{}, nil
	}
	mb.mu.Unlock()
	rendered := make(chan []string, 8)
	mb.mu.Lock()
	mb.cb.onVideoFrame = func(lines []string) { rendered <- lines }
	mb.mu.Unlock()
	if err := ma.StartVideo(); err != nil {
		t.Fatal(err)
	}
	select {
	case lines := <-rendered:
		if len(lines) == 0 {
			t.Fatal("empty render")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no video frame rendered loopback")
	}
	ma.StopVideo()
	ma.mu.Lock()
	on := ma.videoOn
	ma.mu.Unlock()
	if on {
		t.Fatal("StopVideo must clear streaming flag")
	}
}

type identityDecoder struct{}

func (identityDecoder) decode(frame []byte) ([]byte, error) {
	_ = frame
	rgb := make([]byte, videoWidth*videoHeight*3)
	for i := 0; i < len(rgb); i += 3 {
		rgb[i], rgb[i+1], rgb[i+2] = 128, 128, 128
	}
	return rgb, nil
}

func (identityDecoder) close() {}

func TestVideoSidebarSplitAndScroll(t *testing.T) {
	c := newFilterScreen("bob", "", "carol")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	sc.call = &callManager{videoOn: true}
	lines := make([]string, 0, videoPaneRows)
	for i := 0; i < videoPaneRows; i++ {
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
	// Video box present in painted output above the roster title.
	if got := sc.View(); !strings.Contains(got, "VIDEO") || !strings.Contains(got, "ONLINE") {
		t.Fatal("sidebar must show VIDEO above ONLINE")
	}
	// Video box present in painted output above the roster title.
	if got := sc.View(); !strings.Contains(got, "VIDEO") || !strings.Contains(got, "ONLINE") {
		t.Fatal("sidebar must show VIDEO above ONLINE")
	}
	// Video off collapses the split cleanly.
	sc.call = &callManager{videoOn: false}
	l2 := sc.layoutFor()
	if l2.videoRows != 0 || l2.rosterY0 >= l.rosterY0 {
		t.Fatal("video collapse must return rows to the roster")
	}
}

func TestCallDiagLines(t *testing.T) {
	ma, _, _, _, _, _, _ := callPair(t)
	lines := ma.diagLines()
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{"call state=idle", "offered IPs:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("diag missing %q:\n%s", want, joined)
		}
	}
	if err := ma.Call("bob"); err != nil {
		t.Fatal(err)
	}
	joined = ""
	for _, l := range ma.diagLines() {
		joined += l + "\n"
	}
	if !strings.Contains(joined, "roster knows peer:") {
		t.Fatalf("in-call diag must cover roster:\n%s", joined)
	}
	_ = ma.Hangup()
}

func TestMediastatsCommand(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	sc := m.(chatScreen)
	sc.call = &callManager{}
	before := len(sc.localLines)
	sc.runCommand("/mediastats")
	if len(sc.localLines) <= before {
		t.Fatal("/mediastats must print diagnostics")
	}
	last := sc.localLines[len(sc.localLines)-1].text
	_ = last
	found := false
	for _, l := range sc.localLines[before:] {
		if strings.Contains(l.text, "call state=") {
			found = true
		}
	}
	if !found {
		t.Fatal("mediastats output missing call state line")
	}
}

func TestVideoPlaceholderPane(t *testing.T) {
	c := newFilterScreen("bob", "")
	c.vp = *viewportPtr(60, 20)
	m, _ := c.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	sc := m.(chatScreen)
	sc.call = &callManager{videoOn: true}
	l := sc.layoutFor()
	if l.videoRows == 0 {
		t.Fatal("pane must claim rows while video is on, even with no frames yet")
	}
}
