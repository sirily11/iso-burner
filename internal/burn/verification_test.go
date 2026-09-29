package burn

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirily11/iso-burner/internal/drive"
)

func TestVerificationChecksExistingDiscWithoutWriting(t *testing.T) {
	path, data := writeISO(t, verifyChunk+2048)
	for _, kind := range []string{"match", "mismatch", "incomplete", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			fake := newFake()
			fake.discs["1"] = append(append([]byte(nil), data...), make([]byte, 2048)...)
			var burner Burner = fake
			switch kind {
			case "mismatch":
				fake.discs["1"][verifyChunk+5] ^= 0xff
			case "incomplete":
				fake.discs["1"] = fake.discs["1"][:verifyChunk]
			case "unreadable":
				burner = unreadable{fake}
			}
			v, err := StartVerification(burner, testDrives[0], path)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-v.Done():
			case <-time.After(5 * time.Second):
				v.Cancel()
				t.Fatal("verification did not finish")
			}
			s := v.Snapshot()
			if s.Running || s.Total != int64(len(data)) || fake.burns != 0 || fake.ejects != 0 {
				t.Fatalf("verification = %+v; burns=%d ejects=%d", s, fake.burns, fake.ejects)
			}
			switch kind {
			case "match":
				if s.Err != nil || s.Checked != s.Total {
					t.Fatalf("match = %+v", s)
				}
			case "mismatch":
				var mm *MismatchError
				if !errors.As(s.Err, &mm) || mm.Offset != verifyChunk+5 {
					t.Fatalf("mismatch = %+v", s)
				}
			case "incomplete":
				if s.Err == nil || !strings.Contains(s.Err.Error(), "read disc") || s.Checked >= s.Total {
					t.Fatalf("incomplete = %+v", s)
				}
			case "unreadable":
				if !errors.Is(s.Err, os.ErrPermission) || s.Checked != 0 {
					t.Fatalf("unreadable = %+v", s)
				}
			}
		})
	}
}

type blockedDisc struct {
	reading chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *blockedDisc) Read([]byte) (int, error) {
	close(r.reading)
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *blockedDisc) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type blockedReaderBurner struct {
	*fakeBurner
	r *blockedDisc
}

func (f blockedReaderBurner) OpenDisc(context.Context, drive.Drive) (io.ReadCloser, error) {
	return f.r, nil
}

func TestVerificationCancelClosesReader(t *testing.T) {
	path, _ := writeISO(t, 2048)
	r := &blockedDisc{reading: make(chan struct{}), closed: make(chan struct{})}
	v, err := StartVerification(blockedReaderBurner{newFake(), r}, testDrives[0], path)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Cancel()
	select {
	case <-r.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("disc read did not start")
	}
	v.Cancel()
	select {
	case <-v.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not release the disc")
	}
	if s := v.Snapshot(); s.Running || !errors.Is(s.Err, context.Canceled) || s.Checked != 0 {
		t.Fatalf("cancelled verification = %+v", s)
	}
}

func TestVerificationRejectsEmptyISO(t *testing.T) {
	path, _ := writeISO(t, 0)
	if _, err := StartVerification(newFake(), testDrives[0], path); err == nil {
		t.Fatal("empty ISO must not report a complete disc")
	}
}
