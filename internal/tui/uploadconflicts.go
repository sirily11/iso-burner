package tui

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/settings"
)

// conflictChoice is what to do with a file the item already has content for.
type conflictChoice int

const (
	conflictReplace conflictChoice = iota
	conflictSkip
	conflictReplaceAll
	conflictSkipAll
	conflictStop
)

var conflictChoices = []modeChoice{
	{title: "Replace", desc: "Upload this file and replace the item's content with the same name"},
	{title: "Skip", desc: "Keep the item's content and don't upload this file"},
	{title: "Replace all", desc: "Replace this and every remaining file the item already has"},
	{title: "Skip all", desc: "Skip this and every remaining file the item already has"},
	{title: "Stop", desc: "Upload nothing and go back to the review"},
}

// uploadConflicts asks, one file at a time, what to do with the files the
// item already has content for.
type uploadConflicts struct {
	files  []int        // indexes into the matched files the item already has
	at     int          // the conflict being decided
	skip   map[int]bool // matched files chosen to skip
	cursor int          // highlighted choice
}

// contentTitlesMsg carries the titles of the contents item itemID has.
type contentTitlesMsg struct {
	itemID string
	titles []string
	err    error
}

// listContentTitlesCmd reads the titles of every content the chosen item has.
func (m Model) listContentTitlesCmd() tea.Cmd {
	client, itemID := m.sync, m.itemSearch.chosen.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		titles, err := client.ListContentTitles(ctx, itemID)
		return contentTitlesMsg{itemID: itemID, titles: titles, err: err}
	}
}

// checkUploadConflicts starts the upload from the review screen by first
// finding which files the item already has content for.
func (m Model) checkUploadConflicts() (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	f.loading, f.err = true, nil
	return m, m.listContentTitlesCmd()
}

// conflictingFiles returns the indexes of files whose name, which is the
// title they are uploaded with, is already the title of a content.
func conflictingFiles(files []settings.File, titles []string) []int {
	have := make(map[string]bool, len(titles))
	for _, t := range titles {
		have[t] = true
	}
	var out []int
	for i, file := range files {
		if have[path.Base(file.RelPath)] {
			out = append(out, i)
		}
	}
	return out
}

// updateContentTitles asks about the files the item already has, or starts
// the upload when there are none.
func (m Model) updateContentTitles(msg contentTitlesMsg) (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	if f.stage != uploadStageReview || !f.loading || msg.itemID != m.itemSearch.chosen.ID {
		return m, nil
	}
	f.loading = false
	if msg.err != nil {
		f.err = fmt.Errorf("check the item's contents: %w", msg.err)
		return m, nil
	}
	files := conflictingFiles(f.matched, msg.titles)
	if len(files) == 0 {
		return m.startUploadRun(nil)
	}
	f.conflicts = uploadConflicts{files: files, skip: map[int]bool{}}
	f.stage = uploadStageConflicts
	return m, nil
}

// updateUploadConflictsKey handles keys on the screen that asks what to do
// with a file the item already has.
func (m Model) updateUploadConflictsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := &m.uploadFiles.conflicts
	switch key.String() {
	case "up", "k", "shift+tab":
		c.cursor = (c.cursor + len(conflictChoices) - 1) % len(conflictChoices)
	case "down", "j", "tab":
		c.cursor = (c.cursor + 1) % len(conflictChoices)
	case "enter":
		return m.decideConflict(conflictChoice(c.cursor))
	case "1", "2", "3", "4", "5":
		return m.decideConflict(conflictChoice(key.Runes[0] - '1'))
	case "r":
		return m.decideConflict(conflictReplace)
	case "s":
		return m.decideConflict(conflictSkip)
	case "R":
		return m.decideConflict(conflictReplaceAll)
	case "S":
		return m.decideConflict(conflictSkipAll)
	case "esc", "x":
		return m.decideConflict(conflictStop)
	}
	return m, nil
}

// decideConflict applies choice to the file being asked about, or to it and
// every remaining one, and starts the upload once every file is decided.
func (m Model) decideConflict(choice conflictChoice) (tea.Model, tea.Cmd) {
	f := &m.uploadFiles
	c := &f.conflicts
	switch choice {
	case conflictStop:
		f.stage, f.conflicts = uploadStageReview, uploadConflicts{}
		return m, nil
	case conflictReplace, conflictSkip:
		c.skip[c.files[c.at]] = choice == conflictSkip
		c.at++
	case conflictReplaceAll, conflictSkipAll:
		for _, i := range c.files[c.at:] {
			c.skip[i] = choice == conflictSkipAll
		}
		c.at = len(c.files)
	}
	c.cursor = 0
	if c.at < len(c.files) {
		return m, nil
	}
	return m.startUploadRun(c.skip)
}

func (m Model) uploadConflictsView() string {
	f := m.uploadFiles
	c := f.conflicts
	file := f.matched[c.files[c.at]]
	name := path.Base(file.RelPath)
	var b strings.Builder
	b.WriteString(errorStyle.Render(fmt.Sprintf("%d of %d file(s) are already on the item", len(c.files), len(f.matched))) + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("File %d of %d", c.at+1, len(c.files))) + "\n\n")
	b.WriteString(labelStyle.Render(truncate(name, 60)) + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  (%s)", truncate(file.RelPath, 60), settings.FormatBytes(file.Size))) + "\n")
	b.WriteString(fmt.Sprintf("  %s already has content named %q.\n\n", truncate(m.itemSearch.chosen.Title, 40), truncate(name, 40)))
	for i, choice := range conflictChoices {
		line := fmt.Sprintf("%d. %s", i+1, choice.title)
		if i == c.cursor {
			b.WriteString(selectedStyle.Render("› "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("     " + dimStyle.Render(choice.desc) + "\n")
	}
	if c.at > 0 {
		skipped := 0
		for _, skip := range c.skip {
			if skip {
				skipped++
			}
		}
		b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("So far: %d to replace · %d to skip", c.at-skipped, skipped)) + "\n")
	}
	return b.String()
}
