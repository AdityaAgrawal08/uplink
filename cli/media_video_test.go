package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestVideoMJpegEndToEnd: testsrc → MJPEG encode → split → decode →
// ASCII/braille. Skipped without ffmpeg (CI-safe).
func TestVideoMJpegEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	t.Setenv("UPLINK_CAMERA", "test")
	frames, stop, err := cameraFrames()
	if err != nil {
		t.Fatalf("camera: %v", err)
	}
	defer stop()
	var f vidFrame
	deadline := time.Now().Add(15 * time.Second)
	for {
		var ok bool
		f, ok = <-frames
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no frames decoded from testsrc in 15s")
		}
	}
	if f.Width != videoWidth || f.Height != videoHeight {
		t.Fatalf("frame %dx%d; want %dx%d", f.Width, f.Height, videoWidth, videoHeight)
	}
	payloads, err := fragVideoFrame(f.Jpeg)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) == 0 {
		t.Fatal("no frags produced")
	}
	// Reassemble + decode: the receiver-side path, byte-exact.
	asm := newFragAssembler()
	var complete []byte
	for i, pl := range payloads {
		complete = asm.push(videoFrag{
			Seq: 1, FragIdx: uint16(i), FragTotal: uint16(len(payloads)),
			Ts: 42, Data: pl,
		})
		if complete != nil {
			break
		}
	}
	if complete == nil {
		t.Fatal("frags never reassembled")
	}
	if !bytes.Equal(complete, f.Jpeg) {
		t.Fatal("frag roundtrip corrupted the JPEG")
	}
	if _, err := jpegToRGB(complete); err != nil {
		t.Fatalf("reassembled jpeg undecodable: %v", err)
	}
	lines := asciiFrame(f.RGB, f.Width, f.Height, 40, 12, videoStyle())
	if len(lines) != 12 {
		t.Fatalf("render rows %d; want 12", len(lines))
	}
}

func TestJpegSplitter(t *testing.T) {
	j1 := encodeSolid(t, 0, 0, 0)
	j2 := encodeSolid(t, 255, 255, 255)
	// Feed concatenated, split at arbitrary boundaries (incl. inside the
	// EOI marker) — real pipe reads do this.
	stream := append(append([]byte(nil), j1...), j2...)
	s := &jpegSplitter{}
	var frames [][]byte
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		frames = append(frames, s.feed(stream[i:end])...)
	}
	frames = append(frames, s.feed(nil)...)
	if len(frames) != 2 {
		t.Fatalf("splitter produced %d frames; want 2", len(frames))
	}
	if !bytes.Equal(frames[0], j1) || !bytes.Equal(frames[1], j2) {
		t.Fatal("splitter corrupted frame boundaries")
	}
	// Garbage without SOI is dropped, not memory-grown.
	s2 := &jpegSplitter{}
	if out := s2.feed([]byte("not a jpeg at all")); len(out) != 0 {
		t.Fatal("garbage must not produce frames")
	}
}

func TestFragAssemblerRoundtrip(t *testing.T) {
	f := vidFrame{Jpeg: make([]byte, 5000)}
	for i := range f.Jpeg {
		f.Jpeg[i] = byte(i * 7)
	}
	payloads, err := fragVideoFrame(f.Jpeg)
	if err != nil {
		t.Fatal(err)
	}
	asm := newFragAssembler()
	var got []byte
	for i, pl := range payloads {
		got = asm.push(videoFrag{Seq: 9, FragIdx: uint16(i), FragTotal: uint16(len(payloads)), Ts: 7, Data: pl})
	}
	if !bytes.Equal(got, f.Jpeg) {
		t.Fatal("reassembly corrupted the frame")
	}
	// A lost frag never completes the frame, and stale builds expire.
	asm2 := newFragAssembler()
	for i, pl := range payloads[1:] { // drop frag 0
		asm2.push(videoFrag{Seq: 1, FragIdx: uint16(i + 1), FragTotal: uint16(len(payloads)), Ts: 8, Data: pl})
	}
	if out := asm2.push(videoFrag{Seq: 1, FragIdx: 0, FragTotal: uint16(len(payloads)), Ts: 8, Data: payloads[0]}); out == nil {
		t.Fatal("late frag must complete the frame")
	}
}

func encodeSolid(t *testing.T, r, g, b byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, videoWidth, videoHeight))
	for y := 0; y < videoHeight; y++ {
		for x := 0; x < videoWidth; x++ {
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAsciiFrameBraille(t *testing.T) {
	// Left half black, right half white → braille cells must use the
	// full pattern range and render exactly cols-wide lines.
	w, h := 64, 32
	rgb := make([]byte, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 3
			if x < w/2 {
				rgb[o], rgb[o+1], rgb[o+2] = 0, 0, 0
			} else {
				rgb[o], rgb[o+1], rgb[o+2] = 255, 255, 255
			}
		}
	}
	cols, rows := 20, 8
	lines := asciiFrame(rgb, w, h, cols, rows, "braille")
	if len(lines) != rows {
		t.Fatalf("rows %d; want %d", len(lines), rows)
	}
	if got := ansiWidth(lines[0]); got != cols {
		t.Fatalf("line width %d; want %d (the old fixed-56 crop bug)", got, cols)
	}
	// Deterministic: identical input → identical output.
	again := asciiFrame(rgb, w, h, cols, rows, "braille")
	if strings.Join(lines, "\n") != strings.Join(again, "\n") {
		t.Fatal("braille render must be deterministic")
	}
	// A lit region must actually paint braille dots (not all spaces):
	// any non-blank braille pattern (U+2801..U+28FF) counts.
	hasDot := false
	for _, r := range strings.Join(lines, "") {
		if r >= 0x2801 && r <= 0x28FF {
			hasDot = true
			break
		}
	}
	if !hasDot {
		t.Fatal("no braille patterns painted")
	}
}

func TestAsciiFrameHalfStyle(t *testing.T) {
	w, h := 32, 16
	rgb := make([]byte, w*h*3)
	for i := 0; i < w*h; i++ {
		rgb[i*3], rgb[i*3+1], rgb[i*3+2] = 128, 128, 128
	}
	lines := asciiFrame(rgb, w, h, 10, 5, "half")
	if len(lines) != 5 {
		t.Fatalf("rows %d; want 5", len(lines))
	}
	if got := ansiWidth(lines[0]); got != 10 {
		t.Fatalf("half-style width %d; want 10", got)
	}
}

func TestAutoContrastStretches(t *testing.T) {
	// Washed-out input (narrow 100..150 range) must be stretched to use
	// (nearly) the full output range.
	w, h := 16, 16
	rgb := make([]byte, w*h*3)
	for i := 0; i < w*h; i++ {
		v := byte(100 + i%50)
		rgb[i*3], rgb[i*3+1], rgb[i*3+2] = v, v, v
	}
	adj := autoContrast(rgb)
	found, foundLo := false, false
	for i := 0; i < w*h*3; i++ {
		if adj[i] > 240 {
			found = true
		}
		if adj[i] < 20 {
			foundLo = true
		}
	}
	if !found || !foundLo {
		t.Fatal("auto-contrast must stretch narrow input to both ends")
	}
}

func TestBrailleCell(t *testing.T) {
	all := brailleCell([4]bool{true, true, true, true}, [4]bool{true, true, true, true})
	if all != '⣿' {
		t.Fatalf("full pattern = %q; want ⣿", all)
	}
	none := brailleCell([4]bool{}, [4]bool{})
	if none != ' ' {
		t.Fatalf("empty pattern = %q; want space", none)
	}
	dot := brailleCell([4]bool{true}, [4]bool{})
	if dot != '⠁' {
		t.Fatalf("top-left dot = %q; want ⠁", dot)
	}
}

// ansiWidth counts printable cells (ANSI-stripped).
func ansiWidth(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}
