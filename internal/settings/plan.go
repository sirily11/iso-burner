package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"regexp"
)

// SectorSize is the logical block size of ISO 9660 / UDF images. Every file,
// and every part of a split file, occupies a whole number of sectors.
const SectorSize = 2048

// entryOverhead is the conservative metadata cost charged per file entry.
const entryOverhead = SectorSize

// ReservedPerISO is set aside on every ISO for volume descriptors, path
// tables, directory extents and Rock Ridge entries.
const ReservedPerISO = 32 << 20

// ISO 9660 records file lengths in 32 bits. Keep each part sector aligned
// and below that limit, including on discs with more than 4 GiB free.
const MaxPieceSize = int64(math.MaxUint32 / SectorSize * SectorSize)

// File is a candidate file found under the source folder.
type File struct {
	// RelPath is the slash-separated path relative to the source folder; it
	// is what the selection regex is matched against.
	RelPath string
	Size    int64
}

// Piece is a byte range of a source file stored on one ISO. Files that fit on
// a single ISO are stored whole as one piece; larger files are split into
// several pieces, possibly on the same ISO when the ISO 9660 file-length
// limit is reached before the disc is full.
type Piece struct {
	File   File
	Offset int64
	Length int64
	// Part is the 1-based part number; Parts is the total number of parts
	// the file was split into (1 when the file is stored whole).
	Part  int
	Parts int
}

// IsSplit reports whether the piece is one part of a split file.
func (p Piece) IsSplit() bool { return p.Parts > 1 }

// Name is the path the piece is stored under on the ISO. Split parts get a
// zero-padded numeric suffix (movie.mkv.001, movie.mkv.002, …) so they sort
// in order and can be rejoined with `cat movie.mkv.* > movie.mkv`.
func (p Piece) Name() string {
	if !p.IsSplit() {
		return p.File.RelPath
	}
	width := max(3, len(fmt.Sprint(p.Parts)))
	return fmt.Sprintf("%s.%0*d", p.File.RelPath, width, p.Part)
}

// Chunk is the planned content of a single ISO.
type Chunk struct {
	Index  int
	Name   string
	Pieces []Piece
	// Size is the number of file-data bytes on the ISO; Used additionally
	// counts sector padding and per-entry metadata.
	Size int64
	Used int64
}

// ScanFolder recursively lists every regular file under root.
func ScanFolder(root string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, File{RelPath: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return files, err
}

// Filter returns the files whose relative path matches re.
func Filter(files []File, re *regexp.Regexp) []File {
	var out []File
	for _, f := range files {
		if re.MatchString(f.RelPath) {
			out = append(out, f)
		}
	}
	return out
}

// TotalSize sums the sizes of files.
func TotalSize(files []File) int64 {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return total
}

// UsableCapacity is the space available for file entries on a disc of
// discBytes, after reserving room for filesystem structures.
func UsableCapacity(discBytes int64) int64 {
	return alignDown(discBytes - ReservedPerISO)
}

// pieceCost is the space a piece of length bytes occupies on an ISO.
func pieceCost(length int64) int64 {
	return alignUp(length) + entryOverhead
}

func alignUp(n int64) int64   { return (n + SectorSize - 1) / SectorSize * SectorSize }
func alignDown(n int64) int64 { return n / SectorSize * SectorSize }

// PlanChunks packs files, in order, into ISOs whose used space never exceeds
// capacity, naming each ISO with OutputName. Files that fit on an empty ISO
// are kept whole and start a new ISO when the current one is full. Files too
// large for a single ISO or its file-entry limit are split into parts.
func PlanChunks(files []File, capacity int64, isoName string) ([]Chunk, error) {
	capacity = alignDown(capacity)
	if capacity < pieceCost(SectorSize) {
		return nil, errors.New("target ISO size is too small to hold any data")
	}
	p := planner{capacity: capacity, isoName: isoName}
	for _, f := range files {
		if f.Size <= MaxPieceSize && pieceCost(f.Size) <= capacity {
			if p.free() < pieceCost(f.Size) {
				p.open()
			}
			p.add(Piece{File: f, Length: f.Size, Part: 1, Parts: 1})
			continue
		}
		p.addSplit(f)
	}
	return p.chunks, nil
}

type planner struct {
	capacity int64
	isoName  string
	chunks   []Chunk
}

// free is the unused space on the current ISO, or 0 if none is open.
func (p *planner) free() int64 {
	if len(p.chunks) == 0 {
		return 0
	}
	return p.capacity - p.chunks[len(p.chunks)-1].Used
}

func (p *planner) open() {
	idx := len(p.chunks) + 1
	p.chunks = append(p.chunks, Chunk{Index: idx, Name: OutputName(p.isoName, idx)})
}

// add appends pc to the current ISO, which must have room for it.
func (p *planner) add(pc Piece) {
	c := &p.chunks[len(p.chunks)-1]
	c.Pieces = append(c.Pieces, pc)
	c.Size += pc.Length
	c.Used += pieceCost(pc.Length)
}

// addSplit spreads f over as many ISO entries and discs as needed, using
// sector-aligned parts. A disc can hold several parts of the same file.
func (p *planner) addSplit(f File) {
	type ref struct{ chunk, piece int }
	var refs []ref
	for offset := int64(0); offset < f.Size; {
		room := p.free() - entryOverhead
		if room < SectorSize {
			p.open()
			room = p.capacity - entryOverhead
		}
		n := min(f.Size-offset, room, MaxPieceSize)
		p.add(Piece{File: f, Offset: offset, Length: n, Part: len(refs) + 1})
		last := len(p.chunks) - 1
		refs = append(refs, ref{last, len(p.chunks[last].Pieces) - 1})
		offset += n
	}
	for _, r := range refs {
		p.chunks[r.chunk].Pieces[r.piece].Parts = len(refs)
	}
}
