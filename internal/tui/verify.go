package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/burn"
	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/settings"
)

type discVerification struct {
	choosing bool
	picker   isoPicker
	drives   DriveSelector
	isoPath  string
	drive    drive.Drive
	run      *burn.Verification
	snap     burn.VerificationSnapshot
	err      error
	quitting bool
}

func (m Model) startVerify() (tea.Model, tea.Cmd) {
	m.burnMenu = false
	m.verify = discVerification{choosing: true, picker: newSingleISOPicker(m.isoHint)}
	return m, nil
}

func (m Model) startVerification() (tea.Model, tea.Cmd) {
	v := &m.verify
	run, err := burn.StartVerification(m.burner, v.drive, v.isoPath)
	if err != nil {
		v.run = nil
		v.err, v.drives.done = err, false
		return m, nil
	}
	v.run, v.snap, v.err = run, run.Snapshot(), nil
	return m, burnTick()
}

func (m Model) updateVerify(msg tea.Msg) (tea.Model, tea.Cmd) {
	v := &m.verify
	if v.run != nil {
		if _, ok := msg.(burnTickMsg); ok {
			v.snap = v.run.Snapshot()
			if v.snap.Running {
				return m, burnTick()
			}
			if v.quitting {
				return m, tea.Quit
			}
			return m, nil
		}
		if key, ok := msg.(tea.KeyMsg); ok {
			switch key.String() {
			case "ctrl+c":
				v.quitting = true
				if v.snap.Running {
					v.run.Cancel()
					return m, nil
				}
				return m, tea.Quit
			case "esc", "q":
				if v.snap.Running {
					v.run.Cancel()
					return m, nil
				}
				m.verify, m.burnMenu = discVerification{}, true
			case "enter":
				if !v.snap.Running {
					m.verify, m.burnMenu = discVerification{}, true
				}
			case "r":
				if !v.snap.Running {
					return m.startVerification()
				}
			}
		}
		return m, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "ctrl+c" {
		m.cancelled = true
		return m, tea.Quit
	}
	if v.isoPath == "" {
		key, ok := msg.(tea.KeyMsg)
		if !ok {
			return m, nil
		}
		next, chosen, closed := v.picker.update(key)
		v.picker = next
		if !closed {
			return m, nil
		}
		if chosen == nil {
			m.verify, m.burnMenu = discVerification{}, true
			return m, nil
		}
		v.isoPath = chosen[0]
		m.isoHint, m.recent.ISODir = v.picker.dir, v.picker.dir
		m.saveRecent()
		v.drives = newSingleDriveSelector(m.listDrives)
		return m, v.drives.Init()
	}
	var cmd tea.Cmd
	v.drives, cmd = v.drives.update(msg)
	if v.drives.Cancelled() {
		v.isoPath, v.err = "", nil
		return m, nil
	}
	if v.drives.Done() {
		v.drive = v.drives.Selected()[0]
		return m.startVerification()
	}
	return m, cmd
}

func (m Model) verifyView() string {
	var b strings.Builder
	v := m.verify
	b.WriteString(titleStyle.Render("ISO Burner · Verify disc") + "\n\n")
	if v.run == nil {
		if v.isoPath == "" {
			b.WriteString(labelStyle.Render("Select the original ISO to compare with the disc") + "\n\n")
			b.WriteString(v.picker.view())
			b.WriteString("\n" + dimStyle.Render(v.picker.help()))
		} else {
			b.WriteString(labelStyle.Render("ISO") + "  " + v.isoPath + "\n\n")
			b.WriteString(selectedStyle.Render("Insert the recorded disc in the drive before confirming.") + "\n")
			b.WriteString(dimStyle.Render("This checks the disc without writing or ejecting it.") + "\n\n")
			b.WriteString(v.drives.body())
			if v.err != nil {
				b.WriteString("\n" + errorStyle.Render("✗ "+v.err.Error()) + "\n")
			}
			b.WriteString("\n" + dimStyle.Render(strings.Replace(v.drives.help(), "enter: read disc", "enter: verify disc", 1)+" · esc: back"))
		}
		return panelStyle.Render(b.String()) + "\n"
	}
	s := v.snap
	b.WriteString(labelStyle.Render("ISO") + "  " + v.isoPath + "\n")
	b.WriteString(labelStyle.Render("Drive") + "  " + v.drive.ID + " · " + v.drive.Name() + "\n\n")
	switch {
	case s.Running && s.Opening:
		b.WriteString(labelStyle.Render("Opening disc for reading…") + "\n")
	case s.Running:
		b.WriteString(labelStyle.Render("Checking disc against ISO…") + "\n")
	case errors.Is(s.Err, context.Canceled):
		b.WriteString(dimStyle.Render("Verification stopped; completeness has not been confirmed.") + "\n")
	case s.Err != nil:
		var mismatch *burn.MismatchError
		if errors.As(s.Err, &mismatch) {
			b.WriteString(errorStyle.Render("✗ Disc does not match the ISO: "+s.Err.Error()) + "\n")
		} else {
			b.WriteString(errorStyle.Render("✗ Could not verify the complete disc: "+s.Err.Error()) + "\n")
		}
	default:
		b.WriteString(okStyle.Render("✓ Disc is complete and matches the ISO byte for byte.") + "\n")
	}
	bar := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(m.barWidth()))
	f := fraction(s.Checked, s.Total)
	b.WriteString(fmt.Sprintf("%s %3.0f%%  %s / %s checked\n", bar.ViewAs(f), f*100, settings.FormatBytes(s.Checked), settings.FormatBytes(s.Total)))
	b.WriteString(dimStyle.Render(fmt.Sprintf("%s elapsed", s.Elapsed.Round(time.Second))) + "\n")
	help := "enter/esc: back to Burn menu · r: verify again · ctrl+c: exit"
	if s.Running {
		help = "esc: stop verification · ctrl+c: stop and exit"
	}
	b.WriteString("\n" + dimStyle.Render(help))
	return panelStyle.Render(b.String()) + "\n"
}

func (m Model) VerificationResult() (burn.VerificationSnapshot, bool) {
	return m.verify.snap, m.verify.run != nil
}
