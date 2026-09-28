package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// pickerRows is how many directories the folder picker shows at once.
const pickerRows = 10

// folderPicker browses the filesystem one directory at a time so a source
// folder can be chosen without typing its path.
type folderPicker struct {
	dir        string
	entries    []string
	cursor     int
	offset     int
	showHidden bool
	err        error
}

// newFolderPicker starts browsing at hint when it names a directory, else at
// its nearest existing parent, else at the working directory.
func newFolderPicker(hint string) folderPicker {
	var p folderPicker
	p.load(startDir(hint), "")
	return p
}

func startDir(hint string) string {
	hint = strings.TrimSpace(hint)
	if strings.HasPrefix(hint, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			hint = filepath.Join(home, hint[1:])
		}
	}
	if hint != "" {
		for dir := filepath.Clean(hint); ; dir = filepath.Dir(dir) {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				if abs, err := filepath.Abs(dir); err == nil {
					return abs
				}
				return dir
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return string(filepath.Separator)
}

// load lists the subdirectories of dir and places the cursor on focus when
// it is one of them.
func (p *folderPicker) load(dir, focus string) {
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
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(dir, e.Name()))
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			p.entries = append(p.entries, e.Name())
		}
	}
	sort.Slice(p.entries, func(i, j int) bool {
		return strings.ToLower(p.entries[i]) < strings.ToLower(p.entries[j])
	})
	p.cursor, p.offset = 0, 0
	for i, name := range p.entries {
		if name == focus {
			p.moveTo(i)
			break
		}
	}
}

func (p *folderPicker) moveTo(i int) {
	p.cursor = max(0, min(i, len(p.entries)-1))
	if p.cursor < p.offset {
		p.offset = p.cursor
	} else if p.cursor >= p.offset+pickerRows {
		p.offset = p.cursor - pickerRows + 1
	}
}

func (p folderPicker) highlighted() string {
	if len(p.entries) == 0 {
		return p.dir
	}
	return filepath.Join(p.dir, p.entries[p.cursor])
}

// update handles a key press. It returns the chosen folder when the user
// picks one, and closed=true when the picker should be dismissed.
func (p folderPicker) update(msg tea.KeyMsg) (next folderPicker, chosen string, closed bool) {
	switch msg.String() {
	case "esc":
		return p, "", true
	case "enter":
		return p, p.highlighted(), true
	case "s":
		return p, p.dir, true
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
		if len(p.entries) > 0 {
			p.load(p.highlighted(), "")
		}
	case "left", "h", "backspace":
		if parent := filepath.Dir(p.dir); parent != p.dir {
			p.load(parent, filepath.Base(p.dir))
		}
	case ".":
		p.showHidden = !p.showHidden
		focus := ""
		if len(p.entries) > 0 {
			focus = p.entries[p.cursor]
		}
		p.load(p.dir, focus)
	}
	return p, "", false
}

func (p folderPicker) view() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Choose source folder") + "\n")
	b.WriteString(okStyle.Render(p.dir) + "\n\n")
	if len(p.entries) == 0 {
		b.WriteString(dimStyle.Render("  (no subfolders)") + "\n")
	}
	end := min(p.offset+pickerRows, len(p.entries))
	for i := p.offset; i < end; i++ {
		line := "📁 " + p.entries[i] + string(filepath.Separator)
		if i == p.cursor {
			b.WriteString(selectedStyle.Render("› "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if len(p.entries) > pickerRows {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing %d–%d of %d folders", p.offset+1, end, len(p.entries))) + "\n")
	}
	if p.err != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+p.err.Error()) + "\n")
	}
	return b.String()
}

func (p folderPicker) help() string {
	return "↑/↓: move · →: open · ←: parent · enter: use highlighted · s: use this folder · .: hidden · esc: type path"
}
