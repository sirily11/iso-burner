package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFolderPickerBrowsesAndChoosesFolder(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"alpha/inner", "beta", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "alpha", "inner", "a.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(Options{Mode: ModeGenerate, Folder: root})
	if m.picking {
		t.Fatal("picker should stay closed when a folder is pre-filled")
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.picking || m.picker.dir != root {
		t.Fatalf("tab should open the picker at %s, got %q", root, m.picker.dir)
	}
	view := m.View()
	if !strings.Contains(view, "alpha") || !strings.Contains(view, "beta") || strings.Contains(view, ".hidden") {
		t.Fatalf("picker should list visible subfolders:\n%s", view)
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyRight}) // open alpha
	if m.picker.dir != filepath.Join(root, "alpha") {
		t.Fatalf("dir = %s, want alpha", m.picker.dir)
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyLeft}) // back to root, cursor on alpha
	if m.picker.dir != root || m.picker.entries[m.picker.cursor] != "alpha" {
		t.Fatalf("parent should focus alpha, got %s/%v", m.picker.dir, m.picker.entries)
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRight})
	m = send(t, m, enter) // choose alpha/inner and scan it
	if m.picking || m.step != stepPattern || len(m.allFiles) != 1 {
		t.Fatalf("choosing should scan: step=%d files=%d err=%v", m.step, len(m.allFiles), m.err)
	}
	if got := m.folderInput.Value(); got != filepath.Join(root, "alpha", "inner") {
		t.Fatalf("folder input = %q", got)
	}
}

func TestFolderPickerOpensWithoutFolderAndEscReturnsToInput(t *testing.T) {
	m := New(Options{Mode: ModeGenerate})
	if !m.picking {
		t.Fatal("picker should open when no folder is given")
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.picking || m.cancelled || m.step != stepFolder {
		t.Fatal("esc should close the picker, not quit")
	}
}
