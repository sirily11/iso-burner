package burn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/store"
)

// DriveStatus is a point-in-time view of one drive in a session.
type DriveStatus struct {
	Drive drive.Drive
	State store.DriveState
	// Disc is the disc being burned or waited for; nil when idle or finished.
	Disc *store.Disc
	// Progress and Total are the bytes of the current phase.
	Progress, Total int64
	// Last is the disc this drive finished most recently, if any.
	Last *store.Disc
	// Err is why the last attempt failed, cleared once a burn succeeds.
	Err string
	// Completed counts the discs this drive finished in this run.
	Completed int
}

// Snapshot is a point-in-time view of a whole session.
type Snapshot struct {
	Drives []DriveStatus
	// Total and Done count every disc in the session, including those
	// finished before it was resumed.
	Total, Done int
	// Running is false once every drive has stopped.
	Running bool
	// Err is a fatal error, such as the database failing.
	Err error
}

// Config sets up an Engine.
type Config struct {
	Store   *store.Store
	Session int64
	Drives  []drive.Drive
	Burner  Burner
	// DiscsLoaded says blank discs are already in the drives, so the first
	// burn starts without asking for a disc.
	DiscsLoaded bool
	// SaveInterval throttles how often progress is written to the database.
	SaveInterval time.Duration
	// OpenRetries and OpenDelay control how long to wait for a freshly
	// burned disc to become readable.
	OpenRetries int
	OpenDelay   time.Duration
}

// Engine burns a session's discs with several drives in parallel. Every
// drive takes the next disc from the queue, burns it, reads it back to check
// it, ejects it and then waits for the user to insert a new blank disc.
type Engine struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	insert []chan struct{}
	wg     sync.WaitGroup
	done   chan struct{}

	mu              sync.Mutex
	drives          []DriveStatus
	total, finished int
	err             error
}

// Start begins burning in the background.
func Start(cfg Config) (*Engine, error) {
	if cfg.SaveInterval == 0 {
		cfg.SaveInterval = time.Second
	}
	if cfg.OpenRetries == 0 {
		cfg.OpenRetries, cfg.OpenDelay = 15, 2*time.Second
	}
	sess, err := cfg.Store.Session(context.Background(), cfg.Session)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{cfg: cfg, ctx: ctx, cancel: cancel, done: make(chan struct{}), total: sess.Total, finished: sess.Done}
	for _, d := range cfg.Drives {
		e.drives = append(e.drives, DriveStatus{Drive: d, State: store.DriveIdle})
		e.insert = append(e.insert, make(chan struct{}, 1))
	}
	for i := range cfg.Drives {
		e.wg.Add(1)
		go e.run(i)
	}
	go func() {
		e.wg.Wait()
		e.finish()
		close(e.done)
	}()
	return e, nil
}

// Insert tells a waiting drive that a blank disc is in. It reports false when
// the drive is not waiting for one.
func (e *Engine) Insert(driveID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, d := range e.drives {
		if d.Drive.ID == driveID && d.State == store.DriveWaiting {
			select {
			case e.insert[i] <- struct{}{}:
			default:
			}
			return true
		}
	}
	return false
}

// Stop cancels every burn in progress and waits for the drives to stop.
// Unfinished discs go back in the queue so the session can be resumed.
func (e *Engine) Stop() {
	e.cancel()
	<-e.done
}

// Done is closed once every drive has stopped.
func (e *Engine) Done() <-chan struct{} { return e.done }

// Snapshot returns the current state of every drive.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Total: e.total, Done: e.finished, Err: e.err, Drives: make([]DriveStatus, len(e.drives))}
	select {
	case <-e.done:
	default:
		s.Running = true
	}
	for i, d := range e.drives {
		if d.Disc != nil {
			c := *d.Disc
			d.Disc = &c
		}
		if d.Last != nil {
			c := *d.Last
			d.Last = &c
		}
		s.Drives[i] = d
	}
	return s
}

func (e *Engine) update(i int, fn func(*DriveStatus)) {
	e.mu.Lock()
	fn(&e.drives[i])
	e.mu.Unlock()
}

// fail records a fatal error and stops every drive.
func (e *Engine) fail(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
	e.cancel()
}

// finish marks the session done when every disc is burned.
func (e *Engine) finish() {
	e.mu.Lock()
	complete := e.err == nil && e.finished >= e.total
	e.mu.Unlock()
	if complete {
		if err := e.cfg.Store.SetSessionStatus(context.Background(), e.cfg.Session, store.SessionDone); err != nil {
			e.mu.Lock()
			e.err = err
			e.mu.Unlock()
		}
	}
}

// setState moves drive i to state and records it in the database.
func (e *Engine) setState(i int, state store.DriveState, disc *store.Disc, message string) error {
	var id int64
	if disc != nil {
		id = disc.ID
	}
	e.update(i, func(s *DriveStatus) {
		s.State, s.Progress, s.Total = state, 0, 0
		s.Disc = nil
		if disc != nil {
			c := *disc // the worker keeps changing its own copy
			s.Disc = &c
		}
	})
	// The drive's state must be saved even while stopping.
	return e.cfg.Store.SetDrive(context.Background(), e.cfg.Session, e.cfg.Drives[i].ID, state, id, message)
}

// run is one drive's loop.
func (e *Engine) run(i int) {
	defer e.wg.Done()
	d := e.cfg.Drives[i]
	st := e.cfg.Store
	bg := context.Background()
	loaded := e.cfg.DiscsLoaded
	for {
		if e.ctx.Err() != nil {
			e.setState(i, store.DriveIdle, nil, "stopped")
			return
		}
		disc, err := st.Claim(bg, e.cfg.Session, d.ID)
		if err != nil {
			e.fail(fmt.Errorf("claim disc: %w", err))
			return
		}
		if disc == nil {
			if err := e.setState(i, store.DriveFinished, nil, ""); err != nil {
				e.fail(err)
			}
			return
		}
		if !loaded {
			if err := e.setState(i, store.DriveWaiting, disc, "insert a blank disc"); err != nil {
				e.fail(err)
				return
			}
			select {
			case <-e.insert[i]:
			case <-e.ctx.Done():
				st.ReleaseDisc(bg, disc.ID, "")
				e.setState(i, store.DriveIdle, nil, "stopped")
				return
			}
		}
		loaded = false
		if err := e.burnDisc(i, disc); err != nil {
			if e.ctx.Err() != nil {
				st.ReleaseDisc(bg, disc.ID, "stopped while burning")
				e.setState(i, store.DriveIdle, nil, "stopped")
				return
			}
			var fatal *fatalError
			if errors.As(err, &fatal) {
				e.fail(fatal.err)
				return
			}
			// The disc is unusable; keep the job on this drive and ask
			// for a new blank disc.
			if err := st.FailDisc(bg, disc.ID, err); err != nil {
				e.fail(err)
				return
			}
			e.update(i, func(s *DriveStatus) { s.Err = err.Error() })
			e.cfg.Burner.Eject(bg, d)
		}
	}
}

// fatalError stops the whole session rather than retrying the disc.
type fatalError struct{ err error }

func (f *fatalError) Error() string { return f.err.Error() }

// burnDisc burns, verifies and ejects one disc. A returned error means the
// disc must be burned again, unless it is a *fatalError.
func (e *Engine) burnDisc(i int, disc *store.Disc) error {
	d := e.cfg.Drives[i]
	st := e.cfg.Store
	bg := context.Background()

	if err := st.StartPhase(bg, disc.ID, store.DiscBurning, disc.ISOSize); err != nil {
		return &fatalError{err}
	}
	disc.Status, disc.Attempts = store.DiscBurning, disc.Attempts+1
	if err := e.setState(i, store.DriveBurning, disc, ""); err != nil {
		return &fatalError{err}
	}
	e.update(i, func(s *DriveStatus) { s.Total = disc.ISOSize })
	if err := e.cfg.Burner.Burn(e.ctx, d, disc.ISOPath, disc.ISOSize, e.progress(i, disc.ID)); err != nil {
		return fmt.Errorf("burn: %w", err)
	}

	if err := st.StartPhase(bg, disc.ID, store.DiscVerifying, disc.ISOSize); err != nil {
		return &fatalError{err}
	}
	disc.Status = store.DiscVerifying
	if err := e.setState(i, store.DriveVerifying, disc, ""); err != nil {
		return &fatalError{err}
	}
	e.update(i, func(s *DriveStatus) { s.Total = disc.ISOSize })
	note := ""
	r, err := e.openDisc(d)
	if err != nil {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		// The burn itself succeeded; say the check was skipped rather
		// than throwing the disc away.
		note = "not verified: " + err.Error()
	} else {
		err = Verify(e.ctx, disc.ISOPath, r, e.progress(i, disc.ID))
		r.Close()
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}
	}

	if err := st.FinishDisc(bg, disc.ID, note); err != nil {
		return &fatalError{err}
	}
	finished, err := st.Disc(bg, disc.ID)
	if err != nil {
		return &fatalError{err}
	}
	e.mu.Lock()
	e.finished++
	e.drives[i].Completed++
	e.drives[i].Last = &finished
	e.drives[i].Err = ""
	e.mu.Unlock()
	e.cfg.Burner.Eject(bg, d)
	return nil
}

// openDisc opens the burned disc, retrying while the drive reloads it.
func (e *Engine) openDisc(d drive.Drive) (io.ReadCloser, error) {
	var err error
	for attempt := range e.cfg.OpenRetries {
		if attempt > 0 {
			select {
			case <-time.After(e.cfg.OpenDelay):
			case <-e.ctx.Done():
				return nil, e.ctx.Err()
			}
		}
		var r io.ReadCloser
		if r, err = e.cfg.Burner.OpenDisc(e.ctx, d); err == nil {
			return r, nil
		}
	}
	return nil, err
}

// progress returns a callback that records bytes done for drive i, saving
// them to the database at most every SaveInterval.
func (e *Engine) progress(i int, disc int64) func(int64) {
	var saved time.Time
	return func(n int64) {
		e.update(i, func(s *DriveStatus) {
			s.Progress = min(n, s.Total)
			if s.Disc != nil {
				s.Disc.Progress = s.Progress
			}
		})
		if time.Since(saved) >= e.cfg.SaveInterval {
			saved = time.Now()
			e.cfg.Store.SetProgress(context.Background(), disc, n)
		}
	}
}
