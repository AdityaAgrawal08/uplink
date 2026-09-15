package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/pion/rtp/codecs"
)

// ─── Video (LAN calls, 1:1) ─────────────────────────────────────────────────
//
// Camera → ffmpeg (VP8 encode, IVF) → fragment (FU-A via pion/rtp) → Noise
// datagrams. Reverse: reassemble → ffmpeg (VP8 decode, RGB24) → terminal
// renderer. Keyframes every ~2s (-g 30) bound error propagation without any
// keyframe-request round trip. Env UPLINK_CAMERA overrides the capture
// device; ffmpeg missing fails loud with install help.

const (
	videoWidth   = 640
	videoHeight  = 480
	videoFPS     = 15
	videoBitrate = "500k"
	videoFragMTU = 1200 // room for Noise tag + headers under typical MTU

	videoAssembleMaxFrags = 256
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

func videoEncodeArgs(device string) []string {
	size := fmt.Sprintf("%dx%d", videoWidth, videoHeight)
	rate := fmt.Sprintf("%d", videoFPS)
	switch runtime.GOOS {
	case "darwin":
		return []string{"-f", "avfoundation", "-framerate", rate, "-i", device + ":",
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "5",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ivf", "pipe:1"}
	case "windows":
		return []string{"-f", "dshow", "-i", device,
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "5",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ivf", "pipe:1"}
	default:
		return []string{"-f", "v4l2", "-i", device,
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "5",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ivf", "pipe:1"}
	}
}

// ivfReader parses an IVF stream: one 32B file header, then 12B frame
// headers (size + timestamp) + payload each. A fresh reader per stream.
type ivfReader struct {
	r     *bufio.Reader
	first bool
}

func newIvfReader(r *bufio.Reader) *ivfReader { return &ivfReader{r: r, first: true} }

func (v *ivfReader) frame() ([]byte, error) {
	if v.first {
		v.first = false
		fileHdr := make([]byte, 32)
		if _, err := io.ReadFull(v.r, fileHdr); err != nil {
			return nil, err
		}
		if string(fileHdr[0:4]) != "DKIF" {
			return nil, fmt.Errorf("not an IVF stream")
		}
	}
	fh := make([]byte, 12)
	if _, err := io.ReadFull(v.r, fh); err != nil {
		return nil, err
	}
	size := int(binary.LittleEndian.Uint32(fh[0:4]))
	if size < 4 || size > 8<<20 {
		return nil, fmt.Errorf("absurd IVF frame size %d", size)
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(v.r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// ivfWriter emits one IVF stream (file header once, then frames) for the
// decoder's stdin.
type ivfWriter struct {
	w     io.Writer
	first bool
}

func newIvfWriter(w io.Writer) *ivfWriter { return &ivfWriter{w: w, first: true} }

func (v *ivfWriter) writeFrame(frame []byte, width, height int) error {
	if v.first {
		v.first = false
		hdr := make([]byte, 32)
		copy(hdr[0:4], "DKIF")
		binary.LittleEndian.PutUint16(hdr[4:6], 0)
		binary.LittleEndian.PutUint16(hdr[6:8], 32)
		copy(hdr[8:12], "VP80")
		binary.LittleEndian.PutUint16(hdr[12:14], uint16(width))
		binary.LittleEndian.PutUint16(hdr[14:16], uint16(height))
		binary.LittleEndian.PutUint32(hdr[16:20], 1000000)
		binary.LittleEndian.PutUint32(hdr[20:24], 1)
		if _, err := v.w.Write(hdr); err != nil {
			return err
		}
	}
	fh := make([]byte, 12)
	binary.LittleEndian.PutUint32(fh[0:4], uint32(len(frame)))
	if _, err := v.w.Write(fh); err != nil {
		return err
	}
	_, err := v.w.Write(frame)
	return err
}

// vp8KeyFrame reports the VP8 keyframe bit (first payload byte).
func vp8KeyFrame(frame []byte) bool {
	return len(frame) >= 2 && frame[0] == 0x10 && frame[1] == 0x00
}

// fragVP8 splits one VP8 frame into FU-A payloads via pion/rtp.
func fragVP8(frame []byte) ([][]byte, error) {
	p := &codecs.VP8Payloader{}
	out := p.Payload(videoFragMTU, frame)
	if len(out) == 0 {
		return nil, fmt.Errorf("payloader produced no packets")
	}
	return out, nil
}

// fragAssembler rebuilds frames from frags keyed by timestamp.
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

// push returns the complete frame when the last frag lands (nil otherwise).
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

// videoEncoder spawns ffmpeg camera capture; Frames yields IVF frames.
type videoEncoder struct {
	cmd *exec.Cmd
	out *bufio.Reader
}

func startVideoEncoder() (*videoEncoder, error) {
	if err := requireFFmpeg("video capture"); err != nil {
		return nil, err
	}
	cmd := exec.Command("ffmpeg", videoEncodeArgs(cameraDevice())...)
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

func (v *videoEncoder) stop() {
	v.cmd.Process.Kill()
	_ = v.cmd.Wait()
}

// videoDecoder spawns ffmpeg VP8→RGB24; Frames are w*h*3 bytes each.
type videoDecoder struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func startVideoDecoder() (*videoDecoder, error) {
	if err := requireFFmpeg("video decode"); err != nil {
		return nil, err
	}
	size := fmt.Sprintf("%dx%d", videoWidth, videoHeight)
	cmd := exec.Command("ffmpeg", "-f", "ivf", "-i", "pipe:0",
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-s", size, "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &videoDecoder{cmd: cmd, in: stdin,
		out: bufio.NewReaderSize(stdout, videoWidth*videoHeight*3+1024)}, nil
}

func (v *videoDecoder) stop() {
	v.in.Close()
	v.cmd.Process.Kill()
	_ = v.cmd.Wait()
}

// frameDecoder turns reassembled VP8 frames into RGB24 videoWidth*videoHeight.
type frameDecoder interface {
	decode(frame []byte) ([]byte, error)
	close()
}

// ffmpegDecoder implements frameDecoder via an ffmpeg child process.
type ffmpegDecoder struct {
	dec *videoDecoder
	w   *ivfWriter
}

func newFFmpegDecoder() (*ffmpegDecoder, error) {
	dec, err := startVideoDecoder()
	if err != nil {
		return nil, err
	}
	return &ffmpegDecoder{dec: dec, w: newIvfWriter(dec.in)}, nil
}

func (d *ffmpegDecoder) decode(frame []byte) ([]byte, error) {
	if err := d.w.writeFrame(frame, videoWidth, videoHeight); err != nil {
		return nil, err
	}
	out := make([]byte, videoWidth*videoHeight*3)
	if _, err := io.ReadFull(d.dec.out, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *ffmpegDecoder) close() {
	d.dec.stop()
}
