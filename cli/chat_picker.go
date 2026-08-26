package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- file-browser picker -----------------------------------------------------
//
// Selecting "/upload" morphs the command drawer into an ls -a style browser:
// ".." pinned first, directories before files, alphabetical within each group,
// dotfiles included. A selection buffer accumulates paths across directories;
// Ctrl+D hands them all to the upload engine, Esc discards.
//
// NOTE ON CTRL+ENTER: bubbletea v1 cannot distinguish Ctrl+Enter from Enter on
// classic terminals (both arrive as CR/LF), so buffering uses SPACE instead —
// universally delivered — while Enter keeps the folder=browse / file=quick-
// upload split the interaction spec calls for.

const pickerMaxVisible = 8 // entry rows painted before scrolling
const pickerMaxTrayRows = 3

var (
	tuiPickerCrumbStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("62")) // accent breadcrumb

	tuiPickerDirStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("39")) // blue-ish directory names

	tuiPickerBufStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("2")) // green buffered tray rows

	tuiPickerNoticeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("203"))
)

type pickerEntry struct {
	name string
	dir  bool
	size int64
}

// pickerMode selects what the drawer lists: the local file system (upload)
// or the room's shared files (download).
type pickerMode int

const (
	modeBrowse pickerMode = iota
	modeFiles
)

// pickerState is the browser mode of the drawer. All list math goes through
// pure helpers so navigation rules are unit-testable without a filesystem.
type pickerState struct {
	active   bool
	mode     pickerMode
	cwd      string // absolute current directory (modeBrowse)
	home     string // $HOME, for ~/ breadcrumb abbreviation
	entries  []pickerEntry
	files    []sessionFile // modeFiles listing, most recent first
	loading  bool          // modeFiles: waiting on the /files response
	cursor   int
	offset   int      // first visible row in the scroll window
	anchor   int      // range anchor (-1 = no active range)
	visual   bool     // `v` visual mode: plain moves extend the range
	buffered []string // ordered keys: absolute paths OR fileIds
	inBuf    map[string]bool
	notice   string // transient error line ("" = none)
	filter   string // substring filter (active when filtering=true)
	filtering bool  // true while the user is typing a filter query
}

func (p *pickerState) isActive() bool { return p.active }

// openPicker enters browser mode rooted at $HOME.
func (c *chatScreen) openPicker() tea.Cmd {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	c.palette.close() // command drawer hands the slot over
	c.input.SetValue("")
	c.input.Placeholder = ""
	c.picker = pickerState{
		active: true,
		mode:   modeBrowse,
		cwd:    home,
		home:   home,
		anchor: -1,
		inBuf:  map[string]bool{},
	}
	c.loadPickerDir()
	return nil
}

// openFilesDrawer morphs the drawer into the room's shared-files list, most
// recent first. The listing arrives asynchronously (filesListMsg).
func (c *chatScreen) openFilesDrawer() tea.Cmd {
	c.palette.close()
	c.input.SetValue("")
	c.input.Placeholder = ""
	c.picker = pickerState{
		active: true,
		mode:   modeFiles,
		home:   ".",
		anchor: -1,
		inBuf:  map[string]bool{},
		notice: "loading shared files…",
	}
	return c.doFetchAllFiles()
}

// applyFilesList fills the drawer's listing; non-UPLOADED files never show.
func (c *chatScreen) applyFilesList(files []sessionFile) {
	if !c.picker.isActive() || c.picker.mode != modeFiles {
		return
	}
	shown := make([]sessionFile, 0, len(files))
	for _, f := range files {
		if f.Status == "UPLOADED" {
			shown = append(shown, f)
		}
	}
	// Recent first: RFC3339 Z timestamps compare lexicographically.
	for i := 1; i < len(shown); i++ {
		for j := i; j > 0 && shown[j].UploadedAt > shown[j-1].UploadedAt; j-- {
			shown[j], shown[j-1] = shown[j-1], shown[j]
		}
	}
	c.picker.files = shown
	c.picker.loading = false
	c.picker.notice = ""
	if len(shown) == 0 {
		c.picker.notice = "no files shared yet"
	}
	c.picker.clampCursor()
}

// closePicker leaves browser mode; restore becomes the composer placeholder.
func (c *chatScreen) closePicker(restore string) {
	c.picker = pickerState{}
	c.input.Placeholder = restore
}

// loadPickerDir (re)reads cwd into the sorted entry list. Errors surface as
// the transient notice row instead of killing the session.
func (c *chatScreen) loadPickerDir() {
	entries, err := listDir(c.picker.cwd)
	if err != nil {
		c.picker.entries = nil
		c.picker.notice = truncateStringPlain(err.Error(), 40)
		return
	}
	c.picker.entries = entries
	c.picker.notice = ""
	c.picker.clampCursor()
}

// ---- pure navigation helpers -------------------------------------------------

// pickerMatchFilter reports whether the given entry passes the active filter.
func pickerMatchFilter(name, filter string) bool {
	if filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), strings.ToLower(filter))
}

// filteredEntries returns entries matching the active filter (browse mode).
func (p *pickerState) filteredEntries() []pickerEntry {
	if p.filter == "" {
		return p.entries
	}
	var out []pickerEntry
	for _, e := range p.entries {
		if pickerMatchFilter(e.name, p.filter) {
			out = append(out, e)
		}
	}
	return out
}

// filteredFiles returns files matching the active filter (files mode).
func (p *pickerState) filteredFiles() []sessionFile {
	if p.filter == "" {
		return p.files
	}
	var out []sessionFile
	for _, f := range p.files {
		if pickerMatchFilter(f.Filename, p.filter) {
			out = append(out, f)
		}
	}
	return out
}

// sortPickerEntries orders dirs first, then files, alphabetical (case-
// insensitive) inside each group. Stable so equal names keep readdir order.
func sortPickerEntries(entries []pickerEntry) []pickerEntry {
	out := make([]pickerEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.dir != b.dir {
			return a.dir
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	return out
}

// listDir reads dir with ls -a semantics: dotfiles in, "."/".." pseudo
// entries out (the ".." row is synthesized by the renderer). Symlinks resolve
// through Stat so symlinked dirs behave like real ones.
func listDir(dir string) ([]pickerEntry, error) {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]pickerEntry, 0, len(dirents))
	for _, de := range dirents {
		info, err := de.Info()
		if err != nil {
			continue // raced deletion; skip rather than fail the listing
		}
		isDir := info.IsDir()
		size := info.Size()
		if !isDir && info.Mode()&os.ModeSymlink != 0 {
			if resolved, err := os.Stat(filepath.Join(dir, de.Name())); err == nil {
				isDir = resolved.IsDir()
				size = resolved.Size()
			} else {
				continue // broken symlink: nothing to show or upload
			}
		}
		entries = append(entries, pickerEntry{name: de.Name(), dir: isDir, size: size})
	}
	entries = sortPickerEntries(entries)
	return entries, nil
}

// parentDir returns "" when already at the filesystem root.
func parentDir(path string) string {
	parent := filepath.Dir(path)
	if parent == path {
		return ""
	}
	return parent
}

// breadcrumb abbreviates home to ~ for the header row.
func breadcrumb(path, home string) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+string(filepath.Separator))) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// pickerRange normalizes anchor+cursor into inclusive [lo,hi].
func pickerRange(anchor, cursor int) (int, int) {
	if anchor < 0 {
		return cursor, cursor
	}
	if anchor < cursor {
		return anchor, cursor
	}
	return cursor, anchor
}

// humanSize renders byte counts the way chat lines quote them (2.3 MB).
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---- state mutations ----------------------------------------------------------

// rowCount is how many selectable rows exist: browse mode shows the
// synthesized ".." parent row plus directory entries; files mode lists the
// room's UPLOADED shared files. When a filter is active, only matching
// entries are counted.
func (p *pickerState) rowCount() int {
	if p.mode == modeFiles {
		return len(p.filteredFiles())
	}
	n := len(p.filteredEntries())
	if parentDir(p.cwd) != "" {
		n++
	}
	return n
}

// entryAt maps a row index onto its target. key is the buffer identity:
// absolute path (browse) or fileId (files). ok=false for the ".." row.
func (p *pickerState) entryAt(row int) (entry pickerEntry, key string, ok bool) {
	if p.mode == modeFiles {
		ff := p.filteredFiles()
		if row < 0 || row >= len(ff) {
			return pickerEntry{}, "", false
		}
		f := ff[row]
		return pickerEntry{name: f.Filename, size: f.Size}, f.FileId, true
	}
	base := 0
	if parentDir(p.cwd) != "" {
		if row == 0 {
			return pickerEntry{name: "..", dir: true}, parentDir(p.cwd), false
		}
		base = 1
	}
	fe := p.filteredEntries()
	idx := row - base
	if idx < 0 || idx >= len(fe) {
		return pickerEntry{}, "", false
	}
	e := fe[idx]
	return e, filepath.Join(p.cwd, e.name), true
}

func (p *pickerState) clampCursor() {
	if p.cursor < 0 {
		p.cursor = 0
	}
	if max := p.rowCount() - 1; p.cursor > max {
		p.cursor = max
	}
	if p.cursor < 0 { // empty dir
		p.cursor = 0
	}
	p.syncOffset()
}

// syncOffset slides the scroll window so the cursor stays visible.
func (p *pickerState) syncOffset() {
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+pickerMaxVisible {
		p.offset = p.cursor - pickerMaxVisible + 1
	}
}

// moveTo applies a cursor move; extend=true keeps the range anchored.
func (p *pickerState) moveTo(row int, extend bool) {
	if !extend {
		p.anchor = -1
		p.visual = false
	} else if p.anchor < 0 {
		p.anchor = p.cursor // starting a range pins where it began
	}
	if max := p.rowCount() - 1; row > max {
		row = max
	}
	if row < 0 {
		row = 0 // empty dir: nothing selectable but keep geometry valid
	}
	p.cursor = row
	p.syncOffset()
}

// cd navigates into a directory and resets selection geometry.
func (c *chatScreen) pickerCd(path string) {
	c.picker.cwd = path
	c.picker.cursor = 0
	c.picker.offset = 0
	c.picker.anchor = -1
	c.picker.visual = false
	c.loadPickerDir()
}

// pickerToggleBuffer adds/removes the highlighted item(s). With an active
// range the WHOLE range flips in one press; without, only the cursor row.
// Folders are buffered like files (the engine tarballs them).
func (c *chatScreen) pickerToggleBuffer() {
	lo, hi := pickerRange(c.picker.anchor, c.picker.cursor)
	for row := lo; row <= hi; row++ {
		_, key, ok := c.picker.entryAt(row)
		if !ok {
			continue // ".." is not bufferable
		}
		if c.picker.inBuf[key] {
			delete(c.picker.inBuf, key)
			for i, k := range c.picker.buffered {
				if k == key {
					c.picker.buffered = append(c.picker.buffered[:i], c.picker.buffered[i+1:]...)
					break
				}
			}
		} else {
			c.picker.inBuf[key] = true
			c.picker.buffered = append(c.picker.buffered, key)
		}
	}
}

// pickerConfirm hands the buffered set to the active engine and closes:
// browse mode uploads buffered paths; files mode downloads buffered fileIds.
func (c *chatScreen) pickerConfirm() tea.Cmd {
	restore := composerPlaceholder
	if c.picker.mode == modeFiles {
		jobs := make([]dlJob, 0, len(c.picker.buffered))
		for _, id := range c.picker.buffered {
			for _, f := range c.picker.files {
				if f.FileId == id {
					jobs = append(jobs, dlJob{FileId: f.FileId, Filename: f.Filename, Size: f.Size})
					break
				}
			}
		}
		c.closePicker(restore)
		if len(jobs) == 0 {
			return nil
		}
		return c.startDownloads(jobs)
	}

	jobs := make([]uploadJob, 0, len(c.picker.buffered))
	for _, abs := range c.picker.buffered {
		jobs = append(jobs, uploadJob{Path: abs})
	}
	c.closePicker(restore)
	if len(jobs) == 0 {
		return nil
	}
	return c.startUploads(jobs)
}

// ---- key handling --------------------------------------------------------------

// handlePickerKeys intercepts every key while the browser is open. Returns
// handled=true plus an optional action for the caller to schedule.
func (c *chatScreen) handlePickerKeys(msg tea.KeyMsg) (bool, func() tea.Cmd) {
	p := &c.picker

	// Filter mode: capture keystrokes for the filter query.
	if p.filtering {
		switch {
		case msg.Type == tea.KeyEsc:
			p.filter = ""
			p.filtering = false
			p.cursor = 0
			p.offset = 0
			return true, nil
		case msg.Type == tea.KeyEnter:
			p.filtering = false
			p.cursor = 0
			p.offset = 0
			p.clampCursor()
			return true, nil
		case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
			if len(p.filter) > 0 {
				p.filter = p.filter[:len(p.filter)-1]
			} else {
				p.filtering = false
			}
			p.cursor = 0
			p.offset = 0
			return true, nil
		case msg.Type == tea.KeyRunes:
			p.filter += string(msg.Runes)
			p.cursor = 0
			p.offset = 0
			return true, nil
		}
		return true, nil
	}

	switch msg.Type {
	case tea.KeyUp:
		p.moveTo(p.cursor-1, p.visual)
		return true, nil
	case tea.KeyDown:
		p.moveTo(p.cursor+1, p.visual)
		return true, nil
	case tea.KeyShiftUp:
		p.moveTo(p.cursor-1, true)
		p.visual = true
		return true, nil
	case tea.KeyShiftDown:
		p.moveTo(p.cursor+1, true)
		p.visual = true
		return true, nil
	case tea.KeyLeft, tea.KeyBackspace, tea.KeyCtrlH:
		if p.mode == modeBrowse {
			if parent := parentDir(p.cwd); parent != "" {
				c.pickerCd(parent)
			}
		}
		return true, nil
	case tea.KeyEnter:
		entry, key, _ := p.entryAt(p.cursor)
		if p.mode == modeFiles {
			c.pickerToggleBuffer() // quick path: grab this one now
			return true, c.pickerConfirm
		}
		switch {
		case !entry.dir:
			c.pickerToggleBuffer() // quick path: buffer this file…
			return true, c.pickerConfirm
		case key == parentDir(p.cwd):
			c.pickerCd(key) // the ".." row
		default:
			c.pickerCd(key) // browse into the folder
		}
		return true, nil
	case tea.KeyCtrlD:
		return true, c.pickerConfirm
	case tea.KeyEsc:
		restore := composerPlaceholder
		c.closePicker(restore)
		return true, nil
	case tea.KeySpace:
		c.pickerToggleBuffer()
		return true, nil
	}
	// "/" enters filter mode.
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && msg.Runes[0] == '/' {
		p.filtering = true
		p.filter = ""
		p.cursor = 0
		p.offset = 0
		return true, nil
	}
	if msg.Type == tea.KeyRunes && msg.String() == "v" {
		p.visual = !p.visual
		if p.visual {
			p.anchor = p.cursor
		} else {
			p.anchor = -1
		}
		return true, nil
	}
	return false, nil // swallow nothing else; composer stays untouched anyway
}

// ---- rendering -------------------------------------------------------------------

// pickerRows budget: spacer + border + breadcrumb + tray + notice? + entries
// window (+ empty note OR scroll overflow) + footer — must match pickerView's
// painted output exactly so the layout reservation holds.
func (c chatScreen) pickerRows() int {
	tray := len(c.picker.buffered)
	if tray > pickerMaxTrayRows {
		tray = pickerMaxTrayRows + 1 // collapsed tray shows a "+N more" row
	}
	rowCount := c.picker.rowCount()
	rows := 1 /*breadcrumb*/ + tray + min(rowCount, pickerMaxVisible) + 1 /*footer*/
	switch {
	case rowCount == 0:
		rows++ // "(empty …)" note
	case rowCount > pickerMaxVisible:
		rows++ // "… +N more" overflow indicator
	}
	if c.picker.notice != "" {
		rows++
	}
	return 1 + rows + 2 // spacer + panel content + box border
}

func parentRowCount(p *pickerState) int {
	if p.mode == modeBrowse && parentDir(p.cwd) != "" {
		return 1
	}
	return 0
}

// pickerView paints the drawer panel in whichever mode is active; same splice
// contract as paletteView.
func (c chatScreen) pickerView(maxW int) string {
	if !c.picker.isActive() || maxW < 6 {
		return ""
	}
	p := c.picker
	inner := maxW - 2

	fit := func(s string) string {
		if lipgloss.Width(s) > inner {
			return lipgloss.NewStyle().MaxWidth(inner).Render(s)
		}
		return s
	}
	pad := func(s string) string { return padVisible(fit(s), inner) }

	var body []string
	crumb := breadcrumb(p.cwd, p.home)
	if p.mode == modeFiles {
		crumb = fmt.Sprintf("shared files — %d", len(p.filteredFiles()))
	}
	if p.filtering {
		crumb += fmt.Sprintf("  /%s▎", p.filter)
	} else if p.filter != "" {
		crumb += fmt.Sprintf("  /%s", p.filter)
	}
	body = append(body, tuiPickerCrumbStyle.Render(pad(crumb)))

	// Selection tray: checked keys pinned under the breadcrumb.
	shownBuf := p.buffered
	truncated := false
	if len(shownBuf) > pickerMaxTrayRows {
		shownBuf = shownBuf[:pickerMaxTrayRows]
		truncated = true
	}
	for _, key := range shownBuf {
		mark := "✓ "
		if !p.inBuf[key] {
			mark = "· "
		}
		label := key
		if p.mode == modeBrowse {
			label = filepath.Base(key)
		} else {
			label = p.fileLabel(key)
		}
		body = append(body, tuiPickerBufStyle.Render(pad(mark+label)))
	}
	if truncated {
		body = append(body, tuiDimStyle.Render(
			pad(fmt.Sprintf("… +%d more selected", len(p.buffered)-pickerMaxTrayRows))))
	}

	if p.notice != "" {
		style := tuiPickerNoticeStyle
		if p.mode == modeFiles && p.loading {
			style = tuiPaletteHintStyle // loading is not an error
		} else if p.mode == modeFiles && strings.HasPrefix(p.notice, "no files") {
			style = tuiDimStyle
		}
		body = append(body, style.Render(pad("· "+p.notice)))
	}

	hasParent := parentRowAvailable(&p)
	visible := min(p.rowCount(), pickerMaxVisible)
	lo, hi := pickerRange(p.anchor, p.cursor)
	painted := 0
	for row := p.offset; row < p.rowCount() && painted < visible; row++ {
		line := pickerRowView(&p, row, hasParent, lo, hi)
		if line == "" {
			continue
		}
		if row == p.cursor {
			body = append(body, tuiPaletteSelStyle.Render(pad(line)))
		} else {
			body = append(body, pad(line))
		}
		painted++
	}
	if p.rowCount() == 0 && p.notice == "" {
		note := "(empty directory)"
		if p.mode == modeFiles {
			note = "(nothing shared yet)"
		}
		body = append(body, tuiDimStyle.Render(pad(note)))
	}
	if more := p.rowCount() - p.offset - painted; more > 0 {
		body = append(body, tuiDimStyle.Render(pad(fmt.Sprintf("… +%d more", more))))
	}

	hints := pickerFooterHints
	if p.mode == modeFiles {
		hints = pickerDlFooterHints
	}
	body = append(body, tuiPaletteHintStyle.Render(pad(hints)))
	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(body, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

func parentRowAvailable(p *pickerState) bool { return parentRowCount(p) == 1 }

const pickerFooterHints = "↑↓ move · space buffer · v/⇧ range · enter open · ^D upload · / filter · esc cancel"
const pickerDlFooterHints = "↑↓ move · space buffer · v/⇧ range · enter save · ^D download · / filter · esc cancel"

// fileLabel resolves a buffered fileId to its filename for tray rendering.
func (p *pickerState) fileLabel(fileId string) string {
	for _, f := range p.files {
		if f.FileId == fileId {
			return f.Filename
		}
	}
	return fileId
}

// pickerRowView renders ONE selectable row ("": skip). Browse mode: bold blue
// dirs with trailing slash, files with a size gutter. Files mode: shared file
// name, uploader and size.
func pickerRowView(p *pickerState, row int, hasParent bool, lo, hi int) string {
	marker := "  "
	if row >= lo && row <= hi && (p.visual || p.anchor >= 0) {
		marker = "> "
	}
	check := " "
	if p.mode == modeFiles {
		ff := p.filteredFiles()
		if row < 0 || row >= len(ff) {
			return ""
		}
		f := ff[row]
		if p.inBuf[f.FileId] {
			check = "✓"
		}
		return fmt.Sprintf("%s%s %s · %s %s", marker, check,
			tuiPaletteMatchStyle.Render(f.Filename),
			tuiPaletteDescStyle.Render(f.Username),
			tuiDimStyle.Render(humanSize(f.Size)))
	}
	if hasParent && row == 0 {
		return marker + tuiPickerDirStyle.Render("../")
	}
	fe := p.filteredEntries()
	idx := row
	if hasParent {
		idx--
	}
	if idx < 0 || idx >= len(fe) {
		return ""
	}
	e := fe[idx]
	if p.inBuf[filepath.Join(p.cwd, e.name)] {
		check = "✓"
	}
	if e.dir {
		return fmt.Sprintf("%s%s %s", marker, check, tuiPickerDirStyle.Render(e.name+"/"))
	}
	return fmt.Sprintf("%s%s %s%s", marker, check, e.name,
		tuiDimStyle.Render(" "+humanSize(e.size)))
}
