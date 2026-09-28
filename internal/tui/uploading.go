package tui

import (
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
	f.run = uploadRun{progress: prog, statuses: prog.Snapshot(), cancel: cancel, running: true, started: time.Now(),
		sync: m.newReporter(remote.NewJobID())}
	m.reportUpload()
	run := func() tea.Msg { return uploadDoneMsg{err: job.Run(ctx, prog)} }
	return m, tea.Batch(run, uploadTick())
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
		return m, uploadTick()
	case uploadDoneMsg:
		r.running, r.err = false, msg.err
		r.statuses, r.elapsed = r.progress.Snapshot(), time.Since(r.started)
		r.cancel()
		if r.cancelling {
			m.cancelled = true
		}
		m.reportUpload()
		if m.cancelled {
			return m, tea.Quit
		}
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

// uploadOrder lists file indexes with active uploads first, then failed,
// queued and finished ones, each group in upload order.
func uploadOrder(statuses []upload.FileStatus) []int {
	rank := func(s upload.Stage) int {
		switch s {
		case upload.StageExtracting, upload.StagePreparing, upload.StageUploading:
			return 0
		case upload.StageFailed:
			return 1
		case upload.StageQueued:
			return 2
		}
		return 3
	}
	order := make([]int, len(statuses))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return rank(statuses[order[a]].Stage) < rank(statuses[order[b]].Stage)
	})
	return order
}

func (m Model) uploadRunView() string {
	f, r := m.uploadFiles, m.uploadFiles.run
	var b strings.Builder
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(m.barWidth()))

	counts := map[upload.Stage]int{}
	var overall float64
	for _, s := range r.statuses {
		counts[s.Stage]++
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
			counts[upload.StageDone], len(f.matched), failed)) + "\n")
	default:
		b.WriteString(okStyle.Render(fmt.Sprintf("✓ Uploaded %d file(s) to ", len(f.matched))) + item + "\n")
	}
	b.WriteString(fmt.Sprintf("%s %3.0f%%  %d of %d files\n", bar.ViewAs(overall), overall*100,
		counts[upload.StageDone]+failed, len(r.statuses)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d done · %d active · %d queued · %d failed · %s elapsed",
		counts[upload.StageDone], active, counts[upload.StageQueued], failed, r.elapsed.Round(time.Second))) + "\n")
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
			state = dimStyle.Render(s.Label())
		case upload.StageDone:
			state = okStyle.Render("✓ " + s.Label())
		case upload.StageFailed:
			state = errorStyle.Render("✗ " + truncate(s.Err.Error(), 50))
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
	switch {
	case r.running && r.cancelling:
		return "ctrl+c: force quit"
	case r.running:
		return "↑/↓: scroll · ctrl+c: stop"
	}
	return "↑/↓: scroll · enter: exit"
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
