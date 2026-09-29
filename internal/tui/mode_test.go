package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestModeSelection(t *testing.T) {
	m := New(Options{Folder: t.TempDir()})
	view := m.View()
	for _, want := range []string{"1. Generate ISO file", "2. Burn ISO"} {
		if !strings.Contains(view, want) {
			t.Errorf("mode view missing %q:\n%s", want, view)
		}
	}

	m = send(t, m, enter)
	if m.Mode() != ModeGenerate || m.step != stepFolder {
		t.Fatalf("enter should choose generate: mode=%d step=%d", m.Mode(), m.step)
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.Mode() != ModeNone || m.cancelled {
		t.Fatalf("esc on folder step should return to mode selection: mode=%d cancelled=%v", m.Mode(), m.cancelled)
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = send(t, m, enter)
	if m.Mode() != ModeBurn || !strings.Contains(m.View(), "Verify disc against ISO") {
		t.Fatalf("down+enter should choose the Burn submenu: mode=%d\n%s", m.Mode(), m.View())
	}
	if cfg, _ := m.Result(); cfg != nil {
		t.Fatal("burn mode should not produce generate settings")
	}
	m = send(t, m, enter)
	if !strings.Contains(m.View(), "Choose ISO files to burn") {
		t.Fatalf("burn action should open the ISO picker:\n%s", m.View())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.burnMenu || m.cancelled {
		t.Fatal("esc from ISO picker should return to the Burn submenu")
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.Mode() != ModeNone || m.cancelled {
		t.Fatalf("esc in the ISO picker should return to mode selection: mode=%d cancelled=%v", m.Mode(), m.cancelled)
	}
}

func TestModeSelectionByNumberAndQuit(t *testing.T) {
	m := New(Options{})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	if next.(Model).Mode() != ModeBurn {
		t.Fatal("pressing 2 should choose burn")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m = next.(Model); !m.cancelled || m.Mode() != ModeNone {
		t.Fatal("esc on mode selection should quit")
	}
}
