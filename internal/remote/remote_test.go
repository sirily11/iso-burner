package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type staticToken string

func (t staticToken) AccessToken(context.Context) (string, error) { return string(t), nil }

type failingToken struct{}

func (failingToken) AccessToken(context.Context) (string, error) {
	return "", errors.New("not signed in")
}

// recorder is a fake rxstorage server that records every PUT.
type recorder struct {
	mu     sync.Mutex
	paths  []string
	jobs   []Job
	auth   []string
	status int
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var job Job
	if err := json.NewDecoder(req.Body).Decode(&job); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.paths = append(r.paths, req.URL.Path)
	r.jobs = append(r.jobs, job)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	if r.status != 0 {
		w.WriteHeader(r.status)
		json.NewEncoder(w).Encode(map[string]string{"error": "Permission denied"})
		return
	}
	w.Write([]byte(`{}`))
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

func (r *recorder) last() Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[len(r.jobs)-1]
}

func TestPutJob(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := NewClient(srv.URL+"/", staticToken("tok"))
	job := Job{Kind: KindGenerate, Title: "backup", Status: StatusRunning, StartedAt: time.Now()}
	if err := c.PutJob(context.Background(), "abc", job); err != nil {
		t.Fatal(err)
	}
	if rec.paths[0] != "/api/v1/iso-jobs/abc" {
		t.Errorf("path = %q", rec.paths[0])
	}
	if rec.auth[0] != "Bearer tok" {
		t.Errorf("authorization = %q", rec.auth[0])
	}
	if rec.jobs[0].Tasks == nil {
		t.Error("tasks should be sent as an empty array, not null")
	}
}

func TestPutJobErrors(t *testing.T) {
	rec := &recorder{status: http.StatusUnauthorized}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	err := NewClient(srv.URL, staticToken("tok")).PutJob(context.Background(), "a", Job{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401: got %v, want ErrUnauthorized", err)
	}

	rec.status = http.StatusForbidden
	err = NewClient(srv.URL, staticToken("tok")).PutJob(context.Background(), "a", Job{})
	if err == nil || err.Error() != "rxstorage: Permission denied (HTTP 403)" {
		t.Errorf("403: got %v", err)
	}

	err = NewClient(srv.URL, failingToken{}).PutJob(context.Background(), "a", Job{})
	if err == nil {
		t.Error("expected an error without a token")
	}
}

func TestReporterSendsLatestAndFlushesOnClose(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	r := NewReporter(NewClient(srv.URL, staticToken("tok")), "job-1", time.Hour)
	r.Report(Job{Title: "first", Status: StatusRunning})
	waitFor(t, func() bool { return rec.count() == 1 })

	// Reports during the interval are coalesced; only the latest is sent.
	r.Report(Job{Title: "second", Status: StatusRunning})
	r.Report(Job{Title: "third", Status: StatusCompleted})
	if err := r.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 {
		t.Fatalf("sent %d snapshots, want 2", rec.count())
	}
	if got := rec.last(); got.Title != "third" || got.Status != StatusCompleted {
		t.Errorf("last snapshot = %+v", got)
	}
	if sent, err := r.Status(); sent.IsZero() || err != nil {
		t.Errorf("status = %v, %v", sent, err)
	}

	// Reports after Close are ignored.
	r.Report(Job{Title: "late"})
	r.Close(time.Second)
	if rec.count() != 2 {
		t.Errorf("report after close was sent")
	}
}

func TestReporterRetriesFailedDelivery(t *testing.T) {
	rec := &recorder{status: http.StatusInternalServerError}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	r := NewReporter(NewClient(srv.URL, staticToken("tok")), "job-1", 10*time.Millisecond)
	defer r.Close(time.Second)
	r.Report(Job{Title: "only"})
	waitFor(t, func() bool { _, err := r.Status(); return err != nil })

	rec.mu.Lock()
	rec.status = 0
	rec.mu.Unlock()
	waitFor(t, func() bool { _, err := r.Status(); return err == nil })
	if got := rec.last(); got.Title != "only" {
		t.Errorf("retried snapshot = %+v", got)
	}
}

func TestNilReporter(t *testing.T) {
	var r *Reporter
	r.Report(Job{})
	if err := r.Close(time.Second); err != nil {
		t.Error(err)
	}
	if r.ID() != "" {
		t.Error("nil reporter has an ID")
	}
}

func TestJobIDs(t *testing.T) {
	if NewJobID() == NewJobID() {
		t.Error("random job IDs repeat")
	}
	a, b := StableJobID("host", "/db", "3"), StableJobID("host", "/db", "3")
	if a != b || len(a) != 36 {
		t.Errorf("stable IDs %q and %q", a, b)
	}
	if a == StableJobID("host", "/db", "4") {
		t.Error("different sessions share an ID")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
