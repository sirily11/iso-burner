package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/settings"
)

// isoEntry is one row in the ISO picker: a subfolder or an .iso file.
type isoEntry struct {
	name  string
	isDir bool
	size  int64
}

// isoPicker browses the filesystem and lets the user mark any number of ISO
// files, possibly from different folders, to burn.
type isoPicker struct {
	dir        string
	entries    []isoEntry
	cursor     int
	offset     int
	showHidden bool
	selected   map[string]int64 // absolute path → size
	single     bool             // enter picks the highlighted ISO; nothing is marked
	err        error
}

// newISOPicker starts browsing like newFolderPicker does.
func newISOPicker(hint string) isoPicker {
	p := isoPicker{selected: map[string]int64{}}
	p.load(startDir(hint), "")
	return p
}

// newSingleISOPicker browses for one ISO file, chosen with enter.
func newSingleISOPicker(hint string) isoPicker {
	p := newISOPicker(hint)
	p.single = true
	return p
}

// preselect marks the given ISO files that still exist.
func (p *isoPicker) preselect(paths []string) {
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && isISO(path) {
			p.selected[path] = info.Size()
		}
	}
}

func isISO(name string) bool { return strings.EqualFold(filepath.Ext(name), ".iso") }

// load lists the subfolders and ISO files of dir, folders first, and places
// the cursor on focus when it is one of them.
func (p *isoPicker) load(dir, focus string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.err = err
		return
	}
	p.dir, p.err = dir, nil
	p.entries = p.entries[:0]
	for _, e := range entries {
		if !p.showHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, e.Name())) // follows symlinks
		if err != nil {
			continue
		}
		switch {
		case info.IsDir():
			p.entries = append(p.entries, isoEntry{name: e.Name(), isDir: true})
		case info.Mode().IsRegular() && isISO(e.Name()):
			p.entries = append(p.entries, isoEntry{name: e.Name(), size: info.Size()})
		}
	}
	sort.Slice(p.entries, func(i, j int) bool {
		a, b := p.entries[i], p.entries[j]
		if a.isDir != b.isDir {
			return a.isDir
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	p.cursor, p.offset = 0, 0
	for i, e := range p.entries {
		if e.name == focus {
			p.moveTo(i)
			break
		}
	}
}

func (p *isoPicker) moveTo(i int) {
	p.cursor = max(0, min(i, len(p.entries)-1))
	if p.cursor < p.offset {
		p.offset = p.cursor
	} else if p.cursor >= p.offset+pickerRows {
		p.offset = p.cursor - pickerRows + 1
	}
}

func (p isoPicker) path(e isoEntry) string { return filepath.Join(p.dir, e.name) }

// isosHere returns the ISO files listed in the current folder.
func (p isoPicker) isosHere() []isoEntry {
	var out []isoEntry
	for _, e := range p.entries {
		if !e.isDir {
			out = append(out, e)
		}
	}
	return out
}

// toggleAll selects every ISO in the current folder, or clears them when all
// are already selected.
func (p *isoPicker) toggleAll() {
	isos := p.isosHere()
	all := len(isos) > 0
	for _, e := range isos {
		if _, ok := p.selected[p.path(e)]; !ok {
			all = false
			break
		}
	}
	for _, e := range isos {
		if all {
			delete(p.selected, p.path(e))
		} else {
			p.selected[p.path(e)] = e.size
		}
	}
}

// selection returns the selected ISO paths in sorted order.
func (p isoPicker) selection() []string {
	out := make([]string, 0, len(p.selected))
	for path := range p.selected {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func (p isoPicker) selectedSize() int64 {
	var total int64
	for _, size := range p.selected {
		total += size
	}
	return total
}

// update handles a key press. It returns the chosen ISO paths when the user
// confirms, and closed=true when the picker should be dismissed.
func (p isoPicker) update(msg tea.KeyMsg) (next isoPicker, chosen []string, closed bool) {
	p.err = nil
	switch msg.String() {
	case "esc":
		return p, nil, true
	case "enter":
		if p.single {
			if len(p.entries) == 0 {
				p.err = errors.New("no ISO files here")
				return p, nil, false
			}
			if e := p.entries[p.cursor]; !e.isDir {
				return p, []string{p.path(e)}, true
			}
			p.load(p.path(p.entries[p.cursor]), "")
			return p, nil, false
		}
		if len(p.selected) == 0 && len(p.entries) > 0 && !p.entries[p.cursor].isDir {
			e := p.entries[p.cursor]
			p.selected[p.path(e)] = e.size
		}
		if len(p.selected) == 0 {
			p.err = errors.New("select at least one ISO file (space: toggle · a: all)")
			return p, nil, false
		}
		return p, p.selection(), true
	case " ", "space":
		if !p.single && len(p.entries) > 0 && !p.entries[p.cursor].isDir {
			e := p.entries[p.cursor]
			if _, ok := p.selected[p.path(e)]; ok {
				delete(p.selected, p.path(e))
			} else {
				p.selected[p.path(e)] = e.size
			}
			p.moveTo(p.cursor + 1)
		}
	case "a", "ctrl+a":
		if !p.single {
			p.toggleAll()
		}
	case "c":
		if !p.single {
			p.selected = map[string]int64{}
		}
	case "up", "k":
		p.moveTo(p.cursor - 1)
	case "down", "j":
		p.moveTo(p.cursor + 1)
	case "pgup":
		p.moveTo(p.cursor - pickerRows)
	case "pgdown":
		p.moveTo(p.cursor + pickerRows)
	case "home", "g":
		p.moveTo(0)
	case "end", "G":
		p.moveTo(len(p.entries) - 1)
	case "right", "l":
		if len(p.entries) > 0 && p.entries[p.cursor].isDir {
			p.load(p.path(p.entries[p.cursor]), "")
		}
	case "left", "h", "backspace":
		if parent := filepath.Dir(p.dir); parent != p.dir {
			p.load(parent, filepath.Base(p.dir))
		}
	case ".":
		p.showHidden = !p.showHidden
		focus := ""
		if len(p.entries) > 0 {
			focus = p.entries[p.cursor].name
		}
		p.load(p.dir, focus)
	}
	return p, nil, false
}

func (p isoPicker) view() string {
	var b strings.Builder
	title := "Choose ISO files to burn"
	if p.single {
		title = "Choose an ISO file"
	}
	b.WriteString(labelStyle.Render(title) + "\n")
	b.WriteString(okStyle.Render(p.dir) + "\n\n")
	if len(p.entries) == 0 {
		b.WriteString(dimStyle.Render("  (no subfolders or ISO files)") + "\n")
	}
	end := min(p.offset+pickerRows, len(p.entries))
	for i := p.offset; i < end; i++ {
		e := p.entries[i]
		var line string
		if e.isDir {
			line = "    📁 " + e.name + string(filepath.Separator)
		} else if p.single {
			line = fmt.Sprintf("💿 %s  %s", e.name, dimStyle.Render(settings.FormatBytes(e.size)))
		} else {
			mark := "[ ]"
			if _, ok := p.selected[p.path(e)]; ok {
				mark = "[x]"
			}
			line = fmt.Sprintf("%s 💿 %s  %s", mark, e.name, dimStyle.Render(settings.FormatBytes(e.size)))
		}
		if i == p.cursor {
			b.WriteString(selectedStyle.Render("› "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if len(p.entries) > pickerRows {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing %d–%d of %d entries", p.offset+1, end, len(p.entries))) + "\n")
	}
	if p.single {
		if p.err != nil {
			b.WriteString("\n" + errorStyle.Render("✗ "+p.err.Error()) + "\n")
		}
		return b.String()
	}
	summary := fmt.Sprintf("%d ISO file(s) selected · %s", len(p.selected), settings.FormatBytes(p.selectedSize()))
	if n := len(p.isosHere()); n > 0 {
		summary += dimStyle.Render(fmt.Sprintf("  (%d in this folder)", n))
	}
	b.WriteString("\n" + okStyle.Render(summary) + "\n")
	if p.err != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+p.err.Error()) + "\n")
	}
	return b.String()
}

func (p isoPicker) help() string {
	if p.single {
		return "↑/↓: move · →: open · ←: parent · .: hidden · enter: use highlighted ISO · esc: back"
	}
	return "↑/↓: move · →: open · ←: parent · space: toggle · a: all in folder · c: clear · .: hidden · enter: burn selected"
}
