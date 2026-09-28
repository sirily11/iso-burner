package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/auth"
	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/recent"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
)

func TestUploadModeSignedIn(t *testing.T) {
	svc := &fakeAuth{user: &auth.User{ID: "u1", Name: "Ada"}}
	m := New(Options{Auth: svc, Sync: newItemServer(t, sampleItems).client})
	m = run(t, m, checkAuthCmd(svc))
	if !strings.Contains(m.View(), "3. Upload content to item") {
		t.Fatalf("mode view missing upload choice:\n%s", m.View())
	}

	m = send(t, m, key("3"))
	if m.Mode() != ModeUpload || !strings.Contains(m.View(), "Upload content to item") {
		t.Fatalf("3 should open upload: mode=%d\n%s", m.Mode(), m.View())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.Mode() != ModeNone || m.cancelled {
		t.Fatalf("esc in upload should return to mode selection: mode=%d cancelled=%v", m.Mode(), m.cancelled)
	}
}

func TestUploadModeSignsInFirst(t *testing.T) {
	svc := &fakeAuth{}
	m := New(Options{Auth: svc, Sync: newItemServer(t, sampleItems).client})
	m = run(t, m, checkAuthCmd(svc))

	m = send(t, m, key("3"))
	if m.Mode() != ModeNone || !m.showAccount || !strings.Contains(m.View(), "Uploading content needs your rxstorage account") {
		t.Fatalf("signed-out upload should open the account screen: mode=%d\n%s", m.Mode(), m.View())
	}
	next, cmd := m.Update(enter)
	m = run(t, next.(Model), cmd)
	if m.Mode() != ModeUpload || m.showAccount {
		t.Fatalf("signing in should continue into upload: mode=%d account=%v\n%s", m.Mode(), m.showAccount, m.View())
	}
}

func TestUploadModeBackOutOfSignIn(t *testing.T) {
	svc := &fakeAuth{}
	m := New(Options{Auth: svc, Sync: newItemServer(t, sampleItems).client})
	m = run(t, m, checkAuthCmd(svc))
	m = send(t, m, key("3"))
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showAccount || m.uploadAfterSignIn {
		t.Fatal("esc on the account screen should drop the pending upload")
	}

	// Signing in from the account screen later stays on mode selection.
	m = send(t, m, key("a"))
	next, cmd := m.Update(enter)
	m = run(t, next.(Model), cmd)
	if m.Mode() != ModeNone {
		t.Fatalf("plain sign-in should not start an upload: mode=%d", m.Mode())
	}
}

func TestUploadModeUnavailableWithoutServer(t *testing.T) {
	m := New(Options{})
	m = send(t, m, key("3"))
	if m.Mode() != ModeNone || !strings.Contains(m.View(), "uploading content needs rxstorage sign-in") {
		t.Fatalf("upload without a server should explain why: mode=%d\n%s", m.Mode(), m.View())
	}
}

var sampleItems = []remote.Item{
	{ID: "i1", Title: "Movie archive"},
	{ID: "i2", Title: "Photo backup 2024"},
	{ID: "i3", Title: "Photo backup 2025"},
}

// itemServer is a fake rxstorage server that searches items by title.
type itemServer struct {
	client *remote.Client

	mu      sync.Mutex
	queries []string
}

func newItemServer(t *testing.T, items []remote.Item) *itemServer {
	s := &itemServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/items" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := r.URL.Query().Get("search")
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.mu.Unlock()
		found := []remote.Item{}
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.Title), strings.ToLower(q)) {
				found = append(found, it)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": found})
	}))
	t.Cleanup(srv.Close)
	s.client = remote.NewClient(srv.URL, testToken{})
	return s
}

// openUpload signs in and opens the item search.
func openUpload(t *testing.T, items []remote.Item) (Model, *itemServer) {
	t.Helper()
	server := newItemServer(t, items)
	svc := &fakeAuth{user: &auth.User{ID: "u1", Name: "Ada"}}
	m := New(Options{Auth: svc, Sync: server.client})
	m = run(t, m, checkAuthCmd(svc))
	next, cmd := m.Update(key("3"))
	return run(t, next.(Model), cmd), server
}

// search types query and runs the search once typing pauses.
func search(t *testing.T, m Model, query string) Model {
	t.Helper()
	for _, r := range query {
		m = send(t, m, key(string(r)))
	}
	next, cmd := m.Update(itemSearchTickMsg{seq: m.itemSearch.seq})
	return run(t, next.(Model), cmd)
}

func TestUploadSearchListsRecentItems(t *testing.T) {
	m, server := openUpload(t, sampleItems)
	view := m.View()
	for _, it := range sampleItems {
		if !strings.Contains(view, it.Title) {
			t.Fatalf("opening the search should list recent items, missing %q:\n%s", it.Title, view)
		}
	}
	if len(server.queries) != 1 || server.queries[0] != "" {
		t.Fatalf("queries = %q, want one empty search", server.queries)
	}
}

func TestUploadSearchFiltersAndSelects(t *testing.T) {
	m, server := openUpload(t, sampleItems)
	m = search(t, m, "photo")
	if got := server.queries[len(server.queries)-1]; got != "photo" {
		t.Fatalf("searched %q, want photo", got)
	}
	view := m.View()
	if strings.Contains(view, "Movie archive") || !strings.Contains(view, "Photo backup 2025") {
		t.Fatalf("results should only show matching items:\n%s", view)
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = send(t, m, enter)
	if m.itemSearch.chosen == nil || m.itemSearch.chosen.ID != "i3" {
		t.Fatalf("enter should choose the highlighted item, got %+v", m.itemSearch.chosen)
	}
	if !strings.Contains(m.View(), "✓ Photo backup 2025") {
		t.Fatalf("view should show the chosen item:\n%s", m.View())
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.Mode() != ModeUpload || m.itemSearch.chosen != nil || m.itemSearch.input.Value() != "photo" {
		t.Fatalf("esc should return to the search and keep the query: mode=%d chosen=%v", m.Mode(), m.itemSearch.chosen)
	}
}

func TestUploadSearchNoMatches(t *testing.T) {
	m, _ := openUpload(t, sampleItems)
	m = search(t, m, "zzz")
	if !strings.Contains(m.View(), "No items match.") {
		t.Fatalf("empty results should say so:\n%s", m.View())
	}
	m = send(t, m, enter)
	if m.itemSearch.chosen != nil {
		t.Fatal("enter with no results should not choose anything")
	}
}

func TestUploadSearchDropsStaleResults(t *testing.T) {
	m, _ := openUpload(t, sampleItems)
	m = send(t, m, key("p"))
	stale := m.itemSearch.seq
	m = send(t, m, key("h"))
	m = send(t, m, itemSearchMsg{seq: stale, items: sampleItems[:1]})
	if len(m.itemSearch.items) != len(sampleItems) {
		t.Fatalf("results of an older query should be ignored, got %+v", m.itemSearch.items)
	}
	next, _ := m.Update(itemSearchTickMsg{seq: stale})
	if next.(Model).itemSearch.searching {
		t.Fatal("an older typing pause should not start a search")
	}
}

// pickFile sends msg and, when it starts scanning a folder or reading an ISO,
// delivers the result.
func pickFile(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	m = next.(Model)
	if cmd != nil && m.uploadFiles.loading {
		next, _ = m.Update(cmd())
		m = next.(Model)
	}
	return m
}

// openFileRules opens the upload screen with recent picks from last and
// chooses the first item.
func openFileRules(t *testing.T, last recent.Recent) Model {
	t.Helper()
	server := newItemServer(t, sampleItems)
	svc := &fakeAuth{user: &auth.User{ID: "u1", Name: "Ada"}}
	m := New(Options{Auth: svc, Sync: server.client, Recent: last})
	m = run(t, m, checkAuthCmd(svc))
	next, cmd := m.Update(key("3"))
	m = run(t, next.(Model), cmd)
	m = send(t, m, enter)
	if !strings.Contains(m.View(), "Which files do you want to upload?") {
		t.Fatalf("choosing an item should ask which files to upload:\n%s", m.View())
	}
	return m
}

func writeSizedFiles(t *testing.T, root string, sizes map[string]int) {
	t.Helper()
	for name, size := range sizes {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUploadFilesFromFolderByRegex(t *testing.T) {
	root := t.TempDir()
	writeSizedFiles(t, root, map[string]int{"a.jpg": 10, "trip/b.jpg": 20, "notes.txt": 5})
	m := openFileRules(t, recent.Recent{Folder: root})

	m = send(t, m, key("1"))
	if m.uploadFiles.stage != uploadStageFolder || !strings.Contains(m.View(), "Choose source folder") {
		t.Fatalf("1 should open the folder picker:\n%s", m.View())
	}
	m = pickFile(t, m, enter) // the last folder is highlighted
	if m.uploadFiles.stage != uploadStagePattern || m.uploadFiles.folder != root {
		t.Fatalf("picking a folder should ask for a regex: stage=%d folder=%q", m.uploadFiles.stage, m.uploadFiles.folder)
	}
	if !strings.Contains(m.View(), "3 of 3 files match") {
		t.Fatalf("an empty regex should match every file:\n%s", m.View())
	}
	for _, r := range `\.jpg$` {
		m = send(t, m, key(string(r)))
	}
	if !strings.Contains(m.View(), "2 of 3 files match") {
		t.Fatalf("the regex should filter the files:\n%s", m.View())
	}

	m = send(t, m, enter)
	view := m.View()
	if m.uploadFiles.stage != uploadStageReview || !strings.Contains(view, "trip/b.jpg") ||
		strings.Contains(view, "notes.txt") || !strings.Contains(view, "Movie archive") {
		t.Fatalf("enter should review the matching files for the item:\n%s", view)
	}

	m = send(t, m, key("esc"))
	if m.uploadFiles.stage != uploadStagePattern || m.uploadFiles.pattern.Value() != `\.jpg$` {
		t.Fatalf("esc should return to the regex and keep it: stage=%d", m.uploadFiles.stage)
	}
	m = send(t, m, key("esc"))
	m = send(t, m, key("esc"))
	if m.uploadFiles.stage != uploadStageRule {
		t.Fatalf("esc should step back to the rule choice: stage=%d", m.uploadFiles.stage)
	}
	m = send(t, m, key("esc"))
	if m.itemSearch.chosen != nil || m.Mode() != ModeUpload {
		t.Fatal("esc on the rule choice should return to the item search")
	}
}

func TestUploadFilesRegexMustMatch(t *testing.T) {
	root := t.TempDir()
	writeSizedFiles(t, root, map[string]int{"a.jpg": 10})
	m := openFileRules(t, recent.Recent{Folder: root})
	m = send(t, m, key("1"))
	m = pickFile(t, m, enter)

	for _, r := range `\.mp4$` {
		m = send(t, m, key(string(r)))
	}
	m = send(t, m, enter)
	if m.uploadFiles.stage != uploadStagePattern || !strings.Contains(m.View(), "regex matches no files") {
		t.Fatalf("a regex matching nothing should not continue:\n%s", m.View())
	}
	m = send(t, m, key("("))
	m = send(t, m, enter)
	if m.uploadFiles.stage != uploadStagePattern || m.uploadFiles.err == nil {
		t.Fatalf("an invalid regex should not continue: stage=%d", m.uploadFiles.stage)
	}
}

func TestUploadFilesFromISO(t *testing.T) {
	source, isoDir := t.TempDir(), t.TempDir()
	writeSizedFiles(t, source, map[string]int{"photos/beach.jpg": 100, "readme.txt": 5})
	files, err := settings.ScanFolder(source)
	if err != nil {
		t.Fatal(err)
	}
	const target = settings.ReservedPerISO + 1024*1024
	chunks, err := settings.PlanChunks(files, settings.UsableCapacity(target), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := iso.Generate(context.Background(), source, isoDir, target, chunks, nil); err != nil {
		t.Fatal(err)
	}

	m := openFileRules(t, recent.Recent{ISODir: isoDir})
	m = send(t, m, key("2"))
	if m.uploadFiles.stage != uploadStageISO || !strings.Contains(m.View(), "Choose an ISO file") {
		t.Fatalf("2 should open the ISO picker:\n%s", m.View())
	}
	m = pickFile(t, m, enter)
	view := m.View()
	if m.uploadFiles.stage != uploadStageReview || len(m.uploadFiles.matched) != 2 ||
		!strings.Contains(view, "photos/beach.jpg") || !strings.Contains(view, "readme.txt") {
		t.Fatalf("choosing an ISO should review every file in it:\n%s", view)
	}
	if !strings.Contains(view, filepath.Join(isoDir, "backup_1.iso")) {
		t.Fatalf("review should name the ISO:\n%s", view)
	}

	m = send(t, m, key("esc"))
	if m.uploadFiles.stage != uploadStageISO {
		t.Fatalf("esc should return to the ISO picker: stage=%d", m.uploadFiles.stage)
	}
}

func TestUploadFilesUnreadableISO(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.iso"), []byte("not an iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := openFileRules(t, recent.Recent{ISODir: dir})
	m = send(t, m, key("2"))
	m = pickFile(t, m, enter)
	if m.uploadFiles.stage != uploadStageISO || m.uploadFiles.err == nil || !strings.Contains(m.View(), "✗") {
		t.Fatalf("an unreadable ISO should show an error and stay on the picker:\n%s", m.View())
	}
}
