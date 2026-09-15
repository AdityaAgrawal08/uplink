package main

import (
	"fmt"
	"io"
	"math"
	"os/exec"
	"runtime"
	"sync"

	"github.com/tphakala/go-audio-capture"
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

// micCapture abstracts Linux pure-Go capture vs ffmpeg fallback.
type micCapture struct {
	frames chan []int16
	stop   func()
}

func openMic() (*micCapture, error) {
	if runtime.GOOS == "linux" {
		if mc, err := openAlsaMic(); err == nil {
			return mc, nil
		}
	}
	return openFFmpegMic()
}

func openAlsaMic() (*micCapture, error) {
	devs, err := capture.Devices()
	if err != nil || len(devs) == 0 {
		return nil, fmt.Errorf("no capture devices: %v", err)
	}
	tryConfigs := []capture.Config{
		{Device: devs[0].ID, Rate: voiceRate, Channels: 1, Format: capture.FormatS16LE},
		{Device: devs[0].ID, Rate: voiceRate, Channels: 2, Format: capture.FormatS16LE},
		{Device: devs[0].ID, Rate: 44100, Channels: 1, Format: capture.FormatS16LE},
	}
	var stream *capture.Stream
	var stereo, resample bool
	for i, cfg := range tryConfigs {
		s, err := capture.Open(cfg)
		if err != nil {
			continue
		}
		stream, stereo, resample = s, i == 1, i == 2
		break
	}
	if stream == nil {
		return nil, fmt.Errorf("no workable ALSA config")
	}
	if err := stream.Start(); err != nil {
		stream.Close()
		return nil, err
	}
	out := make(chan []int16, 50)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer stream.Close()
		chunker := &frameChunker{}
		buf := make([]byte, 64*1024)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := stream.Read(buf)
			if err != nil {
				return
			}
			samples := bytesToS16(buf[:n])
			if stereo {
				samples = downmixStereo(samples)
			}
			if resample {
				samples = resampleLinear(samples, 44100, voiceRate)
			}
			for _, f := range chunker.push(samples) {
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
	switch runtime.GOOS {
	case "darwin":
		args = []string{"-f", "avfoundation", "-i", ":0", "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
	case "windows":
		args = []string{"-f", "dshow", "-i", "audio=default", "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
	default:
		args = []string{"-f", "alsa", "-i", "default", "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1"}
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
type speaker struct {
	mu  sync.Mutex
	in  io.WriteCloser
	cmd *exec.Cmd
}

func openSpeaker() (*speaker, error) {
	cmd := exec.Command("ffplay", "-nodisp", "-autoexit", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", "pipe:0")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffplay (is ffmpeg installed?): %w", err)
	}
	return &speaker{in: in, cmd: cmd}, nil
}

func (s *speaker) play(pcm []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.in.Write(s16ToBytes(pcm))
}

func (s *speaker) close() {
	s.in.Close()
	s.cmd.Process.Kill()
	_ = s.cmd.Wait()
}
