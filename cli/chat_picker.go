package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- file-browser picker -----------------------------------------------------
//
// Selecting "/upload" morphs the command drawer into an ls -a style browser:
// ".." pinned first, directories before files, alphabetical within each group,
// dotfiles included. A selection buffer accumulates paths across directories;
// Ctrl+Enter hands them all to the upload engine, Esc discards.
//
// Selecting "/download" opens a search-driven file picker with a detail window
// for confirming downloads. In private conversations, the detail window is
// skipped (only one file from one uploader).

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

	tuiPickerDetailStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62")).
				Padding(1, 2)
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
	modeDetail    // detail window for download confirmation
	modeBuffer    // buffer review: shows queued files, deselect with ctrl+d
	modeDelete    // delete own messages/files, 2 sections
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

	// Detail window state (modeDetail).
	detailFile *sessionFile // the file being inspected

	// Delete mode state (modeDelete).
	deleteMsgs  []chatMessage  // own messages, latest first
	deleteFiles []sessionFile  // own files, latest first
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
	// Ensure persistent buffer maps exist.
	if c.uploadBufSet == nil {
		c.uploadBufSet = map[string]bool{}
	}
	c.picker = pickerState{
		active:   true,
		mode:     modeBrowse,
		cwd:      home,
		home:     home,
		anchor:   -1,
		buffered: c.uploadBuf,
		inBuf:    c.uploadBufSet,
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
	if c.uploadBufSet == nil {
		c.uploadBufSet = map[string]bool{}
	}
	c.picker = pickerState{
		active:   true,
		mode:     modeFiles,
		home:     ".",
		anchor:   -1,
		buffered: c.uploadBuf,
		inBuf:    c.uploadBufSet,
		notice:   "loading shared files…",
	}
	return c.doFetchAllFiles()
}

// deleteListMsg is the async result for /delete picker's data.
type deleteListMsg struct {
	msgs  []chatMessage
	files []sessionFile
	err   string
}

type deleteDoneMsg struct {
	kind   string // "msg" or "file"
	seq    int
	fileId string
	ok     string
	err    string
}

// openDeletePicker shows own messages/files for deletion, 2 sections latest-first.
func (c *chatScreen) openDeletePicker() tea.Cmd {
	c.palette.close()
	c.input.SetValue("")
	c.input.Placeholder = ""
	msgs := c.ownMessagesForDelete()
	c.picker = pickerState{
		active:      true,
		mode:        modeDelete,
		home:        ".",
		anchor:      -1,
		inBuf:       map[string]bool{},
		notice:      "loading files…",
		loading:     true,
		cursor:      0,
		offset:      0,
		deleteMsgs:  msgs,
		deleteFiles: nil,
	}
	if len(msgs) == 0 {
		c.picker.notice = "loading files…"
	}
	// Kick async files fetch (non-blocking for messages)
	client := c.client
	conv := c.activeConv()
	return func() tea.Msg {
		files, err := client.fetchFiles("", conv)
		if err != nil {
			return deleteListMsg{msgs: msgs, err: err.Error()}
		}
		// Filter to own, not deleted, and latest first
		var ownFiles []sessionFile
		for _, f := range files {
			if f.Username == c.me && f.Status == "UPLOADED" {
				ownFiles = append(ownFiles, f)
			}
		}
		sort.Slice(ownFiles, func(i, j int) bool {
			return ownFiles[i].UploadedAt > ownFiles[j].UploadedAt
		})
		return deleteListMsg{msgs: msgs, files: ownFiles}
	}
}

// ownMessagesForDelete returns own non-deleted messages in active conv, latest first.
func (c *chatScreen) ownMessagesForDelete() []chatMessage {
	var out []chatMessage
	for _, m := range c.history {
		if m.Username != c.me {
			continue
		}
		if m.Status == "DELETED" {
			continue
		}
		if !c.shouldRender(m) {
			continue
		}
		if m.Kind == "system" {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		// latest first: larger seq or newer CreatedAt
		if out[i].Seq != out[j].Seq {
			return out[i].Seq > out[j].Seq
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}

func (c *chatScreen) applyDeleteList(msg deleteListMsg) {
	if !c.picker.isActive() || c.picker.mode != modeDelete {
		return
	}
	c.picker.loading = false
	if msg.err != "" {
		c.picker.notice = msg.err
		return
	}
	c.picker.deleteMsgs = msg.msgs
	c.picker.deleteFiles = msg.files
	c.picker.notice = ""
	if len(msg.msgs) == 0 && len(msg.files) == 0 {
		c.picker.notice = "no deletable items"
	}
	c.picker.clampCursor()
}

func (c *chatScreen) doDeleteAtCursor() tea.Cmd {
	return func() tea.Msg {
		p := &c.picker
		if !p.isActive() || p.mode != modeDelete {
			return deleteDoneMsg{err: "not in delete mode"}
		}
		total := len(p.deleteMsgs) + len(p.deleteFiles)
		if p.cursor < 0 || p.cursor >= total {
			return deleteDoneMsg{err: "nothing selected"}
		}
		if p.cursor < len(p.deleteMsgs) {
			m := p.deleteMsgs[p.cursor]
			err := c.client.deleteMessage(m.Seq)
			if err != nil {
				return deleteDoneMsg{err: err.Error()}
			}
			return deleteDoneMsg{kind: "msg", seq: m.Seq, ok: "deleted message"}
		}
		f := p.deleteFiles[p.cursor-len(p.deleteMsgs)]
		err := c.client.deleteFile(f.FileId)
		if err != nil {
			return deleteDoneMsg{err: err.Error()}
		}
		return deleteDoneMsg{kind: "file", fileId: f.FileId, ok: "deleted file"}
	}
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

// closePicker leaves browser mode; syncs buffer to persistent storage.
func (c *chatScreen) closePicker(restore string) {
	// Sync buffer back to persistent storage before clearing.
	c.uploadBuf = c.picker.buffered
	c.uploadBufSet = c.picker.inBuf
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
// Search matches against filename OR uploader username.
func (p *pickerState) filteredFiles() []sessionFile {
	if p.filter == "" {
		return p.files
	}
	var out []sessionFile
	for _, f := range p.files {
		if pickerMatchFilter(f.Filename, p.filter) || pickerMatchFilter(f.Username, p.filter) {
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
	if p.mode == modeDelete {
		return len(p.deleteMsgs) + len(p.deleteFiles)
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
// absolute path (browse) or fileId (files). ok=false for the ".." row.
func (p *pickerState) entryAt(row int) (entry pickerEntry, key string, ok bool) {
	if p.mode == modeDelete {
		total := len(p.deleteMsgs) + len(p.deleteFiles)
		if row < 0 || row >= total {
			return pickerEntry{}, "", false
		}
		if row < len(p.deleteMsgs) {
			m := p.deleteMsgs[row]
			preview := m.Text
			if len(preview) > 28 {
				preview = preview[:28] + "…"
			}
			return pickerEntry{name: preview, size: int64(m.Seq)}, fmt.Sprintf("msg:%d", m.Seq), true
		}
		f := p.deleteFiles[row-len(p.deleteMsgs)]
		return pickerEntry{name: f.Filename, size: f.Size}, f.FileId, true
	}
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
	// Determine conversation scope for file segregation.
	var toConv string
	if c.targetUser != "" {
		toConv = conversationKey(c.me, c.targetUser)
	}
	for _, abs := range c.picker.buffered {
		jobs = append(jobs, uploadJob{Path: abs, To: toConv})
	}
	// Clear persistent buffer — files are being sent.
	c.uploadBuf = nil
	c.uploadBufSet = map[string]bool{}
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

	// Detail window mode: simple Enter/Esc only.
	if p.mode == modeDetail {
		switch msg.Type {
		case tea.KeyEnter:
			// Download the file shown in the detail window.
			if p.detailFile != nil {
				job := dlJob{
					FileId:   p.detailFile.FileId,
					Filename: p.detailFile.Filename,
					Size:     p.detailFile.Size,
				}
				restore := composerPlaceholder
				c.closePicker(restore)
				return true, func() tea.Cmd { return c.startDownloads([]dlJob{job}) }
			}
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
		case tea.KeyCtrlJ: // Ctrl+Enter: send all remaining
			return true, c.pickerConfirm
		case tea.KeyEsc:
			restore := composerPlaceholder
			c.closePicker(restore)
			return true, nil
		}
		return true, nil
	}

	// Delete mode: navigate own messages/files, Ctrl+D deletes for all.
	if p.mode == modeDelete {
		switch msg.Type {
		case tea.KeyUp:
			p.moveTo(p.cursor-1, false)
			return true, nil
		case tea.KeyDown:
			p.moveTo(p.cursor+1, false)
			return true, nil
		case tea.KeyCtrlD:
			return true, c.doDeleteAtCursor
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
			// Open detail window for the selected file.
			ff := p.filteredFiles()
			if p.cursor >= 0 && p.cursor < len(ff) {
				f := ff[p.cursor]
				// In private conversations, skip detail and download directly.
				if c.targetUser != "" {
					job := dlJob{FileId: f.FileId, Filename: f.Filename, Size: f.Size}
					restore := composerPlaceholder
					c.closePicker(restore)
					return true, func() tea.Cmd { return c.startDownloads([]dlJob{job}) }
				}
				p.mode = modeDetail
				p.detailFile = &f
			}
			return true, nil
		}
		// modeBrowse: Enter buffers the file or opens the directory.
		// If visual range is active, buffer the ENTIRE range, then clear it.
		if p.anchor >= 0 && p.visual {
			c.pickerToggleBuffer() // buffers the whole range
			p.anchor = -1
			p.visual = false
			return true, nil
		}
		entry, key, _ := p.entryAt(p.cursor)
		switch {
		case !entry.dir:
			c.pickerToggleBuffer() // buffer this single file
			p.anchor = -1
			p.visual = false
			return true, nil
		case key == parentDir(p.cwd):
			p.anchor = -1
			p.visual = false
			c.pickerCd(key) // the ".." row
		default:
			p.anchor = -1
			p.visual = false
			c.pickerCd(key) // browse into the folder
		}
		return true, nil
	case tea.KeyCtrlJ: // Ctrl+Enter (LF — standard on most terminals)
		// Ctrl+Enter: upload/download all buffered.
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
	if p.mode == modeDelete {
		return c.pickerDeleteView(maxW, inner, pad)
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

// pickerDetailView renders the file detail window for download confirmation.
func (c chatScreen) pickerDetailView(maxW, inner int, pad func(string) string) string {
	f := c.picker.detailFile
	var body []string

	body = append(body, tuiPickerCrumbStyle.Render(pad("file details")))
	body = append(body, "")

	// Parse and format the upload time.
	uploadTime := f.UploadedAt
	if t, err := time.Parse(time.RFC3339, f.UploadedAt); err == nil {
		uploadTime = t.Format("2006-01-02 15:04:05")
	}

	body = append(body, pad(fmt.Sprintf("  Name:     %s", tuiPaletteMatchStyle.Render(f.Filename))))
	body = append(body, pad(fmt.Sprintf("  Uploader: %s", tuiPaletteDescStyle.Render(f.Username))))
	body = append(body, pad(fmt.Sprintf("  Size:     %s", humanSize(f.Size))))
	body = append(body, pad(fmt.Sprintf("  Uploaded: %s", tuiDimStyle.Render(uploadTime))))
	body = append(body, "")
	body = append(body, tuiPaletteHintStyle.Render(pad("enter download · esc back")))

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
				body = append(body, tuiPaletteSelStyle.Render(pad(line)))
			} else {
				body = append(body, pad(line))
			}
		}
		if more := count - p.offset - visible; more > 0 {
			body = append(body, tuiDimStyle.Render(pad(fmt.Sprintf("… +%d more", more))))
		}
	}

	body = append(body, tuiPaletteHintStyle.Render(
		pad("↑↓ move · ctrl+d remove · ^⏎ send all · esc back")))

	panel := tuiPaletteBoxStyle.Width(inner).Render(strings.Join(body, "\n"))
	if lipgloss.Width(panel) > maxW {
		panel = lipgloss.NewStyle().MaxWidth(maxW).Render(panel)
	}
	return panel
}

func (c chatScreen) pickerDeleteView(maxW, inner int, pad func(string) string) string {
	p := c.picker
	var body []string
	total := len(p.deleteMsgs) + len(p.deleteFiles)
	body = append(body, tuiPickerCrumbStyle.Render(pad(fmt.Sprintf("delete — %d item%s", total, plural(total)))))
	body = append(body, "")
	if p.loading {
		body = append(body, tuiPaletteHintStyle.Render(pad("· loading…")))
	} else if p.notice != "" {
		style := tuiDimStyle
		if total == 0 {
			style = tuiDimStyle
		}
		body = append(body, style.Render(pad("· "+p.notice)))
	}
	// Section: Messages
	body = append(body, tuiDimStyle.Render(pad(fmt.Sprintf("— Messages — %d —", len(p.deleteMsgs)))))
	if len(p.deleteMsgs) == 0 && !p.loading {
		body = append(body, tuiDimStyle.Render(pad("  (none)")))
	} else if len(p.deleteMsgs) == 0 && p.loading {
		body = append(body, tuiDimStyle.Render(pad("  (loading…)")))
	} else {
		// Find visible window that includes cursor if in messages section
		// For simplicity, use p.offset/cursor over combined list, but render per section
		for i, m := range p.deleteMsgs {
			row := i
			// Determine if this row is visible
			if row < p.offset || row >= p.offset+pickerMaxVisible {
				continue
			}
			ts := ""
			if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
				ts = t.Local().Format("15:04")
			}
			preview := m.Text
			if len(preview) > 28 {
				preview = preview[:28] + "…"
			}
			line := fmt.Sprintf("  %s %s", tuiPaletteMatchStyle.Render(preview), tuiDimStyle.Render(ts))
			if row == p.cursor {
				body = append(body, tuiPaletteSelStyle.Render(pad(line)))
			} else {
				body = append(body, pad(line))
			}
		}
	}
	// Section: Files
	body = append(body, tuiDimStyle.Render(pad(fmt.Sprintf("— Files — %d —", len(p.deleteFiles)))))
	if len(p.deleteFiles) == 0 && p.loading {
		body = append(body, tuiDimStyle.Render(pad("  (loading…)")))
	} else if len(p.deleteFiles) == 0 {
		body = append(body, tuiDimStyle.Render(pad("  (none)")))
	} else {
		for i, f := range p.deleteFiles {
			row := len(p.deleteMsgs) + i
			if row < p.offset || row >= p.offset+pickerMaxVisible {
				continue
			}
			ts := ""
			if t, err := time.Parse(time.RFC3339, f.UploadedAt); err == nil {
				ts = t.Local().Format("15:04")
			}
			line := fmt.Sprintf("  %s %s", tuiPaletteMatchStyle.Render(f.Filename), tuiDimStyle.Render(ts))
			if row == p.cursor {
				body = append(body, tuiPaletteSelStyle.Render(pad(line)))
			} else {
				body = append(body, pad(line))
			}
		}
	}
	if more := p.rowCount() - p.offset - pickerMaxVisible; more > 0 {
		body = append(body, tuiDimStyle.Render(pad(fmt.Sprintf("… +%d more", more))))
	}
	body = append(body, tuiPaletteHintStyle.Render(pad("↑↓ move · ctrl+d delete · esc close")))
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

const pickerFooterHints = "↑↓ move · space buffer · v/⇧ range · enter open · a buffer all · ^⏎ send · / filter · esc cancel"
const pickerDlFooterHints = "↑↓ move · ⇧/⇧ range · enter details · / search · esc cancel"

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
