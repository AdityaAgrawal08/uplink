package main

import (
	"fmt"
	"strings"
)

// ─── Terminal video rendering (ASCII) ───────────────────────────────────────
//
// One renderer, four quality passes, tuned for a chat sidebar:
//   1. auto-contrast (1–99 percentile stretch) — laptop webcams in rooms
//      produce washed-out mid-grey; without this, 16 text rows of dynamic
//      range are wasted on a grey blob;
//   2. box-filter downsample (area average) — point sampling shimmers;
//   3. braille cells: every cell covers 2×4 source dots (4× the spatial
//      detail of half-blocks at the same cell budget), thresholded on
//      luminance with a Bayer-4 matrix to kill banding;
//   4. two-group color: fg = average color of "on" dots, bg = average of
//      "off" dots — truecolor is kept, only shapes are binarized.
//
// UPLINK_VIDEO_STYLE=half switches cells to ▀ half-blocks (2×2 pixels,
// same color model) for terminals with poor braille fonts.

// videoPaneDefault is the fallback pane size used before the TUI reports
// the real viewport (see SetVideoSize). Kept small: the pane clip-fix is
// dynamic sizing (SetVideoSize), this is only the boot value.
const (
	videoPaneDefaultCols = 40
	videoPaneDefaultRows = 15
)

// brailleMap[lo][hi] covers all 256 combinations of 2 columns × 4 rows:
// bit order = (col, row) packed column-major (Unicode braille pattern).
// bit0=top-left, bit1=mid-left(top gap), bit2=lower-mid-left, bit3=bottom-left,
// bit4=top-right, bit5=mid-right(top gap), bit6=lower-mid-right, bit7=bottom-right.
func brailleCell(left [4]bool, right [4]bool) rune {
	code := 0
	for i := 0; i < 4; i++ {
		if left[i] {
			code |= 1 << uint(i)
		}
		if right[i] {
			code |= 1 << uint(i+4)
		}
	}
	// U+2800 + pattern (empty pattern maps to blank cell, not U+2800,
	// which renders as raised dots in some fonts).
	if code == 0 {
		return ' '
	}
	return rune(0x2800 + code)
}

// bayer4 is a 4×4 ordered-dither matrix scaled to 0..255: thresholds dots
// evenly so flat areas do not band into stripes.
var bayer4 = [4][4]int{
	{0, 128, 32, 160},
	{192, 64, 224, 96},
	{48, 176, 16, 144},
	{240, 112, 208, 80},
}

// autoContrast stretches luminance between its 1st and 99th percentile and
// applies the same gain to every channel (hue-preserving). Single pass;
// returns the adjusted copy (input untouched).
func autoContrast(rgb []byte) []byte {
	n := len(rgb) / 3
	if n == 0 {
		return rgb
	}
	// 256-bucket histogram gives exact-enough percentile bounds for the
	// stretch (1KB, one pass).
	hist := [256]int{}
	for i := 0; i < n; i++ {
		o := i * 3
		l := (int(rgb[o])*299 + int(rgb[o+1])*587 + int(rgb[o+2])*114) / 1000
		hist[l]++
	}
	lo, hi := 0, 255
	acc := 0
	loCut := n / 100
	if loCut == 0 {
		loCut = 1
	}
	for b := 0; b < 256; b++ {
		acc += hist[b]
		if acc >= loCut {
			lo = b
			break
		}
	}
	acc = 0
	for b := 255; b >= 0; b-- {
		acc += hist[b]
		if acc >= loCut {
			hi = b
			break
		}
	}
	if hi-lo < 24 { // near-flat frame: stretching would blow noise up
		return rgb
	}
	out := make([]byte, len(rgb))
	gain := float64(255) / float64(hi-lo)
	for i := 0; i < n; i++ {
		o := i * 3
		for k := 0; k < 3; k++ {
			v := float64(int(rgb[o+k])-lo) * gain
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			out[o+k] = byte(v)
		}
	}
	return out
}

// vidFrame is one camera picture: JPEG (wire bytes) + RGB (render).
type vidFrame struct {
	Jpeg   []byte
	RGB    []byte
	Width  int
	Height int
}

// asciiFrame downsamples RGB24 to a cols×rows braille pane. Every dot is an
// area-average; fg/bg per cell are the mean colors of on/off dot groups.
// Pure, deterministic, test-covered. style: "braille" (default) or "half".
func asciiFrame(rgb []byte, srcW, srcH, cols, rows int, style string) []string {
	if cols <= 0 || rows <= 0 || srcW <= 0 || srcH <= 0 || len(rgb) < srcW*srcH*3 {
		return nil
	}
	rgb = autoContrast(rgb)

	// Box filter: mean of the source rect each dot covers (1×2 for
	// braille at 4:3, 1×1-ish for half). Precompute column x-ranges once.
	type rect struct{ x0, x1, y0, y1 int }
	// Braille: 2 dots per cell horizontally, 4 vertically.
	dotsW, dotsH := cols*2, rows*4
	if style == "half" {
		dotsW, dotsH = cols, rows*2
	}
	colRects := make([]rect, dotsW)
	for dx := 0; dx < dotsW; dx++ {
		colRects[dx] = rect{dx * srcW / dotsW, (dx + 1) * srcW / dotsW, 0, 0}
		if colRects[dx].x1 <= colRects[dx].x0 {
			colRects[dx].x1 = colRects[dx].x0 + 1
		}
		if colRects[dx].x1 > srcW {
			colRects[dx].x1 = srcW
		}
	}
	// Row luminance+color accumulators, computed once per dot row.
	type dotStat struct {
		lum     float64
		r, g, b float64
	}
	rowRects := make([]rect, dotsH)
	for dy := 0; dy < dotsH; dy++ {
		rowRects[dy] = rect{0, 0, dy * srcH / dotsH, (dy + 1) * srcH / dotsH}
		if rowRects[dy].y1 <= rowRects[dy].y0 {
			rowRects[dy].y1 = rowRects[dy].y0 + 1
		}
		if rowRects[dy].y1 > srcH {
			rowRects[dy].y1 = srcH
		}
	}
	dots := make([]dotStat, dotsW*dotsH)
	for dy := 0; dy < dotsH; dy++ {
		rr := rowRects[dy]
		h := rr.y1 - rr.y0
		if h <= 0 {
			continue
		}
		for dx := 0; dx < dotsW; dx++ {
			cr := colRects[dx]
			w := cr.x1 - cr.x0
			if w <= 0 {
				continue
			}
			var sr, sg, sb float64
			for y := rr.y0; y < rr.y1; y++ {
				base := (y*srcW + cr.x0) * 3
				for x := 0; x < w; x++ {
					o := base + x*3
					sr += float64(rgb[o])
					sg += float64(rgb[o+1])
					sb += float64(rgb[o+2])
				}
			}
			inv := 1 / float64(w*h)
			d := &dots[dy*dotsW+dx]
			d.r, d.g, d.b = sr*inv, sg*inv, sb*inv
			d.lum = (d.r*299 + d.g*587 + d.b*114) / 1000
		}
	}

	// Bayer-4 tiled on the dot grid keeps flat areas from banding; the
	// threshold compares each dot's luminance directly at paint time.
	_ = bayer4

	out := make([]string, 0, rows)
	if style == "half" {
		for r := 0; r < rows; r++ {
			var sb strings.Builder
			for c := 0; c < cols; c++ {
				top := dots[(r*2)*dotsW+c]
				bot := dots[(r*2+1)*dotsW+c]
				if top.lum <= 12 && bot.lum <= 12 {
					sb.WriteByte(' ')
					continue
				}
				fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀\x1b[0m",
					int(top.r), int(top.g), int(top.b), int(bot.r), int(bot.g), int(bot.b))
			}
			out = append(out, sb.String())
		}
		return out
	}

	// Braille: per cell, threshold 2×4 dots; fg = mean of lit dots, bg =
	// mean of unlit dots (skipping an empty group keeps the other color).
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		for c := 0; c < cols; c++ {
			dx0 := c * 2
			var litR, litG, litB, unR, unG, unB float64
			litN, unN := 0, 0
			var left [4]bool
			var right [4]bool
			for dy := 0; dy < 4; dy++ {
				drow := r*4 + dy
				l := dots[drow*dotsW+dx0]
				rr := dots[drow*dotsW+dx0+1]
				on := l.lum > float64(bayer4[dy%4][dx0%4])
				left[dy] = on
				if on {
					litR, litG, litB, litN = litR+l.r, litG+l.g, litB+l.b, litN+1
				} else {
					unR, unG, unB, unN = unR+l.r, unG+l.g, unB+l.b, unN+1
				}
				on = rr.lum > float64(bayer4[dy%4][(dx0+1)%4])
				right[dy] = on
				if on {
					litR, litG, litB, litN = litR+rr.r, litG+rr.g, litB+rr.b, litN+1
				} else {
					unR, unG, unB, unN = unR+rr.r, unG+rr.g, unB+rr.b, unN+1
				}
			}
			cell := brailleCell(left, right)
			if cell == ' ' {
				sb.WriteByte(' ')
				continue
			}
			fg, bg := "255;255;255", "0;0;0"
			if litN > 0 {
				fg = fmt.Sprintf("%d;%d;%d", int(litR/float64(litN)), int(litG/float64(litN)), int(litB/float64(litN)))
			}
			if unN > 0 {
				bg = fmt.Sprintf("%d;%d;%d", int(unR/float64(unN)), int(unG/float64(unN)), int(unB/float64(unN)))
			}
			fmt.Fprintf(&sb, "\x1b[38;2;%sm\x1b[48;2;%sm%c\x1b[0m", fg, bg, cell)
		}
		out = append(out, sb.String())
	}
	return out
}
