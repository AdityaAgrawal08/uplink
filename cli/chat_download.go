package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- session file download engine ---------------------------------------------
//
// Mirrors the upload queue: sequential transfers, in-place progress line,
// failures annotate without blocking the rest. Files land in ~/Downloads
// (created on demand); name collisions get " (1)", " (2)"… suffixes.

type dlJob struct {
	FileId   string
	Filename string
	Size     int64
}

type dlState struct {
	queue   []dlJob
	active  bool
	cancel  context.CancelFunc
	progCh  chan dlProgressMsg
	name    string
	lineIdx int
	conv    string // conversation scope for the in-flight download
}

func (d *dlState) isActive() bool { return d.active }

// ---- messages -------------------------------------------------------------------

type dlProgressMsg struct {
	done  int64
	total int64
}

type dlDrainMsg struct{}

type dlDoneMsg struct {
	filename string
	err      error
}

// ---- client call ------------------------------------------------------------------

// fetchDownloadURL exchanges a fileId for a short-lived presigned URL.
func (c *chatClient) fetchDownloadURL(fileId string) (string, error) {
	code, body, err := postJSON(c.endpoint("/download/"+urlQueryEscape(fileId)),
		map[string]any{}, c.authHeaders())
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("status %d: %s", code, truncateStringPlain(string(body), 120))
	}
	var r struct {
		DownloadUrl string `json:"downloadUrl"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	if r.DownloadUrl == "" {
		return "", fmt.Errorf("server returned no download url")
	}
	return r.DownloadUrl, nil
}

// ---- queue control -----------------------------------------------------------------

func (c *chatScreen) startDownloads(jobs []dlJob) tea.Cmd {
	c.dlQ.queue = append(c.dlQ.queue, jobs...)
	// Set conversation scope from current context.
	conv := generalConv
	if c.targetUser != "" {
		conv = conversationKey(c.me, c.targetUser)
	}
	c.dlQ.conv = conv
	if c.dlQ.active {
		return nil
	}
	return c.startNextDownload()
}

func (c *chatScreen) startNextDownload() tea.Cmd {
	if len(c.dlQ.queue) == 0 || c.dlQ.active {
		return nil
	}
	job := c.dlQ.queue[0]
	c.dlQ.queue = c.dlQ.queue[1:]

	ctx, cancel := context.WithCancel(context.Background())
	c.dlQ.cancel = cancel

	progCh := make(chan dlProgressMsg, 32)
	c.dlQ.progCh = progCh
	client := c.client

	c.dlQ.active = true
	c.dlQ.name = job.Filename
	c.paintDlLine(tuiDownloadRunStyle.Render(
		progressBar("Downloading…", 0, 0)), c.dlQ.conv)

	run := func() tea.Msg {
		defer close(progCh)
		err := runSessionDownload(ctx, client, job, progCh)
		return dlDoneMsg{filename: job.Filename, err: err}
	}
	return tea.Batch(run, drainDlProgressCmd(progCh))
}

func drainDlProgressCmd(ch <-chan dlProgressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return dlDrainMsg{}
		}
		return msg
	}
}

// ---- pipeline ------------------------------------------------------------------------

// downloadsDir is the user's download folder, created on demand.
func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	dir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// uniquePath avoids clobbering: report.pdf -> report (1).pdf -> report (2).pdf…
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return p // free
	}
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	for i := 1; ; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}

// runSessionDownload fetches one file into ~/Downloads.
//
// B49 FIX: stream into a `.part` temp file and rename on success, removing
// the temp on failure. Previously a failed/cancelled download left a corrupt
// partial file at the destination, and a retry minted " (1)" duplicates.
// B50 FIX: sanitize the server-provided filename with filepath.Base so a
// malicious `../../evil` name cannot escape ~/Downloads.
func runSessionDownload(ctx context.Context, client *chatClient, job dlJob, prog chan<- dlProgressMsg) error {
	url, err := client.fetchDownloadURL(job.FileId)
	if err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	dir, err := downloadsDir()
	if err != nil {
		return err
	}
	safeName := filepath.Base(job.Filename)
	if safeName == "" || safeName == "." || safeName == "/" {
		safeName = "file"
	}
	dest := uniquePath(dir, safeName)
	tmp := dest + ".part"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncateStringPlain(string(body), 120))
	}

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	pr := &progressReader{r: resp.Body, total: job.Size, onProg: func(done, total int64) {
		select {
		case prog <- dlProgressMsg{done: done, total: total}:
		default:
		}
	}}
	_, copyErr := io.Copy(f, pr)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write: %w", copyErr)
	}
	if syncErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("sync: %w", syncErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("close: %w", closeErr)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// ---- transcript paint ------------------------------------------------------------------

var tuiDownloadRunStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("39")) // blue while bytes arrive

func (c *chatScreen) paintDlLine(text string, conv string) {
	if conv == "" {
		conv = generalConv
	}
	line := localLine{conv: conv, text: text}
	if c.dlQ.lineIdx >= 0 && c.dlQ.lineIdx < len(c.localLines) &&
		c.localLines[c.dlQ.lineIdx].conv == conv {
		c.localLines[c.dlQ.lineIdx] = line
	} else {
		c.localLines = append(c.localLines, line)
		c.dlQ.lineIdx = len(c.localLines) - 1
	}
	c.rebuildView()
}

func (c *chatScreen) settleDownloadDone(msg dlDoneMsg) tea.Cmd {
	c.dlQ.active = false
	c.dlQ.cancel = nil
	conv := c.dlQ.conv
	if conv == "" {
		conv = generalConv
	}
	dir, _ := downloadsDir()
	if msg.err == nil {
		c.paintDlLine(tuiSystemStyle.Render(
			fmt.Sprintf("* saved %s → %s", msg.filename, dir)), conv)
	} else {
		c.paintDlLine(tuiErrStyle.Render(
			fmt.Sprintf("✗ download failed: %s (%v)", msg.filename, msg.err)), conv)
	}
	c.dlQ.lineIdx = -1
	if len(c.dlQ.queue) > 0 {
		return c.startNextDownload()
	}
	return nil
}

func (c *chatScreen) cancelDownloads() {
	if c.dlQ.cancel != nil {
		c.dlQ.cancel()
	}
	name := c.dlQ.name
	conv := c.dlQ.conv
	if conv == "" {
		conv = generalConv
	}
	c.dlQ.queue = nil
	c.dlQ.active = false
	c.dlQ.cancel = nil
	c.paintDlLine(tuiSystemStyle.Render(fmt.Sprintf("* download cancelled: %s", name)), conv)
	c.dlQ.lineIdx = -1
}
