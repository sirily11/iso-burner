// Package remote syncs ISO generation and burning progress to the rxstorage
// server, so it can be followed from the rxstorage web and iOS apps.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind says what a job does.
type Kind string

const (
	KindGenerate Kind = "generate"
	KindBurn     Kind = "burn"
)

// Status is the lifecycle state of a job.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusStopped   Status = "stopped" // burning was stopped and can be resumed
)

// Section groups a job's progress rows.
type Section string

const (
	SectionISO   Section = "iso"
	SectionDrive Section = "drive"
)

// Task is one progress row: an ISO file or a disc drive.
type Task struct {
	Section    Section `json:"section"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Detail     string  `json:"detail,omitempty"`
	Progress   float64 `json:"progress"`
	DoneBytes  int64   `json:"doneBytes"`
	TotalBytes int64   `json:"totalBytes"`
	Error      string  `json:"error,omitempty"`
}

// Job is a full snapshot of a job; each report replaces the previous one.
type Job struct {
	Kind       Kind       `json:"kind"`
	Title      string     `json:"title"`
	Status     Status     `json:"status"`
	HostName   string     `json:"hostName,omitempty"`
	Progress   float64    `json:"progress"`
	DoneCount  int        `json:"doneCount"`
	TotalCount int        `json:"totalCount"`
	DoneBytes  int64      `json:"doneBytes"`
	TotalBytes int64      `json:"totalBytes"`
	Message    string     `json:"message,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Tasks      []Task     `json:"tasks"`
}

// TokenSource provides the Bearer token for requests, e.g. the signed-in
// RxAuth account.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// ErrUnauthorized means the server rejected the token; signing in again
// fixes it.
var ErrUnauthorized = errors.New("rxstorage rejected the sign-in; sign in again")

// Client talks to the rxstorage ISO jobs API.
type Client struct {
	BaseURL string
	Tokens  TokenSource
	HTTP    *http.Client
}

// NewClient returns a client for the rxstorage server at baseURL.
func NewClient(baseURL string, tokens TokenSource) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Tokens:  tokens,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// PutJob creates or replaces job id on the server.
func (c *Client) PutJob(ctx context.Context, id string, job Job) error {
	if job.Tasks == nil {
		job.Tasks = []Task{}
	}
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, "/api/v1/iso-jobs/"+url.PathEscape(id), body, "job", id)
	return err
}

// Item is an rxstorage item that content can be uploaded to.
type Item struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Category *struct {
		Name string `json:"name"`
	} `json:"category"`
	Location *struct {
		Title string `json:"title"`
	} `json:"location"`
}

// SearchItems returns up to limit of the signed-in user's items whose title
// matches query; an empty query lists the most recent items.
func (c *Client) SearchItems(ctx context.Context, query string, limit int) ([]Item, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if query = strings.TrimSpace(query); query != "" {
		q.Set("search", query)
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v1/items?"+q.Encode(), nil, "search", query)
	if err != nil {
		return nil, err
	}
	var page struct {
		Data []Item `json:"data"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("rxstorage: reading items: %w", err)
	}
	return page.Data, nil
}

// do sends an authorized request and returns the body of a 2xx response.
// logArgs are added to the log line of a failed request.
func (c *Client) do(ctx context.Context, method, path string, body []byte, logArgs ...any) ([]byte, error) {
	token, err := c.Tokens.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("sign-in: %w", err)
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		slog.Error("rxstorage request failed", append([]any{"method", req.Method, "url", req.URL.String()},
			append(logArgs, "status", resp.StatusCode, "body", string(data[:min(len(data), 4096)]))...)...)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("rxstorage: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return nil, fmt.Errorf("rxstorage: HTTP %d", resp.StatusCode)
	}
	return data, err
}

// NewJobID returns a random ID for a job that is never resumed.
func NewJobID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // UUID version 4
	b[8] = b[8]&0x3f | 0x80
	return formatUUID(b[:])
}

// StableJobID derives the same ID from the same parts, so a resumed burn
// session keeps updating its job.
func StableJobID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x50 // UUID version 5 style
	b[8] = b[8]&0x3f | 0x80
	return formatUUID(b)
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
