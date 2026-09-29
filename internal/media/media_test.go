package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKindOf(t *testing.T) {
	cases := map[string]Kind{
		"a/clip.MP4":   KindVideo,
		"clip.mkv":     KindVideo,
		"00001.m2ts":   KindVideo,
		"photo.JPEG":   KindImage,
		"photo.heic":   KindImage,
		"notes.txt":    KindFile,
		"no-extension": KindFile,
	}
	for name, want := range cases {
		if got := KindOf(name); got != want {
			t.Errorf("KindOf(%q) = %v, want %v", name, got, want)
		}
	}
	if got := MimeType("x.mov"); got != "video/quicktime" {
		t.Errorf("MimeType(x.mov) = %q", got)
	}
	if got := MimeType("x.bin"); got != "application/octet-stream" {
		t.Errorf("MimeType(x.bin) = %q", got)
	}
}

func TestParseProgress(t *testing.T) {
	cases := []struct {
		line string
		want float64
		ok   bool
	}{
		{"out_time_us=1500000", 1.5, true},
		{"out_time_ms=2000000", 2, true},
		{"out_time_us=N/A", 0, false},
		{"out_time=00:00:01.500000", 0, false},
		{"progress=continue", 0, false},
		{"out_time_us=-9223372036854775807", 0, false},
	}
	for _, c := range cases {
		got, ok := parseProgress(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("parseProgress(%q) = %v, %v; want %v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestVideoEncoders(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"windows", []string{"h264_nvenc", "h264_qsv", "h264_amf", "libx264"}},
		{"darwin", []string{"h264_videotoolbox", "libx264"}},
		{"linux", []string{"libx264"}},
	} {
		encoders := videoEncoders(tc.goos)
		if len(encoders) != len(tc.want) {
			t.Fatalf("%s: got %d encoders, want %d", tc.goos, len(encoders), len(tc.want))
		}
		for i, encoder := range encoders {
			if encoder.name != tc.want[i] {
				t.Errorf("%s: encoder %d = %q, want %q", tc.goos, i, encoder.name, tc.want[i])
			}
		}
	}
	macArgs := videoArgs("clip.mov", "preview.mp4", videoEncoders("darwin")[0])
	if i := indexOf(macArgs, "-allow_sw"); i < 0 || macArgs[i+1] != "0" {
		t.Errorf("VideoToolbox must require hardware encoding: %v", macArgs)
	}
	if i := indexOf(macArgs, "-pix_fmt"); i < 0 || macArgs[i+1] != "nv12" {
		t.Errorf("VideoToolbox must receive NV12 frames: %v", macArgs)
	}
}

func TestCompressVideoFallsBackAfterPartialOutput(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "preview.mp4")
	encoders := []videoEncoder{
		{"h264_nvenc", nil, "nv12"},
		{"libx264", nil, "yuv420p"},
	}
	var attempts []string
	var progress []float64
	run := func(_ context.Context, args []string, line func(string)) error {
		name := args[1+indexOf(args, "-c:v")]
		attempts = append(attempts, name)
		if name == "h264_nvenc" {
			if err := os.WriteFile(dst, []byte("partial"), 0o644); err != nil {
				return err
			}
			line("out_time_us=800000")
			return errors.New("GPU unavailable")
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("partial output remains before software retry: %v", err)
		}
		line("out_time_us=500000")
		return os.WriteFile(dst, []byte("complete"), 0o644)
	}
	if err := compressVideo(context.Background(), "clip.mov", dst, 1, func(f float64) {
		progress = append(progress, f)
	}, encoders, run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(attempts, ",") != "h264_nvenc,libx264" {
		t.Errorf("attempts = %v", attempts)
	}
	if len(progress) == 0 || progress[len(progress)-1] != 1 {
		t.Errorf("progress = %v, want to finish at 1", progress)
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] < progress[i-1] {
			t.Errorf("progress regressed: %v", progress)
		}
	}
	if out, err := os.ReadFile(dst); err != nil || string(out) != "complete" {
		t.Errorf("output = %q, %v", out, err)
	}
}

func indexOf(args []string, value string) int {
	for i, arg := range args {
		if arg == value {
			return i
		}
	}
	return -1
}

func TestPrepareFileNeedsNoPreview(t *testing.T) {
	p, err := Prepare(context.Background(), "notes.txt", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindFile || p.Video != "" || p.Image != "" {
		t.Errorf("Prepare(notes.txt) = %+v, want no previews", p)
	}
}

// needFFmpeg skips the test when ffmpeg is not installed.
func needFFmpeg(t *testing.T) {
	t.Helper()
	if err := Available(); err != nil {
		t.Skip(err)
	}
}

// generate writes a test clip or image made by ffmpeg's lavfi sources.
func generate(t *testing.T, name string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, append(args, path)...)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate %s: %v\n%s", name, err, out)
	}
	return path
}

func TestPrepareVideo(t *testing.T) {
	needFFmpeg(t)
	src := generate(t, "big clip.mov",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "mpeg4", "-q:v", "2", "-c:a", "pcm_s16le")

	dir := t.TempDir()
	var last float64
	var calls int
	p, err := Prepare(context.Background(), src, dir, func(f float64) {
		if f < last {
			t.Errorf("progress went back from %v to %v", last, f)
		}
		last = f
		calls++
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindVideo || p.MimeType != "video/quicktime" {
		t.Errorf("kind = %v, mime = %q", p.Kind, p.MimeType)
	}
	if p.Duration < 2.9 || p.Duration > 3.1 {
		t.Errorf("duration = %v, want about 3", p.Duration)
	}
	if calls == 0 || last != 1 {
		t.Errorf("progress calls = %d, last = %v; want some ending at 1", calls, last)
	}
	for _, f := range []string{p.Video, p.Image} {
		if filepath.Dir(f) != dir {
			t.Errorf("%s is not in %s", f, dir)
		}
		if !nonEmpty(f) {
			t.Errorf("%s is missing or empty", f)
		}
	}
	if !strings.HasPrefix(filepath.Base(p.Video), "big clip-preview-") || filepath.Ext(p.Video) != ".mp4" {
		t.Errorf("video name = %s", p.Video)
	}

	srcInfo, _ := os.Stat(src)
	outInfo, _ := os.Stat(p.Video)
	if outInfo.Size() >= srcInfo.Size() {
		t.Errorf("preview is %d bytes, not smaller than the %d byte source", outInfo.Size(), srcInfo.Size())
	}
	if w, h := dimensions(t, p.Video); w != 720 || h != 406 {
		t.Errorf("preview is %dx%d, want 720x406", w, h)
	}
	if w, h := dimensions(t, p.Image); w != 480 || h != 270 {
		t.Errorf("thumbnail is %dx%d, want 480x270", w, h)
	}

	p.Remove()
	if nonEmpty(p.Video) || nonEmpty(p.Image) {
		t.Error("Remove left files behind")
	}
}

func TestPrepareShortSmallVideo(t *testing.T) {
	needFFmpeg(t)
	// Shorter than the thumbnail offset and narrower than the preview width,
	// with no audio and a subtitle stream MP4 cannot hold.
	srt := filepath.Join(t.TempDir(), "subs.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:00,400\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := generate(t, "short.mkv",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=0.5",
		"-i", srt, "-c:v", "mpeg4", "-c:s", "srt")

	p, err := Prepare(context.Background(), src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Remove()
	if w, h := dimensions(t, p.Video); w != 320 || h != 240 {
		t.Errorf("preview is %dx%d, want 320x240 (no upscaling)", w, h)
	}
	if !nonEmpty(p.Image) {
		t.Error("thumbnail is missing")
	}
}

func TestPrepareImage(t *testing.T) {
	needFFmpeg(t)
	src := generate(t, "photo.png", "-f", "lavfi", "-i", "testsrc2=size=1600x1200", "-frames:v", "1")

	p, err := Prepare(context.Background(), src, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Remove()
	if p.Kind != KindImage || p.Video != "" {
		t.Errorf("Prepare(photo.png) = %+v", p)
	}
	if w, h := dimensions(t, p.Image); w != 480 || h != 360 {
		t.Errorf("thumbnail is %dx%d, want 480x360", w, h)
	}
}

func TestPrepareBrokenVideoCleansUp(t *testing.T) {
	needFFmpeg(t)
	src := filepath.Join(t.TempDir(), "broken.mp4")
	if err := os.WriteFile(src, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := Prepare(context.Background(), src, dir, nil); err == nil {
		t.Fatal("Prepare succeeded on a broken video")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files behind", len(entries))
	}
}

func TestCompressVideoCancelled(t *testing.T) {
	needFFmpeg(t)
	src := generate(t, "clip.mp4", "-f", "lavfi", "-i", "testsrc2=size=320x240:duration=1", "-c:v", "mpeg4")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(t.TempDir(), "out.mp4")
	err := CompressVideo(ctx, src, dst, 1, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Error("cancelled compression left its output behind")
	}
}

// dimensions reads the width and height of the first video stream.
func dimensions(t *testing.T, path string) (int, int) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", path).Output()
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	var w, h int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%dx%d", &w, &h); err != nil {
		t.Fatalf("probe %s: %q: %v", path, out, err)
	}
	return w, h
}
