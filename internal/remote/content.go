package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// PreviewRequest describes an image or video whose previews are uploaded to
// an item. rxstorage creates the item content and returns where to put the
// previews.
type PreviewRequest struct {
	Filename string `json:"filename"`
	// Type is "image" or "video".
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	// FilePath is where the original file is kept.
	FilePath string `json:"file_path"`
	// VideoLength is the length of a video in seconds.
	VideoLength *float64 `json:"video_length,omitempty"`
}

// PreviewUpload holds the presigned URLs the previews of one PreviewRequest
// are PUT to.
type PreviewUpload struct {
	ID       string `json:"id"`
	ImageURL string `json:"imageUrl"`
	// VideoURL is set for videos only.
	VideoURL string `json:"videoUrl,omitempty"`
}

// RequestPreviewUploads creates content on item itemID for each request and
// returns, in the same order, the URLs its thumbnail and preview video are
// uploaded to. Content the item already has with the same title is replaced,
// so a file whose earlier upload failed part way can be tried again.
func (c *Client) RequestPreviewUploads(ctx context.Context, itemID string, reqs []PreviewRequest) ([]PreviewUpload, error) {
	body, err := json.Marshal(struct {
		ItemID    string           `json:"item_id"`
		Items     []PreviewRequest `json:"items"`
		Overwrite bool             `json:"overwrite"`
	}{itemID, reqs, true})
	if err != nil {
		return nil, err
	}
	data, err := c.do(ctx, http.MethodPost, "/api/v1/upload/content-preview", body, "item", itemID)
	if err != nil {
		return nil, err
	}
	var ups []PreviewUpload
	if err := json.Unmarshal(data, &ups); err != nil {
		return nil, fmt.Errorf("rxstorage: reading upload URLs: %w", err)
	}
	if len(ups) != len(reqs) {
		return nil, fmt.Errorf("rxstorage: got %d upload URLs for %d files", len(ups), len(reqs))
	}
	return ups, nil
}

// FileContent describes a file without previews, such as a document.
type FileContent struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	// FilePath is where the original file is kept.
	FilePath string `json:"file_path"`
}

// CreateFileContent adds a file content to item itemID, replacing any
// content it already has with the same title.
func (c *Client) CreateFileContent(ctx context.Context, itemID string, data FileContent) error {
	body, err := json.Marshal(struct {
		Type      string      `json:"type"`
		Data      FileContent `json:"data"`
		Overwrite bool        `json:"overwrite"`
	}{"file", data, true})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "/api/v1/items/"+url.PathEscape(itemID)+"/contents", body, "item", itemID)
	return err
}

// PutFile uploads the file at path to a presigned URL. contentType must be
// the one the URL was signed for. progress, when set, is called with the
// bytes sent so far. Uploads have no overall timeout, since preview videos
// can be large; cancel ctx to stop one.
func (c *Client) PutFile(ctx context.Context, putURL, path, contentType string, progress func(sent, total int64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var body io.Reader = f
	if progress != nil {
		body = &countingReader{r: f, total: info.Size(), progress: progress}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, body)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", contentType)
	client := c.Uploads
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload %s: %w", redactURL(putURL), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		slog.Error("rxstorage upload failed", "url", redactURL(putURL), "status", resp.StatusCode, "body", string(data))
		return &StatusError{Code: resp.StatusCode, Storage: true}
	}
	return nil
}

// redactURL drops the query of a presigned URL, which holds its signature.
func redactURL(u string) string {
	base, _, _ := strings.Cut(u, "?")
	return base
}

type countingReader struct {
	r        io.Reader
	sent     int64
	total    int64
	progress func(sent, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.sent += int64(n)
		c.progress(c.sent, c.total)
	}
	return n, err
}
