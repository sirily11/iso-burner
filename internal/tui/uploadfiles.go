package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/settings"
)

// uploadRule is how the files to upload are chosen.
type uploadRule int

const (
	// ruleFolder uploads the files in a folder whose paths match a regex.
	ruleFolder uploadRule = iota
	// ruleISO uploads every file stored in an ISO image.
	ruleISO
	// ruleDisc uploads every file on the disc in a disc drive.
	ruleDisc
)

var uploadRules = []modeChoice{
	{title: "Files in a folder matching a regex", desc: "Pick a folder, then filter its files with a regex or glob"},
	{title: "All files in an ISO", desc: "Pick an ISO image and upload everything stored in it"},
	{title: "All files on a disc", desc: "Pick a disc drive and upload everything on the disc in it"},
}

// uploadStage is the screen shown once an item has been chosen.
type uploadStage int

const (
	uploadStageRule uploadStage = iota
	uploadStageFolder
	uploadStagePattern
	uploadStageISO
	uploadStageDrive
	uploadStageReview
	uploadStageRunning
)

// uploadFiles chooses the files that are uploaded to the chosen item.
type uploadFiles struct {
	stage   uploadStage
	rule    uploadRule
	ruleIdx int

	picker    folderPicker
	isoPicker isoPicker
	pattern   textinput.Model
	drives    DriveSelector

	folder  string      // chosen folder, for ruleFolder, or where the disc is mounted for ruleDisc
	iso     string      // chosen ISO image, for ruleISO
	disc    drive.Drive // chosen drive, for ruleDisc
	all     []settings.File
	matched []settings.File // the files to upload
	loading bool            // the folder is being scanned or the ISO read
	offset  int             // first file shown on the review screen
	err     error

	run uploadRun // the upload, once started from the review screen
}

// isoListMsg carries the files read from an ISO image.
type isoListMsg struct {
	path  string
	files []settings.File
	err   error
}

func listISOCmd(path string) tea.Cmd {
	return func() tea.Msg {
		files, err := iso.ListFiles(path)
		return isoListMsg{path: path, files: files, err: err}
	}
}

// discFilesMsg carries the files on the disc in a drive and where the disc is
// mounted.
type discFilesMsg struct {
	driveID string
	root    string
	files   []settings.File
	err     error
}

// listDiscCmd finds where the disc in d is mounted and lists its files.
func (m Model) listDiscCmd(d drive.Drive) tea.Cmd {
	discRoot := m.discRoot
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		root, err := discRoot(ctx, d)
		if err != nil {
			return discFilesMsg{driveID: d.ID, err: err}
		}
		files, err := settings.ScanFolder(root)
		if err != nil {
			err = fmt.Errorf("read disc at %s: %w", root, err)
		}
		return discFilesMsg{driveID: d.ID, root: root, files: files, err: err}
	}
}

func newUploadFiles() uploadFiles {
	pattern := textinput.New()
	pattern.Placeholder = `.*  (regex like \.(jpg|mp4)$ or glob like **/*.mp4)`
	return uploadFiles{pattern: pattern}
}

// refreshMatches re-applies the pattern to the scanned folder.
func (f *uploadFiles) refreshMatches() {
	re, err := settings.CompilePattern(f.pattern.Value())
	if err != nil {
		f.err, f.matched = err, nil
		return
	}
	f.err, f.matched = nil, settings.Filter(f.all, re)
}

// updateUploadFiles handles non-key messages once an item has been chosen.
func (m Model) updateUploadFiles(msg tea.Msg) (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	if f.stage == uploadStageRunning {
		return m.updateUploadRun(msg)
	}
	switch msg := msg.(type) {
	case scanDoneMsg:
		if f.stage != uploadStageFolder || !f.loading || msg.folder != f.folder {
			return m, nil
		}
		f.loading = false
		if msg.err != nil {
			f.err = fmt.Errorf("scan failed: %w", msg.err)
			return m, nil
		}
		f.all, f.stage = msg.files, uploadStagePattern
		f.refreshMatches()
		return m, f.pattern.Focus()
	case isoListMsg:
		if f.stage != uploadStageISO || !f.loading || msg.path != f.iso {
			return m, nil
		}
		f.loading = false
		switch {
		case msg.err != nil:
			f.err = msg.err
		case len(msg.files) == 0:
			f.err = errors.New("the ISO has no files")
		default:
			f.all, f.matched = msg.files, msg.files
			f.stage, f.offset = uploadStageReview, 0
		}
		return m, nil
	case discFilesMsg:
		if f.stage != uploadStageDrive || !f.loading || msg.driveID != f.disc.ID {
			return m, nil
		}
		f.loading = false
		switch {
		case msg.err != nil:
			f.err = msg.err
		case len(msg.files) == 0:
			f.err = errors.New("the disc has no files")
		default:
			f.folder, f.all, f.matched = msg.root, msg.files, msg.files
			f.stage, f.offset = uploadStageReview, 0
		}
		return m, nil
	}
	if f.stage == uploadStageDrive {
		var cmd tea.Cmd
		f.drives, cmd = f.drives.update(msg)
		return m, cmd
	}
	if f.stage == uploadStagePattern {
		var cmd tea.Cmd
		f.pattern, cmd = f.pattern.Update(msg)
		return m, cmd
	}
	return m, nil
}

// updateUploadFilesKey handles keys once an item has been chosen. Esc steps
// back one screen, and from the rule choice back to the item search.
func (m Model) updateUploadFilesKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	if f.loading {
		return m, nil
	}
	switch f.stage {
	case uploadStageRule:
		return m.updateUploadRule(key)

	case uploadStageFolder:
		next, chosen, closed := f.picker.update(key)
		f.picker = next
		if !closed {
			return m, nil
		}
		if chosen == "" {
			f.stage, f.err = uploadStageRule, nil
			return m, nil
		}
		f.folder, f.loading, f.err = chosen, true, nil
		return m, scanCmd(chosen)

	case uploadStagePattern:
		switch key.Type {
		case tea.KeyEsc:
			f.pattern.Blur()
			f.stage, f.err = uploadStageFolder, nil
			f.picker = newFolderPickerAt(f.folder)
			return m, nil
		case tea.KeyEnter:
			if f.err != nil {
				return m, nil
			}
			if len(f.matched) == 0 {
				f.err = errors.New("regex matches no files")
				return m, nil
			}
			f.pattern.Blur()
			f.stage, f.offset = uploadStageReview, 0
			return m, nil
		}
		prev := f.pattern.Value()
		var cmd tea.Cmd
		f.pattern, cmd = f.pattern.Update(key)
		if f.pattern.Value() != prev {
			f.refreshMatches()
		}
		return m, cmd

	case uploadStageISO:
		f.err = nil
		next, chosen, closed := f.isoPicker.update(key)
		f.isoPicker = next
		if !closed {
			return m, nil
		}
		if len(chosen) == 0 {
			f.stage = uploadStageRule
			return m, nil
		}
		f.iso, f.loading = chosen[0], true
		return m, listISOCmd(f.iso)

	case uploadStageDrive:
		f.err = nil
		var cmd tea.Cmd
		f.drives, cmd = f.drives.update(key)
		switch {
		case f.drives.Cancelled():
			f.stage = uploadStageRule
			return m, nil
		case f.drives.Done():
			f.disc, f.loading = f.drives.Selected()[0], true
			f.drives.done = false // stay on the list if reading the disc fails
			return m, m.listDiscCmd(f.disc)
		}
		return m, cmd

	case uploadStageReview:
		switch key.String() {
		case "enter":
			return m.startUploadRun()
		case "esc":
			switch f.rule {
			case ruleISO:
				f.stage = uploadStageISO
				return m, nil
			case ruleDisc:
				f.stage = uploadStageDrive
				var cmd tea.Cmd
				f.drives, cmd = f.drives.reopen() // the disc may have been swapped
				return m, cmd
			}
			f.stage = uploadStagePattern
			return m, f.pattern.Focus()
		case "up", "k":
			f.offset = max(0, f.offset-1)
		case "down", "j":
			f.offset = max(0, min(f.offset+1, len(f.matched)-maxPreviewFiles))
		case "pgup":
			f.offset = max(0, f.offset-maxPreviewFiles)
		case "pgdown":
			f.offset = max(0, min(f.offset+maxPreviewFiles, len(f.matched)-maxPreviewFiles))
		}
	}
	return m, nil
}

// updateUploadRule handles keys on the screen that picks how files are chosen.
func (m Model) updateUploadRule(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	switch key.String() {
	case "esc":
		m.itemSearch.chosen = nil
		return m, m.itemSearch.input.Focus()
	case "up", "k", "shift+tab":
		f.ruleIdx = (f.ruleIdx + len(uploadRules) - 1) % len(uploadRules)
	case "down", "j", "tab":
		f.ruleIdx = (f.ruleIdx + 1) % len(uploadRules)
	case "1", "2", "3":
		f.ruleIdx = int(key.Runes[0] - '1')
		return m.chooseUploadRule()
	case "enter":
		return m.chooseUploadRule()
	}
	return m, nil
}

// chooseUploadRule opens the browser for the highlighted rule, starting where
// the last folder or ISO was picked.
func (m Model) chooseUploadRule() (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	f.rule, f.err = uploadRule(f.ruleIdx), nil
	if f.rule == ruleISO {
		f.stage = uploadStageISO
		if f.iso != "" {
			f.isoPicker = newSingleISOPicker(filepath.Dir(f.iso))
		} else {
			f.isoPicker = newSingleISOPicker(m.isoHint)
		}
		return m, nil
	}
	if f.rule == ruleDisc {
		f.stage = uploadStageDrive
		f.drives = newSingleDriveSelector(m.listDrives)
		return m, f.drives.Init()
	}
	f.stage = uploadStageFolder
	f.picker = newFolderPickerAt(cmp.Or(f.folder, m.recent.Folder))
	return m, nil
}

func (m Model) uploadFilesView() string {
	f := m.uploadFiles
	var b strings.Builder
	switch f.stage {
	case uploadStageRule:
		b.WriteString(dimStyle.Render("Files will be added to this rxstorage item.") + "\n")
		b.WriteString(selectedStyle.Render("✓ "+itemLabel(*m.itemSearch.chosen)) + "\n\n")
		b.WriteString(labelStyle.Render("Which files do you want to upload?") + "\n\n")
		for i, r := range uploadRules {
			line := fmt.Sprintf("%d. %s", i+1, r.title)
			if i == f.ruleIdx {
				b.WriteString(selectedStyle.Render("› "+line) + "\n")
			} else {
				b.WriteString("  " + line + "\n")
			}
			b.WriteString("     " + dimStyle.Render(r.desc) + "\n")
		}
	case uploadStageFolder:
		b.WriteString(f.picker.view())
		if f.loading {
			b.WriteString("\n" + dimStyle.Render("Scanning folder…") + "\n")
		}
	case uploadStagePattern:
		b.WriteString(labelStyle.Render("File regex") + dimStyle.Render("  (regex or glob, matched against paths relative to the folder)") + "\n")
		b.WriteString(dimStyle.Render(f.folder) + "\n")
		b.WriteString(f.pattern.View() + "\n\n")
		if f.err == nil {
			b.WriteString(okStyle.Render(fmt.Sprintf("%d of %d files match · %s",
				len(f.matched), len(f.all), settings.FormatBytes(settings.TotalSize(f.matched)))) + "\n")
			for i, file := range f.matched {
				if i == maxPreviewFiles {
					b.WriteString(dimStyle.Render(fmt.Sprintf("  … and %d more", len(f.matched)-maxPreviewFiles)) + "\n")
					break
				}
				b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  (%s)", file.RelPath, settings.FormatBytes(file.Size))) + "\n")
			}
		}
	case uploadStageISO:
		b.WriteString(f.isoPicker.view())
		if f.loading {
			b.WriteString("\n" + dimStyle.Render("Reading ISO…") + "\n")
		}
	case uploadStageDrive:
		b.WriteString(f.drives.body())
		if f.loading {
			b.WriteString("\n" + dimStyle.Render("Reading disc…") + "\n")
		}
	case uploadStageReview:
		b.WriteString(m.uploadReviewView())
	case uploadStageRunning:
		b.WriteString(m.uploadRunView())
	}
	if f.err != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+truncate(f.err.Error(), 70)) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render(m.uploadFilesHelp()))
	return b.String()
}

func (m Model) uploadReviewView() string {
	f := m.uploadFiles
	var b strings.Builder
	row := func(k, v string) {
		b.WriteString(labelStyle.Render(fmt.Sprintf("%-8s", k)) + " " + v + "\n")
	}
	row("Item", itemLabel(*m.itemSearch.chosen))
	switch f.rule {
	case ruleISO:
		row("ISO", f.iso)
	case ruleDisc:
		row("Disc", f.disc.Name()+"  "+dimStyle.Render(f.folder))
	default:
		row("Folder", f.folder)
		row("Regex", cmp.Or(f.pattern.Value(), ".*"))
	}
	row("Files", fmt.Sprintf("%d (%s)", len(f.matched), settings.FormatBytes(settings.TotalSize(f.matched))))
	b.WriteString("\n")
	end := min(f.offset+maxPreviewFiles, len(f.matched))
	for _, file := range f.matched[f.offset:end] {
		b.WriteString(fmt.Sprintf("  %s  %s\n", file.RelPath, dimStyle.Render(settings.FormatBytes(file.Size))))
	}
	if len(f.matched) > maxPreviewFiles {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Showing files %d–%d of %d", f.offset+1, end, len(f.matched))) + "\n")
	}
	return b.String()
}

func (m Model) uploadFilesHelp() string {
	f := m.uploadFiles
	switch f.stage {
	case uploadStageRule:
		return "↑/↓: choose · 1-3 or enter: select · esc: choose another item · ctrl+c: quit"
	case uploadStageFolder:
		return "↑/↓: move · →: open · ←: parent · enter: use highlighted · s: use this folder · .: hidden · esc: back"
	case uploadStagePattern:
		return "type: regex · enter: review files · esc: choose another folder · ctrl+c: quit"
	case uploadStageISO:
		return f.isoPicker.help()
	case uploadStageDrive:
		return f.drives.help() + " · esc: back"
	case uploadStageRunning:
		return m.uploadRunHelp()
	}
	return "↑/↓: scroll files · enter: upload · esc: back · ctrl+c: quit"
}
