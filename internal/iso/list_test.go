package iso

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sirily11/iso-burner/internal/settings"
)

func TestListFilesReturnsEveryFileInTheImage(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	want := map[string]int64{
		"Readme.txt":                 5,
		"photos/2025/Beach Day.jpeg": 1234,
		"videos/a-long-name.mkv":     4096,
	}
	for name, size := range want {
		p := filepath.Join(source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := settings.ScanFolder(source)
	if err != nil {
		t.Fatal(err)
	}
	const target = settings.ReservedPerISO + 1024*1024
	chunks, err := settings.PlanChunks(files, settings.UsableCapacity(target), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(context.Background(), source, output, target, chunks, nil); err != nil {
		t.Fatal(err)
	}

	got, err := ListFiles(filepath.Join(output, chunks[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].RelPath < got[j].RelPath })
	if len(got) != len(want) {
		t.Fatalf("listed %+v, want %v", got, want)
	}
	for _, f := range got {
		if size, ok := want[f.RelPath]; !ok || size != f.Size {
			t.Fatalf("listed %+v, want %v", got, want)
		}
	}
}

func TestListFilesRejectsNonISO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fake.iso")
	if err := os.WriteFile(p, []byte("not an iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ListFiles(p); err == nil {
		t.Fatal("listing a non-ISO file should fail")
	}
}
