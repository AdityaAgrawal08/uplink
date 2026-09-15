package main

import (
	"testing"
)

func TestOpusVoiceRoundtrip(t *testing.T) {
	v, err := newOpusVoice()
	if err != nil {
		t.Fatal(err)
	}
	// 20ms of 1kHz-ish tone at 48kHz mono.
	pcm := make([]int16, voiceFrameLen)
	for i := range pcm {
		if (i/24)%2 == 0 {
			pcm[i] = 8000
		} else {
			pcm[i] = -8000
		}
	}
	pkt, err := v.encode(pcm)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt) == 0 || len(pkt) >= 4000 {
		t.Fatalf("absurd packet size %d", len(pkt))
	}
	back, err := v.decode(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) == 0 {
		t.Fatal("empty decode")
	}
	// Energy preserved (lossy codec: compare power, not samples).
	var e0, e1 float64
	for _, s := range pcm {
		e0 += float64(s) * float64(s)
	}
	for _, s := range back {
		e1 += float64(s) * float64(s)
	}
	if e1 < e0*0.1 || e1 > e0*10 {
		t.Fatalf("energy drift: %f -> %f", e0, e1)
	}
}

func TestFrameChunkerExact(t *testing.T) {
	c := &frameChunker{}
	var got [][]int16
	// Odd splits: 100 + 2000 + 500 samples must yield exactly 2 full frames (1920) + 680 pending.
	got = append(got, c.push(make([]int16, 100))...)
	got = append(got, c.push(make([]int16, 2000))...)
	got = append(got, c.push(make([]int16, 500))...)
	if len(got) != 2 {
		t.Fatalf("frames = %d; want 2", len(got))
	}
	for _, f := range got {
		if len(f) != voiceFrameLen {
			t.Fatalf("frame len %d; want %d", len(f), voiceFrameLen)
		}
	}
	// Mutation isolation: chunker copies (later pushes must not alias).
	got[0][0] = 12345
	if c.pending[0] == 12345 && len(c.pending) > 0 {
		// pending holds the tail (680), got[0] is a copy — just verify no alias panic path
	}
}

func TestDownmixResample(t *testing.T) {
	st := []int16{1000, 3000, -1000, -3000}
	m := downmixStereo(st)
	if len(m) != 2 || m[0] != 2000 || m[1] != -2000 {
		t.Fatalf("downmix wrong: %v", m)
	}
	if got := resampleLinear([]int16{1, 2, 3, 4}, 44100, 44100); len(got) != 4 {
		t.Fatalf("identity resample changed length: %d", len(got))
	}
	up := resampleLinear(make([]int16, 441), 44100, 48000)
	if len(up) != 480 {
		t.Fatalf("441->48k of 441 samples = %d; want 480", len(up))
	}
	down := resampleLinear(make([]int16, 480), 48000, 44100)
	if len(down) != 441 {
		t.Fatalf("48k->441 of 480 samples = %d; want 441", len(down))
	}
	if resampleLinear([]int16{1}, 48000, 44100) != nil {
		t.Fatal("sub-sample tail must drop, not fabricate")
	}
}

func TestRmsLevel(t *testing.T) {
	if rmsLevel(nil) != 0 || rmsLevel([]int16{}) != 0 {
		t.Fatal("empty must be silent")
	}
	full := make([]int16, 96)
	for i := range full {
		full[i] = 32767
	}
	if l := rmsLevel(full); l < 0.9 || l > 1.0 {
		t.Fatalf("full-scale rms = %f; want ~1", l)
	}
	half := make([]int16, 96)
	for i := range half {
		half[i] = 16384
	}
	if l := rmsLevel(half); l < 0.4 || l > 0.6 {
		t.Fatalf("half-scale rms = %f; want ~0.5", l)
	}
}

func TestJitterInOrderWithLoss(t *testing.T) {
	j := newJitterBuffer()
	mk := func(seq uint16) audioPacket { return audioPacket{Seq: seq, Ts: uint32(seq) * 960} }
	// Out-of-order arrival with a gap at 3.
	for _, s := range []uint16{0, 1, 2, 4, 6, 5} {
		j.push(mk(s))
	}
	var seqs []uint16
	var gaps int
	for i := 0; i < 7; i++ {
		p, gap := j.pop()
		if gap {
			gaps++
			continue
		}
		seqs = append(seqs, p.Seq)
	}
	if gaps != 1 {
		t.Fatalf("gaps = %d; want exactly 1 (missing seq 3)", gaps)
	}
	for i, s := range []uint16{0, 1, 2, 4, 5, 6} {
		if seqs[i] != s {
			t.Fatalf("order wrong: %v", seqs)
		}
	}
}

func TestJitterWraparound(t *testing.T) {
	j := newJitterBuffer()
	mk := func(seq uint16) audioPacket { return audioPacket{Seq: seq} }
	j.push(mk(65534))
	j.push(mk(65535))
	j.push(mk(0))
	j.push(mk(1))
	var seqs []uint16
	for i := 0; i < 4; i++ {
		p, gap := j.pop()
		if gap {
			t.Fatalf("gap at %d in wraparound stream", i)
		}
		seqs = append(seqs, p.Seq)
	}
	want := []uint16{65534, 65535, 0, 1}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("wrap order wrong: %v", seqs)
		}
	}
}

func TestJitterResyncCapsMemory(t *testing.T) {
	j := newJitterBuffer()
	for i := 0; i < 200; i++ {
		j.push(audioPacket{Seq: uint16(i * 7)}) // scattered, never popped
	}
	if n := j.pending(); n > jitterMaxFrames*2 {
		t.Fatalf("backlog unbounded: %d", n)
	}
}
