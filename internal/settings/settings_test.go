package settings

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

func TestOutputName(t *testing.T) {
	if got := OutputName("backup", 3); got != "backup_3.iso" {
		t.Fatalf("OutputName = %q, want backup_3.iso", got)
	}
}

func TestValidateISOName(t *testing.T) {
	for _, name := range []string{"", "  ", "a/b", "a:b", "what?"} {
		if ValidateISOName(name) == nil {
			t.Errorf("ValidateISOName(%q) = nil, want error", name)
		}
	}
	if err := ValidateISOName("photos-2026"); err != nil {
		t.Errorf("ValidateISOName(valid) = %v", err)
	}
}

func TestCompilePattern(t *testing.T) {
	re, err := CompilePattern("")
	if err != nil || !re.MatchString("anything") {
		t.Fatalf("empty pattern should match everything, err=%v", err)
	}
	if _, err := CompilePattern("[z-a]"); err == nil {
		t.Fatal("expected error for pattern invalid as both regex and glob")
	}
}

func TestCompilePatternGlob(t *testing.T) {
	cases := []struct {
		glob  string
		match []string
		skip  []string
	}{
		{"**/*.MP4", []string{"a.MP4", "x/a.MP4", "x/y/a.MP4"}, []string{"a.mp4", "a.MP4.txt"}},
		{"*.mkv", []string{"a.mkv", "x/y/a.mkv"}, []string{"a.mkv.bak"}},
		{"sub/*.mkv", []string{"sub/a.mkv"}, []string{"sub/x/a.mkv", "other/sub/a.mkv"}},
		{"*.[mM][pP]4", []string{"a.mp4", "d/a.MP4"}, []string{"a.mp3"}},
	}
	for _, c := range cases {
		re, err := CompilePattern(c.glob)
		if err != nil {
			t.Fatalf("%q: %v", c.glob, err)
		}
		for _, p := range c.match {
			if !re.MatchString(p) {
				t.Errorf("%q should match %q (re=%s)", c.glob, p, re)
			}
		}
		for _, p := range c.skip {
			if re.MatchString(p) {
				t.Errorf("%q should not match %q (re=%s)", c.glob, p, re)
			}
		}
	}
}

func TestPlanChunks(t *testing.T) {
	const S = SectorSize
	// Every piece costs its sector-aligned length plus one sector of metadata.
	files := []File{{"a", 5 * S}, {"b", 3 * S}, {"c", 2 * S}, {"d.mkv", 20 * S}, {"e", 1}}
	chunks, err := PlanChunks(files, 10*S, "disc", 1)
	if err != nil {
		t.Fatal(err)
	}
	type piece struct {
		name           string
		offset, length int64
	}
	want := [][]piece{
		{{"a", 0, 5 * S}, {"b", 0, 3 * S}},
		{{"c", 0, 2 * S}, {"d.mkv.001", 0, 6 * S}},
		{{"d.mkv.002", 6 * S, 9 * S}},
		{{"d.mkv.003", 15 * S, 5 * S}, {"e", 0, 1}},
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(want), chunks)
	}
	for i, w := range want {
		c := chunks[i]
		if c.Index != i+1 || c.Name != OutputName("disc", i+1) {
			t.Errorf("chunk %d named %d/%q", i, c.Index, c.Name)
		}
		if c.Used > 10*S {
			t.Errorf("chunk %d uses %d bytes, over capacity", i, c.Used)
		}
		if len(c.Pieces) != len(w) {
			t.Fatalf("chunk %d pieces = %+v, want %+v", i, c.Pieces, w)
		}
		for j, wp := range w {
			p := c.Pieces[j]
			if p.Name() != wp.name || p.Offset != wp.offset || p.Length != wp.length {
				t.Errorf("chunk %d piece %d = %s@%d+%d, want %+v", i, j, p.Name(), p.Offset, p.Length, wp)
			}
		}
	}
	if p := chunks[2].Pieces[0]; p.Part != 2 || p.Parts != 3 || !p.IsSplit() {
		t.Errorf("split part numbering = %d/%d", p.Part, p.Parts)
	}
	if p := chunks[0].Pieces[0]; p.IsSplit() || p.Name() != "a" {
		t.Errorf("whole file should not be split: %+v", p)
	}

	if _, err := PlanChunks(files, S, "disc", 1); err == nil {
		t.Fatal("expected error for capacity too small to hold data")
	}
}

func TestPlanChunksCoversEveryByte(t *testing.T) {
	const capacity = 100 * SectorSize
	rng := rand.New(rand.NewPCG(1, 2))
	var files []File
	for i := range 200 {
		files = append(files, File{fmt.Sprintf("f%d", i), rng.Int64N(3 * capacity)})
	}
	chunks, err := PlanChunks(files, capacity, "disc", 1)
	if err != nil {
		t.Fatal(err)
	}
	next := map[string]int64{} // next expected offset per file
	var total int64
	for _, c := range chunks {
		if c.Used > capacity {
			t.Fatalf("%s uses %d > %d", c.Name, c.Used, capacity)
		}
		for _, p := range c.Pieces {
			if p.Offset != next[p.File.RelPath] {
				t.Fatalf("%s starts at %d, want %d", p.Name(), p.Offset, next[p.File.RelPath])
			}
			next[p.File.RelPath] += p.Length
			total += p.Length
		}
	}
	for _, f := range files {
		if next[f.RelPath] != f.Size {
			t.Fatalf("%s covered %d of %d bytes", f.RelPath, next[f.RelPath], f.Size)
		}
	}
	if total != TotalSize(files) {
		t.Fatalf("planned %d bytes, want %d", total, TotalSize(files))
	}
}

func TestPlanChunksCapsISO9660FileLength(t *testing.T) {
	f := File{RelPath: "large.bin", Size: MaxPieceSize + SectorSize}
	chunks, err := PlanChunks([]File{f}, 25_025_314_816-ReservedPerISO, "backup", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || len(chunks[0].Pieces) != 2 {
		t.Fatalf("expected two parts on one ISO, got %+v", chunks)
	}
	first, second := chunks[0].Pieces[0], chunks[0].Pieces[1]
	if first.Length != MaxPieceSize || second.Offset != MaxPieceSize || second.Length != SectorSize {
		t.Fatalf("wrong part ranges: %+v, %+v", first, second)
	}
	if first.Name() != "large.bin.001" || second.Name() != "large.bin.002" {
		t.Fatalf("wrong part names: %s, %s", first.Name(), second.Name())
	}
}

func TestUsableCapacity(t *testing.T) {
	for _, p := range Presets {
		u := UsableCapacity(p.Bytes)
		if u%SectorSize != 0 || u >= p.Bytes || u < p.Bytes-ReservedPerISO-SectorSize {
			t.Errorf("UsableCapacity(%d) = %d", p.Bytes, u)
		}
	}
}

func TestScanAndFilter(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	must(os.WriteFile(filepath.Join(root, "a.mkv"), []byte("12345"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "b.mkv"), []byte("123"), 0o644))
	must(os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644))

	files, err := ScanFolder(root)
	must(err)
	if len(files) != 3 {
		t.Fatalf("scanned %d files, want 3", len(files))
	}

	re, _ := CompilePattern(`\.mkv$`)
	matched := Filter(files, re)
	if len(matched) != 2 || TotalSize(matched) != 8 {
		t.Fatalf("matched %+v, want 2 mkv files totalling 8 bytes", matched)
	}

	re, _ = CompilePattern(`^sub/`)
	if m := Filter(files, re); len(m) != 1 || m[0].RelPath != "sub/b.mkv" {
		t.Fatalf("relative-path match failed: %+v", m)
	}
}

func TestPlanChunksStartIndex(t *testing.T) {
	const S = SectorSize
	files := []File{{"a", 5 * S}, {"b", 5 * S}, {"c", 5 * S}}
	chunks, err := PlanChunks(files, 10*S, "disc", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	for i, c := range chunks {
		if c.Index != 7+i || c.Name != OutputName("disc", 7+i) {
			t.Errorf("chunk %d named %d/%q", i, c.Index, c.Name)
		}
	}
	if _, err := PlanChunks(files, 10*S, "disc", -1); err == nil {
		t.Error("negative start index should be rejected")
	}
}

func TestParseStartIndex(t *testing.T) {
	for in, want := range map[string]int{"": 1, " 12 ": 12, "0": 0} {
		if got, err := ParseStartIndex(in); err != nil || got != want {
			t.Errorf("ParseStartIndex(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "1.5", "-3", "99999999"} {
		if _, err := ParseStartIndex(in); err == nil {
			t.Errorf("ParseStartIndex(%q) should fail", in)
		}
	}
}
