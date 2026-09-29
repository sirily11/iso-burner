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
	"sync/atomic"
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
	replaced int
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
				ItemID    string                  `json:"item_id"`
				Items     []remote.PreviewRequest `json:"items"`
				Overwrite bool                    `json:"overwrite"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var out []remote.PreviewUpload
			for _, it := range req.Items {
				if s.titles[it.Title] {
					if !req.Overwrite {
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprintf(w, `{"error":"Content with the same name already exists: %s"}`, it.Title)
						return
					}
					s.replaced++
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

	// Running again replaces the images and videos the item already has
	// rather than failing on them.
	p = NewProgress(files)
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	if s.replaced != 2 {
		t.Errorf("replaced %d previews, want 2", s.replaced)
	}
}

func TestRunFromISO(t *testing.T) {
	needFFmpeg(t)
	source, isoDir := t.TempDir(), t.TempDir()
	files := writeMedia(t, source)
	const target = settings.ReservedPerISO + 16*1024*1024
	chunks, err := settings.PlanChunks(files, settings.UsableCapacity(target), "backup", 1)
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

func TestRunPreparesWhileThreeUploadsAreBusy(t *testing.T) {
	needFFmpeg(t)
	root := t.TempDir()
	clip := filepath.Join(root, "clip0.mov")
	ffmpeg(t, clip, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=1", "-c:v", "mpeg4")
	data, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]settings.File, 0, 6)
	for i := range 3 {
		files = append(files, settings.File{RelPath: fmt.Sprintf("file%d.txt", i), Size: 1})
	}
	for i := range 3 {
		name := fmt.Sprintf("clip%d.mov", i)
		if i > 0 {
			if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		files = append(files, settings.File{RelPath: name, Size: int64(len(data))})
	}

	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUploads := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUploads()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/items/item1/contents":
			arrived <- struct{}{}
			<-release
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/upload/content-preview":
			var req struct {
				Items []remote.PreviewRequest `json:"items"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			out := make([]remote.PreviewUpload, len(req.Items))
			for i := range out {
				out[i] = remote.PreviewUpload{ImageURL: srv.URL + "/thumb", VideoURL: srv.URL + "/video"}
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPut:
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := NewProgress(files)
	job := Job{Client: remote.NewClient(srv.URL, testToken{}), ItemID: "item1", Folder: root, Files: files}
	done := make(chan error, 1)
	go func() { done <- job.Run(context.Background(), p) }()
	for range 3 {
		select {
		case <-arrived:
		case err := <-done:
			t.Fatalf("run stopped before three uploads: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("three uploads did not start")
		}
	}
	deadline := time.After(20 * time.Second)
	for {
		st := p.Snapshot()
		ready, uploading := 0, 0
		for _, s := range st {
			if s.Stage == StageReady {
				ready++
			}
			if s.Stage == StageUploading {
				uploading++
			}
		}
		if ready == 3 && uploading == 3 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("run stopped before previews were prepared: %v; statuses: %+v", err, st)
		case <-deadline:
			t.Fatalf("expected 3 uploads and 3 prepared previews together; statuses: %+v", st)
		case <-time.After(20 * time.Millisecond):
		}
	}
	releaseUploads()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not finish after uploads were released")
	}
	checkAllDone(t, p)
}

// flakyServer answers file content requests with the status fail returns for
// each file's nth request, counting from 1, and 201 once it returns 0.
func flakyServer(t *testing.T, fail func(file string, n int) int) (*remote.Client, func(file string) int) {
	t.Helper()
	var mu sync.Mutex
	tries := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Data remote.FileContent }
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		tries[req.Data.FilePath]++
		n := tries[req.Data.FilePath]
		mu.Unlock()
		if code := fail(req.Data.FilePath, n); code != 0 {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return remote.NewClient(srv.URL, testToken{}), func(file string) int {
		mu.Lock()
		defer mu.Unlock()
		return tries[file]
	}
}

func TestRunRetriesFailedFiles(t *testing.T) {
	files := []settings.File{{RelPath: "flaky.txt", Size: 1}, {RelPath: "gone.txt", Size: 1}, {RelPath: "ok.txt", Size: 1}}
	client, tries := flakyServer(t, func(file string, n int) int {
		switch {
		case file == "flaky.txt" && n < 3:
			return http.StatusServiceUnavailable // passes on the last try
		case file == "gone.txt":
			return http.StatusBadGateway // never passes
		}
		return 0
	})
	p := NewProgress(files)
	job := Job{Client: client, ItemID: "item1", Folder: t.TempDir(), Files: files, Workers: 1, RetryDelay: -1}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	st := p.Snapshot()
	if st[0].Stage != StageDone || st[0].Err != nil || st[0].Tries != 3 || tries("flaky.txt") != 3 {
		t.Errorf("flaky file = %+v after %d tries, want done on the third", st[0], tries("flaky.txt"))
	}
	if st[1].Stage != StageFailed || st[1].Tries != DefaultAttempts || tries("gone.txt") != DefaultAttempts ||
		!strings.Contains(st[1].Err.Error(), "HTTP 502") {
		t.Errorf("failing file = %+v after %d tries, want failed after %d", st[1], tries("gone.txt"), DefaultAttempts)
	}
	if st[2].Stage != StageDone || tries("ok.txt") != 1 {
		t.Errorf("ok file = %+v after %d tries", st[2], tries("ok.txt"))
	}
}

func TestRunRetriesAfterQueuedFiles(t *testing.T) {
	files := []settings.File{{RelPath: "a.txt", Size: 1}, {RelPath: "b.txt", Size: 1}, {RelPath: "c.txt", Size: 1}}
	var mu sync.Mutex
	var order []string
	client, _ := flakyServer(t, func(file string, n int) int {
		mu.Lock()
		order = append(order, fmt.Sprintf("%s#%d", file, n))
		mu.Unlock()
		if file == "a.txt" && n == 1 {
			return http.StatusInternalServerError
		}
		return 0
	})
	p := NewProgress(files)
	job := Job{Client: client, ItemID: "item1", Folder: t.TempDir(), Files: files, Workers: 1, RetryDelay: -1}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	if got := strings.Join(order, " "); got != "a.txt#1 b.txt#1 c.txt#1 a.txt#2" {
		t.Errorf("requests = %s, want the failed file tried again after the queued ones", got)
	}
}

func TestRunDoesNotRetryRejectedFiles(t *testing.T) {
	files := []settings.File{{RelPath: "dup.txt", Size: 1}}
	client, tries := flakyServer(t, func(string, int) int { return http.StatusBadRequest })
	p := NewProgress(files)
	job := Job{Client: client, ItemID: "item1", Folder: t.TempDir(), Files: files, RetryDelay: -1}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if st := p.Snapshot()[0]; st.Stage != StageFailed || tries("dup.txt") != 1 {
		t.Errorf("rejected file = %+v after %d tries, want failed at once", st, tries("dup.txt"))
	}
}

func TestRetryFailedRunsFailedFilesAgain(t *testing.T) {
	files := []settings.File{{RelPath: "a.txt", Size: 1}, {RelPath: "b.txt", Size: 1}}
	var down atomic.Bool
	down.Store(true)
	client, tries := flakyServer(t, func(file string, n int) int {
		if file == "a.txt" && down.Load() {
			return http.StatusServiceUnavailable
		}
		return 0
	})
	p := NewProgress(files)
	job := Job{Client: client, ItemID: "item1", Folder: t.TempDir(), Files: files, Attempts: 2, RetryDelay: -1}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if st := p.Snapshot(); st[0].Stage != StageFailed || st[1].Stage != StageDone || p.Pending() != 0 {
		t.Fatalf("statuses = %+v", st)
	}
	down.Store(false)

	if n := p.RetryFailed(); n != 1 || p.Pending() != 1 {
		t.Fatalf("RetryFailed queued %d, pending %d; want the one failed file", n, p.Pending())
	}
	if st := p.Snapshot()[0]; st.Stage != StageQueued || st.Tries != 0 {
		t.Fatalf("retried file = %+v, want queued with fresh tries", st)
	}
	if err := job.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	if tries("a.txt") != 3 || tries("b.txt") != 1 {
		t.Errorf("tries a=%d b=%d; only the failed file should be sent again", tries("a.txt"), tries("b.txt"))
	}
}

func TestRetryFailedWhileRunning(t *testing.T) {
	files := []settings.File{{RelPath: "a.txt", Size: 1}, {RelPath: "slow.txt", Size: 1}}
	release := make(chan struct{})
	client, tries := flakyServer(t, func(file string, n int) int {
		switch {
		case file == "a.txt" && n == 1:
			return http.StatusBadRequest
		case file == "slow.txt":
			<-release // keeps the run going until a.txt has been retried
		}
		return 0
	})
	p := NewProgress(files)
	job := Job{Client: client, ItemID: "item1", Folder: t.TempDir(), Files: files, Workers: 2, RetryDelay: -1}
	done := make(chan error)
	go func() { done <- job.Run(context.Background(), p) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.Snapshot()[0].Stage != StageFailed {
		if time.Now().After(deadline) {
			t.Fatalf("a.txt never failed: %+v", p.Snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := p.RetryFailed(); n != 1 {
		t.Fatalf("RetryFailed = %d", n)
	}
	for p.Snapshot()[0].Stage != StageDone {
		if time.Now().After(deadline) {
			t.Fatalf("the running job should upload the retried file: %+v", p.Snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	checkAllDone(t, p)
	if tries("a.txt") != 2 {
		t.Errorf("a.txt sent %d times, want 2", tries("a.txt"))
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&remote.StatusError{Code: 503}, true},
		{&remote.StatusError{Code: 429}, true},
		{&remote.StatusError{Code: 400, Message: "exists"}, false},
		{&remote.StatusError{Code: 403, Storage: true}, false},
		{fmt.Errorf("wrap: %w", &remote.StatusError{Code: 500, Storage: true}), true},
		{remote.ErrUnauthorized, false},
		{fmt.Errorf("open: %w", os.ErrNotExist), false},
		{context.Canceled, false},
		{io.ErrUnexpectedEOF, true},
	} {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
