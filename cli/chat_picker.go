package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
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

	// loading marks a DEBOUNCED filesystem reload in flight (see
	// schedulePickerReload): the stale listing keeps painting with the
	// header spinner — never a flash of empty. spin advances the spinner
	// frame; reloadGen fences stale reload ticks after navigation.
	loading   bool
	spin      int
	reloadGen int

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
	c.palette.close()    // command drawer hands the slot over
	c.mention.close()    // "@" dropdown hands the slot over too
	c.closeReactionAux() // the anchored aux rows never survive a mode switch
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
	c.mention.close() // "@" dropdown hands the slot over too
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

// pickerMatchFilter reports whether the given entry passes the active
// filter — fuzzy subsequence (fzf semantics), so "abc" matches "aXbYc" the
// same way the command palette matches. The empty filter matches everything.
func pickerMatchFilter(name, filter string) bool {
	if filter == "" {
		return true
	}
	ms := fuzzy.Find(filter, []string{name})
	return len(ms) > 0 && ms[0].Index == 0
}

// filteredEntries returns entries matching the active filter (browse mode),
// reordered by the weighted fuzzy rank INSIDE each structural group: the
// parent row and directories keep floating above files, so navigation stays
// predictable while ranking matches the "/" palette's feel.
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
	var dirs, files []pickerEntry
	for _, e := range out {
		if e.dir {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	byScore := func(list []pickerEntry) []pickerEntry {
		return rankFuzzyList(list,
			func(i int) fuzzyHit { return rankTitleGroup(p.filter, list[i].name, "") },
			func(i int) float64 { return 0 })
	}
	return append(byScore(dirs), byScore(files)...)
}

// filteredFiles returns files matching the active filter (files mode).
// Search matches against filename OR sender username; the better of the two
// fuzzy scores orders the listing.
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
	return rankFuzzyList(out,
		func(i int) fuzzyHit {
			a := rankTitleGroup(p.filter, out[i].filename, "")
			b := rankTitleGroup(p.filter, out[i].from, "")
			if b.q > a.q {
				return b
			}
			return a
		},
		func(i int) float64 { return 0 })
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

// cd navigates into a directory and resets selection geometry. A navigation
// also fences any in-flight debounced reload (loading=false, gen++) — the
// stale read must never repaint over the new directory.
func (c *chatScreen) pickerCd(path string) {
	c.picker.cwd = path
	c.picker.cursor = 0
	c.picker.offset = 0
	c.picker.anchor = -1
	c.picker.visual = false
	c.picker.loading = false
	c.picker.reloadGen++
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

	// OpenCode parity: Ctrl+C dismisses the drawer outright in EVERY mode
	// (browse, files, buffer, detail, filter) — Esc keeps its per-mode
	// staged behaviour, Ctrl+C is the hard dismiss.
	if msg.Type == tea.KeyCtrlC {
		c.closePicker(composerPlaceholder)
		return true, nil
	}

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
		case tea.KeyHome:
			p.moveTo(0, false)
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyEnd:
			p.moveTo(p.rowCount()-1, false)
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyPgUp:
			p.moveTo(p.cursor-10, false)
			c.centerActiveDrawer()
			return true, nil
		case tea.KeyPgDown:
			p.moveTo(p.cursor+10, false)
			c.centerActiveDrawer()
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

	// Filter mode: capture keystrokes for the filter query. Every keystroke
	// filters LIVE over the in-memory list and resets the cursor to 0; the
	// only deferred work is the debounced FILESYSTEM re-read on commit
	// (Enter), never a cursor move.
	if p.filtering {
		switch {
		case msg.Type == tea.KeyEsc:
			p.filter = ""
			p.filtering = false
			p.cursor = 0
			p.offset = 0
			c.drawerHoverLock = 1
			return true, nil
		case msg.Type == tea.KeyEnter:
			p.filtering = false
			p.cursor = 0
			p.offset = 0
			p.clampCursor()
			// ~150ms debounce: while it runs the stale listing keeps
			// painting with the header spinner (never a flash of empty).
			return true, func() tea.Cmd { return c.schedulePickerReload() }
		case msg.Type == tea.KeyBackspace || msg.Type == tea.KeyCtrlH:
			if len(p.filter) > 0 {
				p.filter = p.filter[:len(p.filter)-1]
			} else {
				p.filtering = false
			}
			p.cursor = 0
			p.offset = 0
			c.drawerHoverLock = 1
			return true, nil
		case msg.Type == tea.KeyRunes:
			p.filter += string(msg.Runes)
			p.cursor = 0
			p.offset = 0
			c.drawerHoverLock = 1
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
	case tea.KeyHome:
		p.moveTo(0, false)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyEnd:
		p.moveTo(p.rowCount()-1, false)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyPgUp:
		p.moveTo(p.cursor-10, false)
		c.centerActiveDrawer()
		return true, nil
	case tea.KeyPgDown:
		p.moveTo(p.cursor+10, false)
		c.centerActiveDrawer()
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
				c.bumpFrec(f.filename)
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
			c.bumpFrec(entry.name)
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

// ---- debounced filesystem reload ---------------------------------------------

// pickerDebounce is the filesystem-source debounce (~150ms): the ONLY
// deferred work in the drawer. The in-memory filters re-rank synchronously
// per keystroke; cursor moves never schedule anything. While a reload is in
// flight the stale listing keeps painting with the header spinner — never a
// flash of empty.
const pickerDebounce = 150 * time.Millisecond

// pickerTickStep spaces the loading-spinner frames while a reload is in
// flight (a state indicator, not an animation: no fades or slides).
const pickerTickStep = 100 * time.Millisecond

// reloadPickerMsg fires after the debounce window; gen fences stale reads
// (a navigation during the wait bumps the generation).
type reloadPickerMsg struct{ gen int }

// pickerTickMsg advances the loading spinner frame.
type pickerTickMsg struct{}

// schedulePickerReload debounces a directory re-read: loading goes on, and
// after ~150ms the reload message re-reads the current directory (a
// navigation mid-wait bumps reloadGen, so the stale read is dropped).
func (c *chatScreen) schedulePickerReload() tea.Cmd {
	c.picker.reloadGen++
	gen := c.picker.reloadGen
	c.picker.loading = true
	c.picker.spin = 0
	return tea.Batch(
		tea.Tick(pickerDebounce, func(time.Time) tea.Msg { return reloadPickerMsg{gen: gen} }),
		tea.Tick(pickerTickStep, func(time.Time) tea.Msg { return pickerTickMsg{} }),
	)
}

// ---- rendering -------------------------------------------------------------------

// pickerRows budget: one lift spacer + the exact panel rows pickerView
// paints (pickerPanelRows — the same plan the mouse hit-test walks, so the
// reservation can never drift from the painted height). The row COUNT is
// width-independent (rows truncate, they never wrap), so the budget passes
// width 0 for the plan.
func (c chatScreen) pickerRows() int {
	return 1 + len(c.pickerPanelRows(0))
}

func parentRowCount(p *pickerState) int {
	if p.mode == modeBrowse && parentDir(p.cwd) != "" {
		return 1
	}
	return 0
}

// pickerItemPlan builds the picker's ITEM display plan (rows in filtered
// space): grouped "Folders"/"Files" headers while the filter is empty,
// flattened to the single ranked list while filtering. Buffer review stays
// flat (its rows are queued paths, not a search surface).
func (c chatScreen) pickerItemPlan(win int) drawerPlan {
	p := &c.picker
	switch p.mode {
	case modeBuffer:
		return buildDrawerPlan(len(p.buffered), false, nil, win)
	case modeFiles:
		ff := p.filteredFiles()
		return buildDrawerPlan(len(ff), p.filter == "", func(int) string { return "Files" }, win)
	default: // modeBrowse
		n := p.rowCount()
		fe := p.filteredEntries()
		hasParent := parentRowAvailable(p)
		return buildDrawerPlan(n, p.filter == "", func(i int) string {
			if hasParent && i == 0 {
				return "Folders"
			}
			idx := i
			if hasParent {
				idx--
			}
			if idx < 0 || idx >= len(fe) {
				return "Files"
			}
			if fe[idx].dir {
				return "Folders"
			}
			return "Files"
		}, win)
	}
}

// pickerHits precomputes the fuzzy match data for the picker's filtered
// rows (once per panel build): filenames for browse, the better of
// filename/sender for the files drawer. The empty filter carries nothing.
func (c chatScreen) pickerHits() []fuzzyHit {
	p := &c.picker
	if p.filter == "" {
		return nil
	}
	switch p.mode {
	case modeFiles:
		ff := p.filteredFiles()
		hits := make([]fuzzyHit, len(ff))
		for i, f := range ff {
			a := rankTitleGroup(p.filter, f.filename, "")
			b := rankTitleGroup(p.filter, f.from, "")
			if b.q > a.q {
				hits[i] = b
			} else {
				hits[i] = a
			}
		}
		return hits
	default:
		n := p.rowCount()
		fe := p.filteredEntries()
		hasParent := parentRowAvailable(p)
		hits := make([]fuzzyHit, n)
		for i := 0; i < n; i++ {
			if hasParent && i == 0 {
				hits[i] = fuzzyHit{matched: true}
				continue
			}
			idx := i
			if hasParent {
				idx--
			}
			if idx < 0 || idx >= len(fe) {
				continue
			}
			hits[i] = rankTitleGroup(p.filter, fe[idx].name, "")
		}
		return hits
	}
}

// pickerCrumb is the drawer header title: the breadcrumb (paths truncated
// in the MIDDLE — the tail name stays readable), the live filter fragment,
// and the loading spinner while a debounced reload is in flight.
func (c chatScreen) pickerCrumb(inner int) string {
	p := &c.picker
	crumb := breadcrumb(p.cwd, p.home)
	if p.mode == modeFiles {
		crumb = fmt.Sprintf("shared files — %d", len(p.filteredFiles()))
	}
	if p.filtering {
		crumb += "  /" + p.filter + "▎"
	} else if p.filter != "" {
		crumb += "  /" + p.filter
	}
	if p.loading {
		crumb += " " + spinnerGlyph(p.spin)
	}
	return truncateMiddle(crumb, maxInt(inner-4, 1))
}

// pickerEmptyText is the muted empty-state row: "No matching items" while a
// filter is active (an empty directory keeps its quiet note).
func (c chatScreen) pickerEmptyText() string {
	p := &c.picker
	if p.filter != "" {
		return "No matching items"
	}
	if p.mode == modeFiles {
		return "(nothing shared yet)"
	}
	return "(empty directory)"
}

// pickerPanelRows builds the picker's full painted row plan: border,
// header (crumb), the selection tray, the notice, the windowed item rows
// (grouped or flattened), the overflow marker and the keymap footer. Detail
// and buffer modes build their fixed chrome rows instead. inner is the real
// panel width for truncation; the budget passes 0 (count-only).
func (c chatScreen) pickerPanelRows(inner int) []drawerRow {
	p := &c.picker
	switch p.mode {
	case modeDetail:
		return pickerDetailRows(p, inner)
	case modeBuffer:
		return pickerBufferRows(p, inner, c.height)
	}
	win := drawerMaxRows(c.height)
	plan := c.pickerItemPlan(win)
	plan.hits = c.pickerHits()
	rows, below, above := windowDrawerPlan(plan, p.offset, p.cursor)
	panel := []drawerRow{{kind: drBorder}, {kind: drHeader, text: c.pickerCrumb(inner)}}

	// Selection tray: checked keys pinned under the header.
	shown := p.buffered
	truncated := false
	if len(shown) > pickerMaxTrayRows {
		shown = shown[:pickerMaxTrayRows]
		truncated = true
	}
	for _, key := range shown {
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
		panel = append(panel, drawerRow{kind: drTray, text: mark + label})
	}
	if truncated {
		panel = append(panel, drawerRow{kind: drTray,
			text: fmt.Sprintf("… +%d more selected", len(p.buffered)-pickerMaxTrayRows)})
	}
	if p.notice != "" {
		panel = append(panel, drawerRow{kind: drNotice, text: "· " + p.notice})
	}
	if len(rows) == 0 {
		panel = append(panel, drawerRow{kind: drEmpty, text: c.pickerEmptyText()})
	} else {
		for _, r := range rows {
			if r.kind == drItem {
				r.hit = plan.hitOf(r.item)
			}
			panel = append(panel, r)
		}
		if below > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: below, text: "more"})
		} else if above > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: above, text: "above"})
		}
	}
	hints := pickerFooterHints
	if p.mode == modeFiles {
		hints = pickerDlFooterHints
	}
	panel = append(panel, drawerRow{kind: drFooter, text: hints, n: p.rowCount()})
	return panel
}

// pickerDetailRows is the fixed detail panel: metadata rows plus where the
// file was saved (files arrive complete — nothing left to download).
func pickerDetailRows(p *pickerState, inner int) []drawerRow {
	f := p.detailFile
	if f == nil {
		return nil
	}
	saved := truncateMiddle(f.path, maxInt(inner-12, 1))
	return []drawerRow{
		{kind: drBorder},
		{kind: drHeader, text: "file details"},
		{kind: drBlank},
		{kind: drRaw, text: "  Name:     " + tuiPaletteMatchStyle.Render(f.filename)},
		{kind: drRaw, text: "  From:     " + tuiPaletteDescStyle.Render(f.from)},
		{kind: drRaw, text: "  Size:     " + humanSize(f.size)},
		{kind: drRaw, text: "  Saved:    " + tuiDimStyle.Render(saved)},
		{kind: drRaw, text: "  Received: " + tuiDimStyle.Render(f.at.Format("2006-01-02 15:04:05"))},
		{kind: drBlank},
		{kind: drFooter, text: "enter back · esc close", n: 0},
	}
}

// pickerBufferRows is the buffer review panel: queued upload files with
// deselect, windowed exactly like the browse list.
func pickerBufferRows(p *pickerState, inner, termH int) []drawerRow {
	count := len(p.buffered)
	panel := []drawerRow{
		{kind: drBorder},
		{kind: drHeader, text: fmt.Sprintf("upload buffer — %d file%s", count, plural(count))},
	}
	if count == 0 {
		note := "(empty buffer)"
		if p.notice != "" {
			note = "· " + p.notice
		}
		panel = append(panel, drawerRow{kind: drEmpty, text: note})
	} else {
		win := drawerMaxRows(termH)
		plan := buildDrawerPlan(count, false, nil, win)
		rows, below, above := windowDrawerPlan(plan, p.offset, p.cursor)
		for _, r := range rows {
			if r.kind == drItem {
				r.text = filepath.Base(p.buffered[r.item])
			}
			panel = append(panel, r)
		}
		if below > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: below, text: "more"})
		} else if above > 0 {
			panel = append(panel, drawerRow{kind: drOverflow, n: above, text: "above"})
		}
	}
	panel = append(panel, drawerRow{kind: drFooter, text: pickerBufFooterHints, n: count})
	return panel
}

// pickerView paints the drawer panel in whichever picker mode is active —
// the same row plan as the budget and the mouse hit-test. The panel caps
// itself at min(maxW, termW-2, 80); every row is single-line.
func (c chatScreen) pickerView(maxW int) string {
	if !c.picker.isActive() || maxW < 6 {
		return ""
	}
	inner := drawerMaxW(c.width, maxW)
	rows := c.pickerPanelRows(inner)
	if len(rows) == 0 {
		return ""
	}
	p := &c.picker
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		var line string
		switch r.kind {
		case drBorder:
			line = drawerBorderRow(inner)
		case drHeader:
			line = drawerHeaderRow(inner, r.text)
		case drBlank:
			line = ""
		case drItem:
			line = c.pickerItemRowView(r, inner)
		case drOverflow:
			line = overflowRowView(inner, r.n, r.text == "above")
		case drEmpty:
			line = tuiDimStyle.Render(padVisible(r.text, inner))
		case drFooter:
			line = drawerFooterRow(inner, r.text, p.cursor, r.n)
		case drTray:
			st := tuiPickerBufStyle
			if strings.HasPrefix(r.text, "…") {
				st = tuiDimStyle // the collapsed-tray marker reads as a hint
			}
			line = st.Render(padVisible(r.text, inner))
		case drNotice:
			st := tuiPickerNoticeStyle
			if p.mode == modeFiles && strings.HasPrefix(p.notice, "no files") {
				st = tuiDimStyle // the empty state is a hint, not an error
			}
			line = st.Render(padVisible(r.text, inner))
		case drRaw:
			line = r.text
		}
		out = append(out, fitRow(line, inner))
	}
	return strings.Join(out, "\n")
}

// pickerItemRowView paints ONE selectable picker row (browse/files/buffer):
// range marker, buffer check, dir or file tone, size gutter — the matched
// filter chars pop fzf-style, the selected row wears the shared cursor bar.
func (c chatScreen) pickerItemRowView(r drawerRow, inner int) string {
	p := &c.picker
	selected := r.item == p.cursor
	lo, hi := pickerRange(p.anchor, p.cursor)
	marker := "  "
	if r.item >= lo && r.item <= hi && (p.visual || p.anchor >= 0) {
		marker = "> "
	}
	var line string
	switch p.mode {
	case modeBuffer:
		line = marker + "✓ " + tuiPaletteMatchStyle.Render(r.text)
	case modeFiles:
		ff := p.filteredFiles()
		if r.item < 0 || r.item >= len(ff) {
			return ""
		}
		f := ff[r.item]
		name := highlightMatches(sanitizeDisplay(f.filename), r.hit.matches, selected, inner)
		line = marker + " " + name + " · " +
			tuiPaletteDescStyle.Render(sanitizeDisplay(f.from)) + " " +
			tuiDimStyle.Render(humanSize(f.size))
	default: // modeBrowse
		fe := p.filteredEntries()
		hasParent := parentRowAvailable(p)
		if hasParent && r.item == 0 {
			line = marker + tuiPickerDirStyle.Render("../")
			break
		}
		idx := r.item
		if hasParent {
			idx--
		}
		if idx < 0 || idx >= len(fe) {
			return ""
		}
		e := fe[idx]
		check := " "
		if p.inBuf[filepath.Join(p.cwd, e.name)] {
			check = "✓"
		}
		name := highlightMatches(sanitizeDisplay(e.name), r.hit.matches, selected, inner)
		if e.dir {
			line = marker + check + " " + tuiPickerDirStyle.Render(name+"/")
		} else {
			line = marker + check + " " + name + tuiDimStyle.Render(" "+humanSize(e.size))
		}
	}
	line = padVisible(truncateByWidth(line, inner), inner)
	if selected {
		line = tuiPaletteSelStyle.Render(retint(line, tuiPaletteSelStyle))
	} else {
		line = tuiPaletteRowStyle.Render(line)
	}
	return line
}

// plural returns "s" if n != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func parentRowAvailable(p *pickerState) bool { return parentRowCount(p) == 1 }

// Footer hint contracts, derived from the keys handlePickerKeys actually
// binds per mode (the "·"-joined legend + the count right, see
// drawerFooterRow).
const pickerFooterHints = "↑↓ move · space buffer · v range · enter open · ^S send · / filter · esc cancel"
const pickerDlFooterHints = "↑↓ move · enter details · / search · esc close"
const pickerBufFooterHints = "↑↓ move · ^D remove · ^S send all · esc back"

// fileLabel resolves a buffered path to its filename for tray rendering.
func (p *pickerState) fileLabel(path string) string {
	for _, f := range p.files {
		if f.path == path {
			return f.filename
		}
	}
	return path
}
