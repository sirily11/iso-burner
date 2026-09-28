package iso

import (
	"context"
	"io"
	"sync"
)

// Stage is the lifecycle state of one ISO during generation.
type Stage int

const (
	StageQueued Stage = iota
	StageCopying
	StageFinalizing
	StageDone
	StageFailed
)

func (s Stage) String() string {
	switch s {
	case StageCopying:
		return "copying"
	case StageFinalizing:
		return "finalizing"
	case StageDone:
		return "done"
	case StageFailed:
		return "failed"
	default:
		return "queued"
	}
}

// ChunkStatus is a point-in-time view of one ISO's progress.
type ChunkStatus struct {
	Stage  Stage
	Copied int64
	Err    error
}

// Progress records per-chunk generation progress. It is safe for concurrent
// use; a nil *Progress ignores all updates.
type Progress struct {
	mu     sync.Mutex
	chunks []ChunkStatus
}

// NewProgress tracks n chunks, all initially queued.
func NewProgress(n int) *Progress {
	return &Progress{chunks: make([]ChunkStatus, n)}
}

// Snapshot returns a copy of every chunk's current status.
func (p *Progress) Snapshot() []ChunkStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ChunkStatus(nil), p.chunks...)
}

func (p *Progress) update(i int, fn func(*ChunkStatus)) {
	if p == nil || i < 0 || i >= len(p.chunks) {
		return
	}
	p.mu.Lock()
	fn(&p.chunks[i])
	p.mu.Unlock()
}

func (p *Progress) setStage(i int, s Stage) {
	p.update(i, func(c *ChunkStatus) { c.Stage = s })
}

func (p *Progress) addCopied(i int, n int64) {
	p.update(i, func(c *ChunkStatus) { c.Copied += n })
}

func (p *Progress) fail(i int, err error) {
	p.update(i, func(c *ChunkStatus) { c.Stage, c.Err = StageFailed, err })
}

// progressReader reports bytes read to a chunk's progress and stops reading
// once ctx is cancelled.
type progressReader struct {
	ctx      context.Context
	r        io.Reader
	progress *Progress
	index    int
}

func (r *progressReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(b)
	r.progress.addCopied(r.index, int64(n))
	return n, err
}
