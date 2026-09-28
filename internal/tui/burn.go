package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/drive"
)

// startBurn opens the ISO picker for burn mode, or first offers to resume an
// unfinished session.
func (m Model) startBurn() (tea.Model, tea.Cmd) {
	m.isoPicker = newISOPicker(m.isoHint)
	m.burnISOs, m.burnDrives = nil, nil
	m.burnJobs, m.settingReplicas = nil, false
	m.resuming, m.burnErr = nil, nil
	m.offerResume()
	return m, nil
}

// updateBurn handles burn mode: choosing ISO files, how many copies of each
// to burn, then the drives to burn them with, and finally burning. An
// unfinished session can be resumed instead of choosing ISOs.
func (m Model) updateBurn(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.engine != nil {
		return m.updateBurning(msg)
	}
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyCtrlC {
		m.cancelled = true
		return m, tea.Quit
	}
	if m.burnISOs != nil || m.resuming != nil {
		return m.updateDrives(msg)
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if m.resumeOffer != nil {
		return m.updateResume(key)
	}
	if m.settingReplicas {
		return m.updateReplicas(key)
	}
	next, chosen, closed := m.isoPicker.update(key)
	m.isoPicker = next
	if !closed {
		return m, nil
	}
	if chosen != nil {
		m.replicas = newReplicaEditor(chosen, m.isoPicker.selected, m.replicaCounts)
		m.settingReplicas = true
		return m, nil
	}
	if m.modeChosen {
		m.mode = ModeNone
		return m, nil
	}
	m.cancelled = true
	return m, tea.Quit
}

// updateReplicas handles the copies step. Confirming opens the drive
// selection; going back returns to the ISO picker with its selection intact.
func (m Model) updateReplicas(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	next, cmd, done, back := m.replicas.update(key)
	m.replicas = next
	if back || done {
		m.replicaCounts = map[string]int{}
		for _, j := range m.replicas.jobs {
			m.replicaCounts[j.Path] = j.Copies
		}
	}
	switch {
	case back:
		m.settingReplicas = false
		return m, nil
	case done:
		m.burnJobs = append([]BurnJob(nil), m.replicas.jobs...)
		m.burnISOs = make([]string, len(m.burnJobs))
		for i, j := range m.burnJobs {
			m.burnISOs[i] = j.Path
		}
		if m.drives.list == nil {
			m.drives = NewDriveSelector(m.listDrives)
			return m, m.drives.Init()
		}
		m.drives, cmd = m.drives.reopen()
		return m, cmd
	}
	return m, cmd
}

// updateDrives handles the drive selection step. Going back returns to the
// copies step with its counts intact, or to the resume dialog when resuming.
// Confirming starts burning.
func (m Model) updateDrives(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.drives, cmd = m.drives.update(msg)
	switch {
	case m.drives.Cancelled():
		m.burnErr = nil
		if m.resuming != nil {
			m.offerResume()
			m.resuming = nil
			return m, nil
		}
		m.burnISOs, m.burnJobs = nil, nil
		return m, nil
	case m.drives.Done():
		next, cmd := m.startBurning()
		if nm := next.(Model); nm.engine == nil {
			// Starting failed; stay on the drive step to show why.
			nm.drives.done = false
			return nm, cmd
		}
		return next, cmd
	}
	return m, cmd
}

// BurnDrives returns the disc drives chosen in burn mode, or nil if the user
// quit before confirming them.
func (m Model) BurnDrives() []drive.Drive {
	if m.cancelled {
		return nil
	}
	return m.burnDrives
}

// BurnJobs returns the ISO files chosen in burn mode with the number of discs
// to burn from each, or nil if the user quit before confirming.
func (m Model) BurnJobs() []BurnJob {
	if m.cancelled {
		return nil
	}
	return m.burnJobs
}

// BurnISOs returns the ISO files chosen in burn mode, or nil if the user quit
// before confirming.
func (m Model) BurnISOs() []string {
	if m.cancelled {
		return nil
	}
	return m.burnISOs
}

func (m Model) burnView() string {
	if m.engine != nil {
		return m.burningView()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("ISO Burner · Burn") + "\n\n")
	if m.burnISOs != nil || m.resuming != nil {
		back := "esc: back to copies"
		if m.resuming != nil {
			left := m.resuming.Total - m.resuming.Done
			b.WriteString(dimStyle.Render(fmt.Sprintf("Resuming · %d of %d disc(s) left", left, m.resuming.Total)) + "\n")
			b.WriteString(dimStyle.Render("You will be asked to insert a blank disc before each burn.") + "\n\n")
			back = "esc: back"
		} else {
			b.WriteString(dimStyle.Render(fmt.Sprintf("Burning %d ISO file(s) · %d disc(s)", len(m.burnISOs), m.replicas.totalDiscs())) + "\n")
			b.WriteString(selectedStyle.Render("Load a blank disc in every selected drive before confirming.") + "\n\n")
		}
		b.WriteString(m.drives.body())
		if m.burnErr != nil {
			b.WriteString("\n" + errorStyle.Render("✗ "+m.burnErr.Error()) + "\n")
		}
		b.WriteString("\n" + dimStyle.Render(strings.Replace(m.drives.help(), "enter: confirm", "enter: start burning", 1)+" · "+back))
		return panelStyle.Render(b.String()) + "\n"
	}
	if m.resumeOffer != nil {
		b.WriteString(m.resumeView())
		b.WriteString("\n" + dimStyle.Render(m.resumeHelp()))
		return panelStyle.Render(b.String()) + "\n"
	}
	if m.settingReplicas {
		b.WriteString(m.replicas.view())
		b.WriteString("\n" + dimStyle.Render(m.replicas.help()))
		return panelStyle.Render(b.String()) + "\n"
	}
	b.WriteString(m.isoPicker.view())
	help := m.isoPicker.help()
	if m.modeChosen {
		help += " · esc: back"
	} else {
		help += " · esc: quit"
	}
	b.WriteString("\n" + dimStyle.Render(help))
	return panelStyle.Render(b.String()) + "\n"
}
