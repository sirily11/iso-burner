package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/drive"
)

// DriveLister finds the optical drives available for burning.
type DriveLister func(context.Context) ([]drive.Drive, error)

// drivesLoadedMsg carries the result of a drive scan.
type drivesLoadedMsg struct {
	drives []drive.Drive
	err    error
}

// DriveSelector lets the user tick one or more disc drives to burn with. Each
// selected drive burns its own discs, so more drives mean more discs at once.
type DriveSelector struct {
	list     DriveLister
	loading  bool
	drives   []drive.Drive
	selected map[string]bool // by Drive.ID, so a rescan keeps the ticks
	cursor   int
	err      error
	// single picks exactly one drive with enter instead of ticking several.
	single bool

	done      bool
	cancelled bool
}

// NewDriveSelector builds a selector that scans drives with list, or with
// drive.List when list is nil. preselect ticks drives by ID, e.g. when going
// back to this step.
func NewDriveSelector(list DriveLister, preselect ...string) DriveSelector {
	if list == nil {
		list = drive.List
	}
	s := DriveSelector{list: list, loading: true, selected: map[string]bool{}}
	for _, id := range preselect {
		s.selected[id] = true
	}
	return s
}

// newSingleDriveSelector builds a selector that picks one drive, e.g. the one
// holding a disc to read.
func newSingleDriveSelector(list DriveLister) DriveSelector {
	s := NewDriveSelector(list)
	s.single = true
	return s
}

func (s DriveSelector) Init() tea.Cmd { return s.scan() }

func (s DriveSelector) scan() tea.Cmd {
	list := s.list
	return func() tea.Msg {
		drives, err := list(context.Background())
		return drivesLoadedMsg{drives: drives, err: err}
	}
}

// Selected returns the ticked drives in the order they were listed, or nil if
// the user cancelled.
func (s DriveSelector) Selected() []drive.Drive {
	if s.cancelled {
		return nil
	}
	var out []drive.Drive
	for _, d := range s.drives {
		if s.selected[d.ID] {
			out = append(out, d)
		}
	}
	return out
}

// Done reports whether the user confirmed a selection.
func (s DriveSelector) Done() bool { return s.done && !s.cancelled }

// Cancelled reports whether the user backed out of the selector.
func (s DriveSelector) Cancelled() bool { return s.cancelled }

func (s DriveSelector) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := s.update(msg)
	if next.done || next.cancelled {
		return next, tea.Quit
	}
	return next, cmd
}

// update is Update without quitting, for embedding in a larger wizard: check
// Done and Cancelled afterwards.
func (s DriveSelector) update(msg tea.Msg) (DriveSelector, tea.Cmd) {
	switch msg := msg.(type) {
	case drivesLoadedMsg:
		s.loading = false
		s.drives, s.err = msg.drives, msg.err
		present := map[string]bool{}
		for _, d := range s.drives {
			present[d.ID] = true
		}
		for id := range s.selected {
			if !present[id] {
				delete(s.selected, id)
			}
		}
		s.cursor = min(s.cursor, max(len(s.drives)-1, 0))
		return s, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			s.cancelled = true
			return s, nil
		case "r":
			if !s.loading {
				s.loading, s.err = true, nil
				return s, s.scan()
			}
			return s, nil
		}
		if s.loading || len(s.drives) == 0 {
			return s, nil
		}
		if s.single {
			switch msg.String() {
			case "up", "k", "shift+tab":
				s.cursor = (s.cursor + len(s.drives) - 1) % len(s.drives)
			case "down", "j", "tab":
				s.cursor = (s.cursor + 1) % len(s.drives)
			case "enter":
				clear(s.selected)
				s.selected[s.drives[s.cursor].ID] = true
				s.done = true
			}
			return s, nil
		}
		switch msg.String() {
		case "up", "k", "shift+tab":
			s.cursor = (s.cursor + len(s.drives) - 1) % len(s.drives)
		case "down", "j", "tab":
			s.cursor = (s.cursor + 1) % len(s.drives)
		case " ", "x":
			id := s.drives[s.cursor].ID
			if s.selected[id] {
				delete(s.selected, id)
			} else {
				s.selected[id] = true
			}
			s.err = nil
		case "a":
			all := len(s.selected) == len(s.drives)
			clear(s.selected)
			if !all {
				for _, d := range s.drives {
					s.selected[d.ID] = true
				}
			}
			s.err = nil
		case "enter":
			if len(s.selected) == 0 {
				s.err = fmt.Errorf("select at least one drive")
				return s, nil
			}
			s.done = true
		}
	}
	return s, nil
}

func (s DriveSelector) View() string {
	if s.done || s.cancelled {
		return ""
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("ISO Burner · Burn") + "\n\n")
	b.WriteString(s.body())
	b.WriteString("\n" + dimStyle.Render(s.help()+" · esc: quit"))
	return panelStyle.Render(b.String()) + "\n"
}

// reopen shows the selector again after the user went back past it, and
// rescans since drives may have been plugged in meanwhile. Ticks are kept.
func (s DriveSelector) reopen() (DriveSelector, tea.Cmd) {
	s.done, s.cancelled = false, false
	s.loading, s.err = true, nil
	return s, s.scan()
}

// body renders the drive list without the surrounding panel.
func (s DriveSelector) body() string {
	var b strings.Builder
	if s.single {
		b.WriteString(labelStyle.Render("Disc drive") + dimStyle.Render("  (choose the drive holding the disc)") + "\n\n")
	} else {
		b.WriteString(labelStyle.Render("Disc drives") + dimStyle.Render("  (each selected drive burns in parallel)") + "\n\n")
	}
	switch {
	case s.loading:
		b.WriteString(dimStyle.Render("Looking for disc drives…") + "\n")
	case s.err != nil && len(s.drives) == 0:
		b.WriteString(errorStyle.Render("✗ "+s.err.Error()) + "\n")
	case len(s.drives) == 0:
		b.WriteString(dimStyle.Render("No disc drives found. Connect a drive and press r to rescan.") + "\n")
	default:
		for i, d := range s.drives {
			line := d.Name()
			if !s.single {
				box := "[ ]"
				if s.selected[d.ID] {
					box = "[x]"
				}
				line = box + " " + line
			}
			if i == s.cursor {
				line = selectedStyle.Render("› " + line)
			} else {
				line = "  " + line
			}
			b.WriteString(line + "  " + dimStyle.Render(driveInfo(d)) + "\n")
		}
		if !s.single {
			b.WriteString("\n" + okStyle.Render(fmt.Sprintf("%d of %d drive(s) selected", len(s.selected), len(s.drives))) + "\n")
		}
		if s.err != nil {
			b.WriteString("\n" + errorStyle.Render("✗ "+s.err.Error()) + "\n")
		}
	}
	return b.String()
}

// driveInfo is the dim detail shown after a drive's name.
func driveInfo(d drive.Drive) string {
	if d.Detail == "" {
		return d.ID
	}
	return d.ID + " · " + d.Detail
}

// help lists the selector's keys; the caller appends what esc does.
func (s DriveSelector) help() string {
	if s.loading || len(s.drives) == 0 {
		return "r: rescan"
	}
	if s.single {
		return "↑/↓: move · r: rescan · enter: read disc"
	}
	return "↑/↓: move · space: toggle · a: all/none · r: rescan · enter: confirm"
}
