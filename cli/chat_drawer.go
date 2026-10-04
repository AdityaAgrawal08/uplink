package main

// chat_drawer.go — the craft layer shared by every picker that owns the
// drawer slot above the composer: the "/" command palette, its member-picker
// stage, the "@" mention dropdown, and the file browser. This is the
// OpenCode/fzf-grade restyle: weighted fuzzy ranking with prefix/shortness/
// frecency tiebreaks, a full-row cursor bar, per-keystroke matched-char
// highlight, grouped headers that flatten while filtering, a header/footer
// contract derived from the real keymap, empty states that never flash,
// 80x24-safe geometry caps, and mouse parity (click selects, hover follows,
// wheel steps, Esc/Ctrl+C dismiss).
//
// PURE PRESENTATION + RANK MATH: nothing here touches the network, the
// engine, or any backend state.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
)

// ---- geometry caps ----------------------------------------------------------
//
// The drawer is 80x24-safe by construction: its content window never
// exceeds min(rows, termH/2-6, 10) rows and its width never exceeds
// min(termW-2, 80) — the terminal edge minus two cells, or the OpenCode cap.

// drawerMaxRows is the content-window cap for the drawer slot: at most 10
// visible rows (spec), scaled down to half the terminal minus chrome so the
// drawer can never eat the whole screen. Never below 1 (the empty state
// still paints its single row).
func drawerMaxRows(termH int) int {
	w := termH/2 - 6
	if w > 10 {
		w = 10
	}
	if w < 1 {
		w = 1
	}
	return w
}

// drawerMaxW caps the drawer panel width: the composer column is the natural
// width, but never past the terminal edge minus two cells and never past 80
// (the OpenCode palette cap). Floor at 6 so the chrome always fits.
func drawerMaxW(termW, colW int) int {
	w := colW
	if termW-2 < w {
		w = termW - 2
	}
	if w > 80 {
		w = 80
	}
	if w < 6 {
		w = 6
	}
	return w
}

// ---- frecency ---------------------------------------------------------------

// frecEntry is one item's usage history for the frecency tiebreak.
type frecEntry struct {
	freq int
	last time.Time
}

// frecencyScore decays a pick history: every pick counts, and the whole
// history halves once a day. Pure function => unit-testable with a fixed
// now; a zero time disables the history (no ties broken).
func frecencyScore(e frecEntry, now time.Time) float64 {
	if e.freq <= 0 || now.IsZero() {
		return 0
	}
	age := now.Sub(e.last)
	if age < 0 {
		age = 0
	}
	decay := math.Pow(2, -age.Hours()/24)
	return float64(1+e.freq) * decay
}

// frecFor resolves the frecency score for a key (0 when no history exists).
func frecFor(m map[string]frecEntry, key string, now time.Time) float64 {
	if m == nil {
		return 0
	}
	return frecencyScore(m[key], now)
}

// bumpFrec records one pick of an item (freq+1, last=now). The map is
// created lazily so every constructor path (tests included) stays safe.
func (c *chatScreen) bumpFrec(key string) {
	if c.frec == nil {
		c.frec = map[string]frecEntry{}
	}
	e := c.frec[key]
	e.freq++
	e.last = time.Now()
	c.frec[key] = e
}

// ---- fuzzy ranking ----------------------------------------------------------

// fuzzyHit is the outcome of scoring ONE item against a query: the weighted
// score plus the byte offsets of the matched runes inside the title (the
// fzf-style highlight data, precomputed once per filter keystroke).
type fuzzyHit struct {
	q           int   // weighted score (title 2x + group 1x; higher wins)
	matches     []int // byte offsets of matched runes into the title
	prefixTitle bool  // query is a case-insensitive prefix of the title
	titleLen    int   // rune length of the title (shorter wins ties)
	matched     bool  // false when neither title nor group matched the query
}

// rankTitleGroup scores one item the OpenCode way: the TITLE carries twice
// the weight of the GROUP/category, so a name match always beats a category
// match. Items whose title AND group both fail to contain the full query do
// not match at all (fzf behaviour: non-matches vanish from the list and the
// drawer reaches its "No results found" state). An empty query matches
// everybody with a zero score (the caller keeps its natural order).
func rankTitleGroup(query, title, group string) fuzzyHit {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return fuzzyHit{matched: true}
	}
	var titleMatch *fuzzy.Match
	var groupMatch *fuzzy.Match
	ms := fuzzy.Find(q, []string{title, group})
	for i := range ms {
		if ms[i].Index == 0 {
			titleMatch = &ms[i]
		} else {
			groupMatch = &ms[i]
		}
	}
	h := fuzzyHit{}
	if titleMatch != nil {
		h.q = 2 * titleMatch.Score
		h.matches = titleMatch.MatchedIndexes
		h.prefixTitle = strings.HasPrefix(strings.ToLower(title), q)
		h.titleLen = len([]rune(title))
		h.matched = true
	}
	if groupMatch != nil {
		h.q += groupMatch.Score // group weight 1x
		h.matched = true
	}
	return h
}

// lessRanked is the ranking contract shared by every list picker: weighted
// score desc, then title-prefix, then shorter title, then frecency, then
// stable (original) order — never reordering equal items.
func lessRanked(a, b fuzzyHit, frecA, frecB float64) bool {
	if a.q != b.q {
		return a.q > b.q
	}
	if a.prefixTitle != b.prefixTitle {
		return a.prefixTitle
	}
	if a.titleLen != b.titleLen {
		return a.titleLen < b.titleLen
	}
	if frecA != frecB {
		return frecA > frecB
	}
	return false
}

// rankFuzzyList sorts items by their weighted fuzzy hits (higher first),
// tie-broken by prefix, shortness and frecency, stably. Items that did not
// match the (non-empty) query vanish — that is what makes the drawer reach
// its "No results found" state. hitFor takes the ORIGINAL item index and
// frecOf the same; both are pure so the rank is unit-testable. An empty
// query hits everything with a zero score and the list passes through
// untouched (the caller groups it instead of ranking).
func rankFuzzyList[T any](items []T, hitFor func(i int) fuzzyHit, frecOf func(i int) float64) []T {
	if len(items) == 0 {
		return items
	}
	keep := make([]T, 0, len(items))
	hits := make([]fuzzyHit, 0, len(items))
	src := make([]int, 0, len(items)) // original index of each kept item
	for i := range items {
		h := hitFor(i)
		if !h.matched {
			continue // non-match: gone (fzf semantics)
		}
		keep = append(keep, items[i])
		hits = append(hits, h)
		src = append(src, i)
	}
	ranked := false
	for _, h := range hits {
		if h.q != 0 || h.prefixTitle || len(h.matches) > 0 {
			ranked = true
			break
		}
	}
	if !ranked {
		return keep // empty query: natural order
	}
	perm := make([]int, len(keep))
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(i, j int) bool {
		return lessRanked(hits[perm[i]], hits[perm[j]], frecOf(src[perm[i]]), frecOf(src[perm[j]]))
	})
	out := make([]T, len(keep))
	for i, pi := range perm {
		out[i] = keep[pi]
	}
	return out
}

// ---- the paint plan ---------------------------------------------------------

// drawerRowKind enumerates every row a drawer panel can paint. The panel is
// an INLINE strip above the composer: a single top border line, a header
// (title left, esc right), the windowed rows, an optional overflow marker,
// and a keymap footer — no box, no shadows, no rounded card.
type drawerRowKind int

const (
	drBorder   drawerRowKind = iota // the single top border line
	drHeader                        // title row (text = title)
	drBlank                         // blank-line separator between groups
	drItem                          // one selectable row (item = ranked index)
	drOverflow                      // "… +N more"/"… +N above" (n = count)
	drEmpty                         // muted empty-state row (text)
	drFooter                        // keymap hints left + count right
	drTray                          // picker selection tray row (text = label)
	drNotice                        // picker notice row (text = message)
	drRaw                           // pre-rendered content row (picker detail panel)
)

type drawerRow struct {
	kind drawerRowKind
	item int      // ranked item index (drItem)
	text string   // drHeader/drTray/drNotice/drEmpty content; drItem title
	desc string   // drItem secondary text (description / role tag)
	hit  fuzzyHit // drItem precomputed match data (once per filter keystroke)
	n    int      // drOverflow count; drFooter item count
}

// drawerPlan is the full display structure of one list: the items in their
// ranked order, a pos[] lookup mapping every ranked item to its display row
// (group headers and separators shift them), the window size, and the
// precomputed match data (one fuzzy pass per filter keystroke).
type drawerPlan struct {
	rows []drawerRow // full display rows (headers + items + separators)
	pos  []int       // pos[i] = display row of ranked item i (-1 = absent)
	win  int         // window size in display rows
	hits []fuzzyHit  // hits[i] = precomputed match data of ranked item i
}

// hitOf resolves the precomputed match data for a ranked item (nil hits
// paint plain — empty queries and the member stage carry nothing fuzzy).
func (p drawerPlan) hitOf(item int) fuzzyHit {
	if item < 0 || item >= len(p.hits) {
		return fuzzyHit{}
	}
	return p.hits[item]
}

// buildDrawerPlan assembles the display rows for a list picker: grouped
// (bold headers + blank-line separators, NOT rule lines) when grouped is
// true, flat (a single ranked list) when filtering. Groups appear in
// first-seen ranked order; items inside a group keep their ranked order, so
// the display never invents an order the ranker did not produce.
func buildDrawerPlan(items int, grouped bool, groupOf func(i int) string, win int) drawerPlan {
	plan := drawerPlan{pos: make([]int, items), win: win}
	for i := range plan.pos {
		plan.pos[i] = -1
	}
	if items == 0 {
		return plan
	}
	if !grouped || groupOf == nil {
		for i := 0; i < items; i++ {
			plan.pos[i] = len(plan.rows)
			plan.rows = append(plan.rows, drawerRow{kind: drItem, item: i})
		}
		return plan
	}
	var order []string
	inGroup := map[string][]int{}
	for i := 0; i < items; i++ {
		g := groupOf(i)
		if _, ok := inGroup[g]; !ok {
			order = append(order, g)
		}
		inGroup[g] = append(inGroup[g], i)
	}
	for gi, g := range order {
		idxs := inGroup[g]
		if len(idxs) == 0 {
			continue
		}
		if gi > 0 {
			plan.rows = append(plan.rows, drawerRow{kind: drBlank}) // blank separator, never a rule line
		}
		plan.rows = append(plan.rows, drawerRow{kind: drHeader, text: g})
		for _, i := range idxs {
			plan.pos[i] = len(plan.rows)
			plan.rows = append(plan.rows, drawerRow{kind: drItem, item: i})
		}
	}
	return plan
}

// windowDrawerPlan trims a full plan to the visible window: the window
// starts at the display row of the first visible item (off), pulled back one
// row when that would orphan the item's group header, and spans win rows
// (headers and separators count against the cap). Trailing headers or
// separators are clipped so the window never ends on bare chrome. Returns
// the windowed rows plus the plan rows remaining below / above it (for the
// overflow marker). Pure: paint, budget and the mouse hit-test all share it,
// so the painted height can never drift from the reserved budget.
func windowDrawerPlan(plan drawerPlan, off, sel int) (rows []drawerRow, below, above int) {
	full := plan.rows
	n := len(full)
	win := plan.win
	if win > n {
		win = n
	}
	if win < 1 {
		win = 1
	}
	start := 0
	if off >= 0 && off < len(plan.pos) && plan.pos[off] >= 0 {
		start = plan.pos[off]
		if start > 0 && full[start-1].kind == drHeader {
			start-- // keep the group heading with its first visible item
		}
	}
	if maxStart := n - win; start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}
	// Defensive: typing/keys settle first, but the window must never hide
	// the cursor even if a caller forgot. Self-healing both directions:
	// the window always contains sel's display row.
	if sel >= 0 && sel < len(plan.pos) && plan.pos[sel] >= 0 {
		prow := plan.pos[sel]
		if prow < start {
			start = prow
			if start > 0 && full[start-1].kind == drHeader {
				start--
			}
		}
		if prow >= start+win {
			start = prow - win + 1
			if start < 0 {
				start = 0
			}
			if start > 0 && full[start-1].kind == drHeader {
				start--
			}
		}
	}
	end := start + win
	if end > n {
		end = n
	}
	for end > start && (full[end-1].kind == drHeader || full[end-1].kind == drBlank) {
		end--
	}
	above = start
	below = n - end
	return full[start:end], below, above
}

// settleDrawerWindow re-anchors off (the first visible item) so the window
// keeps sel visible — the plan-aware generalization of followSel: for flat
// plans pos[i]==i and this reduces to the classic minimal-follow. Called by
// the key/mouse handlers after every selection move.
func settleDrawerWindow(sel, off *int, plan drawerPlan) {
	n := len(plan.pos)
	if n <= 0 {
		*sel, *off = 0, 0
		return
	}
	if *sel < 0 {
		*sel = 0
	} else if *sel >= n {
		*sel = n - 1
	}
	if *off < 0 {
		*off = 0
	} else if *off >= n {
		*off = n - 1
	}
	win := plan.win
	if win > len(plan.rows) {
		win = len(plan.rows)
	}
	if win < 1 {
		win = 1
	}
	lo := plan.pos[*sel] - win + 1
	if lo < 0 {
		lo = 0
	}
	best := *sel
	for i := 0; i < n; i++ {
		pi := plan.pos[i]
		if pi >= lo && pi >= 0 && pi < plan.pos[best] {
			best = i
		}
	}
	*off = best
}

// centerDrawerWindow re-anchors off so the window is CENTERED on sel —
// the behaviour for typing resets, Home/End jumps and page moves.
func centerDrawerWindow(sel, off *int, plan drawerPlan) {
	n := len(plan.pos)
	if n <= 0 {
		*sel, *off = 0, 0
		return
	}
	if *sel < 0 {
		*sel = 0
	} else if *sel >= n {
		*sel = n - 1
	}
	win := plan.win
	if win > len(plan.rows) {
		win = len(plan.rows)
	}
	if win < 1 {
		win = 1
	}
	lo := plan.pos[*sel] - win/2
	if lo < 0 {
		lo = 0
	}
	if lo > len(plan.rows)-win {
		lo = len(plan.rows) - win
	}
	best := *sel
	for i := 0; i < n; i++ {
		pi := plan.pos[i]
		if pi >= lo && pi >= 0 && pi < plan.pos[best] {
			best = i
		}
	}
	*off = best
}

// ---- chrome rows ------------------------------------------------------------

// drawerBorderRow is the single top border line (spec: inline drawer with a
// single top border, no box around the list).
func drawerBorderRow(inner int) string {
	return drawerBorderLineStyle.Render(strings.Repeat("─", maxInt(inner, 0)))
}

// drawerHeaderRow paints the header contract: Bold title left, muted esc
// right, truncated to the slot width (never wraps).
func drawerHeaderRow(inner int, title string) string {
	esc := tuiPaletteHintStyle.Render("esc")
	left := maxInt(inner-lipgloss.Width(esc)-1, 1)
	body := tuiPaletteTitleStyle.Render(truncateByWidth(title, left))
	gap := inner - lipgloss.Width(body) - lipgloss.Width(esc)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, body, strings.Repeat(" ", gap), esc)
}

// drawerFooterRow paints the footer contract: keymap hints left (derived
// from the live key handlers, truncated to the slot), count right
// ("3/10" — omitted for empty/single-item lists).
func drawerFooterRow(inner int, hints string, sel, n int) string {
	count := ""
	if n > 1 {
		count = fmt.Sprintf("%d/%d", sel+1, n)
	}
	left := maxInt(inner-lipgloss.Width(count)-1, 1)
	row := truncateByWidth(hints, left)
	if count != "" {
		row += strings.Repeat(" ", maxInt(inner-lipgloss.Width(row)-lipgloss.Width(count), 1)) + count
	}
	return tuiPaletteHintStyle.Render(padVisible(row, inner))
}

// overflowRowView is the "… +N more"/"… +N above" marker.
func overflowRowView(inner, n int, above bool) string {
	word := "more"
	if above {
		word = "above"
	}
	return tuiPaletteHintStyle.Render(padVisible(fmt.Sprintf("… +%d %s", n, word), inner))
}

// ---- matched-char highlight -------------------------------------------------

// runeLenAt returns the UTF-8 length of the rune starting at byte offset off
// (0 when off is not a rune boundary or past the end).
func runeLenAt(s string, off int) int {
	if off < 0 || off >= len(s) {
		return 0
	}
	_, n := utf8.DecodeRuneInString(s[off:])
	if n <= 0 {
		n = 1
	}
	return n
}

// highlightMatches styles the matched runes of a title fzf-style: accent on
// unselected rows, the cursor bar's bright ink on the selected row. The
// matches arrive PRE-COMPUTED (once per filter keystroke, in the rank pass)
// as byte offsets; contiguous matched runs merge into a single styled span.
// Skipped entirely when the row is narrower than 30 cells (spec).
func highlightMatches(plain string, matches []int, selected bool, inner int) string {
	if len(matches) == 0 || inner < 30 {
		return plain
	}
	st := tuiPaletteMatchStyle
	if selected {
		st = tuiPaletteMatchSelStyle
	}
	var b strings.Builder
	last := 0
	i := 0
	for i < len(matches) {
		off := matches[i]
		if off < last || off > len(plain) {
			i++
			continue
		}
		// merge the contiguous run [off, end)
		end := off
		n := runeLenAt(plain, off)
		for i < len(matches) && matches[i] == end {
			end += n
			i++
			if i < len(matches) {
				n = runeLenAt(plain, matches[i])
				if matches[i] != end {
					break
				}
			} else {
				break
			}
		}
		b.WriteString(plain[last:off])
		b.WriteString(st.Render(plain[off:end]))
		last = end
	}
	b.WriteString(plain[last:])
	return b.String()
}

// ---- mouse parity -----------------------------------------------------------

// drawerYRange returns the terminal rows the drawer panel occupies — the
// lift spacer sits one row above, the quote card (when pinned) directly
// below. Paint and hit-test share this, so a click always maps to the row
// that is actually on screen.
func (c chatScreen) drawerYRange(l layout) (y0, y1 int) {
	top := composerTopRows(l) - l.quoteRows - l.paletteRows + 1
	if top < 0 {
		top = 0
	}
	return top, top + maxInt(l.paletteRows-1, 0)
}

// drawerSelAt maps a terminal row onto the selectable item painted there
// (ranked index for the palette/mention, filtered row for the picker). It
// walks the SAME panel row list the painter emits, so hit-test and paint can
// never drift. ok=false when the row is chrome (border/header/footer).
func (c chatScreen) drawerSelAt(y int, l layout) (item int, ok bool) {
	y0, y1 := c.drawerYRange(l)
	if y < y0 || y >= y1 {
		return 0, false
	}
	rows := c.drawerPanelRows()
	row := y - y0
	if row < 0 || row >= len(rows) {
		return 0, false
	}
	r := rows[row]
	if r.kind != drItem {
		return 0, false
	}
	return r.item, true
}

// drawerSelectItem selects one item in whichever mode owns the drawer slot:
// the palette/mention cursor or the picker cursor (a click never activates —
// Enter/Tab stay the keyboard's job, and every action here is
// keyboard-reachable).
func (c *chatScreen) drawerSelectItem(item int, l layout) {
	switch {
	case c.picker.isActive():
		c.picker.anchor = -1
		c.picker.visual = false
		p := &c.picker
		p.cursor = item
		p.syncOffset()
		c.settleActiveDrawer()
	case c.palette.visible():
		c.palette.sel = item
		c.settleActiveDrawer()
	case c.mention.visible():
		c.mention.sel = item
		c.settleActiveDrawer()
	}
}

// drawerStep moves the selection by d (wheel parity with the arrow keys:
// same step, same wrap, no debounce — cursor moves are never deferred).
func (c *chatScreen) drawerStep(d int) {
	switch {
	case c.picker.isActive():
		c.picker.moveTo(c.picker.cursor+d, false)
	case c.palette.visible():
		if d < 0 {
			c.palette.moveUp(len(c.rankedCommands(c.input.Value())))
		} else {
			c.palette.moveDown(len(c.rankedCommands(c.input.Value())))
		}
	case c.mention.visible():
		_, users, ok := c.mentionCandidates()
		if !ok {
			return
		}
		if d < 0 {
			c.mention.moveUp(len(users))
		} else {
			c.mention.moveDown(len(users))
		}
	}
	c.settleActiveDrawer()
}

// spinnerGlyph returns the current spinner frame (Braille arc) for the
// picker's async-loading header.
func spinnerGlyph(spin int) string {
	const frames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	r := []rune(frames)
	return string(r[spin%len(r)])
}

// truncateMiddle elides the middle of a long row so the tail (the filename,
// the last path segment) stays readable: head…tail, width-aware. Used for
// paths in the picker chrome; titles themselves truncate plainly.
func truncateMiddle(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w < 4 {
		return truncateByWidth(s, w)
	}
	// head takes roughly half minus the ellipsis; keep at least 1 char tail.
	const ell = "…"
	headW := (w - lipgloss.Width(ell)) / 2
	var head []rune
	cur := 0
	for _, r := range s {
		cur += runewidthOf(r)
		if cur <= headW {
			head = append(head, r)
		} else {
			break
		}
	}
	tailW := w - headW - lipgloss.Width(ell)
	cur = 0
	tailRev := make([]rune, 0, tailW)
	for i := len(s) - 1; i >= 0; {
		r, size := utf8.DecodeRuneInString(s[i:])
		i -= size
		cur += runewidthOf(r)
		if cur > tailW {
			break
		}
		tailRev = append(tailRev, r)
	}
	for i, j := 0, len(tailRev)-1; i < j; i, j = i+1, j-1 {
		tailRev[i], tailRev[j] = tailRev[j], tailRev[i]
	}
	return string(head) + ell + string(tailRev)
}

func runewidthOf(r rune) int {
	if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6)) {
		return 2
	}
	return 1
}
