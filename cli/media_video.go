package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
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

// testCamera reports whether device selects the synthetic test pattern
// (no camera needed: verifies the whole pipeline end to end).
func testCamera(device string) (w, h, fps int, ok bool) {
	if device != "test" && !strings.HasPrefix(device, "test:") {
		return 0, 0, 0, false
	}
	return videoWidth, videoHeight, videoFPS, true
}

func videoEncodeArgs(device string) []string {
	size := fmt.Sprintf("%dx%d", videoWidth, videoHeight)
	rate := fmt.Sprintf("%d", videoFPS)
	if _, _, _, ok := testCamera(device); ok {
		return []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%s:rate=%s", size, rate),
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "8",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ogg", "pipe:1"}
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{"-f", "avfoundation", "-framerate", rate, "-i", device + ":",
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "8",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ogg", "pipe:1"}
	case "windows":
		return []string{"-f", "dshow", "-i", device,
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "8",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ogg", "pipe:1"}
	default:
		return []string{"-f", "v4l2", "-i", device,
			"-c:v", "libvpx", "-b:v", videoBitrate, "-deadline", "realtime", "-cpu-used", "8",
			"-vf", "scale=" + size, "-r", rate, "-g", "30", "-f", "ogg", "pipe:1"}
	}
}

// oggCRC is the Ogg checksum (CRC-32/ISO-HDLC variant: init 0, no
// reflection, no xor-out). Verified byte-exact against ffmpeg output.
var oggCRCtable [256]uint32

func init() {
	for i := range oggCRCtable {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		oggCRCtable[i] = r
	}
}

func oggCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCtable[byte(crc>>24)^b]
	}
	return crc
}

// oggDemux splits an Ogg VP8 stream into packets (skipping the 2 header
// packets): Ogg pages self-sync, so this streams live — unlike IVF, whose
// demuxer buffers to EOF on pipes.
type oggDemux struct {
	r       *bufio.Reader
	headers int
	carry   []byte // continued packet across pages
}

func newOggDemux(r *bufio.Reader) *oggDemux { return &oggDemux{r: r} }

// packet returns the next complete packet (headers first: identification,
// comment, then VP8 frames).
func (d *oggDemux) packet() ([]byte, error) {
	for {
		page, err := d.page()
		if err != nil {
			return nil, err
		}
		for _, seg := range page {
			d.carry = append(d.carry, seg...)
			if len(seg) < 255 {
				pkt := d.carry
				d.carry = nil
				return pkt, nil
			}
		}
	}
}

// page reads one Ogg page, returning its payload segments.
func (d *oggDemux) page() ([][]byte, error) {
	hdr := make([]byte, 27)
	if _, err := io.ReadFull(d.r, hdr); err != nil {
		return nil, err
	}
	if string(hdr[0:4]) != "OggS" {
		return nil, fmt.Errorf("not an Ogg stream")
	}
	nseg := int(hdr[26])
	if nseg == 0 {
		return nil, fmt.Errorf("empty Ogg page")
	}
	tab := make([]byte, nseg)
	if _, err := io.ReadFull(d.r, tab); err != nil {
		return nil, err
	}
	var segs [][]byte
	for _, n := range tab {
		seg := make([]byte, int(n))
		if _, err := io.ReadFull(d.r, seg); err != nil {
			return nil, err
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// oggMux re-wraps VP8 frames into an Ogg stream for the decoder. Headers
// use fixed templates (identification patched with dimensions); the
// comment block is informational metadata the decoder ignores.
type oggMux struct {
	w        io.Writer
	serial   uint32
	seqno    uint64
	sentHdrs bool
}

func newOggMux(w io.Writer, serial uint32) *oggMux {
	return &oggMux{w: w, serial: serial}
}

var oggCommentPacket = []byte{
	'O', 'V', 'P', '8', 0x30, 0x02, 0x20, 0x0c, 0x00, 0x00, 0x00, 0x4c,
	0x61, 0x76, 0x66, 0x36, 0x33, 0x2e, 0x31, 0x2e, 0x31, 0x30, 0x31, 0x01,
	0x00, 0x00, 0x00, 0x1b, 0x00, 0x00, 0x00, 0x65, 0x6e, 0x63, 0x6f, 0x64,
	0x65, 0x72, 0x3d, 0x4c, 0x61, 0x76, 0x63, 0x36, 0x33, 0x2e, 0x31, 0x2e,
	0x31, 0x30, 0x31, 0x20, 0x6c, 0x69, 0x62, 0x76, 0x70, 0x78,
}

func (m *oggMux) writePage(packet []byte, first, last bool, granule uint64) error {
	var hdr [27]byte
	copy(hdr[0:4], "OggS")
	if first {
		hdr[5] |= 0x02
	}
	if last {
		hdr[5] |= 0x04
	}
	binary.LittleEndian.PutUint64(hdr[6:14], granule)
	binary.LittleEndian.PutUint32(hdr[14:18], m.serial)
	binary.LittleEndian.PutUint32(hdr[18:22], uint32(m.seqno))
	m.seqno++
	// Lacing: 255-byte segments, remainder terminates the packet.
	rest := len(packet)
	nseg := rest/255 + 1
	if nseg > 255 {
		return fmt.Errorf("packet too large for one page")
	}
	hdr[26] = byte(nseg)
	tab := make([]byte, 0, nseg)
	for rest > 0 {
		n := rest
		if n > 255 {
			n = 255
		}
		tab = append(tab, byte(n))
		rest -= n
	}
	page := append(hdr[:], tab...)
	page = append(page, packet...)
	crc := oggCRC(page)
	page[22], page[23], page[24], page[25] = byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24)
	_, err := m.w.Write(page)
	return err
}

func (m *oggMux) writeHeaders(width, height int) error {
	ident := []byte{
		'O', 'V', 'P', '8', 0x30, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00,
		0x00, 0x01,
	}
	binary.BigEndian.PutUint16(ident[8:10], uint16(width))
	binary.BigEndian.PutUint16(ident[10:12], uint16(height))
	if err := m.writePage(ident, true, false, 0); err != nil {
		return err
	}
	return m.writePage(oggCommentPacket, false, false, 0)
}

func (m *oggMux) writeFrame(frame []byte, granule uint64) error {
	return m.writePage(frame, false, false, granule)
}

// vp8KeyFrame reports the VP8 frame type bit (bit 0 clear = keyframe).
func vp8KeyFrame(frame []byte) bool {
	return len(frame) >= 1 && frame[0]&0x01 == 0
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
	cmd := exec.Command("ffmpeg", "-f", "ogg", "-i", "pipe:0",
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
// Streaming design: piped ffmpeg only emits once primed, so frames go in
// one end continuously and pictures come out the other — never
// request/response (which stalls forever on the first frame).
type frameDecoder interface {
	submit(frame []byte) bool // false when full (caller drops, never blocks)
	results() <-chan []byte
	close()
}

// ffmpegDecoder implements frameDecoder via an ffmpeg child process.
type ffmpegDecoder struct {
	dec  *videoDecoder
	mux  *oggMux
	inQ  chan []byte
	outQ chan []byte
	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func newFFmpegDecoder() (*ffmpegDecoder, error) {
	dec, err := startVideoDecoder()
	if err != nil {
		return nil, err
	}
	d := &ffmpegDecoder{
		dec:  dec,
		mux:  newOggMux(dec.in, 0x75706c6b),
		inQ:  make(chan []byte, 8),
		outQ: make(chan []byte, 4),
		done: make(chan struct{}),
	}
	if err := d.mux.writeHeaders(videoWidth, videoHeight); err != nil {
		dec.stop()
		return nil, err
	}
	d.wg.Add(2)
	go d.writePump()
	go d.readPump()
	return d, nil
}

func (d *ffmpegDecoder) writePump() {
	defer d.wg.Done()
	var n uint64
	for {
		select {
		case <-d.done:
			return
		case f, ok := <-d.inQ:
			if !ok {
				return
			}
			n++
			if err := d.mux.writeFrame(f, n); err != nil {
				return
			}
		}
	}
}

func (d *ffmpegDecoder) readPump() {
	defer d.wg.Done()
	for {
		out := make([]byte, videoWidth*videoHeight*3)
		if _, err := io.ReadFull(d.dec.out, out); err != nil {
			return
		}
		// Drop-oldest, never block: a slow UI must shed load, not stall
		// ffmpeg's stdout (a full queue there backpressures the encoder
		// and lag grows without bound).
		select {
		case d.outQ <- out:
		default:
			select {
			case <-d.outQ:
			default:
			}
			select {
			case d.outQ <- out:
			case <-d.done:
				return
			}
		}
	}
}

func (d *ffmpegDecoder) submit(frame []byte) bool {
	select {
	case d.inQ <- frame:
		return true
	case <-d.done:
		return false
	default:
		return false
	}
}

func (d *ffmpegDecoder) results() <-chan []byte { return d.outQ }

func (d *ffmpegDecoder) close() {
	d.once.Do(func() { close(d.done) })
	d.dec.stop() // kill first: unblocks ReadFull/write before wg.Wait
	d.wg.Wait()
}
