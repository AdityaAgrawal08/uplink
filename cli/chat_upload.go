package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AdityaAgrawal08/uplink-delta/cli/pkg/tarball"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- session file upload engine ----------------------------------------------
//
// Wires the picker's buffered paths into the P2P engine: folders are
// tarballed client-side, then bytes stream E2E-encrypted over the direct
// line (or the inbox fallback for small files). No server storage at any
// step — the old announce/init/PUT/confirm chain is gone.
//
// Jobs run strictly sequentially (one in-flight transfer) so progress lines
// stay readable and the wire sees one file at a time. Folders are tarballed
// client-side; everything else uploads verbatim.

const (
	uploadMaxBytes = int64(25 << 20) // 25 MB per item in v1
	tarballSuffix  = ".tar.gz"
)

// uploadJob is one queued transfer. Path may be a directory (tarballed at
// prepare time); Display is the name receivers will see. To targets a
// private conversation recipient (empty = public/general).
type uploadJob struct {
	Path    string
	Display string
	To      string
}

// uploadState lives on chatScreen and survives across jobs.
type uploadState struct {
	queue   []uploadJob
	active  bool
	cancel  context.CancelFunc
	progCh  chan uploadProgressMsg
	name    string // display name of the in-flight job
	lineIdx int    // transcript row of the in-flight echo (-1 = none)
	conv    string // conversation scope for the in-flight job
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
	display := filepath.Base(job.Path)

	c.uploadQ.active = true
	c.uploadQ.name = display
	// job.To is a raw recipient username ("" = room broadcast); the card
	// paints into the matching conversation bucket.
	conv := convFor(c.me, job.To)
	c.uploadQ.conv = conv
	c.paintUploadLine(tuiUploadRunStyle.Render(
		progressBar("Uploading…", 0, 0)), conv)

	run := func() tea.Msg {
		defer close(progCh)
		disp, size, err := runSessionUpload(ctx, c.eng, job, progCh)
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

// ---- transcript paint --------------------------------------------------------------

var tuiUploadRunStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("214")) // amber while bytes move

// paintUploadLine replaces the in-flight echo row in place, or appends one.
func (c *chatScreen) paintUploadLine(text string, conv string) {
	if conv == "" {
		conv = generalConv
	}
	line := localLine{conv: conv, text: text, kind: lineText}
	if c.uploadQ.lineIdx >= 0 && c.uploadQ.lineIdx < len(c.localLines) &&
		c.localLines[c.uploadQ.lineIdx].conv == conv {
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
	conv := c.uploadQ.conv
	if conv == "" {
		conv = generalConv
	}
	if msg.err == nil {
		// Replace progress line with a styled file attachment card.
		now := time.Now()
		ts := now.Format("15:04")
		rfc := now.Format(time.RFC3339)
		card := localLine{
			conv: conv,
			kind: lineFileCard,
			fileData: &fileCardData{
				// Local FS names can carry escapes too (self-inflicted or
				// synced folders); sanitize like peer filenames.
				filename:  sanitizeDisplay(msg.display),
				username:  c.me,
				size:      humanSize(msg.size),
				time:      ts,
				createdAt: rfc,
			},
		}
		if c.uploadQ.lineIdx >= 0 && c.uploadQ.lineIdx < len(c.localLines) &&
			c.localLines[c.uploadQ.lineIdx].conv == conv {
			c.localLines[c.uploadQ.lineIdx] = card
		} else {
			c.localLines = append(c.localLines, card)
			c.uploadQ.lineIdx = len(c.localLines) - 1
		}
		c.rebuildView()
	} else {
		c.paintUploadLine(tuiErrStyle.Render(
			fmt.Sprintf("✗ upload failed: %s (%v)", msg.display, msg.err)), conv)
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
	conv := c.uploadQ.conv
	if conv == "" {
		conv = generalConv
	}
	c.uploadQ.queue = nil
	c.uploadQ.active = false
	c.uploadQ.cancel = nil
	c.paintUploadLine(tuiSystemStyle.Render(fmt.Sprintf("* upload cancelled: %s", name)), conv)
	c.uploadQ.lineIdx = -1
}

// ---- per-job pipeline -------------------------------------------------------------

// runSessionUpload prepares one job (tarballing directories, enforcing caps)
// and hands the bytes to the engine, which streams them E2E-encrypted over
// the direct line (or the inbox fallback for small files). All failures
// return as errors; the caller annotates the transcript.
func runSessionUpload(ctx context.Context, eng *engine, job uploadJob, prog chan<- uploadProgressMsg) (string, int64, error) {
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

	select {
	case <-ctx.Done():
		return display, 0, ctx.Err()
	default:
	}

	// job.To is a raw recipient username ("" = broadcast); the engine
	// encrypts for exactly that audience.
	return eng.sendFile(job.To, path, display, prog)
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
