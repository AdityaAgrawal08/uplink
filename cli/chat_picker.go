package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
// Ctrl+S hands them all to the upload engine, Esc discards. Enter opens
// folders and never touches files; Space buffers (ranges included).
//
// Selecting "/download" opens a read-only browser of files received this
// session (newest first) with a detail window showing sender, size, and
// save location. Files arrive complete over the wire — nothing left to
// download, so the drawer never mutates anything.

const pickerMaxVisible = 8 // entry rows painted before scrolling
const pickerMaxTrayRows = 3

var (
	tuiPickerCrumbStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colAccent) // accent breadcrumb

	tuiPickerDirStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "#0b62c9", Dark: "#4cc9f0"}) // directories

	tuiPickerBufStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#0f7a52", Dark: "#34d399"}) // buffered rows

	tuiPickerNoticeStyle = lipgloss.NewStyle().
				Foreground(colAmber)
)

type pickerEntry struct {
	name string
	dir  bool
	size int64
}

// pickerMode selects what the drawer lists: the local file system (upload),
// the room's shared files (download), a file detail view, buffer review, or delete.
type pickerMode int

const (
	modeBrowse pickerMode = iota
	modeFiles
	modeDetail // detail window for download confirmation
	modeBuffer // buffer review: shows queued files, deselect with ctrl+d
)

// pickerState is the browser mode of the drawer. All list math goes through
// pure helpers so navigation rules are unit-testable without a filesystem.
type pickerState struct {
	active    bool
	mode      pickerMode
	cwd       string // absolute current directory (modeBrowse)
	home      string // $HOME, for ~/ breadcrumb abbreviation
	entries   []pickerEntry
	files     []receivedFile // modeFiles listing (received this session), most recent first
	cursor    int
	offset    int      // first visible row in the scroll window
	anchor    int      // range anchor (-1 = no active range)
	visual    bool     // `v` visual mode: plain moves extend the range
	buffered  []string // ordered keys: absolute local paths
	inBuf     map[string]bool
	notice    string // transient error line ("" = none)
	filter    string // substring filter (active when filtering=true)
	filtering bool   // true while the user is typing a filter query

	// Detail window state (modeDetail).
	detailFile *receivedFile // the file being inspected
}

func (p *pickerState) isActive() bool { return p.active }

// documentsDir resolves the OS-aware starting folder for the upload
// browser via the Go runtime (no hardcoded paths): the user's Documents
// folder on each OS, falling back to $HOME when it doesn't exist.
// Install-time configs can't know the user's layout, so this resolves at
// runtime on every open.
func documentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		// %USERPROFILE%\Documents is the default; OneDrive KFR moves it
		// under %OneDriveCommercial%\%USERPROFILE-relative path, and
		// HOMEDRIVE+HOMEPATH covers roaming profiles.
		profile := os.Getenv("USERPROFILE")
		candidates = []string{
			filepath.Join(profile, "Documents"),
			filepath.Join(os.Getenv("OneDriveCommercial"), profileRel(profile), "Documents"),
			filepath.Join(os.Getenv("HOMEDRIVE")+os.Getenv("HOMEPATH"), "Documents"),
			filepath.Join(home, "Documents"),
		}
	case "darwin":
		candidates = []string{filepath.Join(home, "Documents")}
	default: // linux and other unix-likes: XDG first (localized names)
		if xdg := xdgDocumentsDir(home); xdg != "" {
			return xdg
		}
		candidates = []string{filepath.Join(home, "Documents")}
	}
	for _, dir := range candidates {
		if dir == "" || dir == "." {
			continue
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return home
}

// profileRel strips the drive/volume prefix so a %USERPROFILE% path can be
// re-rooted under OneDrive (C:\Users\bob -> Users\bob). "" when unusable.
func profileRel(profile string) string {
	if profile == "" {
		return ""
	}
	if vol := filepath.VolumeName(profile); vol != "" {
		profile = strings.TrimPrefix(profile, vol)
	}
	return strings.TrimLeft(profile, `/\`)
}

// xdgDocumentsDir reads XDG_DOCUMENTS_DIR from ~/.config/user-dirs.dirs
// (handles $HOME prefixes and quoting); "" when unset or unusable.
func xdgDocumentsDir(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, ".config", "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "XDG_DOCUMENTS_DIR=") {
			continue
		}
		val := strings.Trim(strings.TrimPrefix(line, "XDG_DOCUMENTS_DIR="), `"`)
		val = strings.ReplaceAll(val, "$HOME", home)
		if !filepath.IsAbs(val) {
			return ""
		}
		if st, err := os.Stat(val); err == nil && st.IsDir() {
			return val
		}
		return ""
	}
	return ""
}

// openPicker enters browser mode rooted at the OS Documents folder.
func (c *chatScreen) openPicker() tea.Cmd {
	c.focus = focusComposer // the browser takes the drawer's focus
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	c.palette.close() // command drawer hands the slot over
	c.pendingReactionMsgId = ""
	c.input.SetValue("")
	c.input.Placeholder = ""
	// Ensure persistent buffer maps exist.
	if c.uploadBufSet == nil {
		c.uploadBufSet = map[string]bool{}
	}
	c.picker = pickerState{
		active:   true,
		mode:     modeBrowse,
		cwd:      documentsDir(),
		home:     home,
		anchor:   -1,
		buffered: c.uploadBuf,
		inBuf:    c.uploadBufSet,
	}
	c.loadPickerDir()
	return nil
}

// openFilesDrawer morphs the drawer into the files-received-this-session
// list, most recent first. There is no server file index anymore; the
// listing is synchronous and local. Files mode is read-only: Space/selection
// is disabled (nothing to fetch — everything listed is already saved), so it
// gets a private empty buffer that closePicker must not sync back.
func (c *chatScreen) openFilesDrawer() tea.Cmd {
	c.focus = focusComposer
	c.palette.close()
	c.input.SetValue("")
	c.input.Placeholder = ""
	files := make([]receivedFile, len(c.received))
	copy(files, c.received)
	// Recent first.
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j].at.After(files[j-1].at); j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
	c.picker = pickerState{
		active:   true,
		mode:     modeFiles,
		home:     ".",
		anchor:   -1,
		files:    files,
		buffered: nil,
		inBuf:    map[string]bool{},
	}
	if len(files) == 0 {
		c.picker.notice = "no files received yet — they appear here automatically"
	}
	c.picker.clampCursor()
	return nil
}

// closePicker leaves browser mode; syncs buffer to persistent storage.
// Files mode owns a private empty buffer (read-only listing) that must
// never overwrite the upload buffer.
func (c *chatScreen) closePicker(restore string) {
	if c.picker.mode != modeFiles {
		c.uploadBuf = c.picker.buffered
		c.uploadBufSet = c.picker.inBuf
	}
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
// Search matches against filename OR sender username.
func (p *pickerState) filteredFiles() []receivedFile {
	if p.filter == "" {
		return p.files
	}
	var out []receivedFile
	for _, f := range p.files {
		if pickerMatchFilter(f.filename, p.filter) || pickerMatchFilter(f.from, p.filter) {
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
// room's UPLOADED shared files; buffer mode lists queued upload files;
// detail mode returns 0 (no scrollable rows).
func (p *pickerState) rowCount() int {
	if p.mode == modeDetail {
		return 0
	}
	if p.mode == modeBuffer {
		return len(p.buffered)
	}
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
// absolute path (browse) or save path (files). ok=false for the ".." row.
func (p *pickerState) entryAt(row int) (entry pickerEntry, key string, ok bool) {
	if p.mode == modeBuffer {
		if row < 0 || row >= len(p.buffered) {
			return pickerEntry{}, "", false
		}
		key = p.buffered[row]
		// Extract filename from path for display.
		name := filepath.Base(key)
		return pickerEntry{name: name}, key, true
	}
	if p.mode == modeFiles {
		ff := p.filteredFiles()
		if row < 0 || row >= len(ff) {
			return pickerEntry{}, "", false
		}
		f := ff[row]
		return pickerEntry{name: f.filename, size: f.size}, f.path, true
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
// Folders are buffered like files (the engine tarballs them). Files mode is
// read-only: nothing there can be buffered (everything is already saved).
func (c *chatScreen) pickerToggleBuffer() {
	if c.picker.mode == modeFiles {
		c.picker.notice = "already saved — Enter for details"
		return
	}
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

// pickerBufferFolder adds all files in the current directory (recursively)
// to the buffer. In browse mode this walks the directory tree.
func (c *chatScreen) pickerBufferFolder() {
	p := &c.picker
	if p.mode == modeBrowse {
		// Buffer all files in current directory (non-recursive for now).
		fe := p.filteredEntries()
		for _, e := range fe {
			if e.dir {
				continue // skip subdirectories
			}
			key := filepath.Join(p.cwd, e.name)
			if !p.inBuf[key] {
				p.inBuf[key] = true
				p.buffered = append(p.buffered, key)
			}
		}
	}
}

// pickerConfirm hands the buffered set to the active engine and closes:
// browse mode uploads buffered paths; files mode is read-only (no buffering).
func (c *chatScreen) pickerConfirm() tea.Cmd {
	restore := composerPlaceholder
	if c.picker.mode == modeFiles {
		// Read-only listing: everything shown is already saved. Just close.
		c.closePicker(restore)
		return nil
	}

	jobs := make([]uploadJob, 0, len(c.picker.buffered))
	// Raw recipient username ("" = room broadcast); the engine encrypts
	// for exactly that audience.
	to := c.targetUser
	for _, abs := range c.picker.buffered {
		jobs = append(jobs, uploadJob{Path: abs, To: to})
	}
	// Clear persistent buffer — files are being sent. Close first: it
	// syncs the buffer back from the picker, which would resurrect the
	// just-cleared slice on reopen (duplicate re-upload).
	c.closePicker(restore)
	c.uploadBuf = nil
	c.uploadBufSet = map[string]bool{}
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

	// Detail window mode: simple Enter/Esc only. Files are already saved;
	// Enter returns to the list.
	if p.mode == modeDetail {
		switch msg.Type {
		case tea.KeyEnter:
			p.mode = modeFiles
			p.detailFile = nil
			p.clampCursor()
			return true, nil
		case tea.KeyEsc:
			// Back to files list.
			p.mode = modeFiles
			p.detailFile = nil
			p.clampCursor()
			return true, nil
		}
		return true, nil
	}

	// Buffer review mode: navigate and deselect queued files.
	if p.mode == modeBuffer {
		switch msg.Type {
		case tea.KeyUp:
			p.moveTo(p.cursor-1, false)
			return true, nil
		case tea.KeyDown:
			p.moveTo(p.cursor+1, false)
			return true, nil
		case tea.KeyCtrlD: // deselect current item
			if p.cursor >= 0 && p.cursor < len(p.buffered) {
				key := p.buffered[p.cursor]
				delete(p.inBuf, key)
				p.buffered = append(p.buffered[:p.cursor], p.buffered[p.cursor+1:]...)
				p.clampCursor()
			}
			if len(p.buffered) == 0 {
				p.notice = "buffer empty — use /upload to add files"
			}
			return true, nil
		case tea.KeyCtrlS: // Ctrl+S: send all remaining
			return true, c.pickerConfirm
		case tea.KeyEsc:
			restore := composerPlaceholder
			c.closePicker(restore)
			return true, nil
		}
		return true, nil
	}

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
		if p.mode == modeFiles {
			// Open detail window for the selected file (already saved).
			ff := p.filteredFiles()
			if p.cursor >= 0 && p.cursor < len(ff) {
				f := ff[p.cursor]
				p.mode = modeDetail
				p.detailFile = &f
			}
			return true, nil
		}
		// modeBrowse: Enter OPENS directories and does nothing on files
		// (product direction). Buffering is Space's job; sending is
		// Ctrl+S for everything buffered. Visual ranges included: Enter
		// never mutates the buffer, it only navigates or hints.
		p.anchor = -1
		p.visual = false
		entry, key, _ := p.entryAt(p.cursor)
		if entry.dir {
			c.pickerCd(key) // folders (and "..") navigate
		} else {
			p.notice = "space buffers files · ctrl+s sends everything"
		}
		return true, nil
	case tea.KeyCtrlS: // Ctrl+S: send everything buffered (files + folders)
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
	if c.picker.mode == modeDetail {
		return 1 + 8 + 2 // spacer + detail box + border
	}
	if c.picker.mode == modeBuffer {
		// Buffer review: breadcrumb + items + footer
		rowCount := len(c.picker.buffered)
		rows := 1 /*breadcrumb*/ + min(rowCount, pickerMaxVisible) + 1 /*footer*/
		if rowCount == 0 {
			rows++ // notice row
		}
		if rowCount > pickerMaxVisible {
			rows++ // overflow indicator
		}
		return 1 + rows + 2 // spacer + panel content + box border
	}
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

	// Detail window mode: render a centered info box.
	if p.mode == modeDetail && p.detailFile != nil {
		return c.pickerDetailView(maxW, inner, pad)
	}

	// Buffer review mode: show queued files with deselect option.
	if p.mode == modeBuffer {
		return c.pickerBufferView(maxW, inner, pad)
	}

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
		if p.mode == modeFiles && strings.HasPrefix(p.notice, "no files") {
			style = tuiDimStyle // the empty state is a hint, not an error
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
			body = append(body, tuiPaletteSelStyle.Render(retint(pad(line), tuiPaletteSelStyle)))
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

// pickerDetailView renders the received-file detail window: metadata plus
// where it was saved (files arrive complete — nothing left to download).
func (c chatScreen) pickerDetailView(maxW, inner int, pad func(string) string) string {
	f := c.picker.detailFile
	var body []string

	body = append(body, tuiPickerCrumbStyle.Render(pad("file details")))
	body = append(body, "")

	body = append(body, pad(fmt.Sprintf("  Name:     %s", tuiPaletteMatchStyle.Render(f.filename))))
	body = append(body, pad(fmt.Sprintf("  From:     %s", tuiPaletteDescStyle.Render(f.from))))
	body = append(body, pad(fmt.Sprintf("  Size:     %s", humanSize(f.size))))
	body = append(body, pad(fmt.Sprintf("  Saved:    %s", tuiDimStyle.Render(f.path))))
	body = append(body, pad(fmt.Sprintf("  Received: %s", tuiDimStyle.Render(f.at.Format("2006-01-02 15:04:05")))))
	body = append(body, "")
	body = append(body, tuiPaletteHintStyle.Render(pad("enter back · esc close")))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(body, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

// pickerBufferView renders the buffer review panel showing queued upload files.
func (c chatScreen) pickerBufferView(maxW, inner int, pad func(string) string) string {
	p := c.picker
	var body []string

	count := len(p.buffered)
	body = append(body, tuiPickerCrumbStyle.Render(
		pad(fmt.Sprintf("upload buffer — %d file%s", count, plural(count)))))
	body = append(body, "")

	if count == 0 {
		if p.notice != "" {
			body = append(body, tuiDimStyle.Render(pad("· "+p.notice)))
		} else {
			body = append(body, tuiDimStyle.Render(pad("(empty buffer)")))
		}
	} else {
		visible := min(count, pickerMaxVisible)
		lo, hi := pickerRange(p.anchor, p.cursor)
		for row := p.offset; row < count && row-p.offset < visible; row++ {
			key := p.buffered[row]
			name := filepath.Base(key)
			marker := "  "
			if row >= lo && row <= hi {
				marker = "> "
			}
			line := fmt.Sprintf("%s✓ %s", marker, tuiPaletteMatchStyle.Render(name))
			if row == p.cursor {
				body = append(body, tuiPaletteSelStyle.Render(retint(pad(line), tuiPaletteSelStyle)))
			} else {
				body = append(body, pad(line))
			}
		}
		if more := count - p.offset - visible; more > 0 {
			body = append(body, tuiDimStyle.Render(pad(fmt.Sprintf("… +%d more", more))))
		}
	}

	body = append(body, tuiPaletteHintStyle.Render(
		pad("↑↓ move · ctrl+d remove · ^S send all · esc back")))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(body, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

// plural returns "s" if n != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func parentRowAvailable(p *pickerState) bool { return parentRowCount(p) == 1 }

const pickerFooterHints = "↑↓ move · space buffer · v range · enter open folder · ^S send all · / filter · esc cancel"
const pickerDlFooterHints = "↑↓ move · enter details · / search · esc close"

// fileLabel resolves a buffered path to its filename for tray rendering.
func (p *pickerState) fileLabel(path string) string {
	for _, f := range p.files {
		if f.path == path {
			return f.filename
		}
	}
	return path
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
		return fmt.Sprintf("%s%s %s · %s %s", marker, check,
			tuiPaletteMatchStyle.Render(sanitizeDisplay(f.filename)),
			tuiPaletteDescStyle.Render(sanitizeDisplay(f.from)),
			tuiDimStyle.Render(humanSize(f.size)))
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
		return fmt.Sprintf("%s%s %s", marker, check, tuiPickerDirStyle.Render(sanitizeDisplay(e.name)+"/"))
	}
	return fmt.Sprintf("%s%s %s%s", marker, check, sanitizeDisplay(e.name),
		tuiDimStyle.Render(" "+humanSize(e.size)))
}
