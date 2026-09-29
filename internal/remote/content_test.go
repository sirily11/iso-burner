package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestPreviewUploads(t *testing.T) {
	var got map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/upload/content-preview" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`[{"id":"f1","imageUrl":"https://s3/img","videoUrl":"https://s3/vid"}]`))
	}))
	defer srv.Close()

	length := 12.5
	ups, err := NewClient(srv.URL, staticToken("tok")).RequestPreviewUploads(context.Background(), "item1", []PreviewRequest{{
		Filename: "clip.mov", Type: "video", Title: "clip.mov", MimeType: "video/quicktime",
		Size: 42, FilePath: "trip/clip.mov", VideoLength: &length,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].ID != "f1" || ups[0].ImageURL != "https://s3/img" || ups[0].VideoURL != "https://s3/vid" {
		t.Errorf("uploads = %+v", ups)
	}
	if gotAuth != "Bearer tok" || got["item_id"] != "item1" || got["overwrite"] != true {
		t.Errorf("auth = %q, body = %v", gotAuth, got)
	}
	item := got["items"].([]any)[0].(map[string]any)
	if item["file_path"] != "trip/clip.mov" || item["mime_type"] != "video/quicktime" ||
		item["video_length"] != 12.5 || item["size"] != 42.0 || item["type"] != "video" {
		t.Errorf("item = %v", item)
	}
}

func TestRequestPreviewUploadsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"Content with the same name already exists: a.jpg"}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, staticToken("tok")).RequestPreviewUploads(context.Background(), "item1",
		[]PreviewRequest{{Filename: "a.jpg", Type: "image", Title: "a.jpg", MimeType: "image/jpeg", FilePath: "a.jpg"}})
	if err == nil || !strings.Contains(err.Error(), "already exists: a.jpg") {
		t.Fatalf("err = %v, want the server's message", err)
	}
}

func TestCreateFileContent(t *testing.T) {
	var gotPath string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	err := NewClient(srv.URL, staticToken("tok")).CreateFileContent(context.Background(), "item 1",
		FileContent{Title: "notes.txt", MimeType: "text/plain", Size: 5, FilePath: "docs/notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /api/v1/items/item 1/contents" || got["type"] != "file" || got["overwrite"] != true {
		t.Errorf("request = %s %v", gotPath, got)
	}
	data := got["data"].(map[string]any)
	if data["title"] != "notes.txt" || data["file_path"] != "docs/notes.txt" || data["size"] != 5.0 {
		t.Errorf("data = %v", data)
	}
}

func TestPutFile(t *testing.T) {
	var gotType, gotAuth string
	var gotBody []byte
	var gotLength int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Query().Get("X-Amz-Signature") != "sig" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		gotType, gotAuth, gotLength = r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "thumb.jpg")
	want := []byte(strings.Repeat("x", 100_000))
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	var last, total int64
	c := NewClient("http://unused", staticToken("tok"))
	if err := c.PutFile(context.Background(), srv.URL+"/previews/a?X-Amz-Signature=sig", path, "image/jpeg", func(sent, size int64) {
		last, total = sent, size
	}); err != nil {
		t.Fatal(err)
	}
	if gotType != "image/jpeg" || gotAuth != "" || gotLength != int64(len(want)) || string(gotBody) != string(want) {
		t.Errorf("type = %q, auth = %q, length = %d, body = %d bytes", gotType, gotAuth, gotLength, len(gotBody))
	}
	if last != int64(len(want)) || total != int64(len(want)) {
		t.Errorf("progress ended at %d of %d", last, total)
	}

	err := c.PutFile(context.Background(), srv.URL+"/previews/a?X-Amz-Signature=bad", path, "image/jpeg", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v, want HTTP 403 without the signature", err)
	}
}

func TestCountItemContents(t *testing.T) {
	var gotPath, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotLimit = r.URL.Path, r.URL.Query().Get("limit")
		w.Write([]byte(`{"data":[{"id":"c1"}],"pagination":{"totalCount":12,"hasNextPage":true}}`))
	}))
	defer srv.Close()

	n, err := NewClient(srv.URL, staticToken("tok")).CountItemContents(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 12 || gotPath != "/api/v1/items/i1/contents" || gotLimit != "1" {
		t.Errorf("count = %d, path = %q, limit = %q", n, gotPath, gotLimit)
	}
}

func TestListContentTitles(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/items/i1/contents" || r.URL.Query().Get("limit") != "100" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		if cursor == "" {
			w.Write([]byte(`{"data":[{"id":"c1","data":{"title":"a.jpg"}},{"id":"c2","data":{"title":"b.mp4"}}],
				"pagination":{"nextCursor":"p2","hasNextPage":true,"totalCount":3}}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"c3","data":{"title":"c.pdf"}}],"pagination":{"nextCursor":null,"hasNextPage":false,"totalCount":3}}`))
	}))
	defer srv.Close()

	titles, err := NewClient(srv.URL, staticToken("tok")).ListContentTitles(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(titles, ",") != "a.jpg,b.mp4,c.pdf" || strings.Join(cursors, ",") != ",p2" {
		t.Errorf("titles = %v, cursors = %q", titles, cursors)
	}
}
