package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/pion/rtp/codecs"
)

func synthIVF(t *testing.T, frames ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	hdr := make([]byte, 32)
	copy(hdr[0:4], "DKIF")
	copy(hdr[8:12], "VP80")
	buf.Write(hdr)
	for _, f := range frames {
		fh := make([]byte, 12)
		fh[0] = byte(len(f))
		fh[1] = byte(len(f) >> 8)
		fh[2] = byte(len(f) >> 16)
		fh[3] = byte(len(f) >> 24)
		buf.Write(fh)
		buf.Write(f)
	}
	return buf.Bytes()
}

func TestIvfReaderWriter(t *testing.T) {
	f1 := []byte{0x10, 0x00, 0x11, 0x22, 0x33}
	f2 := []byte{0x20, 0x01, 0x44, 0x55}
	r := newIvfReader(bufio.NewReader(bytes.NewReader(synthIVF(t, f1, f2))))
	g1, err := r.frame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(g1, f1) {
		t.Fatalf("frame1 wrong: %x", g1)
	}
	g2, err := r.frame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(g2, f2) {
		t.Fatalf("frame2 wrong: %x", g2)
	}
	if _, err := r.frame(); err == nil {
		t.Fatal("EOF must error")
	}
	// Writer emits what the reader parses.
	var wb bytes.Buffer
	w := newIvfWriter(&wb)
	if err := w.writeFrame(f1, 640, 480); err != nil {
		t.Fatal(err)
	}
	if err := w.writeFrame(f2, 640, 480); err != nil {
		t.Fatal(err)
	}
	r2 := newIvfReader(bufio.NewReader(&wb))
	h1, err := r2.frame()
	if err != nil || !bytes.Equal(h1, f1) {
		t.Fatalf("writer/reader mismatch: %x %v", h1, err)
	}
	// Garbage is not IVF.
	rb := newIvfReader(bufio.NewReader(bytes.NewReader([]byte("not-an-ivf-stream-00000000000000"))))
	if _, err := rb.frame(); err == nil {
		t.Fatal("garbage must fail")
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
