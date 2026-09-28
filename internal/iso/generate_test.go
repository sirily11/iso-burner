package iso

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"

	"github.com/sirily11/iso-burner/internal/settings"
)

func TestGenerateWritesMountableSizedImagesWithSplitParts(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"nested/intro.txt": []byte("hello from a nested folder"),
		"nested/movie.bin": bytes.Repeat([]byte("part-content-"), 400_000),
		"zero.txt":         {},
	}
	for name, data := range contents {
		if err := os.WriteFile(filepath.Join(source, filepath.FromSlash(name)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := settings.ScanFolder(source)
	if err != nil {
		t.Fatal(err)
	}
	const target = settings.ReservedPerISO + 3*1024*1024
	chunks, err := settings.PlanChunks(files, settings.UsableCapacity(target), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	progress := NewProgress(len(chunks))
	if err := Generate(context.Background(), source, output, target, chunks, progress); err != nil {
		t.Fatal(err)
	}
	for i, s := range progress.Snapshot() {
		if s.Stage != StageDone || s.Copied != chunks[i].Size || s.Err != nil {
			t.Fatalf("chunk %d progress = %+v, want done with %d bytes", i, s, chunks[i].Size)
		}
	}
	for i, chunk := range chunks {
		if chunk.Name != settings.OutputName("backup", i+1) {
			t.Fatalf("wrong output name %q", chunk.Name)
		}
		imagePath := filepath.Join(output, chunk.Name)
		info, err := os.Stat(imagePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > target || info.Size() < 19*settings.SectorSize {
			t.Fatalf("%s has size %d, target %d", chunk.Name, info.Size(), target)
		}
		d, err := diskfs.Open(imagePath)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := d.GetFilesystem(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, piece := range chunk.Pieces {
			f, err := fs.OpenFile(piece.Name(), os.O_RDONLY)
			if err != nil {
				t.Fatalf("open %s in %s: %v", piece.Name(), chunk.Name, err)
			}
			got, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := contents[piece.File.RelPath][piece.Offset : piece.Offset+piece.Length]
			if !bytes.Equal(got, want) {
				t.Fatalf("%s in %s differs from source range", piece.Name(), chunk.Name)
			}
		}
		fs.Close()
		d.Close()
	}
	if err := Generate(context.Background(), source, output, target, chunks, nil); err == nil {
		t.Fatal("existing images must not be replaced")
	}
}

func TestGenerateCancelledPublishesNothing(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	progress := NewProgress(len(chunks))
	if err := Generate(ctx, source, output, target, chunks, progress); err == nil {
		t.Fatal("cancelled generation should fail")
	}
	if s := progress.Snapshot()[0]; s.Stage != StageFailed {
		t.Fatalf("stage = %v, want failed", s.Stage)
	}
	entries, _ := os.ReadDir(output)
	if len(entries) != 0 {
		t.Fatalf("output should be empty, got %d entries", len(entries))
	}
}
