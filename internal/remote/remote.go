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
	"net/http"
	"net/url"
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
	token, err := c.Tokens.AccessToken(ctx)
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	if job.Tasks == nil {
		job.Tasks = []Task{}
	}
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.BaseURL+"/api/v1/iso-jobs/"+url.PathEscape(id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("rxstorage: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("rxstorage: HTTP %d", resp.StatusCode)
	}
	return nil
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
