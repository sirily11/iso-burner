package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/recent"
	"github.com/sirily11/iso-burner/internal/settings"
	"github.com/sirily11/iso-burner/internal/store"
)

func TestChangingISOFolderIgnoresOldChoicesAndSession(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "old/previous.iso", "new/current.iso")
	oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
	oldISO := filepath.Join(oldDir, "previous.iso")
	st := testStore(t)
	if _, err := st.CreateSession(context.Background(), []store.Job{{Path: oldISO, Size: 3, Copies: 1}}, []drive.Drive{{ID: "E:"}}); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Mode: ModeBurn, Folder: newDir, Recent: recent.Recent{ISODir: oldDir, ISOs: []string{oldISO}}, Store: st})
	m = send(t, m, enter)
	if m.resumeOffer != nil || len(m.isoPicker.selection()) != 0 || m.isoPicker.dir != newDir {
		t.Fatalf("old folder leaked into new choice: resume=%v selected=%v dir=%q", m.resumeOffer, m.isoPicker.selection(), m.isoPicker.dir)
	}
	if sess, err := st.Unfinished(context.Background()); err != nil || sess == nil {
		t.Fatalf("old session should remain available in database: session=%v err=%v", sess, err)
	}
}

func TestGenerateRemembersSelections(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeFiles(t, src, "a.mkv", "b.txt")
	path := filepath.Join(root, "recent.json")

	m := New(Options{Mode: ModeGenerate, Folder: src, OutputDir: filepath.Join(root, "out"), RecentPath: path})
	m = send(t, m, enter)
	m = typeText(t, m, `\.mkv$`)
	m = send(t, m, enter) // pattern
	m = send(t, m, enter) // size
	m = typeText(t, m, "movies")
	m = send(t, m, enter) // name
	m.submit()            // confirm; the generation command is not run

	got, err := recent.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := recent.Recent{Folder: src, Pattern: `\.mkv$`, ISOName: "movies", Preset: settings.Presets[0].Name}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("saved %+v, want %+v", got, want)
	}
}

func TestGeneratePrefillsFromRecent(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	writeFiles(t, src, "a.mkv")
	last := recent.Recent{Folder: src, Pattern: `\.mkv$`, ISOName: "movies", Preset: settings.Presets[1].Name}

	m := New(Options{Mode: ModeGenerate, Recent: last})
	if !m.picking || m.picker.dir != root || m.picker.highlighted() != src {
		t.Fatalf("picker should highlight the last folder: dir=%q highlighted=%q", m.picker.dir, m.picker.highlighted())
	}
	if m.folderInput.Value() != src || m.patternInput.Value() != `\.mkv$` || m.nameInput.Value() != "movies" || m.presetIdx != 1 {
		t.Errorf("not pre-filled: folder=%q pattern=%q name=%q preset=%d",
			m.folderInput.Value(), m.patternInput.Value(), m.nameInput.Value(), m.presetIdx)
	}

	// Flags win over remembered values.
	m = New(Options{Mode: ModeGenerate, Folder: root, Pattern: "x", Recent: last})
	if m.folderInput.Value() != root || m.patternInput.Value() != "x" {
		t.Errorf("flags should win: folder=%q pattern=%q", m.folderInput.Value(), m.patternInput.Value())
	}

	// A folder that no longer exists is not pre-filled.
	m = New(Options{Mode: ModeGenerate, Recent: recent.Recent{Folder: filepath.Join(root, "gone")}})
	if m.folderInput.Value() != "" {
		t.Errorf("missing folder pre-filled: %q", m.folderInput.Value())
	}
}

func TestBurnRemembersISOs(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "isos/a_1.iso", "isos/b_2.iso", "other/c.iso")
	dir := filepath.Join(root, "isos")
	path := filepath.Join(root, "recent.json")

	m := New(Options{Mode: ModeBurn, Folder: dir, RecentPath: path})
	m = send(t, m, enter)
	m = send(t, m, space)
	m = send(t, m, space)
	m = send(t, m, enter)
	if !m.settingReplicas {
		t.Fatalf("enter should open the copies step:\n%s", m.View())
	}
	got, err := recent.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	wantISOs := []string{filepath.Join(dir, "a_1.iso"), filepath.Join(dir, "b_2.iso")}
	if got.ISODir != dir || !reflect.DeepEqual(got.ISOs, wantISOs) {
		t.Fatalf("saved %+v", got)
	}

	// The next run starts in the same folder with the same ISOs selected,
	// skipping any that were deleted since.
	if err := os.Remove(wantISOs[1]); err != nil {
		t.Fatal(err)
	}
	m = New(Options{Mode: ModeBurn, OutputDir: root, Recent: got})
	if m.isoPicker.dir != dir {
		t.Errorf("picker starts in %q, want %q", m.isoPicker.dir, dir)
	}
	if sel := m.isoPicker.selection(); !reflect.DeepEqual(sel, wantISOs[:1]) {
		t.Errorf("selection = %v, want %v", sel, wantISOs[:1])
	}
}
