// Package iso writes planned chunks as ISO 9660 images.
package iso

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"github.com/sirily11/iso-burner/internal/settings"
)

// Generate writes chunks concurrently into outputDir. Completed images remain
// available if another chunk fails; an incomplete image is never published.
// Existing output files are never replaced. Per-chunk progress is recorded in
// progress (which may be nil), indexed by position in chunks. Cancelling ctx
// stops copying; chunks that have not been published are discarded.
func Generate(ctx context.Context, sourceDir, outputDir string, targetBytes int64, chunks []settings.Chunk, progress *Progress) error {
	if len(chunks) == 0 {
		return nil
	}
	if targetBytes <= settings.ReservedPerISO {
		return fmt.Errorf("target ISO size %d is too small", targetBytes)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	outputNames := make(map[string]bool, len(chunks))
	for _, chunk := range chunks {
		if err := validateChunk(chunk, targetBytes); err != nil {
			return err
		}
		if outputNames[chunk.Name] {
			return fmt.Errorf("duplicate output name %s", chunk.Name)
		}
		outputNames[chunk.Name] = true
		if _, err := os.Lstat(filepath.Join(outputDir, chunk.Name)); err == nil {
			return fmt.Errorf("output %s already exists", chunk.Name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check output %s: %w", chunk.Name, err)
		}
	}

	workers := min(len(chunks), runtime.GOMAXPROCS(0), 4)
	jobs := make(chan int)
	errs := make(chan error, len(chunks))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				chunk := chunks[i]
				if err := ctx.Err(); err != nil {
					progress.fail(i, err)
					errs <- fmt.Errorf("%s: %w", chunk.Name, err)
					continue
				}
				progress.setStage(i, StageCopying)
				if err := writeChunk(ctx, sourceDir, outputDir, targetBytes, chunk, progress, i); err != nil {
					progress.fail(i, err)
					errs <- fmt.Errorf("%s: %w", chunk.Name, err)
					continue
				}
				progress.setStage(i, StageDone)
			}
		}()
	}
	for i := range chunks {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func validateChunk(chunk settings.Chunk, targetBytes int64) error {
	if chunk.Name == "" || filepath.Base(chunk.Name) != chunk.Name || chunk.Name == "." || chunk.Name == ".." {
		return fmt.Errorf("invalid output name %q", chunk.Name)
	}
	if chunk.Used+settings.ReservedPerISO > targetBytes {
		return fmt.Errorf("%s exceeds target size in plan", chunk.Name)
	}
	seen := make(map[string]bool, len(chunk.Pieces))
	for _, piece := range chunk.Pieces {
		name := piece.Name()
		if name == "." || name == ".." || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") || len(name) == 0 {
			return fmt.Errorf("invalid entry path %q in %s", name, chunk.Name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate entry %q in %s", name, chunk.Name)
		}
		seen[name] = true
		if piece.Length < 0 || piece.Length > settings.MaxPieceSize || piece.Offset < 0 || piece.Offset > piece.File.Size || piece.Length > piece.File.Size-piece.Offset {
			return fmt.Errorf("invalid byte range for %q in %s", name, chunk.Name)
		}
	}
	return nil
}

func writeChunk(ctx context.Context, sourceDir, outputDir string, targetBytes int64, chunk settings.Chunk, progress *Progress, index int) error {
	workDir, err := os.MkdirTemp(outputDir, ".iso-burner-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	imagePath := filepath.Join(workDir, chunk.Name)
	d, err := diskfs.Create(imagePath, targetBytes, diskfs.SectorSizeDefault)
	if err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	defer d.Close()
	d.LogicalBlocksize = settings.SectorSize
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660})
	if err != nil {
		return fmt.Errorf("create ISO filesystem: %w", err)
	}
	defer fs.Close()

	for _, piece := range chunk.Pieces {
		if err := copyPiece(fs, sourceDir, piece, &progressReader{ctx: ctx, progress: progress, index: index}); err != nil {
			return err
		}
	}
	progress.setStage(index, StageFinalizing)
	isoFS := fs.(*iso9660.FileSystem)
	if err := isoFS.Finalize(iso9660.FinalizeOptions{RockRidge: true}); err != nil {
		return fmt.Errorf("finalize ISO: %w", err)
	}

	// diskfs creates a target-sized backing file. Trim it to the actual ISO
	// extent recorded in the primary volume descriptor before publishing.
	f, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	var descriptor [6]byte
	if _, err := f.ReadAt(descriptor[:], 16*settings.SectorSize); err != nil {
		return fmt.Errorf("read volume descriptor: %w", err)
	}
	if descriptor[0] != 1 || string(descriptor[1:]) != "CD001" {
		return errors.New("invalid ISO primary volume descriptor")
	}
	var volumeSize [4]byte
	if _, err := f.ReadAt(volumeSize[:], 16*settings.SectorSize+80); err != nil {
		return fmt.Errorf("read ISO volume size: %w", err)
	}
	actualBytes := int64(binary.LittleEndian.Uint32(volumeSize[:])) * settings.SectorSize
	if actualBytes < 19*settings.SectorSize || actualBytes > targetBytes {
		return fmt.Errorf("finished ISO is %d bytes; target is %d", actualBytes, targetBytes)
	}
	if err := f.Truncate(actualBytes); err != nil {
		return fmt.Errorf("trim ISO: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync ISO: %w", err)
	}
	// Linking publishes only a complete image and fails if another process
	// created the destination after the preflight check.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(imagePath, filepath.Join(outputDir, chunk.Name)); err != nil {
		return fmt.Errorf("publish ISO: %w", err)
	}
	return nil
}

// copyPiece copies piece into fs, reading the source through r (whose
// underlying reader is set here) so progress and cancellation apply.
func copyPiece(fs filesystem.FileSystem, sourceDir string, piece settings.Piece, r *progressReader) error {
	sourcePath := filepath.Join(sourceDir, filepath.FromSlash(piece.File.RelPath))
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", piece.File.RelPath, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != piece.File.Size {
		return fmt.Errorf("source %s changed since preview", piece.File.RelPath)
	}
	if _, err := source.Seek(piece.Offset, io.SeekStart); err != nil {
		return err
	}
	name := piece.Name()
	if dir := path.Dir(name); dir != "." {
		if err := fs.Mkdir(dir); err != nil {
			return fmt.Errorf("create directory for %s: %w", name, err)
		}
	}
	dest, err := fs.OpenFile(name, os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("create ISO entry %s: %w", name, err)
	}
	r.r = source
	_, copyErr := io.CopyN(dest, r, piece.Length)
	closeErr := dest.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s: %w", name, copyErr)
	}
	return closeErr
}
