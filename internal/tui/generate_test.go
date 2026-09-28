package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/settings"
)

func TestGenerateViewListsActiveFirstAndScrolls(t *testing.T) {
	m := New(Options{Mode: ModeGenerate, Folder: "x", OutputDir: "/out"})
	m.step, m.generating = stepGenerate, true
	for i := 1; i <= 8; i++ {
		m.chunks = append(m.chunks, settings.Chunk{Index: i, Name: fmt.Sprintf("backup_%d.iso", i), Size: 100})
	}
	m.genStatuses = make([]iso.ChunkStatus, 8)
	m.genStatuses[0] = iso.ChunkStatus{Stage: iso.StageDone, Copied: 100}
	m.genStatuses[6] = iso.ChunkStatus{Stage: iso.StageCopying, Copied: 50}
	m.genStatuses[7] = iso.ChunkStatus{Stage: iso.StageFailed, Err: errors.New("disk full")}

	view := m.View()
	first := strings.Index(view, "backup_7.iso")
	if first < 0 || first > strings.Index(view, "backup_2.iso") {
		t.Fatalf("active ISO should be listed first:\n%s", view)
	}
	for _, want := range []string{"50%", "disk full", "Showing ISOs 1–5 of 8", "1 done · 1 active · 5 queued · 1 failed"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "backup_1.iso") {
		t.Fatal("finished ISO should start below the visible window")
	}
	for range 10 {
		m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.genOffset != 3 || !strings.Contains(m.View(), "backup_1.iso") || !strings.Contains(m.View(), "Showing ISOs 4–8 of 8") {
		t.Fatalf("scrolling failed (offset %d):\n%s", m.genOffset, m.View())
	}
}

func TestConfirmGeneratesISOsInsideTUI(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.mkv"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "output")
	m := New(Options{Mode: ModeGenerate, Folder: root, OutputDir: out})
	m = send(t, m, enter)
	m = send(t, m, enter)
	m = send(t, m, enter)
	m = typeText(t, m, "movies")
	m = send(t, m, enter)
	next, _ := m.Update(enter)
	m = next.(Model)
	if m.step != stepGenerate || !m.generating {
		t.Fatalf("confirm should start generation, step=%d", m.step)
	}

	// Run the generation synchronously instead of via the runtime.
	err := iso.Generate(t.Context(), root, out, m.result.Preset.Bytes, m.chunks, m.genProgress)
	next, _ = m.Update(genDoneMsg{err: err})
	m = next.(Model)
	if m.GenerateErr() != nil {
		t.Fatal(m.GenerateErr())
	}
	view := m.View()
	for _, want := range []string{"Created 1 ISO file(s)", "movies_1.iso", "✓ done", "100%", "enter: exit"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if _, cmd := m.Update(enter); cmd == nil {
		t.Fatal("enter should quit once finished")
	}
	if cfg, _ := m.Result(); cfg == nil {
		t.Fatal("result should be available after generation")
	}
}
