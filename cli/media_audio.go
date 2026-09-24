package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tphakala/go-opus/opus"
)

// ─── Voice audio (LAN calls) ────────────────────────────────────────────────
//
// 48kHz mono, 20ms frames (960 samples): capture → chunk → Opus → datagrams
// out; datagrams → jitter → Opus → playback in. Capture prefers the pure-Go
// ALSA path on Linux and falls back to ffmpeg everywhere else (macOS/
// Windows capture backends are not ready upstream). Playback is always
// ffplay over a pipe: mature on every OS, zero cgo.

const (
	voiceRate     = 48000
	voiceChannels = 1
	voiceFrameLen = 960 // 20ms at 48kHz
	voiceBitrate  = 32000
)

// opusVoice wraps one stateful Opus stream (encoder+decoder are NOT safe
// for concurrent use; each call direction owns its own).
type opusVoice struct {
	enc *opus.Encoder
	dec *opus.Decoder
}

func newOpusVoice() (*opusVoice, error) {
	enc, err := opus.NewEncoder(opus.EncoderConfig{
		SampleRate: voiceRate, Channels: voiceChannels, Bitrate: voiceBitrate,
	})
	if err != nil {
		return nil, err
	}
	dec, err := opus.NewDecoder(voiceRate, voiceChannels)
	if err != nil {
		return nil, err
	}
	return &opusVoice{enc: enc, dec: dec}, nil
}

func (o *opusVoice) encode(pcm []int16) ([]byte, error) {
	buf := make([]byte, 4000)
	n, err := o.enc.Encode(pcm, buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}

func (o *opusVoice) decode(pkt []byte) ([]int16, error) {
	out := make([]int16, voiceFrameLen)
	n, err := o.dec.Decode(pkt, out)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

// decodeFEC reconstructs a lost frame from the next packet's in-band data.
func (o *opusVoice) decodeFEC(next []byte) ([]int16, error) {
	out := make([]int16, voiceFrameLen)
	n, err := o.dec.DecodeFEC(next, out)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

// frameChunker accumulates arbitrary PCM byte streams into exact 960-sample
// mono frames (devices deliver whole periods of whatever size they like).
type frameChunker struct {
	pending []int16
}

func (c *frameChunker) push(samples []int16) [][]int16 {
	c.pending = append(c.pending, samples...)
	var out [][]int16
	for len(c.pending) >= voiceFrameLen {
		f := make([]int16, voiceFrameLen)
		copy(f, c.pending[:voiceFrameLen])
		c.pending = append([]int16(nil), c.pending[voiceFrameLen:]...)
		out = append(out, f)
	}
	return out
}

// downmixStereo folds interleaved stereo to mono by averaging.
func downmixStereo(stereo []int16) []int16 {
	mono := make([]int16, 0, len(stereo)/2)
	for i := 0; i+1 < len(stereo); i += 2 {
		mono = append(mono, int16((int32(stereo[i])+int32(stereo[i+1]))/2))
	}
	return mono
}

// resampleLinear converts mono between rates (nearest for voice; devices
// that refuse 48kHz, typically 44.1kHz, still join the call).
func resampleLinear(in []int16, fromRate, toRate int) []int16 {
	if fromRate == toRate {
		return in
	}
	n := int(int64(len(in)) * int64(toRate) / int64(fromRate))
	if n == 0 {
		return nil
	}
	out := make([]int16, n)
	for i := range out {
		src := float64(i) * float64(fromRate) / float64(toRate)
		j := int(src)
		frac := src - float64(j)
		a := float64(in[j])
		b := a
		if j+1 < len(in) {
			b = float64(in[j+1])
		}
		out[i] = int16(a + frac*(b-a))
	}
	return out
}

func bytesToS16(b []byte) []int16 {
	out := make([]int16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		out = append(out, int16(b[i])|int16(b[i+1])<<8)
	}
	return out
}

func s16ToBytes(s []int16) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, v := range s {
		out = append(out, byte(v), byte(v>>8))
	}
	return out
}

// rmsLevel returns 0..1 loudness for the VU meter (perceptual sqrt scale).
func rmsLevel(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, v := range pcm {
		f := float64(v) / 32768
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// micDevice resolves the capture device from UPLINK_MIC:
//   - unset/empty → system default (ALSA enumeration on Linux)
//   - "test"      → synthetic sine (full-chain self-test without a mic)
//   - otherwise   → exact device name for ALSA/ffmpeg
func micDevice() string {
	return os.Getenv("UPLINK_MIC")
}

func testMic() bool {
	d := micDevice()
	return d == "test" || strings.HasPrefix(d, "test:")
}

// micCapture abstracts a capture source (name feeds status lines).
type micCapture struct {
	name   string
	frames chan []int16
	stop   func()
}

// micCandidate is one capture backend tried in order until one actually
// produces frames (opening successfully is NOT enough: HDMI/webcam inputs
// open fine and then read silence forever).
type micCandidate struct {
	name string
	open func() (*micCapture, error)
}

func micCandidates() []micCandidate {
	dev := micDevice()
	var cands []micCandidate
	if testMic() {
		cands = append(cands, micCandidate{"test-sine", openTestMic})
		return cands
	}
	if runtime.GOOS == "linux" {
		cands = append(cands, micCandidate{"pulse:default", openPulseMic})
		cands = append(cands, micCandidate{"alsa:" + firstNonEmpty(dev, "auto"), func() (*micCapture, error) {
			return openAlsaMic(dev)
		}})
	}
	if dev != "" {
		// An explicit device skips defaults: one ffmpeg attempt.
		cands = append(cands, micCandidate{"ffmpeg:" + dev, openFFmpegMic})
	} else {
		cands = append(cands, micCandidate{"ffmpeg:default", openFFmpegMic})
	}
	return cands
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// micFrameTimeout bounds the first-frame wait per candidate: an input that
// opens but produces nothing must not stall the fallback chain.
const micFrameTimeout = 2500 * time.Millisecond

// lastMicSource records the capture backend that was actually selected
// (startMicTx surfaces it in the "mic live" status line).
var lastMicSource atomic.Value // string

// micSilenceFloor is the peak amplitude below which a probe window counts
// as digital silence. A live mic's noise floor (room, fan, hiss) virtually
// always exceeds this; a dead HDMI/monitor input reads exact zeros. Kept
// low so a quiet room never rejects a working mic.
const micSilenceFloor = 64

// openMicResilient tries every capture backend in order and keeps the
// first one that actually yields frames. Emitted name lands in the
// "mic live" status line so dead-device failures are visible.
func openMicResilient() (*micCapture, string, error) {
	var tried []string
	for _, cand := range micCandidates() {
		mc, err := cand.open()
		if err != nil {
			tried = append(tried, cand.name+" ("+err.Error()+")")
			continue
		}
		// Energy-gated probe: opening is not enough (dead inputs open
		// fine and read silence forever). Accept only a candidate whose
		// window peak clears the digital-silence floor.
		peak, ok := probeMicPeak(mc.frames)
		if !ok {
			mc.stop()
			tried = append(tried, cand.name+" (closed)")
			continue
		}
		if peak < micSilenceFloor {
			mc.stop()
			tried = append(tried, cand.name+" (silent)")
			continue
		}
		// Feed frames back: the TX loop consumes from here on.
		fwd := &frameForwarder{in: mc.frames}
		refill := fwd.start()
		return &micCapture{name: cand.name, frames: refill,
			stop: func() { fwd.stop(); mc.stop() }}, cand.name, nil
	}
	return nil, "", fmt.Errorf("no capture device produced audio — tried: %s", strings.Join(tried, "; "))
}

// probeMicPeak drains the probe window and returns its peak amplitude.
// ok=false only when the channel closes with zero frames. Early-accepts
// on a loud frame so a live mic doesn't stall the toggle for the full
// window; quiet-but-alive inputs use the whole window to prove it.
func probeMicPeak(frames <-chan []int16) (peak int, ok bool) {
	deadline := time.After(micFrameTimeout)
	for {
		select {
		case f, alive := <-frames:
			if !alive {
				return peak, peak > 0
			}
			for _, v := range f {
				a := int(v)
				if a < 0 {
					a = -a
				}
				if a > peak {
					peak = a
				}
			}
			if peak >= 512 {
				return peak, true
			}
		case <-deadline:
			return peak, true
		}
	}
}

// frameForwarder relays frames into a fresh channel after the first-frame
// probe consumed one (single close guaranteed).
type frameForwarder struct {
	in   <-chan []int16
	out  chan []int16
	done chan struct{}
	once sync.Once
}

func (f *frameForwarder) start() chan []int16 {
	f.out = make(chan []int16, 64)
	f.done = make(chan struct{})
	go func() {
		defer close(f.out)
		for {
			select {
			case <-f.done:
				return
			case g, ok := <-f.in:
				if !ok {
					return
				}
				select {
				case f.out <- g:
				case <-f.done:
					return
				}
			}
		}
	}()
	return f.out
}

func (f *frameForwarder) stop() { f.once.Do(func() { close(f.done) }) }

// micSource is the manager-facing wrapper: selects the backend and records
// its name for status lines.
func micSource() (<-chan []int16, func(), error) {
	mc, name, err := openMicResilient()
	if err != nil {
		return nil, nil, err
	}
	lastMicSource.Store(name)
	return mc.frames, mc.stop, nil
}

// requireFFmpeg reports a friendly error when ffmpeg is unavailable
// (test-mic synthesis shells out to it).
func requireFFmpeg(what string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found (needed for %s): install it (apt/pacman/brew) and retry", what)
	}
	return nil
}

// openTestMic synthesizes a 440Hz sine at 48kHz mono (UPLINK_MIC=test):
// proves capture → Opus → UDP → jitter → playout end to end with zero
// hardware, and lets a user verify the app path in one tab.
func openTestMic() (*micCapture, error) {
	if err := requireFFmpeg("test mic"); err != nil {
		return nil, err
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d", voiceRate),
		"-f", "s16le", "-ac", "1", "-ar", fmt.Sprint(voiceRate), "pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start test mic: %w", err)
	}
	out := make(chan []int16, 50)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
		chunker := &frameChunker{}
		buf := make([]byte, voiceFrameLen*2*4)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := io.ReadFull(stdout, buf)
			if err != nil {
				return
			}
			for _, f := range chunker.push(bytesToS16(buf[:n])) {
				select {
				case out <- f:
				case <-done:
					return
				}
			}
		}
	}()
	return &micCapture{frames: out, stop: func() { close(done) }}, nil
}

// openPulseMic captures the PipeWire/PulseAudio DEFAULT source via ffmpeg
// (the source the desktop actually routes: USB mic, laptop DMIC — whatever
// the user selected). This runs FIRST on Linux: raw ALSA hw devices bypass
// the sound server and routinely land on a silent/unrouted input (the top
// cause of dead-but-"working" audio in the field).
func openPulseMic() (*micCapture, error) {
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "pulse", "-i", "default",
		"-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
	if dev := micDevice(); dev != "" {
		args[4] = dev
	}
	cmd := exec.Command("ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg pulse mic: %w", err)
	}
	out := make(chan []int16, 50)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
		chunker := &frameChunker{}
		buf := make([]byte, voiceFrameLen*2*4)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := io.ReadFull(stdout, buf)
			if err != nil {
				return
			}
			for _, f := range chunker.push(bytesToS16(buf[:n])) {
				select {
				case out <- f:
				case <-done:
					return
				}
			}
		}
	}()
	return &micCapture{frames: out, stop: func() { close(done) }}, nil
}

func openFFmpegMic() (*micCapture, error) {
	var args []string
	dev := micDevice()
	switch runtime.GOOS {
	case "darwin":
		args = []string{"-f", "avfoundation", "-i", ":" + dev, "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
	case "windows":
		if dev == "" {
			dev = "audio=default"
		}
		args = []string{"-f", "dshow", "-i", dev, "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
	default:
		if dev == "" {
			dev = "default"
		}
		args = []string{"-f", "alsa", "-i", dev, "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
	}
	cmd := exec.Command("ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg mic (is ffmpeg installed?): %w", err)
	}
	out := make(chan []int16, 50)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
		chunker := &frameChunker{}
		buf := make([]byte, voiceFrameLen*2*4)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := io.ReadFull(stdout, buf)
			if err != nil {
				return
			}
			for _, f := range chunker.push(bytesToS16(buf[:n])) {
				select {
				case out <- f:
				case <-done:
					return
				}
			}
		}
	}()
	return &micCapture{frames: out, stop: func() { close(done) }}, nil
}

// speaker plays PCM frames through ffplay (ships with ffmpeg, every OS).
// Writes happen on a dedicated goroutine with drop-oldest semantics: a
// stalled ffplay must shed audio, never stall the 20ms playout ticker
// (a blocking write there silently killed audio in the field).
type speaker struct {
	mu     sync.Mutex
	in     io.WriteCloser
	cmd    *exec.Cmd
	queue  chan []int16
	done   chan struct{}
	once   sync.Once
	closed chan struct{}
	dead   atomic.Bool   // writer hit a write error (device gone)
	onPlay func([]int16) // test hook: observes frames before queueing
}

func speakerArgs() []string {
	return []string{"-nodisp", "-autoexit",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32",
		"-f", "s16le", "-ar", "48000", "-ac", "1", "-i", "pipe:0"}
}

// openPaplay pipes raw PCM through paplay with explicit wire format.
func openPaplay() (*speaker, error) {
	cmd := exec.Command("paplay", "--raw",
		"--format=s16le", "--rate=48000", "--channels=1")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return newSpeaker(in, cmd), nil
}

func openSpeaker() (*speaker, error) {
	// Test hook: UPLINK_SPEAKER_OUT writes raw s16le to a file (loopback
	// verification without a sound device).
	if path := os.Getenv("UPLINK_SPEAKER_OUT"); path != "" {
		return openFileSpeaker(path)
	}
	// paplay first: native PulseAudio/PipeWire playback — low latency, no
	// buffering surprises (ffplay's pipe buffering was a plausible audio
	// killer on Linux desktops). ffplay is the portable fallback.
	if _, err := exec.LookPath("paplay"); err == nil {
		if s, err := openPaplay(); err == nil {
			return s, nil
		}
	}
	cmd := exec.Command("ffplay", speakerArgs()...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffplay (is ffmpeg installed?): %w", err)
	}
	return newSpeaker(in, cmd), nil
}

// openFileSpeaker is the deterministic sink used by tests and the
// UPLINK_SPEAKER_OUT hook: frames append verbatim to a raw PCM file.
func openFileSpeaker(path string) (*speaker, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return newSpeaker(f, nil), nil
}

func newSpeaker(in io.WriteCloser, cmd *exec.Cmd) *speaker {
	s := &speaker{
		in:     in,
		cmd:    cmd,
		queue:  make(chan []int16, 16),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	go s.writer()
	return s
}

func (s *speaker) writer() {
	defer close(s.closed)
	for {
		select {
		case <-s.done:
			return
		case pcm := <-s.queue:
			if _, err := s.in.Write(s16ToBytes(pcm)); err != nil {
				s.dead.Store(true)
				return
			}
		}
	}
}

// Dead reports whether the output device failed (write error).
func (s *speaker) Dead() bool { return s.dead.Load() }

func (s *speaker) play(pcm []int16) {
	// Drop-if-full: gaps beat a stalled playout loop (the ticker must
	// advance or the jitter clock desyncs and audio dies).
	if s.onPlay != nil {
		s.onPlay(pcm)
	}
	select {
	case s.queue <- pcm:
	default:
	}
}

func (s *speaker) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	<-s.closed
	s.in.Close()
	if s.cmd != nil {
		s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
}
