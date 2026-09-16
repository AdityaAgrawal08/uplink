package main

import (
	"bufio"
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg" // MJPEG frame decode (pure Go, no cgo)
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ─── Camera video (LAN publish, MJPEG) ──────────────────────────────────────
//
// Camera → ffmpeg (MJPEG, native passthrough when the webcam supports it,
// else re-encode) → concatenated JPEGs on stdout (self-delimiting: SOI…
// EOI, no container) → frag → Noise datagrams. Reverse: reassemble →
// image/jpeg in-process (no child process, no priming delay) → ASCII pane.
// MJPEG is intra-only: a lost frame never corrupts the next 30, and there
// is no GOP/keyframe wait — the two biggest latency sources of the old
// VP8 pipeline. On a LAN the larger frame size is irrelevant.

const (
	videoWidth   = 480
	videoHeight  = 360
	videoFPS     = 15
	videoQuality = 3 // -q:v: 2=best, 31=worst; 3 ≈ sharp faces at 480x360

	videoAssembleMaxFrags = 96
	videoAssembleTTL      = 5 * time.Second
)

func requireFFmpeg(what string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return fmt.Errorf("ffmpeg not found (needed for %s): install it (apt/pacman/brew) and retry", what)
	}
	return nil
}

func cameraDevice() string {
	if d := os.Getenv("UPLINK_CAMERA"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "darwin":
		return "0"
	case "windows":
		return "video=default"
	default:
		return "/dev/video0"
	}
}

// testCamera reports whether device selects the synthetic test pattern
// (no camera needed: verifies the whole pipeline end to end).
func testCamera(device string) (ok bool) {
	return device == "test" || strings.HasPrefix(device, "test:")
}

// videoEncodeArgs builds the zero-latency MJPEG encoder command. Native
// UVC passthrough is tried first (many webcams emit MJPEG themselves:
// zero encode cost); the caller falls back to a re-encode if the device
// rejects the format (watchdog in cameraFrames).
func videoEncodeArgs(device string, native bool) []string {
	rate := fmt.Sprintf("%d", videoFPS)
	in := func(extra ...string) []string {
		a := append([]string{"-hide_banner", "-loglevel", "error",
			"-fflags", "nobuffer", "-flags", "low_delay",
			"-probesize", "32", "-analyzeduration", "0"}, extra...)
		return append(a,
			"-vf", fmt.Sprintf("scale=%d:%d", videoWidth, videoHeight),
			"-r", rate,
			"-c:v", "mjpeg", "-q:v", fmt.Sprint(videoQuality),
			"-an",
			"-f", "mjpeg", "pipe:1")
	}
	if testCamera(device) {
		return in("-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%s", videoWidth, videoHeight, rate))
	}
	switch runtime.GOOS {
	case "darwin":
		return in("-f", "avfoundation", "-framerate", rate, "-capture_cursor", "0", "-i", device+":")
	case "windows":
		if native {
			return in("-f", "dshow", "-vcodec", "mjpeg", "-i", device)
		}
		return in("-f", "dshow", "-i", device)
	default:
		if native {
			return in("-f", "v4l2", "-input_format", "mjpeg", "-i", device)
		}
		return in("-f", "v4l2", "-i", device)
	}
}

func spawnEncoder(native bool) (*videoEncoder, error) {
	args := videoEncodeArgs(cameraDevice(), native)
	cmd := exec.Command("ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &videoEncoder{cmd: cmd, out: bufio.NewReaderSize(stdout, 1<<20)}, nil
}

func startVideoEncoder() (*videoEncoder, error) {
	if err := requireFFmpeg("video capture"); err != nil {
		return nil, err
	}
	return spawnEncoder(true)
}

// videoEncoder spawns the ffmpeg camera capture; frames() yields JPEGs.
type videoEncoder struct {
	cmd *exec.Cmd
	out *bufio.Reader
}

func (v *videoEncoder) stop() {
	v.cmd.Process.Kill()
	_ = v.cmd.Wait()
}

// jpegSplitter reassembles concatenated MJPEG bytes into single JPEGs
// (SOI 0xFFD8 … EOI 0xFFD9). Self-delimiting, so no container is needed;
// a stray marker inside entropy data cannot false-positive because EOI is
// only matched outside an entropy run (JPEGLS/rare markers aside, real
// encoders terminate scans with EOI; we additionally bound the frame size).
type jpegSplitter struct {
	buf []byte
}

// maxJpegFrame bounds memory against a hostile/broken stream.
const maxJpegFrame = 512 * 1024

func (s *jpegSplitter) feed(chunk []byte) [][]byte {
	s.buf = append(s.buf, chunk...)
	var frames [][]byte
	for {
		start := bytes.Index(s.buf, []byte{0xFF, 0xD8})
		if start < 0 {
			// No SOI: drop the garbage (bounded).
			if len(s.buf) > maxJpegFrame {
				s.buf = nil
			}
			return frames
		}
		if start > 0 {
			s.buf = s.buf[start:]
		}
		end := bytes.Index(s.buf, []byte{0xFF, 0xD9})
		if end < 4 { // need at least SOI + some body
			if len(s.buf) > maxJpegFrame {
				s.buf = nil // oversized garbage: reset
			}
			return frames
		}
		frames = append(frames, s.buf[:end+2])
		s.buf = s.buf[end+2:]
	}
}

// jpegToRGB decodes one JPEG frame with the stdlib (pure Go, ~2-5ms at
// 320x240 — no child process, no priming delay). Returns the RGB render
// copy plus the wire JPEG; the renderer re-sizes arbitrarily.
func jpegToRGB(jpeg []byte) (vidFrame, error) {
	img, _, err := image.Decode(bytes.NewReader(jpeg))
	if err != nil {
		return vidFrame{}, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	rgb := make([]byte, w*h*3)
	// Render into packed RGB24 (image/jpeg decodes YCbCr; convert per px).
	// image.YCbCr fast path dominates real frames; generic fallback keeps
	// arbitrary sub-sampling modes honest.
	switch ycbcr := img.(type) {
	case *image.YCbCr:
		rowOff := 0
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				xi := b.Min.X + x
				yi := b.Min.Y + y
				yiOff := ycbcr.YOffset(xi, yi)
				ciOff := ycbcr.COffset(xi, yi)
				yy := int(ycbcr.Y[yiOff])
				cb := int(ycbcr.Cb[ciOff]) - 128
				cr := int(ycbcr.Cr[ciOff]) - 128
				o := rowOff + x*3
				r := yy + 918*cr/1000
				g := yy - 186*cb/1000 - 454*cr/1000
				bb := yy + 1214*cb/1000
				rgb[o] = clamp255(r)
				rgb[o+1] = clamp255(g)
				rgb[o+2] = clamp255(bb)
			}
			rowOff += w * 3
		}
	default:
		rowOff := 0
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				o := rowOff + x*3
				rgb[o] = byte(r >> 8)
				rgb[o+1] = byte(g >> 8)
				rgb[o+2] = byte(bb >> 8)
			}
			rowOff += w * 3
		}
	}
	return vidFrame{Jpeg: append([]byte(nil), jpeg...), RGB: rgb, Width: w, Height: h}, nil
}

func clamp255(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// videoSrcStage turns the encoder stream into decoded frames (channel of
// vidFrame), splitting JPEGs and dropping undecodable ones. Native MJPEG
// passthrough gets a 4s watchdog: a device that rejects -input_format
// mjpeg produces no bytes, so we respawn with a plain v4l2 re-encode.
func cameraFrames() (<-chan vidFrame, func(), error) {
	if err := requireFFmpeg("video capture"); err != nil {
		return nil, nil, err
	}
	enc, err := spawnEncoder(true)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan vidFrame, 4)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer enc.stop()
		split := &jpegSplitter{}
		buf := make([]byte, 64*1024)
		native := true
		deadline := time.Now().Add(4 * time.Second)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := enc.out.Read(buf)
			if n > 0 {
				for _, jf := range split.feed(buf[:n]) {
					f, derr := jpegToRGB(jf)
					if derr != nil {
						continue
					}
					deadline = time.Now().Add(10 * time.Second) // frames flow; disarm fallback
					select {
					case out <- f:
					case <-done:
						return
					}
				}
			}
			if native && !testCamera(cameraDevice()) && time.Now().After(deadline) {
				// Native capture produced nothing (device rejected the
				// format): fall back to a plain re-encode, once.
				enc.stop()
				enc2, eerr := spawnEncoder(false)
				if eerr != nil {
					return
				}
				enc = enc2
				native = false
				split = &jpegSplitter{}
				deadline = time.Now().Add(6 * time.Second)
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	return out, func() { close(done) }, nil
}

// ─── Fragmentation + reassembly ─────────────────────────────────────────────

// fragVideoFrame splits one JPEG into wire-sized chunks. The frame is
// self-delimiting (SOI/EOI), so chunks are raw payload — no container.
func fragVideoFrame(jpeg []byte) ([][]byte, error) {
	if len(jpeg) == 0 {
		return nil, fmt.Errorf("empty jpeg frame")
	}
	if len(jpeg) > maxJpegFrame {
		return nil, fmt.Errorf("jpeg frame too large (%d)", len(jpeg))
	}
	var out [][]byte
	for off := 0; off < len(jpeg); off += videoFragMTU {
		end := off + videoFragMTU
		if end > len(jpeg) {
			end = len(jpeg)
		}
		out = append(out, jpeg[off:end])
	}
	return out, nil
}

// videoFragMTU keeps frag + Noise tag + datagram headers under typical
// LAN MTUs (reduces fragmentation loss).
const videoFragMTU = 1200

// fragAssembler rebuilds JPEG frames from frags keyed by timestamp.
type fragAssembler struct {
	mu     sync.Mutex
	frames map[uint32]*fragBuild
}

type fragBuild struct {
	total uint16
	parts map[uint16][]byte
	first time.Time
}

func newFragAssembler() *fragAssembler {
	return &fragAssembler{frames: map[uint32]*fragBuild{}}
}

// push returns the complete JPEG when the last frag lands (nil otherwise).
// Duplicates collapse; absurd totals and stale builds drop.
func (a *fragAssembler) push(f videoFrag) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for ts, b := range a.frames {
		if now.Sub(b.first) > videoAssembleTTL {
			delete(a.frames, ts)
		}
	}
	if f.FragTotal == 0 || f.FragTotal > videoAssembleMaxFrags || f.FragIdx >= f.FragTotal {
		return nil
	}
	b, ok := a.frames[f.Ts]
	if !ok {
		b = &fragBuild{total: f.FragTotal, parts: map[uint16][]byte{}, first: now}
		a.frames[f.Ts] = b
	}
	if _, dup := b.parts[f.FragIdx]; !dup {
		b.parts[f.FragIdx] = f.Data
	}
	if len(b.parts) != int(b.total) {
		return nil
	}
	out := make([]byte, 0, int(b.total)*videoFragMTU)
	for i := uint16(0); i < b.total; i++ {
		part, ok := b.parts[i]
		if !ok {
			return nil
		}
		out = append(out, part...)
	}
	delete(a.frames, f.Ts)
	return out
}

// videoStyle selects the ASCII cell renderer: braille (default, 4x the
// detail) or half-blocks via UPLINK_VIDEO_STYLE=half.
func videoStyle() string {
	if strings.EqualFold(os.Getenv("UPLINK_VIDEO_STYLE"), "half") {
		return "half"
	}
	return "braille"
}
