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
// full call stack, no server, no hardware.
func callPair(t *testing.T) (a, b *callManager, feedA, feedB chan []int16, heardA, heardB *[][]int16, states chan string) {
	t.Helper()
	ida, _ := generateIdentity()
	idb, _ := generateIdentity()
	rosterA := map[string][]byte{"bob": idb.publicKey()}
	rosterB := map[string][]byte{"alice": ida.publicKey()}
	states = make(chan string, 16)
	var ma, mb *callManager
	mkSink := func(store *[][]int16, mu *sync.Mutex) (func([]int16), func(), error) {
		return func(pcm []int16) {
			mu.Lock()
			*store = append(*store, append([]int16(nil), pcm...))
			mu.Unlock()
		}, func() {}, nil
	}
	var muA, muB sync.Mutex
	var gotA, gotB [][]int16
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
	ma.playSink = func() (func([]int16), func(), error) { return mkSink(&gotA, &muA) }
	mb.playSink = func() (func([]int16), func(), error) { return mkSink(&gotB, &muB) }
	heardA, heardB = &gotA, &gotB
	t.Cleanup(func() {
		_ = ma.Hangup()
		_ = mb.Hangup()
	})
	return ma, mb, feedA, feedB, heardA, heardB, states
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
		if len(*heardA) >= 4 && len(*heardB) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audio not flowing both ways: a=%d b=%d frames", len(*heardA), len(*heardB))
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Energy check: received audio is signal, not silence.
	var energy func([][]int16) float64
	energy = func(frames [][]int16) float64 {
		var e float64
		var n int
		for _, f := range frames {
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
	if energy(*heardA) < 1000 || energy(*heardB) < 1000 {
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
	n0 := len(*heardB)
	feedTone(feedA, 10)
	time.Sleep(1500 * time.Millisecond)
	if n1 := len(*heardB); n1 != n0 {
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
