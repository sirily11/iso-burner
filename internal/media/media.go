// Package media prepares files for upload to rxstorage: it compresses videos
// into small MP4 previews and creates JPEG thumbnails with ffmpeg. The
// settings match the rxstorage web uploader so content looks the same no
// matter where it was uploaded from.
package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrFFmpegMissing is returned when ffmpeg or ffprobe is not on the PATH.
var ErrFFmpegMissing = errors.New("ffmpeg is not installed (install ffmpeg and make sure ffmpeg and ffprobe are on the PATH)")

const (
	// MaxPreviewSeconds is the longest preview video; longer videos are cut.
	MaxPreviewSeconds = 300
	// previewWidth is the widest a preview video is, in pixels.
	previewWidth = 720
	// thumbnailWidth is the widest a thumbnail is, in pixels.
	thumbnailWidth = 480
	// thumbnailAt is how far into a video the thumbnail frame is taken.
	thumbnailAt = "1"
)

// Kind is the rxstorage content type of a file.
type Kind int

const (
	KindFile Kind = iota
	KindImage
	KindVideo
)

// String is the content type name used by the rxstorage API.
func (k Kind) String() string {
	switch k {
	case KindImage:
		return "image"
	case KindVideo:
		return "video"
	}
	return "file"
}

var mimeTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".heic": "image/heic",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".mov":  "video/quicktime",
	".avi":  "video/x-msvideo",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".mts":  "video/mp2t",
	".m2ts": "video/mp2t",
	".ts":   "video/mp2t",
	".mpg":  "video/mpeg",
	".mpeg": "video/mpeg",
	".wmv":  "video/x-ms-wmv",
	".flv":  "video/x-flv",
	".3gp":  "video/3gpp",
}

// MimeType is the MIME type of a file, from its extension.
func MimeType(name string) string {
	if t, ok := mimeTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

// KindOf is the content type of a file, from its extension.
func KindOf(name string) Kind {
	switch t := MimeType(name); {
	case strings.HasPrefix(t, "video/"):
		return KindVideo
	case strings.HasPrefix(t, "image/"):
		return KindImage
	}
	return KindFile
}

// Available reports whether ffmpeg and ffprobe can be run.
func Available() error {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := tool(name); err != nil {
			return err
		}
	}
	return nil
}

func tool(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", ErrFFmpegMissing
	}
	return p, nil
}

// Duration is the length of a video in seconds, read with ffprobe.
func Duration(ctx context.Context, path string) (float64, error) {
	ffprobe, err := tool("ffprobe")
	if err != nil {
		return 0, err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, toolError(ctx, "read video length", err, stderr.String())
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("read video length: ffprobe reported %q", strings.TrimSpace(string(out)))
	}
	return d, nil
}

// CompressVideo writes a small preview of the video at src to dst as H.264
// MP4 at most 720 pixels wide with AAC audio, cut to the first
// MaxPreviewSeconds. duration is the video length in seconds, used to report
// progress from 0 to 1; progress may be nil.
func CompressVideo(ctx context.Context, src, dst string, duration float64, progress func(float64)) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", src,
		"-t", strconv.Itoa(MaxPreviewSeconds),
		// Subtitle and data streams often cannot be stored in MP4.
		"-sn", "-dn",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", previewWidth),
		"-c:v", "libx264", "-preset", "fast", "-crf", "28",
		// 8-bit 4:2:0 plays everywhere, including 10-bit and 4:4:4 sources.
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		dst,
	}
	total := min(duration, MaxPreviewSeconds)
	err := runFFmpeg(ctx, args, func(line string) {
		if progress == nil || total <= 0 {
			return
		}
		if secs, ok := parseProgress(line); ok {
			progress(min(secs/total, 1))
		}
	})
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("compress %s: %w", filepath.Base(src), err)
	}
	if progress != nil {
		progress(1)
	}
	return nil
}

// VideoThumbnail writes a JPEG of the frame one second into the video at src
// to dst, at most 480 pixels wide. Videos shorter than a second use their
// first frame.
func VideoThumbnail(ctx context.Context, src, dst string) error {
	err := frame(ctx, src, dst, thumbnailAt)
	if err != nil || !nonEmpty(dst) {
		err = frame(ctx, src, dst, "")
	}
	if err == nil && !nonEmpty(dst) {
		err = errors.New("ffmpeg wrote no frame")
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("thumbnail %s: %w", filepath.Base(src), err)
	}
	return nil
}

// ImagePreview writes a JPEG of the image at src to dst, at most 480 pixels
// wide.
func ImagePreview(ctx context.Context, src, dst string) error {
	err := frame(ctx, src, dst, "")
	if err == nil && !nonEmpty(dst) {
		err = errors.New("ffmpeg wrote no image")
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("preview %s: %w", filepath.Base(src), err)
	}
	return nil
}

// frame writes one frame of src, at seek seconds when set, to dst as JPEG.
func frame(ctx context.Context, src, dst, seek string) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	if seek != "" {
		args = append(args, "-ss", seek)
	}
	args = append(args, "-i", src,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", thumbnailWidth),
		"-q:v", "5",
		"-update", "1",
		dst,
	)
	return runFFmpeg(ctx, args, nil)
}

// Preview is what Prepare made for a file.
type Preview struct {
	Kind     Kind
	MimeType string
	// Duration is the length of the original video in seconds.
	Duration float64
	// Video is the compressed preview video, for videos.
	Video string
	// Image is the thumbnail, for videos and images.
	Image string
}

// Prepare makes the previews rxstorage shows for the file at src, writing
// them into dir: a compressed video and a thumbnail for videos, and a
// thumbnail for images. Other files need no preview. Output names are unique,
// so many files can be prepared into the same dir at once. progress reports
// the compression from 0 to 1 and may be nil.
func Prepare(ctx context.Context, src, dir string, progress func(float64)) (p Preview, err error) {
	p = Preview{Kind: KindOf(src), MimeType: MimeType(src)}
	if p.Kind == KindFile {
		return p, nil
	}
	if err := Available(); err != nil {
		return p, err
	}
	defer func() {
		if err != nil {
			removeAll(p.Video, p.Image)
			p.Video, p.Image = "", ""
		}
	}()

	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	if p.Image, err = reserve(dir, stem+"-thumb-*.jpg"); err != nil {
		return p, err
	}
	if p.Kind == KindImage {
		return p, ImagePreview(ctx, src, p.Image)
	}

	if p.Duration, err = Duration(ctx, src); err != nil {
		return p, fmt.Errorf("%s: %w", filepath.Base(src), err)
	}
	if err = VideoThumbnail(ctx, src, p.Image); err != nil {
		return p, err
	}
	if p.Video, err = reserve(dir, stem+"-preview-*.mp4"); err != nil {
		return p, err
	}
	return p, CompressVideo(ctx, src, p.Video, p.Duration, progress)
}

// Remove deletes the files a Prepare made.
func (p Preview) Remove() {
	removeAll(p.Video, p.Image)
}

// reserve creates an empty file with a unique name in dir for ffmpeg to
// overwrite.
func reserve(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

func removeAll(paths ...string) {
	for _, p := range paths {
		if p != "" {
			os.Remove(p)
		}
	}
}

func nonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// runFFmpeg runs ffmpeg, passing each stdout line to line when set.
func runFFmpeg(ctx context.Context, args []string, line func(string)) error {
	ffmpeg, err := tool("ffmpeg")
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Stderr = &stderr
	if line == nil {
		cmd.Stdout = io.Discard
		if err := cmd.Run(); err != nil {
			return toolError(ctx, "ffmpeg", err, stderr.String())
		}
		return nil
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line(sc.Text())
	}
	io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return toolError(ctx, "ffmpeg", err, stderr.String())
	}
	return nil
}

// toolError adds the last line ffmpeg or ffprobe printed to err, since the
// exit status alone does not say what went wrong. A cancelled ctx is reported
// as such rather than as the kill it caused.
func toolError(ctx context.Context, what string, err error, stderr string) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return fmt.Errorf("%s: %s", what, last)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// parseProgress reads the encoded position, in seconds, from an ffmpeg
// `-progress` line such as "out_time_us=1500000". out_time_ms is also in
// microseconds despite its name.
func parseProgress(line string) (float64, bool) {
	k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok || (k != "out_time_us" && k != "out_time_ms") {
		return 0, false
	}
	us, err := strconv.ParseInt(v, 10, 64)
	if err != nil || us < 0 {
		return 0, false
	}
	return float64(us) / 1e6, true
}
