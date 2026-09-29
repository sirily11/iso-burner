package burn

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sirily11/iso-burner/internal/drive"
)

// VerificationSnapshot describes a read-only comparison of a disc with an ISO.
type VerificationSnapshot struct {
	Checked, Total int64
	Running        bool
	Opening        bool
	Elapsed        time.Duration
	Err            error
}

// Verification reads an existing disc without burning, ejecting, or changing
// burn history. A match requires every byte of the ISO to be read and compared.
type Verification struct {
	mu      sync.Mutex
	status  VerificationSnapshot
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
}

func StartVerification(burner Burner, d drive.Drive, isoPath string) (*Verification, error) {
	info, err := os.Stat(isoPath)
	if err != nil {
		return nil, fmt.Errorf("open ISO: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, fmt.Errorf("choose a non-empty ISO file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	v := &Verification{
		status:  VerificationSnapshot{Total: info.Size(), Running: true, Opening: true},
		started: time.Now(), cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer close(v.done)
		defer cancel()
		err := v.compare(ctx, burner, d, isoPath)
		v.mu.Lock()
		v.status.Running, v.status.Opening = false, false
		v.status.Err, v.status.Elapsed = err, time.Since(v.started)
		v.mu.Unlock()
	}()
	return v, nil
}

func (v *Verification) compare(ctx context.Context, burner Burner, d drive.Drive, path string) error {
	r, err := burner.OpenDisc(ctx, d)
	if err != nil {
		return fmt.Errorf("read disc: %w", err)
	}
	defer r.Close()
	// Closing the raw reader interrupts a blocked read when cancelling.
	stopClose := context.AfterFunc(ctx, func() { r.Close() })
	defer stopClose()
	v.mu.Lock()
	v.status.Opening = false
	v.mu.Unlock()
	err = Verify(ctx, path, r, func(n int64) {
		v.mu.Lock()
		v.status.Checked = n
		v.mu.Unlock()
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	// Do not report a complete match if the source changed size during reading.
	v.mu.Lock()
	complete := v.status.Checked == v.status.Total
	v.mu.Unlock()
	if !complete {
		return fmt.Errorf("the ISO changed size during verification; check it again")
	}
	return nil
}

func (v *Verification) Snapshot() VerificationSnapshot {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.status
	if s.Running {
		s.Elapsed = time.Since(v.started)
	}
	return s
}

// Cancel stops reading; Done closes after the reader has been released.
func (v *Verification) Cancel()               { v.cancel() }
func (v *Verification) Done() <-chan struct{} { return v.done }
