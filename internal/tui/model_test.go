package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/settings"
)

// send delivers msg to m and synchronously runs any returned command whose
// result is a scanDoneMsg, mimicking the Bubble Tea runtime for folder scans.
func send(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	m = next.(Model)
	if cmd != nil && m.scanning {
		if done, ok := cmd().(scanDoneMsg); ok {
			next, _ = m.Update(done)
			m = next.(Model)
		}
	}
	return m
}

func typeText(t *testing.T, m Model, s string) Model {
	for _, r := range s {
		m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

var enter = tea.KeyMsg{Type: tea.KeyEnter}

func TestWizardFlow(t *testing.T) {
	root := t.TempDir()
	for name, size := range map[string]int{"a.mkv": 10, "b.mkv": 20, "c.txt": 5} {
		if err := os.WriteFile(filepath.Join(root, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := New(Options{Mode: ModeGenerate, Folder: root, OutputDir: filepath.Join(root, "output")})
	m = send(t, m, enter)
	if m.step != stepPattern || len(m.allFiles) != 3 {
		t.Fatalf("after folder: step=%d files=%d err=%v", m.step, len(m.allFiles), m.err)
	}

	m = typeText(t, m, `\.mkv$`)
	if len(m.matched) != 2 {
		t.Fatalf("live preview matched %d, want 2", len(m.matched))
	}
	m = send(t, m, enter)

	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.presetIdx != 1 {
		t.Fatalf("presetIdx = %d, want 1", m.presetIdx)
	}
	m = send(t, m, enter)

	m = send(t, m, enter)
	if m.step != stepName || m.err == nil {
		t.Fatal("empty ISO name should be rejected")
	}
	m = typeText(t, m, "movies")
	m = send(t, m, enter)
	if m.step != stepConfirm {
		t.Fatalf("step = %d, want confirm (err=%v)", m.step, m.err)
	}
	preview := m.View()
	for _, want := range []string{"Planned ISO 1 of 1", "movies_1.iso", "Files", "2", "Data size", "30 B", "Est. size", settings.Presets[1].Name, "a.mkv", "b.mkv", filepath.Join(root, "output"), "enter: generate"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview missing %q:\n%s", want, preview)
		}
	}

	m = send(t, m, enter)
	cfg, chunks := m.Result()
	if cfg == nil {
		t.Fatal("expected confirmed settings")
	}
	if cfg.Preset != settings.Presets[1] || cfg.ISOName != "movies" || cfg.Pattern.String() != `\.mkv$` {
		t.Fatalf("unexpected settings: %+v", cfg)
	}
	if len(chunks) != 1 || chunks[0].Name != "movies_1.iso" {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
}

func TestWizardRejectsBadInput(t *testing.T) {
	m := New(Options{Mode: ModeGenerate, Folder: filepath.Join(t.TempDir(), "missing")})
	m = send(t, m, enter)
	if m.step != stepFolder || m.err == nil {
		t.Fatal("missing folder should be rejected")
	}

	m = New(Options{Mode: ModeGenerate, Folder: t.TempDir()})
	m = send(t, m, enter)
	m = typeText(t, m, "[z-a]")
	m = send(t, m, enter)
	if m.step != stepPattern || m.err == nil {
		t.Fatal("invalid regex should be rejected")
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.step != stepFolder {
		t.Fatalf("esc should go back, step=%d", m.step)
	}
}

func TestConfirmViewListsSplitFiles(t *testing.T) {
	big := settings.File{RelPath: "movie.mkv", Size: 25 * settings.SectorSize}
	chunks, err := settings.PlanChunks([]settings.File{big}, 10*settings.SectorSize, "movies")
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{Mode: ModeGenerate})
	m.step, m.chunks = stepConfirm, chunks
	view := m.View()
	for _, want := range []string{"Planned ISO 1 of 3", "movies_1.iso", "Split files (1)", "movie.mkv", "3 parts"} {
		if !strings.Contains(view, want) {
			t.Errorf("confirm view missing %q:\n%s", want, view)
		}
	}
	for i := 1; i < len(chunks); i++ {
		m = send(t, m, tea.KeyMsg{Type: tea.KeyRight})
	}
	if !strings.Contains(m.View(), "movies_3.iso") {
		t.Fatalf("last planned ISO is not available:\n%s", m.View())
	}
}

func TestConfirmPreviewScrollsFilesAndResetsOnNextISO(t *testing.T) {
	files := make([]settings.File, 10)
	for i := range files {
		files[i] = settings.File{RelPath: fmt.Sprintf("file-%02d.txt", i), Size: int64(i + 1)}
	}
	m := New(Options{Mode: ModeGenerate})
	m.step = stepConfirm
	m.chunks = []settings.Chunk{{Index: 1, Name: "backup_1.iso", Pieces: make([]settings.Piece, len(files))}}
	for i := 2; i <= 10; i++ {
		m.chunks = append(m.chunks, settings.Chunk{
			Index:  i,
			Name:   fmt.Sprintf("backup_%d.iso", i),
			Pieces: []settings.Piece{{File: settings.File{RelPath: "last.txt", Size: 1}, Length: 1, Part: 1, Parts: 1}},
		})
	}
	for i, f := range files {
		m.chunks[0].Pieces[i] = settings.Piece{File: f, Length: f.Size, Part: 1, Parts: 1}
		m.chunks[0].Size += f.Size
	}
	if strings.Contains(m.View(), "file-09.txt") {
		t.Fatal("last file should start outside the preview window")
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(m.View(), "file-09.txt") || !strings.Contains(m.View(), "Showing files 3–10 of 10") {
		t.Fatalf("file scrolling failed:\n%s", m.View())
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if m.previewFileOffset != 0 || !strings.Contains(m.View(), "backup_2.iso") || !strings.Contains(m.View(), "last.txt") {
		t.Fatalf("next ISO preview is wrong:\n%s", m.View())
	}
	for i := 3; i <= 10; i++ {
		m = send(t, m, tea.KeyMsg{Type: tea.KeyRight})
	}
	if !strings.Contains(m.View(), "Planned ISO 10 of 10") || !strings.Contains(m.View(), "backup_10.iso") {
		t.Fatalf("later ISO preview is unavailable:\n%s", m.View())
	}
}
