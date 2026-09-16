package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
)

func TestOggDemuxSplitsPackets(t *testing.T) {
	// Synthetic stream: 2 header packets, 1 small video packet, 1 video
	// packet spanning pages via 255-continued lacing, then EOF.
	var stream bytes.Buffer
	writeRawPage := func(bodyParts ...[]byte) {
		var tab []byte
		for _, p := range bodyParts {
			for left := len(p); left > 0; {
				n := left
				if n > 255 {
					n = 255
				}
				tab = append(tab, byte(n))
				left -= n
			}
			if len(p)%255 == 0 {
				tab = append(tab, 0) // terminator lacing value
			}
		}
		hdr := make([]byte, 27)
		copy(hdr[0:4], "OggS")
		hdr[26] = byte(len(tab))
		stream.Write(hdr)
		stream.Write(tab)
		for _, p := range bodyParts {
			stream.Write(p)
		}
	}
	h1 := []byte("OVP8HDR1")
	h2 := []byte("OVP8HDR2-SECOND-PACKET")
	v1 := []byte{0x10, 0x00, 0x11, 0x22}
	big := make([]byte, 700)
	for i := range big {
		big[i] = byte(i)
	}
	writeRawPage(h1)
	writeRawPage(h2)
	writeRawPage(v1)
	writeRawPage(big)

	d := newOggDemux(bufio.NewReader(&stream))
	got := [][]byte{}
	for {
		p, err := d.packet()
		if err != nil {
			break
		}
		got = append(got, p)
	}
	if len(got) != 4 || !bytes.Equal(got[0], h1) || !bytes.Equal(got[1], h2) ||
		!bytes.Equal(got[2], v1) || !bytes.Equal(got[3], big) {
		t.Fatalf("demux wrong: %d packets", len(got))
	}
	// Garbage is not Ogg.
	rb := newOggDemux(bufio.NewReader(bytes.NewReader([]byte("not-an-ogg-stream-0000000000"))))
	if _, err := rb.packet(); err == nil {
		t.Fatal("garbage must fail")
	}
}

func TestOggMuxRoundtrip(t *testing.T) {
	var wb bytes.Buffer
	m := newOggMux(&wb, 0x12345678)
	if err := m.writeHeaders(640, 480); err != nil {
		t.Fatal(err)
	}
	f1 := []byte{0x10, 0x00, 0xaa, 0xbb}
	f2 := make([]byte, 5000)
	for i := range f2 {
		f2[i] = byte(i * 7)
	}
	if err := m.writeFrame(f1, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.writeFrame(f2, 2); err != nil {
		t.Fatal(err)
	}
	// Re-demux: 2 headers + 2 frames, CRC-validated implicitly by parse.
	d := newOggDemux(bufio.NewReader(&wb))
	var packets [][]byte
	for {
		p, err := d.packet()
		if err != nil {
			break
		}
		packets = append(packets, p)
	}
	if len(packets) != 4 {
		t.Fatalf("remux gave %d packets; want 4", len(packets))
	}
	if !bytes.Equal(packets[2], f1) || !bytes.Equal(packets[3], f2) {
		t.Fatal("frame payloads corrupted through mux roundtrip")
	}
	if string(packets[0][0:4]) != "OVP8" {
		t.Fatal("identification header malformed")
	}
}

func TestFragAssembler(t *testing.T) {
	a := newFragAssembler()
	payload := make([]byte, 3000)
	for i := range payload {
		payload[i] = byte(i)
	}
	frags, err := fragVP8(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) < 2 {
		t.Fatalf("expected fragmentation, got %d", len(frags))
	}
	ts := uint32(4242)
	depay := &codecs.VP8Packet{}
	// Deliver reversed with a duplicate: still exactly one frame. Payloads
	// are depacketized first (descriptors stripped), then assembled.
	var got []byte
	for i := len(frags) - 1; i >= 0; i-- {
		frame, err := depay.Unmarshal(frags[i])
		if err != nil {
			t.Fatal(err)
		}
		hdr := []byte{0, 7,
			byte(i >> 8), byte(i),
			byte(len(frags) >> 8), byte(len(frags)),
			byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts),
		}
		f, err := decodeVideoFrag(append(hdr, frame...))
		if err != nil {
			t.Fatal(err)
		}
		if out := a.push(f); out != nil {
			got = out
		}
	}
	_, err = depay.Unmarshal(frags[0])
	if err != nil {
		t.Fatal(err)
	}
	dup, _ := decodeVideoFrag([]byte{0, 7, 0, 0,
		byte(len(frags) >> 8), byte(len(frags)),
		byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts),
	})
	if out := a.push(dup); out != nil {
		t.Fatal("duplicate must not re-emit")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembly mismatch: %d vs %d bytes", len(got), len(payload))
	}
	// Absurd totals drop.
	bad, _ := decodeVideoFrag(append([]byte{0, 1, 0, 5, 9, 9, 0, 0, 0, 1}, 0xAA))
	if out := a.push(bad); out != nil {
		t.Fatal("absurd frame must drop")
	}
}

func TestAsciiFrameDeterministic(t *testing.T) {
	w, h := 8, 4
	rgb := make([]byte, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 3
			rgb[o] = uint8(x * 32)
			rgb[o+1] = uint8(y * 64)
			rgb[o+2] = 128
		}
	}
	a := asciiFrame(rgb, w, h, 4, 2)
	b := asciiFrame(rgb, w, h, 4, 2)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("rows wrong: %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("not deterministic")
		}
		if !strings.Contains(a[i], "▀") {
			t.Fatalf("row %d has no half-blocks: %q", i, a[i])
		}
	}
	black := make([]byte, w*h*3)
	rows := asciiFrame(black, w, h, 4, 2)
	for _, r := range rows {
		if strings.Contains(r, "▀") {
			t.Fatalf("black frame must be blank: %q", r)
		}
	}
	if asciiFrame(rgb, w, h, 0, 2) != nil || asciiFrame(nil, w, h, 4, 2) != nil {
		t.Fatal("degenerate inputs must yield nil")
	}
}

func TestKittySixelEnvelopes(t *testing.T) {
	rgb := make([]byte, 4*2*3)
	for i := range rgb {
		rgb[i] = uint8(i * 3)
	}
	k := kittyFrame(rgb, 4, 2)
	if !strings.HasPrefix(k, "\x1b_Gf=24,s=4,v=2,o=z;") || !strings.HasSuffix(k, "\x1b\\") {
		t.Fatalf("kitty envelope wrong: %q", k[:40])
	}
	if !strings.Contains(kittyDelete(7), "a=d,i=7") {
		t.Fatal("kitty delete wrong")
	}
	s := sixelFrame(rgb, 4, 2)
	if !strings.HasPrefix(s, "\x1bPq") || !strings.HasSuffix(s, "\x1b\\") {
		t.Fatal("sixel envelope wrong")
	}
}

func TestProbeVideoCap(t *testing.T) {
	t.Setenv("KITTY_WINDOW_ID", "7")
	if probeVideoCap() != videoCapKitty {
		t.Fatal("kitty env must probe kitty")
	}
	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("TERM", "xterm-256color")
	if probeVideoCap() != videoCapASCII {
		t.Fatal("plain xterm must fall back to ascii")
	}
	t.Setenv("TERM", "foot")
	if probeVideoCap() != videoCapSixel {
		t.Fatal("foot must probe sixel")
	}
}

// Full ffmpeg fidelity: testsrc encode -> IVF parse -> FU-A ->
// reassemble -> decode -> ASCII. Skipped without ffmpeg (CI-safe).
func TestVideoFFmpegEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	t.Setenv("UPLINK_CAMERA", "test")
	enc, err := startVideoEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.stop()
	d := newOggDemux(enc.out)
	// Skip the 2 stream headers; wait for a keyframe like the live path.
	var frame []byte
	for skipped := 0; skipped < 2; skipped++ {
		if _, err := d.packet(); err != nil {
			t.Fatalf("header packet: %v", err)
		}
	}
	kfDeadline := time.Now().Add(15 * time.Second)
	for {
		f, err := d.packet()
		if err != nil {
			t.Fatalf("encoded frame: %v", err)
		}
		if vp8KeyFrame(f) {
			frame = f
			break
		}
		if time.Now().After(kfDeadline) {
			t.Fatal("no keyframe in 15s of test pattern")
		}
	}
	dec, err := newFFmpegDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.close()
	// Streaming: keep feeding frames (decoder only emits once primed);
	// the first keyframe alone never yields output on a pipe.
	go func() {
		_ = frame
		for {
			f, err := d.packet()
			if err != nil {
				return
			}
			if !dec.submit(f) {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	dec.submit(frame)
	var rgb []byte
	select {
	case rgb = <-dec.results():
	case <-time.After(20 * time.Second):
		t.Fatal("no decoded picture after 20s of streaming test pattern")
	}
	if len(rgb) != videoWidth*videoHeight*3 {
		t.Fatalf("rgb size %d; want %d", len(rgb), videoWidth*videoHeight*3)
	}
	lines := asciiFrame(rgb, videoWidth, videoHeight, 40, 12)
	if len(lines) != 12 {
		t.Fatalf("ascii rows %d; want 12", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "▀") {
		t.Fatal("test pattern rendered blank")
	}
}
