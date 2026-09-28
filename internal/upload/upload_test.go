package upload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirily11/iso-burner/internal/iso"
	"github.com/sirily11/iso-burner/internal/media"
	"github.com/sirily11/iso-burner/internal/remote"
	"github.com/sirily11/iso-burner/internal/settings"
)

type testToken struct{}

func (testToken) AccessToken(context.Context) (string, error) { return "tok", nil }

// put is a preview uploaded to the fake storage.
type put struct {
	contentType string
	body        []byte
}

// fakeServer is rxstorage and its S3 bucket in one: it hands out upload URLs
// on itself and keeps what is created and uploaded.
type fakeServer struct {
	mu       sync.Mutex
	previews []remote.PreviewRequest
	files    []remote.FileContent
	puts     map[string]put
	titles   map[string]bool
	client   *remote.Client
}

func newFakeServer(t *testing.T) *fakeServer {
	s := &fakeServer{puts: map[string]put{}, titles: map[string]bool{}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/upload/content-preview":
			var req struct {
				ItemID string                  `json:"item_id"`
				Items  []remote.PreviewRequest `json:"items"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var out []remote.PreviewUpload
			for _, it := range req.Items {
				if s.titles[it.Title] {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"error":"Content with the same name already exists: %s"}`, it.Title)
					return
				}
				s.titles[it.Title] = true
				s.previews = append(s.previews, it)
				n := len(s.previews)
				up := remote.PreviewUpload{ID: fmt.Sprint(n), ImageURL: fmt.Sprintf("%s/s3/%d-thumb?sig=1", srv.URL, n)}
				if it.Type == "video" {
					up.VideoURL = fmt.Sprintf("%s/s3/%d-video?sig=1", srv.URL, n)
				}
				out = append(out, up)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/items/item1/contents":
			var req struct {
				Type string             `json:"type"`
				Data remote.FileContent `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Type != "file" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.files = append(s.files, req.Data)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/s3/"):
			body, _ := io.ReadAll(r.Body)
			s.puts[strings.TrimPrefix(r.URL.Path, "/s3/")] = put{r.Header.Get("Content-Type"), body}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	s.client = remote.NewClient(srv.URL, testToken{})
	return s
}

func needFFmpeg(t *testing.T) {
	t.Helper()
	if err := media.Available(); err != nil {
		t.Skip(err)
	}
}

// ffmpeg writes a test clip or image made by ffmpeg's lavfi sources.
func ffmpeg(t *testing.T, path string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, append(args, path)...)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", path, err, out)
	}
}

// writeMedia fills root with a video, an image and a text file.
func writeMedia(t *testing.T, root string) []settings.File {
	t.Helper()
	ffmpeg(t, filepath.Join(root, "trip", "clip.mov"),
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "mpeg4", "-c:a", "pcm_s16le")
	ffmpeg(t, filepath.Join(root, "photo.png"), "-f", "lavfi", "-i", "testsrc2=size=1000x800", "-frames:v", "1")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := settings.ScanFolder(root)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func checkUploaded(t *testing.T, s *fakeServer, files []settings.File) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	sizes := map[string]int64{}
	for _, f := range files {
		sizes[f.RelPath] = f.Size
	}
	if len(s.previews) != 2 || len(s.files) != 1 {
		t.Fatalf("previews = %+v, files = %+v", s.previews, s.files)
	}
	for i, p := range s.previews {
		if p.Size != sizes[p.FilePath] || p.Title != filepath.Base(p.FilePath) || p.Filename != p.Title {
			t.Errorf("preview %d = %+v", i, p)
		}
		thumb := s.puts[fmt.Sprintf("%d-thumb", i+1)]
		if thumb.contentType != "image/jpeg" || !strings.HasPrefix(string(thumb.body), "\xff\xd8") {
			t.Errorf("%s thumbnail: type %q, %d bytes", p.FilePath, thumb.contentType, len(thumb.body))
		}
		switch p.FilePath {
		case "trip/clip.mov":
			if p.Type != "video" || p.MimeType != "video/quicktime" || p.VideoLength == nil || *p.VideoLength < 1.9 {
				t.Errorf("video request = %+v", p)
			}
			video := s.puts[fmt.Sprintf("%d-video", i+1)]
			if video.contentType != "video/quicktime" || len(video.body) == 0 || int64(len(video.body)) >= p.Size {
				t.Errorf("preview video: type %q, %d bytes of %d", video.contentType, len(video.body), p.Size)
			}
		case "photo.png":
			if p.Type != "image" || p.MimeType != "image/png" || p.VideoLength != nil {
				t.Errorf("image request = %+v", p)
			}
		default:
			t.Errorf("unexpected preview for %s", p.FilePath)
		}
	}
	if f := s.files[0]; f.FilePath != "notes.txt" || f.Title != "notes.txt" || f.MimeType != "application/octet-stream" || f.Size != 5 {
		t.Errorf("file content = %+v", f)
	}
}

func checkAllDone(t *testing.T, p *Progress) {
	t.Helper()
	for i, s := range p.Snapshot() {
		if s.Stage != StageDone || s.Completion() != 1 {
			t.Errorf("file %d: %s (%v)", i, s.Label(), s.Err)
		}
	}
}

func TestRunFromFolder(t *testing.T) {
	needFFmpeg(t)
	root := t.TempDir()
	files := writeMedia(t, root)
	s := newFakeServer(t)

	p := NewProgress(files)
	job := Job{Client: s.client, ItemID: "item1", Folder: root, Files: files}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	checkUploaded(t, s, files)

	// Running again fails each image and video, since the item already has
	// them, but carries on to the end.
	p = NewProgress(files)
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for i, st := range p.Snapshot() {
		wantFail := media.KindOf(files[i].RelPath) != media.KindFile
		if (st.Stage == StageFailed) != wantFail {
			t.Errorf("%s: %s (%v)", files[i].RelPath, st.Label(), st.Err)
		}
		if wantFail && !strings.Contains(st.Err.Error(), "already exists") {
			t.Errorf("%s: err = %v", files[i].RelPath, st.Err)
		}
	}
}

func TestRunFromISO(t *testing.T) {
	needFFmpeg(t)
	source, isoDir := t.TempDir(), t.TempDir()
	files := writeMedia(t, source)
	const target = settings.ReservedPerISO + 16*1024*1024
	chunks, err := settings.PlanChunks(files, settings.UsableCapacity(target), "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := iso.Generate(context.Background(), source, isoDir, target, chunks, nil); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(isoDir, chunks[0].Name)
	isoFiles, err := iso.ListFiles(image)
	if err != nil {
		t.Fatal(err)
	}
	s := newFakeServer(t)

	p := NewProgress(isoFiles)
	if err := (Job{Client: s.client, ItemID: "item1", ISO: image, Files: isoFiles}).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	checkUploaded(t, s, isoFiles)
}

func TestRunMissingFileContinues(t *testing.T) {
	needFFmpeg(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []settings.File{{RelPath: "gone.mp4", Size: 10}, {RelPath: "a.txt", Size: 1}}
	s := newFakeServer(t)
	p := NewProgress(files)
	if err := (Job{Client: s.client, ItemID: "item1", Folder: root, Files: files}).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	st := p.Snapshot()
	if st[0].Stage != StageFailed || st[0].Err == nil || st[1].Stage != StageDone {
		t.Fatalf("statuses = %+v", st)
	}
	if len(s.previews) != 0 || len(s.files) != 1 {
		t.Errorf("a file that cannot be read should create no content: %+v", s.previews)
	}
}

func TestRunCancelled(t *testing.T) {
	files := []settings.File{{RelPath: "a.txt", Size: 1}}
	s := newFakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewProgress(files)
	err := (Job{Client: s.client, ItemID: "item1", Folder: t.TempDir(), Files: files}).Run(ctx, p)
	if err != context.Canceled || p.Snapshot()[0].Stage != StageQueued || len(s.files) != 0 {
		t.Fatalf("err = %v, status = %+v", err, p.Snapshot())
	}
}

func TestRunUploadsFilesInParallel(t *testing.T) {
	var files []settings.File
	for i := range 6 {
		files = append(files, settings.File{RelPath: fmt.Sprintf("f%d.txt", i), Size: 1})
	}
	// Each request waits until DefaultWorkers requests are in flight, so the
	// run finishes only if files are uploaded side by side.
	arrived := make(chan struct{}, len(files))
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		if len(arrived) >= DefaultWorkers {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		var req struct{ Data remote.FileContent }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req.Data.FilePath)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	p := NewProgress(files)
	job := Job{Client: remote.NewClient(srv.URL, testToken{}), ItemID: "item1", Folder: t.TempDir(), Files: files}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	if len(got) != len(files) {
		t.Errorf("uploaded %v, want %d files", got, len(files))
	}
}
