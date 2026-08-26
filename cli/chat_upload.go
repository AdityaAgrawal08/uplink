package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AdityaAgrawal08/uplink-delta/cli/pkg/tarball"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- session file upload engine ----------------------------------------------
//
// Wires the picker's buffered paths into the server's session-file contract:
//
//	announce {filename,size,sha256} -> {fileId,shareId}
//	share/init (with that shareId)  -> presigned PUT url
//	PUT bytes                       -> storage
//	share/{id}/confirm              -> share goes live
//	session upload-complete         -> status flips to UPLOADED
//
// Jobs run strictly sequentially (one in-flight transfer) so progress lines
// stay readable and the wire sees one file at a time. Folders are tarballed
// client-side; everything else uploads verbatim.

const (
	uploadMaxBytes = int64(25 << 20) // 25 MB per item in v1
	tarballSuffix  = ".tar.gz"
)

// uploadJob is one queued transfer. Path may be a directory (tarballed at
// prepare time); Display is the name receivers will see.
type uploadJob struct {
	Path    string
	Display string
}

// uploadState lives on chatScreen and survives across jobs.
type uploadState struct {
	queue   []uploadJob
	active  bool
	cancel  context.CancelFunc
	progCh  chan uploadProgressMsg
	name    string // display name of the in-flight job
	lineIdx int    // transcript row of the in-flight echo (-1 = none)
}

func (u *uploadState) isActive() bool { return u.active }

// ---- messages ------------------------------------------------------------------

// uploadProgressMsg carries cumulative bytes for the in-flight job.
type uploadProgressMsg struct {
	done  int64
	total int64
}

// uploadDrainMsg arrives when the progress channel closes (job finished
// reporting); Update stops rescheduling the drain at that point.
type uploadDrainMsg struct{}

type uploadDoneMsg struct {
	display string
	size    int64
	err     error
}

// filesFetchedMsg lands after each room-files poll.
type filesFetchedMsg struct {
	files []sessionFile
	err   error
}

// ---- queue control ---------------------------------------------------------------

// startUploads enqueues jobs and kicks the first transfer.
func (c *chatScreen) startUploads(jobs []uploadJob) tea.Cmd {
	c.uploadQ.queue = append(c.uploadQ.queue, jobs...)
	if c.uploadQ.active {
		return nil // already running; this batch rides behind the current one
	}
	return c.startNextUpload()
}

// startNextUpload promotes the head of the queue into an async transfer.
func (c *chatScreen) startNextUpload() tea.Cmd {
	if len(c.uploadQ.queue) == 0 || c.uploadQ.active {
		return nil
	}
	job := c.uploadQ.queue[0]
	c.uploadQ.queue = c.uploadQ.queue[1:]

	ctx, cancel := context.WithCancel(context.Background())
	c.uploadQ.cancel = cancel

	progCh := make(chan uploadProgressMsg, 32)
	c.uploadQ.progCh = progCh
	client := c.client
	me := c.me
	display := filepath.Base(job.Path)

	c.uploadQ.active = true
	c.uploadQ.name = display
	c.paintUploadLine(tuiUploadRunStyle.Render(fmt.Sprintf("[↑] %s …", display)))

	run := func() tea.Msg {
		defer close(progCh)
		disp, size, err := runSessionUpload(ctx, client, me, job, progCh)
		return uploadDoneMsg{display: disp, size: size, err: err}
	}

	return tea.Batch(
		run,
		drainUploadProgressCmd(progCh),
	)
}

// drainUploadProgressCmd relays ONE progress sample per Update cycle; Update
// reschedules until the channel closes.
func drainUploadProgressCmd(ch <-chan uploadProgressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return uploadDrainMsg{}
		}
		return msg
	}
}

// ---- session-file HTTP client ----------------------------------------------------

type announceResponse struct {
	FileId  string `json:"fileId"`
	ShareId string `json:"shareId"`
}

// announceFile registers an intent-to-upload and mints {fileId, shareId}.
func (c *chatClient) announceFile(filename string, size int64, shaHex string) (string, string, error) {
	code, body, err := postJSON(c.endpoint("/announce"),
		map[string]any{"filename": filename, "size": size, "sha256": shaHex},
		c.authHeaders())
	if err != nil {
		return "", "", err
	}
	if code != 201 && code != 200 {
		return "", "", fmt.Errorf("status %d: %s", code, truncateStringPlain(string(body), 120))
	}
	var r announceResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", err
	}
	if r.FileId == "" || r.ShareId == "" {
		return "", "", fmt.Errorf("server returned empty ids")
	}
	return r.FileId, r.ShareId, nil
}

// completeUpload flips the announced file to UPLOADED after bytes land.
func (c *chatClient) completeUpload(fileId, shareId string) error {
	code, body, err := postJSON(c.endpoint("/upload-complete"),
		map[string]any{"fileId": fileId, "shareId": shareId},
		c.authHeaders())
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("status %d: %s", code, truncateStringPlain(string(body), 120))
	}
	return nil
}

// fetchFiles lists room files uploaded after `since` (RFC3339; "" = all).
func (c *chatClient) fetchFiles(since string) ([]sessionFile, error) {
	url := c.endpoint("/files")
	if since != "" {
		url += "?since=" + urlQueryEscape(since)
	}
	code, body, err := getJSON(url, c.authHeaders())
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("status %d: %s", code, truncateStringPlain(string(body), 120))
	}
	var r struct {
		Files []sessionFile `json:"files"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Files, nil
}

// doFetchFiles polls the room file list (receiver side).
func (c *chatScreen) doFetchFiles() tea.Cmd {
	client := c.client
	since := c.lastFilesAt
	return func() tea.Msg {
		files, err := client.fetchFiles(since)
		return filesFetchedMsg{files: files, err: err}
	}
}

// filesListMsg fills the /download drawer (full listing, no watermark).
type filesListMsg struct {
	files []sessionFile
	err   error
}

func (c *chatScreen) doFetchAllFiles() tea.Cmd {
	client := c.client
	return func() tea.Msg {
		files, err := client.fetchFiles("")
		return filesListMsg{files: files, err: err}
	}
}

// applyRoomFiles paints unseen UPLOADED files as system announcements.
func (c *chatScreen) applyRoomFiles(files []sessionFile) {
	for _, f := range files {
		if f.Status != "UPLOADED" {
			continue // ANNOUNCED/FAILED never announce
		}
		if c.filesSeen[f.FileId] {
			continue
		}
		c.filesSeen[f.FileId] = true
		if f.UploadedAt > c.lastFilesAt {
			c.lastFilesAt = f.UploadedAt
		}
		if f.Username == c.me {
			continue // own uploads already painted by the engine
		}
		c.appendLocal(generalConv, tuiSystemStyle.Render(
			fmt.Sprintf("* %s shared %s (%s)", f.Username, f.Filename, humanSize(f.Size))))
	}
}

// ---- transcript paint --------------------------------------------------------------

var tuiUploadRunStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("214")) // amber while bytes move

// paintUploadLine replaces the in-flight echo row in place, or appends one.
func (c *chatScreen) paintUploadLine(text string) {
	line := localLine{conv: generalConv, text: text}
	if c.uploadQ.lineIdx >= 0 && c.uploadQ.lineIdx < len(c.localLines) &&
		c.localLines[c.uploadQ.lineIdx].conv == generalConv {
		c.localLines[c.uploadQ.lineIdx] = line
	} else {
		c.localLines = append(c.localLines, line)
		c.uploadQ.lineIdx = len(c.localLines) - 1
	}
	c.rebuildView()
}

// settleUploadDone annotates the finished transfer and promotes the next job.
func (c *chatScreen) settleUploadDone(msg uploadDoneMsg) tea.Cmd {
	c.uploadQ.active = false
	c.uploadQ.cancel = nil
	if msg.err == nil {
		c.paintUploadLine(tuiSystemStyle.Render(
			fmt.Sprintf("* you shared %s (%s)", msg.display, humanSize(msg.size))))
	} else {
		c.paintUploadLine(tuiErrStyle.Render(
			fmt.Sprintf("✗ upload failed: %s (%v)", msg.display, msg.err)))
	}
	c.uploadQ.lineIdx = -1 // next job paints its own row
	if len(c.uploadQ.queue) > 0 {
		return c.startNextUpload()
	}
	return nil
}

// cancelUploads stops the in-flight transfer and drops everything queued.
func (c *chatScreen) cancelUploads() {
	if c.uploadQ.cancel != nil {
		c.uploadQ.cancel()
	}
	name := c.uploadQ.name
	c.uploadQ.queue = nil
	c.uploadQ.active = false
	c.uploadQ.cancel = nil
	c.paintUploadLine(tuiSystemStyle.Render(fmt.Sprintf("* upload cancelled: %s", name)))
	c.uploadQ.lineIdx = -1
}

// ---- per-job pipeline -------------------------------------------------------------

// runSessionUpload executes the full announce->bytes->complete sequence for
// one job. All failures return as errors; the caller annotates the transcript.
func runSessionUpload(ctx context.Context, client *chatClient, me string, job uploadJob, prog chan<- uploadProgressMsg) (string, int64, error) {
	path := job.Path
	display := filepath.Base(path)

	info, err := os.Stat(path)
	if err != nil {
		return display, 0, fmt.Errorf("stat %s: %w", display, err)
	}

	var tmpTar string
	isDir := info.IsDir()
	if isDir {
		tmpTar, err = tarballDir(path)
		if err != nil {
			return display, 0, fmt.Errorf("folder prep: %w", err)
		}
		defer os.Remove(tmpTar)
		path = tmpTar
		display = filepath.Base(path)
		info, err = os.Stat(path)
		if err != nil {
			return display, 0, err
		}
	}

	if info.Size() == 0 {
		return display, 0, fmt.Errorf("%s is empty", display)
	}
	if info.Size() > uploadMaxBytes {
		return display, 0, fmt.Errorf("%s is %s — v1 cap is %s",
			display, humanSize(info.Size()), humanSize(uploadMaxBytes))
	}

	sum, err := sha256File(ctx, path)
	if err != nil {
		return display, 0, fmt.Errorf("hash: %w", err)
	}

	// 1. announce within the session
	fileId, shareId, err := client.announceFile(display, info.Size(), sum)
	if err != nil {
		return display, 0, fmt.Errorf("announce: %w", err)
	}

	// 2-4. bytes through the share pipeline under OUR shareId
	err = putShareBytes(ctx, client.serverURL, path, display, info.Size(), sum, shareId, prog)
	if err != nil {
		return display, 0, err
	}

	// 5. flip session_files status to UPLOADED
	if err := client.completeUpload(fileId, shareId); err != nil {
		return display, 0, fmt.Errorf("complete: %w", err)
	}
	return display, info.Size(), nil
}

// sha256File streams the file through SHA-256 honouring cancellation.
func sha256File(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// tarballDir packs dir into a temp <name>.tar.gz preserving the full
// directory tree (subdirectories included). Uses the shared tarball.Pack
// which also excludes .git, .env, and other standard junk.
func tarballDir(dir string) (string, error) {
	base := filepath.Base(dir)
	out := filepath.Join(os.TempDir(), fmt.Sprintf("uplink-%d-%s%s",
		time.Now().UnixNano(), base, tarballSuffix))

	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if err := tarball.Pack(dir, f); err != nil {
		os.Remove(out)
		return "", err
	}
	return out, nil
}

// putShareBytes runs share/init (reusing our announced shareId), PUTs the
// single-part payload, then confirms the share. Session v1 caps sizes below
// the multipart threshold so exactly one PUT happens here.
func putShareBytes(ctx context.Context, serverURL, path, display string, size int64, sumHex, shareId string, prog chan<- uploadProgressMsg) error {
	mimeType := mimeByExt(path)

	initReq := InitRequest{
		Filename:  display,
		Size:      size,
		MimeType:  mimeType,
		HashValue: sumHex,
		ShareId:   shareId,
	}
	jsonBytes, err := json.Marshal(initReq)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", serverURL+"/api/v1/share/init", bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("init: status %d: %s", resp.StatusCode, truncateStringPlain(string(body), 120))
	}
	var initResp InitResponse
	if err := json.NewDecoder(resp.Body).Decode(&initResp); err != nil {
		return fmt.Errorf("init decode: %w", err)
	}
	if initResp.UploadUrl == "" {
		return fmt.Errorf("init returned no single-part URL")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	putReq, err := http.NewRequestWithContext(ctx, "PUT", initResp.UploadUrl, &progressReader{r: f, total: size, onProg: func(done, total int64) {
		select {
		case prog <- uploadProgressMsg{done: done, total: total}:
		default:
		}
	}})
	if err != nil {
		return err
	}
	putReq.ContentLength = size
	putReq.Header.Set("Content-Type", mimeType)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != 200 && putResp.StatusCode != 204 {
		body, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("upload: status %d: %s", putResp.StatusCode, truncateStringPlain(string(body), 120))
	}

	confirmURL := fmt.Sprintf("%s/api/v1/share/%s/confirm", serverURL, shareId)
	conf, err := http.NewRequestWithContext(ctx, "POST", confirmURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	conf.Header.Set("Content-Type", "application/json")
	confResp, err := http.DefaultClient.Do(conf)
	if err != nil {
		return fmt.Errorf("confirm: %w", err)
	}
	defer confResp.Body.Close()
	if confResp.StatusCode != 200 {
		body, _ := io.ReadAll(confResp.Body)
		return fmt.Errorf("confirm: status %d: %s", confResp.StatusCode, truncateStringPlain(string(body), 120))
	}
	return nil
}

// progressReader reports cumulative bytes through onProg (throttled to
// ~every 512KB or at EOF). Callbacks must not block; senders use select.
type progressReader struct {
	r        io.Reader
	total    int64
	n        int64
	lastEmit int64
	onProg   func(done, total int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.n += int64(n)
	if p.n-p.lastEmit >= 512<<10 || err == io.EOF {
		p.lastEmit = p.n
		if p.onProg != nil {
			p.onProg(p.n, p.total)
		}
	}
	return n, err
}

func mimeByExt(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return t
	}
	return "application/octet-stream"
}
