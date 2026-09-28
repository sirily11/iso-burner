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
	"time"

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

// itemServer is a fake rxstorage server that searches items by title and
// keeps the file contents added to them and the upload progress reported.
type itemServer struct {
	client *remote.Client

	mu       sync.Mutex
	queries  []string
	contents map[string][]remote.FileContent // by item ID
	jobs     []remote.Job
}

func (s *itemServer) lastJob(t *testing.T) remote.Job {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		t.Fatal("no upload progress was reported")
	}
	return s.jobs[len(s.jobs)-1]
}

func newItemServer(t *testing.T, items []remote.Item) *itemServer {
	s := &itemServer{contents: map[string][]remote.FileContent{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/iso-jobs/") && r.Method == http.MethodPut {
			var job remote.Job
			json.NewDecoder(r.Body).Decode(&job)
			s.mu.Lock()
			s.jobs = append(s.jobs, job)
			s.mu.Unlock()
			w.Write([]byte(`{}`))
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/items/"); ok && r.Method == http.MethodPost {
			var req struct{ Data remote.FileContent }
			json.NewDecoder(r.Body).Decode(&req)
			s.mu.Lock()
			s.contents[strings.TrimSuffix(id, "/contents")] = append(s.contents[strings.TrimSuffix(id, "/contents")], req.Data)
			s.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			return
		}
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
	m, _ := openFileRulesOn(t, last)
	return m
}

func openFileRulesOn(t *testing.T, last recent.Recent) (Model, *itemServer) {
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
	return m, server
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

func TestUploadRunsFromReview(t *testing.T) {
	root := t.TempDir()
	writeSizedFiles(t, root, map[string]int{"docs/a.txt": 10, "b.pdf": 20})
	m, server := openFileRulesOn(t, recent.Recent{Folder: root})
	m = send(t, m, key("1"))
	m = pickFile(t, m, enter)
	m = send(t, m, enter)
	if m.uploadFiles.stage != uploadStageReview || !strings.Contains(m.View(), "enter: upload") {
		t.Fatalf("review should offer to upload:\n%s", m.View())
	}

	next, cmd := m.Update(enter)
	m = next.(Model)
	if m.uploadFiles.stage != uploadStageRunning || !strings.Contains(m.View(), "Uploading 2 file(s) to") {
		t.Fatalf("enter should start the upload:\n%s", m.View())
	}
	// Run the upload and one progress tick, then deliver the result.
	var done tea.Msg
	for _, c := range cmd().(tea.BatchMsg) {
		switch msg := c().(type) {
		case uploadDoneMsg:
			done = msg
		default:
			next, _ = m.Update(msg)
			m = next.(Model)
		}
	}
	next, _ = m.Update(done)
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "✓ Uploaded 2 file(s) to") || !strings.Contains(view, "2 done") {
		t.Fatalf("finished upload should say so:\n%s", view)
	}
	server.mu.Lock()
	got := server.contents["i1"]
	server.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("item i1 contents = %+v, want 2 files", got)
	}
	if uploaded, failed, total, err, ok := m.UploadResult(); !ok || uploaded != 2 || failed != 0 || total != 2 || err != nil {
		t.Errorf("UploadResult = %d, %d, %d, %v, %v", uploaded, failed, total, err, ok)
	}
	if err := m.CloseSync(time.Second); err != nil {
		t.Fatal(err)
	}
	job := server.lastJob(t)
	if job.Kind != remote.KindUpload || job.Status != remote.StatusCompleted || job.Progress != 1 ||
		job.DoneCount != 2 || job.TotalCount != 2 || job.TotalBytes != 30 || job.FinishedAt == nil ||
		!strings.HasPrefix(job.Title, "Upload to ") {
		t.Fatalf("final upload job = %+v", job)
	}
	if len(job.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want one per file", job.Tasks)
	}
	for _, task := range job.Tasks {
		if task.Section != remote.SectionFile || task.Status != "done" || task.Progress != 1 || task.DoneBytes != task.TotalBytes {
			t.Errorf("task = %+v, want a finished file row", task)
		}
	}

	next, cmd = m.Update(enter)
	if cmd == nil || cmd() != tea.Quit() {
		t.Fatal("enter after the upload should exit")
	}
}

func TestUploadRunCtrlCStops(t *testing.T) {
	root := t.TempDir()
	writeSizedFiles(t, root, map[string]int{"a.txt": 1})
	m, server := openFileRulesOn(t, recent.Recent{Folder: root})
	m = send(t, m, key("1"))
	m = pickFile(t, m, enter)
	m = send(t, m, enter)
	next, _ := m.Update(enter)
	m = next.(Model)

	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.uploadFiles.run.cancelling || m.cancelled || !strings.Contains(m.View(), "Cancelling") {
		t.Fatalf("ctrl+c should stop the upload rather than quit at once:\n%s", m.View())
	}
	next, cmd := m.Update(uploadDoneMsg{err: context.Canceled})
	m = next.(Model)
	if !m.cancelled || cmd == nil || cmd() != tea.Quit() {
		t.Fatal("the stopped upload should quit as cancelled")
	}
	if _, _, _, _, ok := m.UploadResult(); ok {
		t.Error("a cancelled upload should not report a result")
	}
	if err := m.CloseSync(time.Second); err != nil {
		t.Fatal(err)
	}
	if job := server.lastJob(t); job.Status != remote.StatusCancelled || job.FinishedAt == nil {
		t.Errorf("stopped upload job = %+v, want cancelled", job)
	}
}
