package remote

import (
	"context"
	"sync"
	"time"
)

// DefaultInterval is how often a Reporter sends progress at most.
const DefaultInterval = 3 * time.Second

// Reporter sends a job's latest snapshot to the server in the background, at
// most once per interval, so a slow or unreachable server never holds up
// generating or burning. A nil *Reporter ignores everything.
type Reporter struct {
	client   *Client
	id       string
	interval time.Duration

	mu       sync.Mutex
	latest   *Job
	pending  bool
	lastSent time.Time
	lastErr  error
	closed   bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// NewReporter starts reporting job id through client.
func NewReporter(client *Client, id string, interval time.Duration) *Reporter {
	if interval <= 0 {
		interval = DefaultInterval
	}
	r := &Reporter{
		client: client, id: id, interval: interval,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go r.loop()
	return r
}

// ID is the server-side job ID.
func (r *Reporter) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

// Report queues job as the latest snapshot. It never blocks on the network.
func (r *Reporter) Report(job Job) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.latest, r.pending = &job, true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Status says when progress was last delivered and why the latest attempt
// failed, if it did.
func (r *Reporter) Status() (lastSent time.Time, err error) {
	if r == nil {
		return time.Time{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSent, r.lastErr
}

// Close stops reporting after trying, for up to timeout, to deliver the
// latest snapshot. It is safe to call more than once.
func (r *Reporter) Close(timeout time.Duration) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	already := r.closed
	r.closed = true
	r.mu.Unlock()
	if !already {
		close(r.stop)
	}
	<-r.done
	if r.hasPending() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		r.send(ctx)
	}
	_, err := r.Status()
	return err
}

func (r *Reporter) hasPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending
}

func (r *Reporter) loop() {
	defer close(r.done)
	for {
		// A failed delivery stays pending and is retried after the interval.
		if !r.hasPending() {
			select {
			case <-r.wake:
			case <-r.stop:
				return
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			// Abandon a slow request when closing; Close retries it.
			select {
			case <-r.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		r.send(ctx)
		cancel()
		select {
		case <-time.After(r.interval):
		case <-r.stop:
			return
		}
	}
}

// send delivers the latest snapshot, keeping it pending if delivery fails.
func (r *Reporter) send(ctx context.Context) {
	r.mu.Lock()
	job := r.latest
	if !r.pending || job == nil {
		r.mu.Unlock()
		return
	}
	r.pending = false
	r.mu.Unlock()
	err := r.client.PutJob(ctx, r.id, *job)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErr = err
		if r.latest == job {
			r.pending = true
		}
		return
	}
	r.lastErr, r.lastSent = nil, time.Now()
}
