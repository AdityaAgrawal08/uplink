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

// micCapture abstracts Linux pure-Go capture vs ffmpeg fallback.
type micCapture struct {
	frames chan []int16
	stop   func()
}

func openMic() (*micCapture, error) {
	dev := micDevice()
	if testMic() {
		return openTestMic()
	}
	if runtime.GOOS == "linux" {
		if mc, err := openAlsaMic(dev); err == nil {
			return mc, nil
		}
	}
	return openFFmpegMic()
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
		defer cmd.Process.Kill()
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
		defer cmd.Process.Kill()
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
}

func speakerArgs() []string {
	return []string{"-nodisp", "-autoexit",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32",
		"-f", "s16le", "-ar", "48000", "-ac", "1", "-i", "pipe:0"}
}

func openSpeaker() (*speaker, error) {
	// Test hook: UPLINK_SPEAKER_OUT writes raw s16le to a file (loopback
	// verification without a sound device).
	if path := os.Getenv("UPLINK_SPEAKER_OUT"); path != "" {
		return openFileSpeaker(path)
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
				return
			}
		}
	}
}

func (s *speaker) play(pcm []int16) {
	// Drop-if-full: gaps beat a stalled playout loop (the ticker must
	// advance or the jitter clock desyncs and audio dies).
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
