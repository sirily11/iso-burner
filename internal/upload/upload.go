// Package upload adds files to an rxstorage item as content. Images get a
// thumbnail and videos a thumbnail and a compressed preview video, made the
// same way the rxstorage web uploader makes them; other files are added
// without previews. The original files stay where they are: rxstorage records
// their path, not their bytes.
package upload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/media"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
)

// Stage is where one file is in its upload.
type Stage int

const (
	StageQueued Stage = iota
	StageExtracting
	StagePreparing
	StageUploading
	StageDone
	StageFailed
)

// FileStatus is a point-in-time view of one file's upload.
type FileStatus struct {
	Stage Stage
	Kind  media.Kind
	// Fraction is how far the current stage is, from 0 to 1, for stages that
	// report it: compressing a video and uploading previews.
	Fraction float64
	Err      error
}

// Label says what is happening to the file.
func (s FileStatus) Label() string {
	switch s.Stage {
	case StageExtracting:
		return "reading from ISO"
	case StagePreparing:
		if s.Kind == media.KindVideo {
			return "compressing"
		}
		return "making thumbnail"
	case StageUploading:
		return "uploading"
	case StageDone:
		return "done"
	case StageFailed:
		return "failed"
	}
	return "queued"
}

// Completion estimates how much of the file's upload is done, from 0 to 1.
// Making previews takes most of the time, and compressing a video the most.
func (s FileStatus) Completion() float64 {
	switch s.Stage {
	case StageExtracting:
		return 0.05
	case StagePreparing:
		return 0.1 + 0.7*s.Fraction
	case StageUploading:
		return 0.8 + 0.2*s.Fraction
	case StageDone, StageFailed:
		return 1
	}
	return 0
}

// Progress records per-file upload progress. It is safe for concurrent use;
// a nil *Progress ignores all updates.
type Progress struct {
	mu    sync.Mutex
	files []FileStatus
}

// NewProgress tracks files, all initially queued.
func NewProgress(files []settings.File) *Progress {
	p := &Progress{files: make([]FileStatus, len(files))}
	for i, f := range files {
		p.files[i].Kind = media.KindOf(f.RelPath)
	}
	return p
}

// Snapshot returns a copy of every file's current status.
func (p *Progress) Snapshot() []FileStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]FileStatus(nil), p.files...)
}

func (p *Progress) update(i int, fn func(*FileStatus)) {
	if p == nil || i < 0 || i >= len(p.files) {
		return
	}
	p.mu.Lock()
	fn(&p.files[i])
	p.mu.Unlock()
}

func (p *Progress) setStage(i int, s Stage) {
	p.update(i, func(f *FileStatus) { f.Stage, f.Fraction = s, 0 })
}

func (p *Progress) setFraction(i int, v float64) {
	p.update(i, func(f *FileStatus) { f.Fraction = v })
}

func (p *Progress) fail(i int, err error) {
	p.update(i, func(f *FileStatus) { f.Stage, f.Err = StageFailed, err })
}

// DefaultWorkers is how many files a Job prepares and uploads at once when
// Workers is not set: enough that uploads run while other files compress,
// without running many ffmpeg processes side by side.
const DefaultWorkers = 3

// Job uploads Files to the rxstorage item ItemID. The files are read from
// Folder, or from the ISO image ISO when it is set.
type Job struct {
	Client *remote.Client
	ItemID string
	Folder string
	ISO    string
	Files  []settings.File
	// Workers is how many files are handled at once; DefaultWorkers if 0.
	Workers int
}

// Run uploads the files, several at a time, recording each in p. A file that
// fails is marked failed and the rest still upload; Run itself fails only
// when nothing can be uploaded, or when ctx is cancelled.
func (j Job) Run(ctx context.Context, p *Progress) error {
	if needsFFmpeg(j.Files) {
		if err := media.Available(); err != nil {
			return err
		}
	}
	tmp, err := os.MkdirTemp("", "iso-burner-upload-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var src *source
	if j.ISO != "" {
		im, err := iso.OpenImage(j.ISO)
		if err != nil {
			return err
		}
		defer im.Close()
		src = &source{im: im}
	}

	workers := j.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, len(j.Files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if ctx.Err() != nil {
					continue // stopped before this file started: leave it queued
				}
				f := j.Files[i]
				if err := j.upload(ctx, src, tmp, i, f, p); err != nil {
					if ctx.Err() != nil {
						p.fail(i, context.Cause(ctx))
						continue
					}
					slog.Error("upload failed", "item", j.ItemID, "file", f.RelPath, "err", err)
					p.fail(i, err)
					continue
				}
				p.setStage(i, StageDone)
			}
		}()
	}
	// Files are handed out in order, so they start in the order listed.
feed:
	for i := range j.Files {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// source reads files out of an ISO image. The image is read through a single
// handle, so one file is extracted at a time.
type source struct {
	mu sync.Mutex
	im *iso.Image
}

func (s *source) extract(relPath, dst string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.im.Extract(relPath, dst)
}

func needsFFmpeg(files []settings.File) bool {
	for _, f := range files {
		if media.KindOf(f.RelPath) != media.KindFile {
			return true
		}
	}
	return false
}

// upload adds file i to the item. Its title is the file name and its path the
// one relative to the folder or ISO root.
func (j Job) upload(ctx context.Context, src *source, tmp string, i int, f settings.File, p *Progress) error {
	name := path.Base(f.RelPath)
	kind := media.KindOf(name)
	if kind == media.KindFile {
		p.setStage(i, StageUploading)
		return j.Client.CreateFileContent(ctx, j.ItemID, remote.FileContent{
			Title:    name,
			MimeType: media.MimeType(name),
			Size:     f.Size,
			FilePath: f.RelPath,
		})
	}

	local := filepath.Join(j.Folder, filepath.FromSlash(f.RelPath))
	if src != nil {
		p.setStage(i, StageExtracting)
		local = filepath.Join(tmp, fmt.Sprintf("%d-%s", i, name))
		if err := src.extract(f.RelPath, local); err != nil {
			return err
		}
		defer os.Remove(local)
	}

	p.setStage(i, StagePreparing)
	prev, err := media.Prepare(ctx, local, tmp, func(v float64) { p.setFraction(i, v) })
	if err != nil {
		return err
	}
	defer prev.Remove()

	req := remote.PreviewRequest{
		Filename: name,
		Type:     kind.String(),
		Title:    name,
		MimeType: prev.MimeType,
		Size:     f.Size,
		FilePath: f.RelPath,
	}
	if kind == media.KindVideo {
		req.VideoLength = &prev.Duration
	}
	ups, err := j.Client.RequestPreviewUploads(ctx, j.ItemID, []remote.PreviewRequest{req})
	if err != nil {
		return err
	}
	up := ups[0]
	if prev.Video != "" && up.VideoURL == "" {
		return errors.New("rxstorage returned no upload URL for the preview video")
	}

	p.setStage(i, StageUploading)
	parts := []struct{ url, file, contentType string }{{up.ImageURL, prev.Image, "image/jpeg"}}
	if prev.Video != "" {
		// The URL is signed for the original file's type, as the web
		// uploader does, so the preview is sent with it too.
		parts = append(parts, struct{ url, file, contentType string }{up.VideoURL, prev.Video, prev.MimeType})
	}
	var total, base int64
	for _, part := range parts {
		info, err := os.Stat(part.file)
		if err != nil {
			return err
		}
		total += info.Size()
	}
	for _, part := range parts {
		err := j.Client.PutFile(ctx, part.url, part.file, part.contentType, func(sent, size int64) {
			if total > 0 {
				p.setFraction(i, float64(base+sent)/float64(total))
			}
		})
		if err != nil {
			return err
		}
		info, _ := os.Stat(part.file)
		base += info.Size()
	}
	return nil
}
