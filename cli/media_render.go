package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// ─── Terminal video rendering ───────────────────────────────────────────────
//
// Capability ladder, probed once per session: Kitty graphics (full frames)
// → Sixel → ASCII half-blocks (universal). Only ASCII paints inside the TUI;
// Kitty/Sixel emitters are byte-exact helpers wired to an external viewer
// in a later phase (alt-screen redraws would ghost their placements).

type termVideoCap int

const (
	videoCapASCII termVideoCap = iota
	videoCapSixel
	videoCapKitty
)

// videoPaneCols/Rows size the in-TUI ASCII video pane (drawer slot).
const videoPaneCols = 56
const videoPaneRows = 16

func probeVideoCap() termVideoCap {
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return videoCapKitty
	}
	term := os.Getenv("TERM")
	if strings.Contains(term, "kitty") {
		return videoCapKitty
	}
	for _, s := range []string{"sixel", "mlterm", "foot"} {
		if strings.Contains(term, s) {
			return videoCapSixel
		}
	}
	return videoCapASCII
}

// kittyFrame encodes one RGB24 frame (deflate + base64, o=z transport).
func kittyFrame(rgb []byte, width, height int) string {
	var zb bytes.Buffer
	w := zlib.NewWriter(&zb)
	_, _ = w.Write(rgb)
	_ = w.Close()
	return fmt.Sprintf("\x1b_Gf=24,s=%d,v=%d,o=z;%s\x1b\\",
		width, height, base64.StdEncoding.EncodeToString(zb.Bytes()))
}

func kittyDelete(imageID int) string {
	return fmt.Sprintf("\x1b_Ga=d,i=%d\x1b\\", imageID)
}

// sixelFrame encodes RGB24 with a 3-3-2 palette (no dithering, v1).
// Output: DCS ... ST envelope with one raster line per sixel band.
func sixelFrame(rgb []byte, width, height int) string {
	var sb strings.Builder
	sb.WriteString("\x1bPq\"1;1")
	pal := map[[3]int]int{}
	nextColor := 0
	reg := func(r, g, b int) int {
		key := [3]int{r >> 5, g >> 5, b >> 6}
		if c, ok := pal[key]; ok {
			return c
		}
		c := nextColor
		nextColor++
		pal[key] = c
		fmt.Fprintf(&sb, "#%d;2;%d;%d;%d", c, r*100/255, g*100/255, b*100/255)
		return c
	}
	at := func(x, y int) (int, int, int) {
		o := (y*width + x) * 3
		return int(rgb[o]), int(rgb[o+1]), int(rgb[o+2])
	}
	for band := 0; band*6 < height; band++ {
		for x := 0; x < width; x++ {
			best, bestScore := 0, -1
			var bits [8]bool
			for row := 0; row < 6; row++ {
				y := band*6 + row
				if y >= height {
					break
				}
				r, g, b := at(x, y)
				score := r + g + b
				if score > bestScore {
					bestScore = score
					best = reg(r, g, b)
				}
				_ = row
				bits[row] = true
			}
			var six byte
			for row := 0; row < 6; row++ {
				if bits[row] {
					six |= 1 << uint(row)
				}
			}
			fmt.Fprintf(&sb, "#%d%c", best, six+63)
		}
		sb.WriteString("$\n-" + "\n")
	}
	sb.WriteString("\x1b\\")
	return sb.String()
}

// asciiFrame downsamples RGB24 to cols×rows half-block cells ("▀" with
// fg=top pixel, bg=bottom pixel). Deterministic, pure, test-covered.
func asciiFrame(rgb []byte, srcW, srcH, cols, rows int) []string {
	if cols <= 0 || rows <= 0 || srcW <= 0 || srcH <= 0 || len(rgb) < srcW*srcH*3 {
		return nil
	}
	px := func(x, y int) (int, int, int, int) {
		if x >= srcW {
			x = srcW - 1
		}
		if y >= srcH {
			y = srcH - 1
		}
		o := (y*srcW + x) * 3
		r, g, b := int(rgb[o]), int(rgb[o+1]), int(rgb[o+2])
		return r, g, b, (r*299 + g*587 + b*114) / 1000
	}
	out := make([]string, 0, rows)
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		for c := 0; c < cols; c++ {
			x := c * srcW / cols
			y0 := (r * 2 * srcH) / (rows * 2)
			y1 := ((r*2 + 1) * srcH) / (rows * 2)
			r0, g0, b0, l0 := px(x, y0)
			r1, g1, b1, l1 := px(x, y1)
			if l0 <= 12 && l1 <= 12 {
				sb.WriteByte(' ')
				continue
			}
			fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀\x1b[0m",
				r0, g0, b0, r1, g1, b1)
		}
		out = append(out, sb.String())
	}
	return out
}
