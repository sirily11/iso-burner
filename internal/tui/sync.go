package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sirily11/iso-burner/internal/burn"
	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/store"
)

// syncing reports whether progress should be sent to rxstorage: a server is
// configured and the user is signed in.
func (m Model) syncing() bool {
	return m.sync != nil && m.user != nil
}

// newReporter starts syncing job id, or returns nil when not syncing.
func (m Model) newReporter(id string) *remote.Reporter {
	if !m.syncing() {
		return nil
	}
	return remote.NewReporter(m.sync, id, m.syncInterval)
}

// CloseSync delivers the final progress of the generate or burn job, waiting
// at most timeout. Call it after the program exits.
func (m Model) CloseSync(timeout time.Duration) error {
	return errors.Join(m.genSync.Close(timeout), m.burnSync.Close(timeout))
}

// syncStatusLine says whether progress is reaching rxstorage.
func (m Model) syncStatusLine(r *remote.Reporter) string {
	if r == nil {
		if m.sync != nil && m.user == nil && m.auth != nil {
			return dimStyle.Render("Sign in from the start screen to follow progress in rxstorage")
		}
		return ""
	}
	sent, err := r.Status()
	switch {
	case err != nil:
		return errorStyle.Render("⚠ Not synced to rxstorage: " + truncate(err.Error(), 60))
	case sent.IsZero():
		return dimStyle.Render("⇅ Syncing progress to rxstorage…")
	}
	return dimStyle.Render("⇅ Progress synced to rxstorage " + sent.Format("15:04:05"))
}

func finishedAt(done bool) *time.Time {
	if !done {
		return nil
	}
	t := time.Now()
	return &t
}

// reportGenerate sends the current generation progress.
func (m Model) reportGenerate() {
	if m.genSync != nil {
		m.genSync.Report(m.generateJob())
	}
}

// generateJob snapshots ISO generation for rxstorage.
func (m Model) generateJob() remote.Job {
	job := remote.Job{
		Kind:       remote.KindGenerate,
		Title:      m.result.ISOName,
		HostName:   m.hostName,
		StartedAt:  m.genStarted,
		TotalCount: len(m.chunks),
		Message:    "Writing to " + m.outputDir,
		Tasks:      make([]remote.Task, 0, len(m.chunks)),
	}
	for i, c := range m.chunks {
		var s iso.ChunkStatus
		if i < len(m.genStatuses) {
			s = m.genStatuses[i]
		}
		copied := min(s.Copied, c.Size)
		t := remote.Task{
			Section:    remote.SectionISO,
			Name:       c.Name,
			Status:     s.Stage.String(),
			Detail:     fmt.Sprintf("%d file(s)", len(c.Pieces)),
			Progress:   fraction(copied, c.Size),
			DoneBytes:  copied,
			TotalBytes: c.Size,
		}
		switch s.Stage {
		case iso.StageQueued:
			t.Progress = 0
		case iso.StageDone:
			t.Progress, t.DoneBytes = 1, c.Size
			job.DoneCount++
		case iso.StageFailed:
			if s.Err != nil {
				t.Error = s.Err.Error()
			}
		}
		job.DoneBytes += t.DoneBytes
		job.TotalBytes += c.Size
		job.Tasks = append(job.Tasks, t)
	}
	job.Progress = fraction(job.DoneBytes, job.TotalBytes)

	switch {
	case m.generating && !m.genCancelling:
		job.Status = remote.StatusRunning
	case m.genCancelling || m.cancelled:
		job.Status = remote.StatusCancelled
		job.Message = "Cancelled"
	case m.genErr != nil:
		job.Status = remote.StatusFailed
		job.Error = m.genErr.Error()
	default:
		job.Status, job.Progress, job.Message = remote.StatusCompleted, 1, "Created in "+m.outputDir
	}
	job.FinishedAt = finishedAt(job.Status != remote.StatusRunning)
	return job
}

// burnJobID keeps a burn session on the same rxstorage job when resumed.
func (m Model) burnJobID() string {
	return remote.StableJobID(m.hostName, m.dbPath, strconv.FormatInt(m.burnSession, 10))
}

// refreshSyncDiscs reloads the session's discs for the per-ISO rows, at most
// once per sync interval since they come from the database.
func (m *Model) refreshSyncDiscs(force bool) {
	if m.burnSync == nil || m.store == nil {
		return
	}
	if !force && time.Since(m.syncDiscsAt) < m.syncInterval {
		return
	}
	if discs, err := m.store.Discs(context.Background(), m.burnSession); err == nil {
		m.syncDiscs, m.syncDiscsAt = discs, time.Now()
	}
}

// reportBurn sends the current burning progress.
func (m Model) reportBurn() {
	if m.burnSync != nil {
		m.burnSync.Report(m.burnJob())
	}
}

// discFraction is how far a disc is, counting burning as the first half and
// checking as the second, like driveFraction.
func discFraction(status store.DiscStatus, progress, total int64) float64 {
	p := fraction(progress, total)
	switch status {
	case store.DiscDone:
		return 1
	case store.DiscBurning:
		return p / 2
	case store.DiscVerifying:
		return 0.5 + p/2
	}
	return 0
}

// burnJob snapshots a burn session for rxstorage.
func (m Model) burnJob() remote.Job {
	s := m.burnSnap
	job := remote.Job{
		Kind:       remote.KindBurn,
		HostName:   m.hostName,
		StartedAt:  m.burnStarted,
		DoneCount:  s.Done,
		TotalCount: s.Total,
		Tasks:      make([]remote.Task, 0, len(s.Drives)),
	}

	// Live progress of the discs in the drives beats the saved progress.
	live := map[int64]burn.DriveStatus{}
	waiting := 0
	for _, d := range s.Drives {
		t := remote.Task{
			Section:    remote.SectionDrive,
			Name:       d.Drive.Name(),
			Status:     string(d.State),
			Progress:   driveFraction(d),
			DoneBytes:  d.Progress,
			TotalBytes: d.Total,
			Error:      d.Err,
		}
		if d.Disc != nil {
			t.Detail = discLabel(d.Disc)
			live[d.Disc.ID] = d
		}
		switch d.State {
		case store.DriveWaiting:
			waiting++
			t.Detail = "Insert a blank disc for " + t.Detail
		case store.DriveFinished:
			t.Progress, t.Detail = 1, fmt.Sprintf("Finished %d disc(s)", d.Completed)
		}
		job.Tasks = append(job.Tasks, t)
	}

	type isoRow struct {
		task         remote.Task
		done, copies int
		fraction     float64
	}
	var order []string
	rows := map[string]*isoRow{}
	for _, d := range m.syncDiscs {
		row := rows[d.ISOPath]
		if row == nil {
			row = &isoRow{task: remote.Task{Section: remote.SectionISO, Name: filepath.Base(d.ISOPath), Status: "pending"}}
			rows[d.ISOPath] = row
			order = append(order, d.ISOPath)
		}
		status, progress, total := d.Status, d.Progress, d.Total
		if l, ok := live[d.ID]; ok {
			switch l.State {
			case store.DriveBurning:
				status, progress, total = store.DiscBurning, l.Progress, l.Total
			case store.DriveVerifying:
				status, progress, total = store.DiscVerifying, l.Progress, l.Total
			}
		}
		f := discFraction(status, progress, total)
		row.copies++
		row.fraction += f
		row.task.TotalBytes += d.ISOSize
		job.TotalBytes += d.ISOSize
		burned := int64(f * 2 * float64(d.ISOSize)) // burning is the first half
		burned = min(burned, d.ISOSize)
		job.DoneBytes += burned
		row.task.DoneBytes += burned
		switch status {
		case store.DiscDone:
			row.done++
		case store.DiscBurning, store.DiscVerifying:
			row.task.Status = "burning"
		}
		if d.Error != "" && status != store.DiscDone {
			row.task.Error = d.Error
		}
	}
	for _, path := range order {
		row := rows[path]
		row.task.Progress = row.fraction / float64(row.copies)
		row.task.Detail = fmt.Sprintf("%d of %d copies burned", row.done, row.copies)
		if row.done == row.copies {
			row.task.Status, row.task.Progress = "done", 1
		}
		job.Tasks = append(job.Tasks, row.task)
	}

	switch len(order) {
	case 0:
		job.Title = "Burn session"
	case 1:
		job.Title = "Burn " + filepath.Base(order[0])
	default:
		job.Title = fmt.Sprintf("Burn %s and %d more", filepath.Base(order[0]), len(order)-1)
	}

	overall := float64(s.Done)
	for _, d := range s.Drives {
		overall += driveFraction(d)
	}
	job.Progress = fraction(int64(overall*1000), int64(s.Total)*1000)

	switch {
	case s.Running:
		job.Status = remote.StatusRunning
		if waiting > 0 {
			job.Message = fmt.Sprintf("%d drive(s) waiting for a blank disc", waiting)
		} else {
			job.Message = fmt.Sprintf("Burning with %d drive(s)", len(s.Drives))
		}
	case s.Err != nil:
		job.Status, job.Error = remote.StatusFailed, s.Err.Error()
	case s.Done >= s.Total:
		job.Status, job.Progress, job.Message = remote.StatusCompleted, 1, fmt.Sprintf("All %d disc(s) burned", s.Total)
	default:
		job.Status = remote.StatusStopped
		job.Message = fmt.Sprintf("Stopped with %d of %d disc(s) done; resume from burn mode", s.Done, s.Total)
	}
	job.FinishedAt = finishedAt(job.Status != remote.StatusRunning)
	return job
}
