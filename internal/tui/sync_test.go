package tui

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sirily11/iso-burner/internal/burn"
	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/media"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/store"
	"github.com/sirily11/iso-burner/internal/upload"
)

type testToken struct{}

func (testToken) AccessToken(context.Context) (string, error) { return "token", nil }

// jobServer is a fake rxstorage server that keeps every reported snapshot.
type jobServer struct {
	mu   sync.Mutex
	ids  []string
	jobs []remote.Job
}

func newJobServer(t *testing.T) (*jobServer, *remote.Client) {
	s := &jobServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var job remote.Job
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.ids = append(s.ids, strings.TrimPrefix(r.URL.Path, "/api/v1/iso-jobs/"))
		s.jobs = append(s.jobs, job)
		s.mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return s, remote.NewClient(srv.URL, testToken{})
}

func (s *jobServer) last(t *testing.T) (string, remote.Job) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		t.Fatal("nothing was reported")
	}
	return s.ids[len(s.ids)-1], s.jobs[len(s.jobs)-1]
}

func (s *jobServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

var testUser = &auth.User{ID: "u1", Name: "Tester"}

func TestGenerateSyncsProgressWhileSignedIn(t *testing.T) {
	server, client := newJobServer(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.mkv"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "output")
	m := New(Options{Mode: ModeGenerate, Folder: root, OutputDir: out, Sync: client, SyncInterval: time.Millisecond})
	m.user = testUser
	m = send(t, m, enter)
	m = send(t, m, enter)
	m = send(t, m, enter)
	m = typeText(t, m, "movies")
	m = send(t, m, enter)
	next, _ := m.Update(enter)
	m = next.(Model)
	if m.genSync == nil {
		t.Fatal("generation should be synced while signed in")
	}
	waitUntil(t, "running report", func() bool { return server.count() > 0 })
	if _, job := server.last(t); job.Status != remote.StatusRunning || job.Kind != remote.KindGenerate || job.Title != "movies" {
		t.Fatalf("first report = %+v", job)
	}
	if !strings.Contains(m.View(), "rxstorage") {
		t.Errorf("view should show the sync state:\n%s", m.View())
	}

	err := iso.Generate(t.Context(), root, out, m.result.Preset.Bytes, m.chunks, m.genProgress)
	next, _ = m.Update(genDoneMsg{err: err})
	m = next.(Model)
	if err := m.CloseSync(time.Second); err != nil {
		t.Fatal(err)
	}

	id, job := server.last(t)
	if id != m.genSync.ID() {
		t.Errorf("reported to %q, want %q", id, m.genSync.ID())
	}
	if job.Status != remote.StatusCompleted || job.Progress != 1 || job.FinishedAt == nil {
		t.Errorf("final report = %+v", job)
	}
	if job.DoneCount != 1 || job.TotalCount != 1 || job.DoneBytes != 4096 || job.TotalBytes != 4096 {
		t.Errorf("counts = %d/%d, bytes = %d/%d", job.DoneCount, job.TotalCount, job.DoneBytes, job.TotalBytes)
	}
	if len(job.Tasks) != 1 || job.Tasks[0].Name != "movies_1.iso" || job.Tasks[0].Status != "done" || job.Tasks[0].Section != remote.SectionISO {
		t.Errorf("tasks = %+v", job.Tasks)
	}
	if job.HostName == "" {
		t.Error("host name should be reported")
	}
}

func TestGenerateCancelIsReported(t *testing.T) {
	server, client := newJobServer(t)
	m := New(Options{Mode: ModeGenerate, Folder: "x", OutputDir: "/out", Sync: client})
	m.user = testUser
	m.result = &settingsForTest
	m.chunks = nil
	m, _ = m.startGenerate()
	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	m.CloseSync(time.Second)
	if _, job := server.last(t); job.Status != remote.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", job.Status)
	}
}

func TestNoSyncWhenSignedOut(t *testing.T) {
	server, client := newJobServer(t)
	m := New(Options{Mode: ModeGenerate, Folder: "x", OutputDir: "/out", Sync: client})
	m.result = &settingsForTest
	m, _ = m.startGenerate()
	if m.genSync != nil {
		t.Fatal("signed-out generation should not be synced")
	}
	m.genCancel()
	m.CloseSync(time.Second)
	if server.count() != 0 {
		t.Fatal("nothing should be reported while signed out")
	}
}

func TestBurnSyncsDrivesAndISOs(t *testing.T) {
	server, client := newJobServer(t)
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	st := testStore(t)
	fake := newFakeBurner()
	fake.release = make(chan struct{})
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives[:1], nil), Store: st, Burner: fake,
		DBPath: "/tmp/burns.db", Sync: client, SyncInterval: time.Millisecond})
	m.user = testUser
	m = startBurnFlow(t, m, "2", " ")
	if m.engine == nil {
		t.Fatalf("burning should start: %v", m.burnErr)
	}
	defer m.engine.Stop()

	m = tickUntil(t, m, "burning", func(m Model) bool { return strings.Contains(m.View(), "burning backup_1.iso") })
	waitUntil(t, "burning report", func() bool {
		_, job := server.last(t)
		return len(job.Tasks) == 2 && job.Tasks[0].Status == "burning"
	})
	id, job := server.last(t)
	if job.Kind != remote.KindBurn || job.Status != remote.StatusRunning || job.TotalCount != 2 || job.Title != "Burn backup_1.iso" {
		t.Errorf("running report = %+v", job)
	}
	drive, isoRow := job.Tasks[0], job.Tasks[1]
	if drive.Section != remote.SectionDrive || drive.Name != "1 · PIONEER BD-RW BDR-XD07" || !strings.Contains(drive.Detail, "copy 1 of 2") {
		t.Errorf("drive row = %+v", drive)
	}
	if isoRow.Section != remote.SectionISO || isoRow.Name != "backup_1.iso" || isoRow.Detail != "0 of 2 copies burned · assigned to 1" || isoRow.Status != "burning" {
		t.Errorf("ISO row = %+v", isoRow)
	}
	if id != m.burnJobID() {
		t.Errorf("reported to %q, want the session's stable ID", id)
	}

	fake.release <- struct{}{}
	m = tickUntil(t, m, "insert dialog", func(m Model) bool { return strings.Contains(m.View(), "Insert a blank disc") })
	waitUntil(t, "waiting report", func() bool {
		_, job := server.last(t)
		return strings.Contains(job.Message, "waiting for a blank disc")
	})
	m = send(t, m, enter)
	fake.release <- struct{}{}
	m = tickUntil(t, m, "all done", func(m Model) bool { return !m.burnSnap.Running })
	m.CloseSync(time.Second)

	_, job = server.last(t)
	if job.Status != remote.StatusCompleted || job.DoneCount != 2 || job.Progress != 1 {
		t.Errorf("final report = %+v", job)
	}
	if isoRow := job.Tasks[1]; isoRow.Status != "done" || isoRow.Detail != "2 of 2 copies burned in 1" || isoRow.Progress != 1 {
		t.Errorf("final ISO row = %+v", isoRow)
	}
}

func TestBurnSyncsMatchingDriveLabelsAndLiveAssignments(t *testing.T) {
	for _, state := range []store.DriveState{store.DriveWaiting, store.DriveBurning, store.DriveVerifying} {
		t.Run(string(state), func(t *testing.T) {
			server, client := newJobServer(t)
			m := New(Options{Sync: client})
			m.user = testUser
			m.burnSync = m.newReporter("drive-labels")
			m.burnSnap = burn.Snapshot{Running: true, Total: 2}
			for i, id := range []string{"E:", "F:"} {
				// Saved assignments may lag behind the live drive snapshot.
				disc := store.Disc{ID: int64(i + 1), ISOPath: "backup_1.iso", ISOSize: 100,
					Copy: i + 1, Copies: 2, Status: store.DiscPending, DriveID: "G:"}
				m.syncDiscs = append(m.syncDiscs, disc)
				m.burnSnap.Drives = append(m.burnSnap.Drives, burn.DriveStatus{
					Drive: drive.Drive{ID: id, Vendor: "ASUS", Model: "BW-16D1HT"},
					State: state, Disc: &disc, Progress: 50, Total: 100,
				})
			}
			m.reportBurn()
			if err := m.CloseSync(time.Second); err != nil {
				t.Fatal(err)
			}
			_, job := server.last(t)
			if len(job.Tasks) != 3 {
				t.Fatalf("synced tasks = %+v, want two drives and one ISO", job.Tasks)
			}
			for i, label := range []string{"E · ASUS BW-16D1HT", "F · ASUS BW-16D1HT"} {
				if row := job.Tasks[i]; row.Section != remote.SectionDrive || row.Name != label {
					t.Errorf("synced drive = %+v, want label %q", row, label)
				}
			}
			if row := job.Tasks[2]; row.Section != remote.SectionISO || row.Detail != "0 of 2 copies burned · assigned to E, F" {
				t.Errorf("synced ISO = %+v, want live assignments to E and F", row)
			}
		})
	}
}

func TestStoppedBurnIsReported(t *testing.T) {
	server, client := newJobServer(t)
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	fake := newFakeBurner()
	fake.release = make(chan struct{})
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives[:1], nil), Store: testStore(t), Burner: fake, Sync: client})
	m.user = testUser
	m = startBurnFlow(t, m, "", " ")
	m = tickUntil(t, m, "burning", func(m Model) bool { return strings.Contains(m.View(), "burning backup_1.iso") })
	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	next, _ := m.Update(key("y"))
	m = next.(Model)
	m.CloseSync(time.Second)
	if _, job := server.last(t); job.Status != remote.StatusStopped || !strings.Contains(job.Message, "resume") {
		t.Fatalf("stopped report = %+v", job)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var settingsForTest = settings.Settings{Folder: "x", Preset: settings.Presets[0], ISOName: "backup"}

func TestUploadJobSnapshot(t *testing.T) {
	m := New(Options{})
	m.itemSearch.chosen = &remote.Item{ID: "i1", Title: "Trip"}
	m.uploadFiles.matched = []settings.File{{RelPath: "a/clip.mp4", Size: 1000}, {RelPath: "b.jpg", Size: 100}, {RelPath: "c.jpg", Size: 10}}
	m.uploadFiles.run = uploadRun{job: upload.Job{Files: m.uploadFiles.matched}, running: true, started: time.Now(), statuses: []upload.FileStatus{
		{Stage: upload.StageDone},
		{Stage: upload.StageFailed, Err: errors.New("boom")},
		{Stage: upload.StageUploading, Kind: media.KindVideo, Fraction: 0.5},
	}}
	job := m.uploadJob()
	if job.Kind != remote.KindUpload || job.Status != remote.StatusRunning || job.Title != "Upload to Trip" ||
		job.DoneCount != 1 || job.TotalCount != 3 || job.TotalBytes != 1110 || job.FinishedAt != nil {
		t.Fatalf("running job = %+v", job)
	}
	// Active first, then failed, then done.
	names := []string{job.Tasks[0].Name, job.Tasks[1].Name, job.Tasks[2].Name}
	if names[0] != "c.jpg" || names[1] != "b.jpg" || names[2] != "clip.mp4" {
		t.Fatalf("task order = %v", names)
	}
	if up := job.Tasks[0]; up.Status != "uploading" || up.Progress != 0.9 || up.Detail != "c.jpg" {
		t.Errorf("uploading row = %+v", up)
	}
	if f := job.Tasks[1]; f.Error != "boom" || f.DoneBytes != 0 {
		t.Errorf("failed row = %+v", f)
	}

	m.uploadFiles.run.running = false
	m.uploadFiles.run.statuses[2] = upload.FileStatus{Stage: upload.StageDone}
	job = m.uploadJob()
	if job.Status != remote.StatusFailed || !strings.Contains(job.Error, "1 of 3") || job.FinishedAt == nil {
		t.Errorf("job with a failed file = %+v", job)
	}
}
