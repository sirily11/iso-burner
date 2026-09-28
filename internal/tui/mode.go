package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Mode is what the user wants iso-burner to do.
type Mode int

const (
	// ModeNone means no mode has been chosen yet; the wizard asks first.
	ModeNone Mode = iota
	// ModeGenerate splits a folder into ISO files.
	ModeGenerate
	// ModeBurn writes existing ISO files to disc.
	ModeBurn
)

// modeChoice is one entry on the mode selection screen.
type modeChoice struct {
	mode  Mode
	title string
	desc  string
}

var modeChoices = []modeChoice{
	{ModeGenerate, "Generate ISO file", "Split a folder into size-limited ISO images"},
	{ModeBurn, "Burn ISO", "Write ISO images to a disc drive"},
}

// updateMode handles keys on the mode selection screen.
func (m Model) updateMode(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c", "esc", "q":
		m.cancelled = true
		return m, tea.Quit
	case "up", "k", "shift+tab":
		m.modeIdx = (m.modeIdx + len(modeChoices) - 1) % len(modeChoices)
	case "down", "j", "tab":
		m.modeIdx = (m.modeIdx + 1) % len(modeChoices)
	case "1", "2":
		m.modeIdx = int(key.Runes[0] - '1')
		return m.chooseMode()
	case "enter":
		return m.chooseMode()
	case "a":
		m.showAccount = true
	}
	return m, nil
}

// chooseMode commits the highlighted mode. Generating continues into the
// settings wizard; burning opens the ISO picker.
func (m Model) chooseMode() (tea.Model, tea.Cmd) {
	m.mode = modeChoices[m.modeIdx].mode
	m.modeChosen = true
	if m.mode == ModeBurn {
		return m.startBurn()
	}
	return m.goTo(stepFolder)
}

func (m Model) modeView() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("What do you want to do?") + "\n\n")
	for i, c := range modeChoices {
		line := fmt.Sprintf("%d. %s", i+1, c.title)
		if i == m.modeIdx {
			b.WriteString(selectedStyle.Render("› "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("     " + dimStyle.Render(c.desc) + "\n")
	}
	return b.String()
}
