package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/settings"
)

// maxReplicas caps how many discs can be burned from one ISO.
const maxReplicas = 999

// BurnJob is one ISO file and how many discs to burn from it.
type BurnJob struct {
	Path   string
	Copies int
}

// replicaTarget says which ISOs the count dialog applies to.
type replicaTarget int

const (
	targetNone replicaTarget = iota // dialog closed
	targetOne                       // the highlighted ISO
	targetAll                       // every ISO
)

// replicaEditor lets the user choose how many discs to burn from each ISO,
// either one at a time or one count for all of them.
type replicaEditor struct {
	jobs   []BurnJob
	sizes  []int64
	cursor int
	offset int

	target replicaTarget
	input  textinput.Model
	err    error
}

// newReplicaEditor lists paths with the counts in prev, or 1 for ISOs that
// have no count yet.
func newReplicaEditor(paths []string, sizes map[string]int64, prev map[string]int) replicaEditor {
	r := replicaEditor{input: textinput.New()}
	r.input.CharLimit = len(strconv.Itoa(maxReplicas))
	r.input.Width = 6
	for _, path := range paths {
		r.jobs = append(r.jobs, BurnJob{Path: path, Copies: max(prev[path], 1)})
		r.sizes = append(r.sizes, sizes[path])
	}
	return r
}

func (r *replicaEditor) moveTo(i int) {
	r.cursor = max(0, min(i, len(r.jobs)-1))
	if r.cursor < r.offset {
		r.offset = r.cursor
	} else if r.cursor >= r.offset+pickerRows {
		r.offset = r.cursor - pickerRows + 1
	}
}

// totalDiscs is the number of discs the whole job will burn.
func (r replicaEditor) totalDiscs() int {
	n := 0
	for _, j := range r.jobs {
		n += j.Copies
	}
	return n
}

// openDialog shows the count dialog for target, pre-filled with value.
func (r *replicaEditor) openDialog(target replicaTarget, value string) tea.Cmd {
	r.target, r.err = target, nil
	r.input.SetValue(value)
	r.input.CursorEnd()
	return r.input.Focus()
}

func (r *replicaEditor) closeDialog() {
	r.target, r.err = targetNone, nil
	r.input.Blur()
}

// parseReplicas validates a typed disc count.
func parseReplicas(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > maxReplicas {
		return 0, fmt.Errorf("enter a whole number from 1 to %d", maxReplicas)
	}
	return n, nil
}

// update handles a key press. It returns done=true when the user confirms the
// counts and back=true when they want to return to the ISO picker.
func (r replicaEditor) update(msg tea.KeyMsg) (next replicaEditor, cmd tea.Cmd, done, back bool) {
	if r.target != targetNone {
		return r.updateDialog(msg)
	}
	r.err = nil
	switch key := msg.String(); key {
	case "esc":
		return r, nil, false, true
	case "enter":
		return r, nil, true, false
	case "up", "k":
		r.moveTo(r.cursor - 1)
	case "down", "j":
		r.moveTo(r.cursor + 1)
	case "home", "g":
		r.moveTo(0)
	case "end", "G":
		r.moveTo(len(r.jobs) - 1)
	case "+", "=", "right", "l":
		r.jobs[r.cursor].Copies = min(r.jobs[r.cursor].Copies+1, maxReplicas)
	case "-", "_", "left", "h":
		r.jobs[r.cursor].Copies = max(r.jobs[r.cursor].Copies-1, 1)
	case "e":
		cmd = r.openDialog(targetOne, strconv.Itoa(r.jobs[r.cursor].Copies))
	case "s", "A":
		cmd = r.openDialog(targetAll, strconv.Itoa(r.jobs[r.cursor].Copies))
	default:
		// Typing a number edits the highlighted ISO's count straight away.
		if len(key) == 1 && key[0] >= '0' && key[0] <= '9' {
			cmd = r.openDialog(targetOne, key)
		}
	}
	return r, cmd, false, false
}

// updateDialog handles keys while the count dialog is open.
func (r replicaEditor) updateDialog(msg tea.KeyMsg) (replicaEditor, tea.Cmd, bool, bool) {
	switch msg.String() {
	case "esc":
		r.closeDialog()
		return r, nil, false, false
	case "enter":
		n, err := parseReplicas(r.input.Value())
		if err != nil {
			r.err = err
			return r, nil, false, false
		}
		if r.target == targetAll {
			for i := range r.jobs {
				r.jobs[i].Copies = n
			}
		} else {
			r.jobs[r.cursor].Copies = n
		}
		r.closeDialog()
		return r, nil, false, false
	}
	var cmd tea.Cmd
	r.input, cmd = r.input.Update(msg)
	r.err = nil
	return r, cmd, false, false
}

func (r replicaEditor) view() string {
	if r.target != targetNone {
		return r.dialogView()
	}
	var b strings.Builder
	b.WriteString(labelStyle.Render("Copies per ISO") + dimStyle.Render("  (discs to burn from each image)") + "\n\n")
	end := min(r.offset+pickerRows, len(r.jobs))
	for i := r.offset; i < end; i++ {
		j := r.jobs[i]
		line := fmt.Sprintf("💿 %s  %s  × %d", filepath.Base(j.Path), dimStyle.Render(settings.FormatBytes(r.sizes[i])), j.Copies)
		if i == r.cursor {
			b.WriteString(selectedStyle.Render("› "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if len(r.jobs) > pickerRows {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing %d–%d of %d ISOs", r.offset+1, end, len(r.jobs))) + "\n")
	}
	b.WriteString("\n" + okStyle.Render(fmt.Sprintf("%d ISO file(s) · %d disc(s) in total", len(r.jobs), r.totalDiscs())) + "\n")
	return b.String()
}

// dialogView is the count dialog, shown in place of the list.
func (r replicaEditor) dialogView() string {
	var b strings.Builder
	if r.target == targetAll {
		b.WriteString(labelStyle.Render(fmt.Sprintf("Copies for all %d ISO file(s)", len(r.jobs))) + "\n")
		b.WriteString(dimStyle.Render("Replaces every ISO's current count") + "\n\n")
	} else {
		b.WriteString(labelStyle.Render("Copies of "+filepath.Base(r.jobs[r.cursor].Path)) + "\n")
		b.WriteString(dimStyle.Render(r.jobs[r.cursor].Path) + "\n\n")
	}
	b.WriteString(r.input.View() + "\n")
	if r.err != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+r.err.Error()) + "\n")
	}
	return b.String()
}

func (r replicaEditor) help() string {
	if r.target != targetNone {
		return "enter: apply · esc: cancel"
	}
	return "↑/↓: move · +/-: adjust · e or 0-9: edit count · s: set all · enter: burn · esc: back"
}
