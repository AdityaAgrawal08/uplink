package main

import "sync"

// ─── Adaptive jitter buffer (voice) ─────────────────────────────────────────
//
// Hides network jitter behind a small playout delay: frames queue by
// sequence and release in order on a 20ms tick. Gaps emit a repair request
// (FEC from the next packet, else resync) instead of stalling. Target delay
// adapts between bounds from observed jitter.

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
// when base is missing (caller repairs via FEC). The clock advances either
// way — stalling on loss would wedge playout behind one dropped datagram.
// Frames behind base are orphaned by the advance, reclaimed by the cap.
func (j *jitterBuffer) pop() (audioPacket, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.hasSeq {
		return audioPacket{}, true
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
}

func (j *jitterBuffer) pending() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.buf)
}
