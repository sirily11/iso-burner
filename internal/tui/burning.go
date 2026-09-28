package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/burn"
	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/store"
)

const burnTickInterval = 200 * time.Millisecond

type burnTickMsg struct{}

func burnTick() tea.Cmd {
	return tea.Tick(burnTickInterval, func(time.Time) tea.Msg { return burnTickMsg{} })
}

// errNoStore is shown when burning is attempted without a database.
var errNoStore = errors.New("the burn history database is not available")

// offerResume looks for an unfinished session and, if there is one, shows
// the resume dialog instead of the ISO picker.
func (m *Model) offerResume() {
	m.resumeOffer, m.resumeDiscs, m.resumeErr = nil, nil, nil
	if m.store == nil {
		return
	}
	ctx := context.Background()
	sess, err := m.store.Unfinished(ctx)
	if err != nil || sess == nil {
		m.resumeErr = err
		return
	}
	discs, err := m.store.Discs(ctx, sess.ID)
	if err != nil {
		m.resumeErr = err
		return
	}
	m.resumeOffer, m.resumeDiscs = sess, discs
}

// updateResume handles the resume dialog: resume the saved session, discard
// it and start a new one, or leave.
func (m Model) updateResume(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "enter", "r":
		m.resuming, m.resumeOffer = m.resumeOffer, nil
		if m.drives.list == nil {
			m.drives = NewDriveSelector(m.listDrives, m.resuming.DriveIDs...)
			return m, m.drives.Init()
		}
		var cmd tea.Cmd
		m.drives, cmd = m.drives.reopen()
		return m, cmd
	case "n":
		if err := m.store.SetSessionStatus(context.Background(), m.resumeOffer.ID, store.SessionDiscarded); err != nil {
			m.resumeErr = err
			return m, nil
		}
		m.resumeOffer = nil
		return m, nil
	case "esc", "q":
		if m.modeChosen {
			m.mode, m.resumeOffer = ModeNone, nil
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) resumeView() string {
	var b strings.Builder
	s := m.resumeOffer
	b.WriteString(labelStyle.Render("Resume unfinished burn?") + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("Started %s · last activity %s",
		s.CreatedAt.Format("2006-01-02 15:04"), s.UpdatedAt.Format("2006-01-02 15:04"))) + "\n\n")

	type isoCount struct{ done, total int }
	var order []string
	counts := map[string]*isoCount{}
	for _, d := range m.resumeDiscs {
		c := counts[d.ISOPath]
		if c == nil {
			c = &isoCount{}
			counts[d.ISOPath] = c
			order = append(order, d.ISOPath)
		}
		c.total++
		if d.Status == store.DiscDone {
			c.done++
		}
	}
	for i, path := range order {
		if i == maxPreviewFiles {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  … and %d more", len(order)-maxPreviewFiles)) + "\n")
			break
		}
		c := counts[path]
		b.WriteString(fmt.Sprintf("  💿 %s  %s\n", filepath.Base(path), dimStyle.Render(fmt.Sprintf("%d of %d disc(s) done", c.done, c.total))))
	}
	b.WriteString("\n" + okStyle.Render(fmt.Sprintf("%d of %d disc(s) done · %d left", s.Done, s.Total, s.Total-s.Done)) + "\n")
	if m.resumeErr != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+m.resumeErr.Error()) + "\n")
	}
	return b.String()
}

func (m Model) resumeHelp() string {
	esc := "esc: quit"
	if m.modeChosen {
		esc = "esc: back"
	}
	return "enter: resume · n: discard and start a new burn · " + esc
}

// startBurning records the session in the database and starts burning with
// the chosen drives.
func (m Model) startBurning() (tea.Model, tea.Cmd) {
	drives := m.drives.Selected()
	if m.store == nil {
		m.burnErr = errNoStore
		return m, nil
	}
	ctx := context.Background()
	var id int64
	loaded := false
	if m.resuming != nil {
		id = m.resuming.ID
		if err := m.store.Resume(ctx, id, drives); err != nil {
			m.burnErr = err
			return m, nil
		}
	} else {
		jobs := make([]store.Job, len(m.burnJobs))
		for i, j := range m.burnJobs {
			jobs[i] = store.Job{Path: j.Path, Size: m.replicas.sizes[i], Copies: j.Copies}
		}
		var err error
		if id, err = m.store.CreateSession(ctx, jobs, drives); err != nil {
			m.burnErr = err
			return m, nil
		}
		// The drive step asks for blank discs to be loaded before confirming.
		loaded = true
	}
	engine, err := burn.Start(burn.Config{Store: m.store, Session: id, Drives: drives, Burner: m.burner,
		Speed: burn.Speed(m.recent.Speed), DiscsLoaded: loaded})
	if err != nil {
		m.burnErr = err
		return m, nil
	}
	m.burnDrives, m.burnErr = drives, nil
	m.engine, m.burnSession = engine, id
	m.burnSnap = engine.Snapshot()
	m.burnStarted = time.Now()
	m.dismissed = map[string]int64{}
	m.burnSync = m.newReporter(m.burnJobID())
	m.refreshSyncDiscs(true)
	m.reportBurn()
	return m, burnTick()
}

// promptDrive returns the index of the first drive waiting for a disc whose
// dialog the user has not put off, or -1.
func (m Model) promptDrive() int {
	for i, d := range m.burnSnap.Drives {
		if d.State == store.DriveWaiting && d.Disc != nil && m.dismissed[d.Drive.ID] != d.Disc.ID {
			return i
		}
	}
	return -1
}

// updateBurning handles the burn progress screen and its dialogs.
func (m Model) updateBurning(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case burnTickMsg:
		m.burnSnap = m.engine.Snapshot()
		m.burnElapsed = time.Since(m.burnStarted)
		if m.burnSnap.Running {
			m.refreshSyncDiscs(false)
			m.reportBurn()
			return m, burnTick()
		}
		m.burnDiscs, _ = m.store.Discs(context.Background(), m.burnSession)
		m.syncDiscs = m.burnDiscs
		m.reportBurn()
		return m, nil
	case tea.KeyMsg:
		return m.updateBurningKey(msg)
	}
	return m, nil
}

func (m Model) updateBurningKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	running := m.burnSnap.Running
	if m.confirmStop {
		switch key.String() {
		case "y", "ctrl+c":
			m.engine.Stop()
			m.burnSnap = m.engine.Snapshot()
			m.burnStopped, m.confirmStop = true, false
			m.refreshSyncDiscs(true)
			m.reportBurn()
			return m, tea.Quit
		case "n", "esc":
			m.confirmStop = false
		}
		return m, nil
	}
	if key.String() == "ctrl+c" {
		if running {
			m.confirmStop = true
			return m, nil
		}
		return m, tea.Quit
	}
	if i := m.promptDrive(); i >= 0 && running {
		d := m.burnSnap.Drives[i]
		switch key.String() {
		case "enter":
			m.engine.Insert(d.Drive.ID)
			// Keep the dialog closed until the drive reports it is burning.
			m.dismissed[d.Drive.ID] = d.Disc.ID
		case "esc":
			m.dismissed[d.Drive.ID] = d.Disc.ID
		}
		return m, nil
	}
	switch key.String() {
	case "enter", "i":
		if !running {
			return m, tea.Quit
		}
		// Reopen the dialog of a drive the user put off.
		for _, d := range m.burnSnap.Drives {
			if d.State == store.DriveWaiting {
				delete(m.dismissed, d.Drive.ID)
				break
			}
		}
	case "q", "esc":
		if !running {
			return m, tea.Quit
		}
		m.confirmStop = true
	}
	return m, nil
}

// discLabel names a disc as "backup_1.iso · copy 2 of 3".
func discLabel(d *store.Disc) string {
	return fmt.Sprintf("%s · copy %d of %d", filepath.Base(d.ISOPath), d.Copy, d.Copies)
}

func (m Model) burningView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("ISO Burner · Burn") + "\n\n")
	switch i := m.promptDrive(); {
	case m.confirmStop:
		b.WriteString(m.stopDialogView())
		b.WriteString("\n" + dimStyle.Render("y: stop · n/esc: keep burning"))
	case i >= 0 && m.burnSnap.Running:
		b.WriteString(m.insertDialogView(m.burnSnap.Drives[i]))
		b.WriteString("\n" + dimStyle.Render("enter: disc inserted, start burning · esc: later"))
	default:
		b.WriteString(m.burnProgressView())
		b.WriteString("\n" + dimStyle.Render(m.burningHelp()))
	}
	return panelStyle.Render(b.String()) + "\n"
}

func (m Model) stopDialogView() string {
	var b strings.Builder
	b.WriteString(errorStyle.Render("Stop burning?") + "\n\n")
	burning := 0
	for _, d := range m.burnSnap.Drives {
		if d.State == store.DriveBurning || d.State == store.DriveVerifying {
			burning++
		}
	}
	if burning > 0 {
		b.WriteString(fmt.Sprintf("%d disc(s) are being burned or checked and will be unusable.\n", burning))
	}
	b.WriteString(fmt.Sprintf("%d of %d disc(s) are done. Progress is saved, so you can\nresume later from burn mode.\n", m.burnSnap.Done, m.burnSnap.Total))
	return b.String()
}

func (m Model) insertDialogView(d burn.DriveStatus) string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Insert a blank disc") + "\n")
	b.WriteString(dimStyle.Render("Drive "+d.Drive.ID+" · "+d.Drive.Name()) + "\n\n")
	if d.Last != nil {
		line := "✓ Finished " + discLabel(d.Last)
		if d.Last.VerifyNote != "" {
			b.WriteString(okStyle.Render(line) + "  " + errorStyle.Render("⚠ "+d.Last.VerifyNote) + "\n")
		} else {
			b.WriteString(okStyle.Render(line+" · verified") + "\n")
		}
	}
	if d.Err != "" {
		b.WriteString(errorStyle.Render("✗ Last burn failed: "+truncate(d.Err, 70)) + "\n")
		b.WriteString(dimStyle.Render("  That disc is unusable; the same image will be burned again.") + "\n")
	}
	b.WriteString("\n" + labelStyle.Render("Next") + "  " + discLabel(d.Disc) + "  " + dimStyle.Render(settings.FormatBytes(d.Disc.ISOSize)) + "\n\n")
	b.WriteString(selectedStyle.Render(fmt.Sprintf("Put a new blank disc in drive %s (%s), then press enter.", d.Drive.ID, d.Drive.Name())) + "\n")
	if waiting := m.waitingCount(); waiting > 1 {
		b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("%d drives are waiting for a disc", waiting)) + "\n")
	}
	return b.String()
}

func (m Model) waitingCount() int {
	n := 0
	for _, d := range m.burnSnap.Drives {
		if d.State == store.DriveWaiting {
			n++
		}
	}
	return n
}

// driveFraction is how far a drive is through its current disc, counting
// burning as the first half and verifying as the second.
func driveFraction(d burn.DriveStatus) float64 {
	p := fraction(d.Progress, d.Total)
	switch d.State {
	case store.DriveBurning:
		return p / 2
	case store.DriveVerifying:
		return 0.5 + p/2
	}
	return 0
}

func (m Model) burnProgressView() string {
	var b strings.Builder
	s := m.burnSnap
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(m.barWidth()))

	switch {
	case s.Err != nil:
		b.WriteString(errorStyle.Render("✗ Burning stopped: "+s.Err.Error()) + "\n")
	case s.Running:
		b.WriteString(labelStyle.Render(fmt.Sprintf("Burning %d disc(s) with %d drive(s)", s.Total, len(s.Drives))) + "\n")
	case s.Done >= s.Total:
		b.WriteString(okStyle.Render(fmt.Sprintf("✓ All %d disc(s) burned", s.Total)) + "\n")
	default:
		b.WriteString(errorStyle.Render(fmt.Sprintf("Stopped with %d of %d disc(s) done", s.Done, s.Total)) + "\n")
	}
	overall := float64(s.Done)
	for _, d := range s.Drives {
		overall += driveFraction(d)
	}
	overall = fraction(int64(overall*1000), int64(s.Total)*1000)
	b.WriteString(fmt.Sprintf("%s %3.0f%%  %d of %d disc(s) done\n", bar.ViewAs(overall), overall*100, s.Done, s.Total))
	b.WriteString(dimStyle.Render(fmt.Sprintf("%s elapsed", m.burnElapsed.Round(time.Second))) + "\n")
	if line := m.syncStatusLine(m.burnSync); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	nameWidth := 0
	for _, d := range s.Drives {
		nameWidth = max(nameWidth, len(d.Drive.Name()))
	}
	nameWidth = min(nameWidth, 24)
	for _, d := range s.Drives {
		pct := fraction(d.Progress, d.Total)
		var state string
		switch d.State {
		case store.DriveBurning:
			state = fmt.Sprintf("burning %s · %s / %s", discLabel(d.Disc), settings.FormatBytes(d.Progress), settings.FormatBytes(d.Total))
			if d.Stage != "" {
				state += " · " + truncate(d.Stage, 50)
			} else if d.Speed != "" {
				state += " · " + truncate(d.Speed, 50)
			}
		case store.DriveVerifying:
			state = fmt.Sprintf("checking %s · %s / %s", discLabel(d.Disc), settings.FormatBytes(d.Progress), settings.FormatBytes(d.Total))
			if d.Stage != "" {
				state += " · " + truncate(d.Stage, 50)
			}
		case store.DriveWaiting:
			pct, state = 0, selectedStyle.Render("⏏ insert a blank disc for "+discLabel(d.Disc)+" · press enter")
		case store.DriveFinished:
			pct, state = 1, okStyle.Render(fmt.Sprintf("✓ finished · %d disc(s)", d.Completed))
		default:
			pct, state = 0, dimStyle.Render("stopped")
		}
		name := fmt.Sprintf("%-*s", nameWidth, truncate(d.Drive.Name(), nameWidth))
		b.WriteString(fmt.Sprintf("  %s %s %3.0f%%  %s\n", name, bar.ViewAs(pct), pct*100, state))
		if d.Err != "" && d.State != store.DriveFinished {
			b.WriteString("  " + errorStyle.Render("  ✗ "+truncate(d.Err, 80)) + "\n")
		}
	}

	if !s.Running {
		var unverified []store.Disc
		for _, d := range m.burnDiscs {
			if d.Status == store.DiscDone && d.VerifyNote != "" {
				unverified = append(unverified, d)
			}
		}
		if len(unverified) > 0 {
			b.WriteString("\n" + errorStyle.Render(fmt.Sprintf("⚠ %d disc(s) burned but not verified", len(unverified))) + "\n")
			for _, d := range unverified[:min(len(unverified), maxPreviewFiles)] {
				b.WriteString(dimStyle.Render("  "+discLabel(&d)+" · "+truncate(d.VerifyNote, 60)) + "\n")
			}
		} else if s.Done >= s.Total && s.Err == nil {
			b.WriteString("\n" + okStyle.Render("Every disc was read back and matches its ISO.") + "\n")
		}
	}
	if m.dbPath != "" {
		b.WriteString("\n" + dimStyle.Render("Progress is saved in "+m.dbPath) + "\n")
	}
	return b.String()
}

func (m Model) burningHelp() string {
	switch {
	case !m.burnSnap.Running:
		return "enter: exit"
	case m.waitingCount() > 0:
		return "enter: insert disc · ctrl+c: stop"
	default:
		return "ctrl+c: stop (progress is saved)"
	}
}

// BurnResult reports how burn mode ended: the final state of the session and
// whether the user stopped it early. ok is false if burning never started.
func (m Model) BurnResult() (snap burn.Snapshot, stopped, ok bool) {
	if m.engine == nil {
		return burn.Snapshot{}, false, false
	}
	return m.burnSnap, m.burnStopped, true
}
