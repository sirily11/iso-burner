package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/drive"
)

var testDrives = []drive.Drive{
	{ID: "1", Vendor: "PIONEER", Model: "BD-RW BDR-XD07", Detail: "USB"},
	{ID: "2", Vendor: "ASUS", Model: "BW-16D1HT", Detail: "SATA"},
	{ID: "3", Vendor: "LG", Model: "WH16NS60"},
}

func fixedDrives(drives []drive.Drive, err error) DriveLister {
	return func(context.Context) ([]drive.Drive, error) { return drives, err }
}

// loadDrives runs the selector's initial scan.
func loadDrives(t *testing.T, s DriveSelector) DriveSelector {
	t.Helper()
	return sendDrive(t, s, s.Init()())
}

func sendDrive(t *testing.T, s DriveSelector, msg tea.Msg) DriveSelector {
	t.Helper()
	next, _ := s.Update(msg)
	return next.(DriveSelector)
}

func key(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func ids(drives []drive.Drive) string {
	var out []string
	for _, d := range drives {
		out = append(out, d.ID)
	}
	return strings.Join(out, ",")
}

func TestDriveSelectorMultiSelect(t *testing.T) {
	s := NewDriveSelector(fixedDrives(testDrives, nil))
	if !strings.Contains(s.View(), "Looking for disc drives") {
		t.Fatalf("expected loading view:\n%s", s.View())
	}
	s = loadDrives(t, s)
	view := s.View()
	for _, want := range []string{"PIONEER BD-RW BDR-XD07", "ASUS BW-16D1HT", "LG WH16NS60", "0 of 3 drive(s) selected"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}

	s = sendDrive(t, s, key("enter"))
	if s.Done() || s.err == nil {
		t.Fatal("enter with nothing selected should show an error, not finish")
	}

	// Tick drives 3 and 1 out of order; the result keeps list order.
	s = sendDrive(t, s, key("down"))
	s = sendDrive(t, s, key("down"))
	s = sendDrive(t, s, key(" "))
	s = sendDrive(t, s, key("down")) // wraps to the first drive
	s = sendDrive(t, s, key("x"))
	if s.err != nil || !strings.Contains(s.View(), "2 of 3 drive(s) selected") {
		t.Fatalf("expected two selected:\n%s", s.View())
	}

	next, cmd := s.Update(key("enter"))
	s = next.(DriveSelector)
	if !s.Done() || cmd == nil {
		t.Fatal("enter with a selection should finish and quit")
	}
	if got := ids(s.Selected()); got != "1,3" {
		t.Fatalf("selected %q, want 1,3", got)
	}
}

func TestDriveSelectorToggleAll(t *testing.T) {
	s := loadDrives(t, NewDriveSelector(fixedDrives(testDrives, nil)))
	s = sendDrive(t, s, key("a"))
	if got := ids(s.Selected()); got != "1,2,3" {
		t.Fatalf("a should select all, got %q", got)
	}
	s = sendDrive(t, s, key(" ")) // untick the first
	s = sendDrive(t, s, key("a"))
	if got := ids(s.Selected()); got != "1,2,3" {
		t.Fatalf("a with a partial selection should select all, got %q", got)
	}
	s = sendDrive(t, s, key("a"))
	if got := ids(s.Selected()); got != "" {
		t.Fatalf("a with everything selected should clear, got %q", got)
	}
}

func TestDriveSelectorRescanKeepsSelection(t *testing.T) {
	drives := testDrives
	s := loadDrives(t, NewDriveSelector(func(context.Context) ([]drive.Drive, error) { return drives, nil }, "2", "3"))
	if got := ids(s.Selected()); got != "2,3" {
		t.Fatalf("preselect: got %q", got)
	}

	// Drive 3 is unplugged before the rescan.
	drives = testDrives[:2]
	next, cmd := s.Update(key("r"))
	s = next.(DriveSelector)
	if !s.loading || cmd == nil {
		t.Fatal("r should rescan")
	}
	s = sendDrive(t, s, cmd())
	if got := ids(s.Selected()); got != "2" {
		t.Fatalf("after rescan: got %q, want 2", got)
	}
	if !strings.Contains(s.View(), "1 of 2 drive(s) selected") {
		t.Fatalf("stale count after rescan:\n%s", s.View())
	}
}

func TestDriveSelectorEmptyAndError(t *testing.T) {
	s := loadDrives(t, NewDriveSelector(fixedDrives(nil, nil)))
	if !strings.Contains(s.View(), "No disc drives found") {
		t.Fatalf("expected empty view:\n%s", s.View())
	}
	if s = sendDrive(t, s, key("enter")); s.Done() {
		t.Fatal("enter without drives should not finish")
	}

	s = loadDrives(t, NewDriveSelector(fixedDrives(nil, errors.New("drutil list: boom"))))
	if !strings.Contains(s.View(), "drutil list: boom") {
		t.Fatalf("expected error view:\n%s", s.View())
	}
}

func TestDriveSelectorCancel(t *testing.T) {
	s := loadDrives(t, NewDriveSelector(fixedDrives(testDrives, nil)))
	s = sendDrive(t, s, key("a"))
	next, cmd := s.Update(key("esc"))
	s = next.(DriveSelector)
	if !s.Cancelled() || s.Done() || cmd == nil || s.Selected() != nil {
		t.Fatal("esc should cancel and quit with no drives")
	}
}

func TestBurnModeSelectsDrivesAfterISOs(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives, nil), Store: testStore(t), Burner: newFakeBurner()})
	m = send(t, m, enter)        // Burn submenu
	m = send(t, m, enter)        // burn the highlighted ISO
	next, cmd := m.Update(enter) // keep one copy
	m = next.(Model)
	if cmd == nil {
		t.Fatal("confirming the copies should scan for drives")
	}
	m = send(t, m, cmd())
	view := m.View()
	for _, want := range []string{"Burning 1 ISO file(s)", "PIONEER BD-RW BDR-XD07", "LG WH16NS60", "esc: back to copies"} {
		if !strings.Contains(view, want) {
			t.Errorf("drive step missing %q:\n%s", want, view)
		}
	}

	// Going back keeps the ISO selection and returning keeps the drive ticks.
	m = send(t, m, key("a"))
	m = send(t, m, key("esc"))
	if !strings.Contains(m.View(), "Copies per ISO") {
		t.Fatalf("esc should return to the copies step:\n%s", m.View())
	}
	m = send(t, m, key("esc"))
	if m.cancelled || m.BurnISOs() != nil || !strings.Contains(m.View(), "Choose ISO files to burn") {
		t.Fatalf("esc should return to the ISO picker:\n%s", m.View())
	}
	m = send(t, m, enter)
	next, cmd = m.Update(enter)
	m = send(t, next.(Model), cmd())
	if !strings.Contains(m.View(), "3 of 3 drive(s) selected") {
		t.Fatalf("drive ticks should survive going back:\n%s", m.View())
	}

	if !strings.Contains(m.View(), "Write speed  Max") {
		t.Fatalf("speed should default to max:\n%s", m.View())
	}
	m = send(t, m, key("s"))
	m = send(t, m, key("s"))
	if !strings.Contains(m.View(), "Write speed  4x") {
		t.Fatalf("s should cycle Max → 2x → 4x:\n%s", m.View())
	}

	m = send(t, m, key(" ")) // untick the first drive
	next, cmd = m.Update(enter)
	m = next.(Model)
	if cmd == nil || m.engine == nil {
		t.Fatalf("confirming drives should start burning: %v", m.burnErr)
	}
	if m.recent.Speed != 4 {
		t.Fatalf("recent speed = %d, want 4", m.recent.Speed)
	}
	m.engine.Stop()
	if got := ids(m.BurnDrives()); got != "2,3" {
		t.Fatalf("BurnDrives = %q, want 2,3", got)
	}
	if got := m.BurnISOs(); len(got) != 1 || !strings.HasSuffix(got[0], "backup_1.iso") {
		t.Fatalf("BurnISOs = %v", got)
	}
}

func TestBurnModeCtrlCOnDrives(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "backup_1.iso")
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives, nil)})
	m = send(t, m, enter) // Burn submenu
	m = send(t, m, enter) // ISOs
	m = send(t, m, enter) // copies
	m = send(t, m, enter) // drives are still loading
	m = send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.cancelled || m.BurnDrives() != nil || m.BurnISOs() != nil {
		t.Fatal("ctrl+c on the drive step should cancel everything")
	}
}
