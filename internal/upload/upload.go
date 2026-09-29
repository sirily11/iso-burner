// Package upload adds files to an rxstorage item as content. Images get a
// thumbnail and videos a thumbnail and a compressed preview video, made the
// same way the rxstorage web uploader makes them; other files are added
// without previews. The original files stay where they are: rxstorage records
// their path, not their bytes.
package upload

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

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
	StageReady
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
	// Err is why the file failed, or why its last try failed while it waits
	// in the queue to be tried again.
	Err error
	// Tries is how many times the file has been tried since it was queued.
	Tries int
}

// Retrying reports whether the file is queued to be tried again after a
// failed try.
func (s FileStatus) Retrying() bool { return s.Stage == StageQueued && s.Err != nil }

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
	case StageReady:
		return "waiting to upload"
	case StageUploading:
		return "uploading"
	case StageDone:
		return "done"
	case StageFailed:
		return "failed"
	}
	if s.Retrying() {
		return "queued to retry"
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
	case StageReady:
		return 0.8
	case StageUploading:
		return 0.8 + 0.2*s.Fraction
	case StageDone, StageFailed:
		return 1
	}
	return 0
}

// Progress records per-file upload progress and holds the queue of files
// still to upload. It is safe for concurrent use; a nil *Progress ignores
// all updates.
type Progress struct {
	mu      sync.Mutex
	files   []FileStatus
	queue   []int // files waiting to be uploaded, in order
	active  int   // files taken from the queue and not yet finished
	changed *sync.Cond
}

// NewProgress tracks files, all initially queued in order.
func NewProgress(files []settings.File) *Progress {
	p := &Progress{files: make([]FileStatus, len(files)), queue: make([]int, len(files))}
	p.changed = sync.NewCond(&p.mu)
	for i, f := range files {
		p.files[i].Kind = media.KindOf(f.RelPath)
		p.queue[i] = i
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

// Pending reports how many files are queued to upload.
func (p *Progress) Pending() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// RetryFailed queues every failed file to be uploaded again, with a fresh
// set of tries, and returns how many it queued. A running Job picks them up
// once the files ahead of them are done; otherwise run the Job again.
func (p *Progress) RetryFailed() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.files {
		if f := &p.files[i]; f.Stage == StageFailed {
			f.Stage, f.Fraction, f.Tries = StageQueued, 0, 0
			p.queue = append(p.queue, i)
			n++
		}
	}
	if n > 0 {
		p.changed.Broadcast()
	}
	return n
}

// next takes the next queued file. While the queue is empty but other files
// are still uploading it waits, since one of them may fail and be queued
// again. It returns false when nothing is left or ctx is done.
func (p *Progress) next(ctx context.Context) (int, FileStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return 0, FileStatus{}, false
		}
		if len(p.queue) > 0 {
			i := p.queue[0]
			p.queue = p.queue[1:]
			p.active++
			p.files[i].Tries++
			return i, p.files[i], true
		}
		if p.active == 0 {
			return 0, FileStatus{}, false
		}
		p.changed.Wait()
	}
}

// finish records how file i's try ended. A failed file with tries left goes
// to the back of the queue, so it is tried again after the files ahead of it.
func (p *Progress) finish(i int, err error, retry bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := &p.files[i]
	p.active--
	switch {
	case err == nil:
		f.Stage, f.Fraction, f.Err = StageDone, 0, nil
	case retry:
		f.Stage, f.Fraction, f.Err = StageQueued, 0, err
		p.queue = append(p.queue, i)
	default:
		f.Stage, f.Err = StageFailed, err
	}
	p.changed.Broadcast()
}

// wake wakes workers waiting in next, so they notice ctx is done.
func (p *Progress) wake() {
	p.mu.Lock()
	p.changed.Broadcast()
	p.mu.Unlock()
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

// DefaultWorkers is the number of simultaneous preparations and the number
// of simultaneous uploads when Workers is not set.
const DefaultWorkers = 3

// DefaultAttempts is how many times a Job tries each file when Attempts is
// not set.
const DefaultAttempts = 3

// DefaultRetryDelay is how long a Job waits before trying a file again when
// RetryDelay is not set; it grows with each try.
const DefaultRetryDelay = 2 * time.Second

// Job uploads Files to the rxstorage item ItemID. The files are read from
// Folder, or from the ISO image ISO when it is set.
type Job struct {
	Client *remote.Client
	ItemID string
	Folder string
	ISO    string
	Files  []settings.File
	// Workers limits each stage independently: up to Workers files preparing
	// and Workers files uploading at the same time; DefaultWorkers if 0.
	Workers int
	// Attempts is how many times a file is tried before it is marked failed;
	// DefaultAttempts if 0. Only errors that may pass are tried again.
	Attempts int
	// RetryDelay is the wait before a file's second try, doubled for each
	// later one; DefaultRetryDelay if 0, no wait if negative.
	RetryDelay time.Duration
}

// Run uploads the files queued in p, several at a time, recording each in p.
// A file that fails with an error that may pass, such as a network or server
// error, goes to the back of the queue to be tried again; once out of tries,
// or on an error that will not pass, it is marked failed and the rest still
// upload. Files queued by p.RetryFailed while Run works are uploaded too.
// Run itself fails only when nothing can be uploaded, or when ctx is
// cancelled, which leaves files not yet started queued.
func (j Job) Run(ctx context.Context, p *Progress) error {
	if p == nil {
		p = NewProgress(j.Files)
	}
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

	workers := cmp.Or(max(j.Workers, 0), DefaultWorkers)
	attempts := cmp.Or(max(j.Attempts, 0), DefaultAttempts)
	prepareSlots := make(chan struct{}, workers)
	uploadSlots := make(chan struct{}, workers)
	stop := context.AfterFunc(ctx, p.wake)
	defer stop()

	// Files are taken from the queue in order, so they start in the order
	// listed, with files being tried again after them.
	var wg sync.WaitGroup
	for range min(2*workers, len(j.Files)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i, st, ok := p.next(ctx)
				if !ok {
					return
				}
				f := j.Files[i]
				err := j.wait(ctx, st.Tries)
				if err == nil {
					err = j.upload(ctx, src, tmp, i, f, p, prepareSlots, uploadSlots)
				}
				switch {
				case err == nil:
				case ctx.Err() != nil:
					err = context.Cause(ctx)
				case st.Tries < attempts && Retryable(err):
					slog.Warn("upload failed; will retry", "item", j.ItemID, "file", f.RelPath,
						"try", st.Tries, "of", attempts, "err", err)
					p.finish(i, err, true)
					continue
				default:
					slog.Error("upload failed", "item", j.ItemID, "file", f.RelPath, "tries", st.Tries, "err", err)
				}
				p.finish(i, err, false)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// runLimited holds one slot only for the work in its stage. Waiting for a
// slot respects cancellation and does not appear as an active stage in p.
func runLimited(ctx context.Context, slots chan struct{}, work func() error) error {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return work()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// wait pauses before a file's try when it has been tried before.
func (j Job) wait(ctx context.Context, try int) error {
	delay := cmp.Or(j.RetryDelay, DefaultRetryDelay)
	if try <= 1 || delay < 0 {
		return nil
	}
	t := time.NewTimer(delay << min(try-2, 5))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Retryable reports whether an upload that failed with err may succeed if
// tried again. Missing files, rejected sign-ins and requests the server
// refused, such as a file the item already has, will fail the same way.
func Retryable(err error) bool {
	var status *remote.StatusError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, fs.ErrNotExist),
		errors.Is(err, fs.ErrPermission), errors.Is(err, remote.ErrUnauthorized):
		return false
	case errors.As(err, &status):
		return status.Temporary()
	}
	return true
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
func (j Job) upload(ctx context.Context, src *source, tmp string, i int, f settings.File, p *Progress, prepareSlots, uploadSlots chan struct{}) error {
	name := path.Base(f.RelPath)
	kind := media.KindOf(name)
	if kind == media.KindFile {
		return runLimited(ctx, uploadSlots, func() error {
			p.setStage(i, StageUploading)
			return j.Client.CreateFileContent(ctx, j.ItemID, remote.FileContent{
				Title:    name,
				MimeType: media.MimeType(name),
				Size:     f.Size,
				FilePath: f.RelPath,
			})
		})
	}

	var prev media.Preview
	err := runLimited(ctx, prepareSlots, func() error {
		local := filepath.Join(j.Folder, filepath.FromSlash(f.RelPath))
		if src != nil {
			p.setStage(i, StageExtracting)
			local = filepath.Join(tmp, fmt.Sprintf("%d-%s", i, name))
			if err := src.extract(f.RelPath, local); err != nil {
				return err
			}
			defer os.Remove(local)
		} else if _, err := os.Stat(local); err != nil {
			return err // a missing file is not tried again
		}

		p.setStage(i, StagePreparing)
		var err error
		prev, err = media.Prepare(ctx, local, tmp, func(v float64) { p.setFraction(i, v) })
		return err
	})
	if err != nil {
		return err
	}
	defer prev.Remove()
	p.setStage(i, StageReady)
	return runLimited(ctx, uploadSlots, func() error {
		p.setStage(i, StageUploading)
		return j.uploadPreview(ctx, i, f, name, kind, prev, p)
	})
}

func (j Job) uploadPreview(ctx context.Context, i int, f settings.File, name string, kind media.Kind, prev media.Preview, p *Progress) error {
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
