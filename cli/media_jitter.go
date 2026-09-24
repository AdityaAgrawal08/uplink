package main

import "sync"

// ─── Adaptive jitter buffer (voice) ─────────────────────────────────────────
//
// Hides network jitter behind a small playout delay: frames queue by
// sequence and release in order on a 20ms tick. Gaps emit a repair request
// (FEC from the next packet, else resync) instead of stalling. Target delay
// is currently fixed (jitterTargetFrames); adaptive tuning from observed
// jitter is future work (see jitterBuffer.target).

const (
	jitterTargetFrames = 3  // 60ms nominal playout delay
	jitterMaxFrames    = 12 // 240ms ceiling; beyond this resync
)

type jitterBuffer struct {
	mu     sync.Mutex
	buf    map[uint16]audioPacket
	base   uint16 // next sequence due for playout
	maxSeq uint16 // highest sequence seen (wraparound-aware)
	hasSeq bool
	primed bool // pre-roll complete: safe to advance the clock
	target int
}

func newJitterBuffer() *jitterBuffer {
	return &jitterBuffer{buf: map[uint16]audioPacket{}, target: jitterTargetFrames}
}

// fwd returns the forward distance b-a mod 2^16 (<0x8000 means b is newer;
// exact at our tiny windows).
func fwd(a, b uint16) uint16 { return b - a }

func (j *jitterBuffer) push(pkt audioPacket) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.hasSeq {
		j.base, j.maxSeq, j.hasSeq = pkt.Seq, pkt.Seq, true
	} else if d := fwd(j.maxSeq, pkt.Seq); d > 0 && d < 0x8000 {
		j.maxSeq = pkt.Seq
	}
	j.buf[pkt.Seq] = pkt
	if len(j.buf) > jitterMaxFrames*2 {
		j.resyncLocked()
	}
}

// pop returns the next due frame: (packet, gap=false) on time, (zero, gap)
// when base is missing (caller repairs via FEC). Pre-roll: the first
// target frames are HELD so the queue primes before playout starts —
// popping immediately on packet #1 would starve the buffer (every pop is
// a gap, in-band FEC never has a reference, and the receiver hears
// nothing at all: exactly the field symptom). The clock advances once we
// start; stalling on loss would wedge playout behind one dropped datagram.
// Frames behind base are orphaned by the advance, reclaimed by the cap.
func (j *jitterBuffer) pop() (audioPacket, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.hasSeq {
		return audioPacket{}, true
	}
	if !j.primed {
		if len(j.buf) < j.target {
			return audioPacket{}, true // priming: hold, clock does NOT advance
		}
		j.primed = true
	}
	// Opportunistic catch-up: real networks idle at exactly the target
	// depth, so a standing queue deeper than target+1 is a clock that fell
	// behind (stall, GC pause, scheduling gap). Skip forward ONE frame per
	// pop instead of resync-dropping the whole backlog at the ceiling —
	// the old behavior played a burst of stale audio as noise. The skip
	// never jumps past the newest packet minus the target window: we
	// re-base to (maxSeq - target), keeping the freshest target frames.
	for len(j.buf) > j.target+1 {
		if fwd(j.base, j.maxSeq) <= uint16(j.target) {
			break // base is already inside the fresh window: stop
		}
		delete(j.buf, j.base)
		j.base++
	}
	pkt, ok := j.buf[j.base]
	delete(j.buf, j.base)
	j.base++
	if !ok {
		return audioPacket{}, true
	}
	// Shrink the standing queue toward target by skipping ahead when the
	// backlog exceeds the ceiling (clock resync, not silent drift).
	if len(j.buf) > jitterMaxFrames {
		j.resyncLocked()
	}
	return pkt, false
}

// resyncLocked keeps the newest target frames and re-bases playout on the
// oldest kept (drops ancient backlog after floods or clock jumps).
func (j *jitterBuffer) resyncLocked() {
	if len(j.buf) == 0 {
		j.hasSeq = false
		return
	}
	keep := len(j.buf)
	if keep > j.target {
		keep = j.target
	}
	for seq := range j.buf {
		if fwd(seq, j.maxSeq) >= uint16(keep) {
			delete(j.buf, seq)
		}
	}
	if len(j.buf) == 0 {
		j.hasSeq = false
		return
	}
	j.base = j.maxSeq - uint16(keep) + 1
	j.hasSeq = true
	j.primed = false
}

func (j *jitterBuffer) pending() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.buf)
}
