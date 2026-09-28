package tui

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// replicaModel selects every ISO in a fresh folder and opens the copies step.
func replicaModel(t *testing.T, names ...string) (Model, string) {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, names...)
	m := New(Options{Mode: ModeBurn, Folder: root, ListDrives: fixedDrives(testDrives, nil)})
	m = send(t, m, key("a"))
	m = send(t, m, enter)
	if !m.settingReplicas {
		t.Fatalf("confirming ISOs should open the copies step:\n%s", m.View())
	}
	return m, root
}

func copies(m Model) []int {
	var out []int
	for _, j := range m.replicas.jobs {
		out = append(out, j.Copies)
	}
	return out
}

func TestReplicasDefaultToOneAndSetAll(t *testing.T) {
	m, _ := replicaModel(t, "a.iso", "b.iso", "c.iso")
	if got := copies(m); !reflect.DeepEqual(got, []int{1, 1, 1}) {
		t.Fatalf("copies = %v, want all 1", got)
	}
	m = send(t, m, key("s"))
	if !strings.Contains(m.View(), "Copies for all 3 ISO file(s)") {
		t.Fatalf("s should open the set-all dialog:\n%s", m.View())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m = typeText(t, m, "4")
	m = send(t, m, enter)
	if got := copies(m); !reflect.DeepEqual(got, []int{4, 4, 4}) {
		t.Fatalf("copies = %v, want all 4", got)
	}
	if !strings.Contains(m.View(), "3 ISO file(s) · 12 disc(s) in total") {
		t.Fatalf("summary should count every disc:\n%s", m.View())
	}
}

func TestReplicasEditOne(t *testing.T) {
	m, root := replicaModel(t, "a.iso", "b.iso")
	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})

	// Typing a digit opens the dialog for the highlighted ISO.
	m = typeText(t, m, "12")
	if !strings.Contains(m.View(), "Copies of b.iso") {
		t.Fatalf("typing a number should edit b.iso:\n%s", m.View())
	}
	m = send(t, m, enter)
	m = send(t, m, key("+"))
	m = send(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = send(t, m, key("-")) // never below 1
	if got := copies(m); !reflect.DeepEqual(got, []int{1, 13}) {
		t.Fatalf("copies = %v, want [1 13]", got)
	}

	// e edits with the current count, and esc cancels without changing it.
	m = send(t, m, key("e"))
	m = typeText(t, m, "9")
	m = send(t, m, key("esc"))
	if got := copies(m); !reflect.DeepEqual(got, []int{1, 13}) || m.replicas.target != targetNone {
		t.Fatalf("esc should cancel the edit, copies = %v", got)
	}

	m = send(t, m, enter)
	m = send(t, m, enter) // the drive scan is still pending, so this is ignored
	want := []BurnJob{{filepath.Join(root, "a.iso"), 1}, {filepath.Join(root, "b.iso"), 13}}
	if got := m.BurnJobs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("BurnJobs = %v, want %v", got, want)
	}
}

func TestReplicasRejectInvalidCount(t *testing.T) {
	m, _ := replicaModel(t, "a.iso")
	for _, bad := range []string{"0", "x"} {
		m = send(t, m, key("e"))
		m = send(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
		m = typeText(t, m, bad)
		m = send(t, m, enter)
		if m.replicas.err == nil || m.replicas.target != targetOne {
			t.Fatalf("%q should keep the dialog open with an error", bad)
		}
		if !strings.Contains(m.View(), "whole number from 1 to 999") {
			t.Fatalf("dialog should explain the error:\n%s", m.View())
		}
		m = send(t, m, key("esc"))
	}
	if got := copies(m); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("copies = %v, want [1]", got)
	}
}

func TestReplicasSurviveGoingBack(t *testing.T) {
	m, _ := replicaModel(t, "a.iso", "b.iso")
	m = send(t, m, key("+"))
	m = send(t, m, key("esc"))
	if m.settingReplicas || !strings.Contains(m.View(), "Choose ISO files to burn") {
		t.Fatalf("esc should return to the ISO picker:\n%s", m.View())
	}
	m = send(t, m, enter)
	if got := copies(m); !reflect.DeepEqual(got, []int{2, 1}) {
		t.Fatalf("copies should survive going back, got %v", got)
	}

	// Backing out of the drive step returns to the copies step.
	next, cmd := m.Update(enter)
	m = send(t, next.(Model), cmd())
	m = send(t, m, key("esc"))
	if m.BurnJobs() != nil || !strings.Contains(m.View(), "Copies per ISO") {
		t.Fatalf("esc on drives should return to copies:\n%s", m.View())
	}
}
