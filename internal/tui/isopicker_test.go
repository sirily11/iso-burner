package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

var space = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}

func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("iso"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestISOPickerSelectsAllISOs(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "b_2.iso", "a_1.ISO", "notes.txt", "more/c_3.iso")

	m := New(Options{Mode: ModeBurn, Folder: root})
	view := m.View()
	for _, want := range []string{"Choose ISO files to burn", "more", "a_1.ISO", "b_2.iso", "2 in this folder"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "notes.txt") {
		t.Errorf("non-ISO files should be hidden:\n%s", view)
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if len(m.isoPicker.selected) != 2 {
		t.Fatalf("a should select both ISOs, got %v", m.isoPicker.selection())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if len(m.isoPicker.selected) != 0 {
		t.Fatalf("a again should clear the folder, got %v", m.isoPicker.selection())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})

	// Add an ISO from a subfolder too.
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if m.isoPicker.dir != filepath.Join(root, "more") {
		t.Fatalf("dir = %s, want more", m.isoPicker.dir)
	}
	m = send(t, m, space)
	m = send(t, m, enter)
	if !strings.Contains(m.View(), "Copies per ISO") {
		t.Fatalf("enter should ask for copies next:\n%s", m.View())
	}
	next, cmd := m.Update(enter)
	m = next.(Model)
	if cmd == nil || !strings.Contains(m.View(), "Disc drives") {
		t.Fatalf("confirming copies should scan for drives next:\n%s", m.View())
	}
	want := []string{filepath.Join(root, "a_1.ISO"), filepath.Join(root, "b_2.iso"), filepath.Join(root, "more", "c_3.iso")}
	if got := m.BurnISOs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("BurnISOs = %v, want %v", got, want)
	}
}

func TestISOPickerEnterUsesHighlightedOrRequiresSelection(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "only.iso", "sub/x.txt")

	m := New(Options{Mode: ModeBurn, Folder: root})
	m = send(t, m, enter) // cursor on the "sub" folder, nothing selected
	if m.BurnISOs() != nil || m.isoPicker.err == nil {
		t.Fatal("enter with nothing selected should show an error")
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = send(t, m, enter)
	m = send(t, m, enter)
	if got := m.BurnISOs(); !reflect.DeepEqual(got, []string{filepath.Join(root, "only.iso")}) {
		t.Fatalf("enter should burn the highlighted ISO, got %v", got)
	}
}

func TestISOPickerEscQuitsWhenModeFromFlag(t *testing.T) {
	m := New(Options{Mode: ModeBurn, Folder: t.TempDir()})
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cancelled || m.Mode() != ModeNone || m.BurnISOs() != nil {
		t.Fatal("esc should quit when burn mode came from a flag")
	}
}
