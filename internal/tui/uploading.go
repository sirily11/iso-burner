package tui

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/upload"
)

// uploadRows is how many files the upload progress list shows at once.
const uploadRows = 6

// uploadRun is an upload of the reviewed files to the chosen item.
type uploadRun struct {
	job        upload.Job
	progress   *upload.Progress
	statuses   []upload.FileStatus
	cancel     context.CancelFunc
	running    bool
	cancelling bool
	started    time.Time
	elapsed    time.Duration
	offset     int
	err        error            // why the upload stopped early
	sync       *remote.Reporter // nil unless upload progress is synced
}

type uploadTickMsg struct{}

type uploadDoneMsg struct{ err error }

func uploadTick() tea.Cmd {
	return tea.Tick(genTickInterval, func(time.Time) tea.Msg { return uploadTickMsg{} })
}

// startUploadRun uploads the reviewed files in the background.
func (m Model) startUploadRun() (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	ctx, cancel := context.WithCancel(context.Background())
	job := upload.Job{
		Client: m.sync,
		ItemID: m.itemSearch.chosen.ID,
		Files:  f.matched,
	}
	if f.rule == ruleISO {
		job.ISO = f.iso
	} else {
		job.Folder = f.folder
	}
	prog := upload.NewProgress(f.matched)
	f.stage = uploadStageRunning
	f.run = uploadRun{job: job, progress: prog, statuses: prog.Snapshot(), cancel: cancel, running: true, started: time.Now(),
		sync: m.newReporter(remote.NewJobID())}
	m.reportUpload()
	return m, tea.Batch(runUpload(ctx, job, prog), uploadTick())
}

func runUpload(ctx context.Context, job upload.Job, prog *upload.Progress) tea.Cmd {
	return func() tea.Msg { return uploadDoneMsg{err: job.Run(ctx, prog)} }
}

// retryUpload queues the failed files to be uploaded again. A running upload
// picks them up after the files queued ahead of them; a finished one runs
// again for just those files.
func (m Model) retryUpload() (tea.Model, tea.Cmd) {
	r := &m.uploadFiles.run
	if r.cancelling || r.progress.RetryFailed() == 0 {
		return m, nil
	}
	r.statuses = r.progress.Snapshot()
	m.reportUpload()
	if r.running {
		return m, nil
	}
	return m, m.resumeUpload()
}

// resumeUpload runs the finished upload again for the files queued in it.
func (m *Model) resumeUpload() tea.Cmd {
	r := &m.uploadFiles.run
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.running, r.err = cancel, true, nil
	return tea.Batch(runUpload(ctx, r.job, r.progress), uploadTick())
}

// updateUploadRun handles progress and completion messages of the upload.
func (m Model) updateUploadRun(msg tea.Msg) (tea.Model, tea.Cmd) {
	r := &m.uploadFiles.run
	switch msg := msg.(type) {
	case uploadTickMsg:
		if !r.running {
			return m, nil
		}
		r.statuses, r.elapsed = r.progress.Snapshot(), time.Since(r.started)
		m.reportUpload()
		return m, tea.Batch(uploadTick(), m.refreshContents())
	case uploadDoneMsg:
		r.running, r.err = false, msg.err
		r.statuses, r.elapsed = r.progress.Snapshot(), time.Since(r.started)
		r.cancel()
		if r.cancelling {
			m.cancelled = true
		} else if msg.err == nil && r.progress.Pending() > 0 {
			// Files were retried just as the upload finished.
			cmd := m.resumeUpload()
			m.reportUpload()
			return m, cmd
		}
		m.reportUpload()
		if m.cancelled {
			return m, tea.Quit
		}
		return m, m.countContents()
	}
	return m, nil
}

// updateUploadRunKey handles keys during and after the upload. ctrl+c stops a
// running upload after the current step; once it has finished, enter exits.
func (m Model) updateUploadRunKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := &m.uploadFiles.run
	switch key.String() {
	case "ctrl+c":
		if !r.running || r.cancelling {
			m.cancelled = r.running
			m.reportUpload()
			return m, tea.Quit
		}
		r.cancelling = true
		r.cancel()
		m.reportUpload()
		return m, nil
	case "enter", "q", "esc":
		if !r.running {
			return m, tea.Quit
		}
	case "r":
		return m.retryUpload()
	case "up", "k":
		r.offset--
	case "down", "j":
		r.offset++
	case "pgup":
		r.offset -= uploadRows
	case "pgdown":
		r.offset += uploadRows
	}
	r.offset = max(0, min(r.offset, len(r.statuses)-uploadRows))
	return m, nil
}

// uploadOrder lists file indexes with active uploads first, then failed ones
// and ones waiting to be retried, then queued and finished ones, each group
// in upload order.
func uploadOrder(statuses []upload.FileStatus) []int {
	rank := func(s upload.FileStatus) int {
		switch s.Stage {
		case upload.StageExtracting, upload.StagePreparing, upload.StageUploading:
			return 0
		case upload.StageFailed:
			return 1
		case upload.StageQueued:
			if s.Err != nil {
				return 1 // waiting to be retried
			}
			return 2
		}
		return 3
	}
	order := make([]int, len(statuses))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return rank(statuses[order[a]]) < rank(statuses[order[b]])
	})
	return order
}

func (m Model) uploadRunView() string {
	f, r := m.uploadFiles, m.uploadFiles.run
	var b strings.Builder
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(m.barWidth()))

	counts := map[upload.Stage]int{}
	retrying := 0
	var overall float64
	for _, s := range r.statuses {
		counts[s.Stage]++
		if s.Retrying() {
			retrying++
		}
		overall += s.Completion()
	}
	overall /= float64(max(1, len(r.statuses)))
	active := counts[upload.StageExtracting] + counts[upload.StagePreparing] + counts[upload.StageUploading]
	failed := counts[upload.StageFailed]

	item := itemLabel(*m.itemSearch.chosen)
	switch {
	case r.cancelling && r.running:
		b.WriteString(errorStyle.Render("Cancelling… finishing the current step") + "\n")
	case r.running:
		b.WriteString(labelStyle.Render(fmt.Sprintf("Uploading %d file(s) to ", len(f.matched))) + item + "\n")
	case r.err != nil:
		b.WriteString(errorStyle.Render("✗ Upload stopped: "+r.err.Error()) + "\n")
	case failed > 0:
		b.WriteString(errorStyle.Render(fmt.Sprintf("✗ Uploaded %d of %d file(s); %d failed",
			counts[upload.StageDone], len(f.matched), failed)) + dimStyle.Render("  (press r to retry them)") + "\n")
	default:
		b.WriteString(okStyle.Render(fmt.Sprintf("✓ Uploaded %d file(s) to ", len(f.matched))) + item + "\n")
	}
	b.WriteString(fmt.Sprintf("%s %3.0f%%  %d of %d files\n", bar.ViewAs(overall), overall*100,
		counts[upload.StageDone]+failed, len(r.statuses)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d done · %d active · %d queued · %d retrying · %d failed · %s elapsed",
		counts[upload.StageDone], active, counts[upload.StageQueued]-retrying, retrying, failed, r.elapsed.Round(time.Second))) + "\n")
	b.WriteString(dimStyle.Render("Item has "+m.contentCountLabel()) + "\n")
	if line := m.syncStatusLine(r.sync); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	const nameWidth = 32
	order := uploadOrder(r.statuses)
	end := min(r.offset+uploadRows, len(order))
	for _, i := range order[r.offset:end] {
		s := r.statuses[i]
		name := truncate(path.Base(f.matched[i].RelPath), nameWidth)
		var state string
		switch s.Stage {
		case upload.StageQueued:
			if s.Retrying() {
				attempts := cmp.Or(r.job.Attempts, upload.DefaultAttempts)
				state = errorStyle.Render(fmt.Sprintf("↻ retry %d of %d: ", s.Tries+1, attempts)) +
					dimStyle.Render(truncate(s.Err.Error(), 40))
			} else {
				state = dimStyle.Render(s.Label())
			}
		case upload.StageDone:
			state = okStyle.Render("✓ " + s.Label())
		case upload.StageFailed:
			state = errorStyle.Render("✗ " + truncate(s.Err.Error(), 50))
			if s.Tries > 1 {
				state += dimStyle.Render(fmt.Sprintf(" (%d tries)", s.Tries))
			}
		default:
			state = s.Label() + "…"
		}
		b.WriteString(fmt.Sprintf("  %-*s %s %3.0f%%  %s\n", nameWidth, name, bar.ViewAs(s.Fraction), s.Fraction*100, state))
	}
	if len(order) > uploadRows {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing files %d–%d of %d", r.offset+1, end, len(order))) + "\n")
	}
	return b.String()
}

func (m Model) uploadRunHelp() string {
	r := m.uploadFiles.run
	retry := ""
	for _, s := range r.statuses {
		if s.Stage == upload.StageFailed {
			retry = " · r: retry failed"
			break
		}
	}
	switch {
	case r.running && r.cancelling:
		return "ctrl+c: force quit"
	case r.running:
		return "↑/↓: scroll" + retry + " · ctrl+c: stop"
	}
	return "↑/↓: scroll" + retry + " · enter: exit"
}

// UploadResult reports how an upload ended: how many files were uploaded and
// how many failed, and why it stopped early. ok is false when no upload ran
// or it was cancelled.
func (m Model) UploadResult() (done, failed, total int, err error, ok bool) {
	r := m.uploadFiles.run
	if m.mode != ModeUpload || r.progress == nil || m.cancelled {
		return 0, 0, 0, nil, false
	}
	for _, s := range r.progress.Snapshot() {
		switch s.Stage {
		case upload.StageDone:
			done++
		case upload.StageFailed:
			failed++
		}
	}
	return done, failed, len(m.uploadFiles.matched), r.err, true
}
