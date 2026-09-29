package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/remote"
)

// errUploadUnavailable is shown when there is no rxstorage server to upload to.
var errUploadUnavailable = errors.New(`uploading content needs rxstorage sign-in and a server (see -server)`)

const (
	// itemSearchLimit is how many matching items are listed.
	itemSearchLimit = 20
	// itemSearchDelay waits for typing to pause before searching.
	itemSearchDelay = 250 * time.Millisecond
	// contentCountInterval is how often the chosen item's content count is
	// refreshed while files are uploaded to it.
	contentCountInterval = 2 * time.Second
)

// itemSearch finds the rxstorage item that uploaded content is added to.
type itemSearch struct {
	input  textinput.Model
	items  []remote.Item
	cursor int
	offset int

	seq       int  // bumped on every query change so stale results are dropped
	searching bool // a search for seq is in flight
	err       error

	chosen *remote.Item // the item the content goes to

	contents      int       // how many contents the chosen item has
	contentsKnown bool      // contents has been counted for the chosen item
	countSeq      int       // bumped on every count so stale counts are dropped
	counting      bool      // a count for countSeq is in flight
	countedAt     time.Time // when the last count was requested
}

// itemSearchTickMsg fires once typing has paused for query seq.
type itemSearchTickMsg struct{ seq int }

// itemSearchMsg carries the results of search seq.
type itemSearchMsg struct {
	seq   int
	items []remote.Item
	err   error
}

// itemContentsMsg carries count seq of the contents of item itemID.
type itemContentsMsg struct {
	seq    int
	itemID string
	count  int
	err    error
}

func newItemSearch() itemSearch {
	in := textinput.New()
	in.Placeholder = "Type to search items by title"
	in.Focus()
	return itemSearch{input: in}
}

func (s *itemSearch) moveTo(i int) {
	s.cursor = max(0, min(i, len(s.items)-1))
	if s.cursor < s.offset {
		s.offset = s.cursor
	} else if s.cursor >= s.offset+pickerRows {
		s.offset = s.cursor - pickerRows + 1
	}
}

// searchItemsCmd runs search seq against rxstorage.
func (m Model) searchItemsCmd(seq int, query string) tea.Cmd {
	client := m.sync
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		items, err := client.SearchItems(ctx, query, itemSearchLimit)
		return itemSearchMsg{seq: seq, items: items, err: err}
	}
}

// countContents counts the contents of the chosen item, dropping any count
// still in flight.
func (m *Model) countContents() tea.Cmd {
	s := &m.itemSearch
	if s.chosen == nil {
		return nil
	}
	s.countSeq++
	s.counting, s.countedAt = true, time.Now()
	client, seq, itemID := m.sync, s.countSeq, s.chosen.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		n, err := client.CountItemContents(ctx, itemID)
		return itemContentsMsg{seq: seq, itemID: itemID, count: n, err: err}
	}
}

// refreshContents recounts the chosen item's contents when the last count is
// older than contentCountInterval and none is in flight.
func (m *Model) refreshContents() tea.Cmd {
	s := m.itemSearch
	if s.counting || time.Since(s.countedAt) < contentCountInterval {
		return nil
	}
	return m.countContents()
}

// contentCountLabel describes how many contents the chosen item has.
func (m Model) contentCountLabel() string {
	s := m.itemSearch
	if !s.contentsKnown {
		return "counting contents…"
	}
	return fmt.Sprintf("%d content(s)", s.contents)
}

// chooseUpload starts uploading content to an rxstorage item. Uploads go
// through the signed-in account, so a signed-out user is sent to the account
// screen first and continues here once signed in.
func (m Model) chooseUpload() (tea.Model, tea.Cmd) {
	if m.sync == nil {
		m.modeErr = errUploadUnavailable
		return m, nil
	}
	if m.user == nil {
		m.showAccount, m.uploadAfterSignIn = true, true
		return m, nil
	}
	return m.startUpload()
}

// startUpload opens the item search, listing the most recent items until the
// user types a query.
func (m Model) startUpload() (Model, tea.Cmd) {
	m.mode, m.modeChosen = ModeUpload, true
	m.itemSearch = newItemSearch()
	m.itemSearch.searching = true
	return m, m.searchItemsCmd(0, "")
}

// updateUpload handles messages while uploading content to an item.
func (m Model) updateUpload(msg tea.Msg) (tea.Model, tea.Cmd) {
	s := &m.itemSearch
	switch msg := msg.(type) {
	case itemSearchTickMsg:
		if msg.seq != s.seq {
			return m, nil
		}
		s.searching = true
		return m, m.searchItemsCmd(msg.seq, s.input.Value())
	case itemSearchMsg:
		if msg.seq != s.seq {
			return m, nil
		}
		s.searching, s.err = false, msg.err
		if msg.err == nil {
			s.items, s.cursor, s.offset = msg.items, 0, 0
		}
		return m, nil
	case itemContentsMsg:
		if msg.seq != s.countSeq || s.chosen == nil || msg.itemID != s.chosen.ID {
			return m, nil
		}
		s.counting = false
		if msg.err == nil {
			s.contents, s.contentsKnown = msg.count, true
		}
		return m, nil
	case tea.KeyMsg:
		return m.updateUploadKey(msg)
	}
	if s.chosen != nil {
		return m.updateUploadFiles(msg)
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	return m, cmd
}

func (m Model) updateUploadKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := &m.itemSearch
	if s.chosen != nil && m.uploadFiles.stage == uploadStageRunning {
		return m.updateUploadRunKey(key)
	}
	switch key.Type {
	case tea.KeyCtrlC:
		m.cancelled = true
		return m, tea.Quit
	}
	if s.chosen != nil {
		return m.updateUploadFilesKey(key)
	}
	switch key.Type {
	case tea.KeyEsc:
		if m.modeChosen {
			m.mode = ModeNone
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	case tea.KeyUp:
		s.moveTo(s.cursor - 1)
		return m, nil
	case tea.KeyDown:
		s.moveTo(s.cursor + 1)
		return m, nil
	case tea.KeyPgUp:
		s.moveTo(s.cursor - pickerRows)
		return m, nil
	case tea.KeyPgDown:
		s.moveTo(s.cursor + pickerRows)
		return m, nil
	case tea.KeyEnter:
		if s.cursor < len(s.items) {
			item := s.items[s.cursor]
			s.chosen = &item
			s.contentsKnown = false
			s.input.Blur()
			m.uploadFiles = newUploadFiles()
			return m, m.countContents()
		}
		return m, nil
	}

	before := s.input.Value()
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(key)
	if s.input.Value() == before {
		return m, cmd
	}
	s.seq++
	seq := s.seq
	tick := tea.Tick(itemSearchDelay, func(time.Time) tea.Msg { return itemSearchTickMsg{seq: seq} })
	return m, tea.Batch(cmd, tick)
}

// itemLabel is the item's title followed by where it is kept.
func itemLabel(it remote.Item) string {
	var meta []string
	if it.Category != nil && it.Category.Name != "" {
		meta = append(meta, it.Category.Name)
	}
	if it.Location != nil && it.Location.Title != "" {
		meta = append(meta, it.Location.Title)
	}
	label := truncate(it.Title, 50)
	if len(meta) > 0 {
		label += "  " + dimStyle.Render(truncate(strings.Join(meta, " · "), 40))
	}
	return label
}

func (m Model) uploadView() string {
	s := m.itemSearch
	var b strings.Builder
	b.WriteString(titleStyle.Render("ISO Burner · Upload content") + "\n")
	b.WriteString(m.accountSummary() + "\n\n")
	b.WriteString(labelStyle.Render("Upload content to item") + "\n")

	if s.chosen != nil {
		b.WriteString(m.uploadFilesView())
		return panelStyle.Render(b.String()) + "\n"
	}

	b.WriteString(dimStyle.Render("Search for the rxstorage item that the files are added to.") + "\n\n")
	b.WriteString(s.input.View() + "\n\n")
	switch {
	case s.err != nil:
		b.WriteString(errorStyle.Render("✗ "+truncate(s.err.Error(), 70)) + "\n")
	case s.searching && len(s.items) == 0:
		b.WriteString(dimStyle.Render("Searching…") + "\n")
	case len(s.items) == 0 && strings.TrimSpace(s.input.Value()) == "":
		b.WriteString(dimStyle.Render("You have no items yet.") + "\n")
	case len(s.items) == 0:
		b.WriteString(dimStyle.Render("No items match.") + "\n")
	default:
		end := min(s.offset+pickerRows, len(s.items))
		for i := s.offset; i < end; i++ {
			if i == s.cursor {
				b.WriteString(selectedStyle.Render("› ") + itemLabel(s.items[i]) + "\n")
			} else {
				b.WriteString("  " + itemLabel(s.items[i]) + "\n")
			}
		}
		if s.searching {
			b.WriteString(dimStyle.Render("Searching…") + "\n")
		}
	}
	b.WriteString("\n" + dimStyle.Render("type: search · ↑/↓: choose · enter: select · esc: back · ctrl+c: quit"))
	return panelStyle.Render(b.String()) + "\n"
}
