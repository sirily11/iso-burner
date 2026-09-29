package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/burn"
	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "burns.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeBurner burns into memory. Burns wait for a value on release when it
// is set, so tests can look at a drive mid-burn.
type fakeBurner struct {
	mu      sync.Mutex
	discs   map[string][]byte
	release chan struct{}
}

func newFakeBurner() *fakeBurner { return &fakeBurner{discs: map[string][]byte{}} }

func (f *fakeBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, opts burn.BurnOptions) error {
	progress := opts.Progress
	progress(size / 2)
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	data, err := os.ReadFile(iso)
	if err != nil {
		return err
	}
	progress(size)
	f.mu.Lock()
	f.discs[d.ID] = data
	f.mu.Unlock()
	return nil
}

func (f *fakeBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.discs[d.ID]
	if !ok {
		return nil, errors.New("no disc")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeBurner) Eject(ctx context.Context, d drive.Drive) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.discs, d.ID)
	return nil
}

// tickUntil feeds burn ticks to m until cond holds.
func tickUntil(t *testing.T, m Model, what string, cond func(Model) bool) Model {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond(m) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s:\n%s", what, m.View())
		}
		time.Sleep(5 * time.Millisecond)
		m = send(t, m, burnTickMsg{})
	}
	return m
}

// startBurnFlow picks the ISO in root, sets copies and confirms drives.
func startBurnFlow(t *testing.T, m Model, copies string, driveKeys ...string) Model {
	t.Helper()
	if m.burnMenu {
		m = send(t, m, enter) // Burn submenu
	}
	m = send(t, m, enter) // ISO picker
	if copies != "" {
		m = send(t, m, key(copies)) // opens the count dialog
		m = send(t, m, enter)
	}
	next, cmd := m.Update(enter) // copies → drives
	m = send(t, next.(Model), cmd())
	for _, k := range driveKeys {
		m = send(t, m, key(k))
	}
	return send(t, m, enter)
}

func TestBurnProgressIdentifiesMatchingDrives(t *testing.T) {
	for _, state := range []store.DriveState{store.DriveBurning, store.DriveVerifying, store.DriveWaiting, store.DriveFinished, store.DriveIdle} {
		t.Run(string(state), func(t *testing.T) {
			m := Model{burnSnap: burn.Snapshot{Running: true, Total: 2}}
			for i, id := range []string{"E:", "F:"} {
				m.burnSnap.Drives = append(m.burnSnap.Drives, burn.DriveStatus{
					Drive: drive.Drive{ID: id, Vendor: "ASUS", Model: "BW-16D1HT with a long model name"},
					State: state,
					Disc:  &store.Disc{ID: int64(i + 1), ISOPath: "backup_1.iso", Copy: i + 1, Copies: 2},
				})
			}
			view := m.burnProgressView()
			for _, label := range []string{"E · ASUS BW-16D1HT", "F · ASUS BW-16D1HT"} {
				if !strings.Contains(view, label) {
					t.Errorf("drive label %q missing from progress view:\n%s", label, view)
				}
			}
		})
	}
}

func TestBurningShowsPerDriveProgressAndAsksForDiscs(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	st := testStore(t)
	fake := newFakeBurner()
	fake.release = make(chan struct{})
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives[:2], nil), Store: st, Burner: fake, DBPath: "/tmp/burns.db"})
	m = startBurnFlow(t, m, "3", "a")
	if m.engine == nil {
		t.Fatalf("burning should start: %v\n%s", m.burnErr, m.View())
	}
	defer m.engine.Stop()

	m = tickUntil(t, m, "both drives half burned", func(m Model) bool {
		return strings.Count(m.View(), " 33%  burning backup_1.iso") == 2
	})
	view := m.View()
	for _, want := range []string{"Burning 3 disc(s) with 2 drive(s)", "PIONEER BD-RW BDR-XD07", "ASUS BW-16D1HT", "copy 1 of 3", "copy 2 of 3", " 33%  burning", "Progress is saved in /tmp/burns.db"} {
		if !strings.Contains(view, want) {
			t.Errorf("progress view missing %q:\n%s", want, view)
		}
	}

	// Finish both burns: one drive asks for a disc for copy 3.
	fake.release <- struct{}{}
	fake.release <- struct{}{}
	m = tickUntil(t, m, "insert dialog", func(m Model) bool { return strings.Contains(m.View(), "Insert a blank disc") })
	view = m.View()
	for _, want := range []string{"✓ Finished backup_1.iso · copy", "verified", "Next  backup_1.iso · copy 3 of 3", "then press enter"} {
		if !strings.Contains(view, want) {
			t.Errorf("insert dialog missing %q:\n%s", want, view)
		}
	}

	// esc puts the dialog off; enter on the progress screen reopens it.
	m = send(t, m, key("esc"))
	if !strings.Contains(m.View(), "⏏ insert a blank disc for backup_1.iso · copy 3 of 3") {
		t.Fatalf("waiting drive should show in the list:\n%s", m.View())
	}
	m = send(t, m, enter)
	if !strings.Contains(m.View(), "Insert a blank disc") {
		t.Fatalf("enter should reopen the dialog:\n%s", m.View())
	}
	m = send(t, m, enter) // disc inserted
	fake.release <- struct{}{}
	m = tickUntil(t, m, "all done", func(m Model) bool { return !m.burnSnap.Running })
	view = m.View()
	for _, want := range []string{"✓ All 3 disc(s) burned", "3 of 3 disc(s) done", "Every disc was read back and matches its ISO",
		"Burned discs", "💿 backup_1.iso · copy 1 of 3", "💿 backup_1.iso · copy 3 of 3"} {
		if !strings.Contains(view, want) {
			t.Errorf("final view missing %q:\n%s", want, view)
		}
	}
	snap, stopped, ok := m.BurnResult()
	if !ok || stopped || snap.Done != 3 {
		t.Fatalf("BurnResult = %+v %v %v", snap, stopped, ok)
	}
	if _, cmd := m.Update(enter); cmd == nil {
		t.Fatal("enter should exit once burning is done")
	}
}

func TestStopBurningAndResume(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	st := testStore(t)
	fake := newFakeBurner()
	fake.release = make(chan struct{})
	opts := Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives[:1], nil), Store: st, Burner: fake}
	m := startBurnFlow(t, New(opts), "2", " ")
	m = tickUntil(t, m, "burning", func(m Model) bool { return strings.Contains(m.View(), "burning backup_1.iso") })

	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !strings.Contains(m.View(), "Stop burning?") || !strings.Contains(m.View(), "1 disc(s) are being burned") {
		t.Fatalf("ctrl+c should ask before stopping:\n%s", m.View())
	}
	m = send(t, m, key("n"))
	if strings.Contains(m.View(), "Stop burning?") {
		t.Fatal("n should keep burning")
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("stopping should quit")
	}
	if _, stopped, ok := m.BurnResult(); !ok || !stopped {
		t.Fatal("BurnResult should report the stop")
	}

	// A new run offers to resume the saved session.
	fake.release = nil
	m = New(opts)
	m = send(t, m, enter) // choose burning, then offer to resume
	view := m.View()
	for _, want := range []string{"Resume unfinished burn?", "backup_1.iso  0 of 2 disc(s) done", "0 of 2 disc(s) done · 2 left"} {
		if !strings.Contains(view, want) {
			t.Errorf("resume dialog missing %q:\n%s", want, view)
		}
	}
	next, cmd = m.Update(enter)
	m = send(t, next.(Model), cmd())
	if !strings.Contains(m.View(), "Resuming · 2 of 2 disc(s) left") || !strings.Contains(m.View(), "1 of 1 drive(s) selected") {
		t.Fatalf("resume should preselect the session's drives:\n%s", m.View())
	}
	m = send(t, m, enter)
	if m.engine == nil {
		t.Fatalf("resume should start burning: %v", m.burnErr)
	}
	defer m.engine.Stop()
	// Resumed discs are not assumed to be loaded.
	for range 2 {
		m = tickUntil(t, m, "insert dialog", func(m Model) bool { return strings.Contains(m.View(), "Insert a blank disc") })
		m = send(t, m, enter)
	}
	m = tickUntil(t, m, "all done", func(m Model) bool { return !m.burnSnap.Running })
	if m.burnSnap.Done != 2 {
		t.Fatalf("done = %d", m.burnSnap.Done)
	}
	if u, _ := st.Unfinished(context.Background()); u != nil {
		t.Fatal("finished session should not be offered again")
	}
}

func TestResumeDialogDiscardStartsNew(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	st := testStore(t)
	st.CreateSession(context.Background(), []store.Job{{Path: filepath.Join(root, "backup_1.iso"), Size: 3, Copies: 1}}, testDrives[:1])
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives, nil), Store: st, Burner: newFakeBurner()})
	m = send(t, m, enter)
	if !strings.Contains(m.View(), "Resume unfinished burn?") {
		t.Fatalf("expected resume dialog:\n%s", m.View())
	}
	m = send(t, m, key("n"))
	if !strings.Contains(m.View(), "Choose ISO files to burn") {
		t.Fatalf("n should open the ISO picker:\n%s", m.View())
	}
	if u, _ := st.Unfinished(context.Background()); u != nil {
		t.Fatal("discarded session should be gone")
	}
}

type failedISOBurner struct{ *fakeBurner }

func (f failedISOBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, opts burn.BurnOptions) error {
	if filepath.Base(iso) == "a.iso" {
		return errors.New("write failed")
	}
	return f.fakeBurner.Burn(ctx, d, iso, size, opts)
}

func TestFailedBurnCanSkipISOAndContinue(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "a.iso", "b.iso")
	st := testStore(t)
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives[:1], nil), Store: st, Burner: failedISOBurner{newFakeBurner()}})
	m = send(t, m, enter) // Burn submenu
	m = send(t, m, key("a"))
	m = startBurnFlow(t, m, "", " ")
	defer m.engine.Stop()
	m = tickUntil(t, m, "failed ISO", func(m Model) bool { return strings.Contains(m.View(), "Last burn failed") })
	if !strings.Contains(m.View(), "s: skip this ISO") || !strings.Contains(m.View(), "a.iso") {
		t.Fatalf("failure must offer skipping the selected ISO:\n%s", m.View())
	}
	// The skip command also works after dismissing the insert dialog.
	m = send(t, m, key("esc"))
	m = send(t, m, key("s"))
	m = tickUntil(t, m, "next ISO", func(m Model) bool {
		return m.burnSnap.Skipped == 1 && strings.Contains(m.View(), "Next  b.iso")
	})
	if strings.Contains(m.View(), "Last burn failed") {
		t.Fatal("next ISO must not display the previous failure")
	}
	m = send(t, m, enter)
	m = tickUntil(t, m, "completion", func(m Model) bool { return !m.burnSnap.Running })
	view := m.View()
	for _, want := range []string{"1 disc(s) burned · 1 skipped", "1 of 2 disc(s) done · 1 skipped", "Skipped: a.iso"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "All 2 disc(s) burned") || strings.Contains(view, "Every disc was read back") {
		t.Fatal("skipped discs must not be described as burned or verified")
	}
}

func TestFailedBurnPromptsCanChooseDrive(t *testing.T) {
	m := Model{dismissed: map[string]int64{}, burnSnap: burn.Snapshot{Running: true, Total: 2, Drives: []burn.DriveStatus{
		{Drive: testDrives[0], State: store.DriveWaiting, Disc: &store.Disc{ID: 1, ISOPath: "a.iso"}},
		{Drive: testDrives[1], State: store.DriveWaiting, Disc: &store.Disc{ID: 2, ISOPath: "b.iso"}, Err: "bad disc"},
	}}}
	if m.promptDrive() != 1 {
		t.Fatal("failed drive must be offered before a routine insert prompt")
	}
	next, _ := m.updateBurningKey(key("tab"))
	m = next.(Model)
	if m.promptDrive() != 0 {
		t.Fatal("tab should select the other waiting drive")
	}
	next, _ = m.updateBurningKey(key("up"))
	m = next.(Model)
	if m.promptDrive() != 1 || !strings.Contains(m.burningView(), "s: skip this ISO") {
		t.Fatal("up should return to the failed drive with its skip option")
	}
}
