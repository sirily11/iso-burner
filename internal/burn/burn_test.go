package burn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirily11/iso-burner/internal/drive"
	"github.com/sirily11/iso-burner/internal/store"
)

func writeISO(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	path := filepath.Join(t.TempDir(), "backup_1.iso")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func TestVerify(t *testing.T) {
	path, data := writeISO(t, 3*verifyChunk+4096)
	var checked int64
	// Discs may hold padding after the image.
	disc := append(append([]byte(nil), data...), make([]byte, 8192)...)
	if err := Verify(context.Background(), path, bytes.NewReader(disc), func(n int64) { checked = n }); err != nil {
		t.Fatal(err)
	}
	if checked != int64(len(data)) {
		t.Fatalf("checked %d of %d bytes", checked, len(data))
	}

	disc[verifyChunk+5] ^= 0xff
	var mm *MismatchError
	if err := Verify(context.Background(), path, bytes.NewReader(disc), nil); !errors.As(err, &mm) || mm.Offset != verifyChunk+5 {
		t.Fatalf("want mismatch at %d, got %v", verifyChunk+5, err)
	}

	if err := Verify(context.Background(), path, bytes.NewReader(data[:100]), nil); err == nil || !strings.Contains(err.Error(), "read disc") {
		t.Fatalf("short disc: %v", err)
	}
}

func TestParsers(t *testing.T) {
	if pct, ok := parsePuppetPercent("PERCENT:12.500000"); !ok || pct != 12.5 {
		t.Errorf("puppet = %v %v", pct, ok)
	}
	if _, ok := parsePuppetPercent("PERCENT:-1.000000"); ok {
		t.Error("indeterminate percent should be ignored")
	}
	if n, ok := parseGrowisofs("  52068352/4700372992 ( 1.1%) @2.2x, remaining 5:27 RBU 100.0% UBU  99.8%"); !ok || n != 52068352 {
		t.Errorf("growisofs = %v %v", n, ok)
	}
	if n, ok := parseWindowsProgress("PROGRESS 8388608"); !ok || n != 8388608 {
		t.Errorf("windows = %v %v", n, ok)
	}
	status := " Vendor   Product           Rev\n PIONEER  BD-RW   BDR-XD07  1.00\n\n           Type: BD-R                 Name: /dev/disk4\n"
	if node, ok := parseDrutilDevNode(status); !ok || node != "/dev/disk4" {
		t.Errorf("drutil status = %q %v", node, ok)
	}
	if _, ok := parseDrutilDevNode("Type: No Media Inserted"); ok {
		t.Error("no media should have no device node")
	}
	list := "IOService:/AppleARMPE/arm-io/USB/BDR@1/IOBDServices\nIOService:/AppleARMPE/arm-io/USB/BW@2/IOBDServices\n"
	if got := parseHdiutilDevices(list); len(got) != 2 || !strings.HasSuffix(got[1], "BW@2/IOBDServices") {
		t.Errorf("hdiutil devices = %q", got)
	}
}

// fakeBurner burns into memory, one "disc" per drive.
type fakeBurner struct {
	mu      sync.Mutex
	discs   map[string][]byte
	corrupt map[string]int // drive → number of burns to corrupt
	block   chan struct{}  // when set, burns wait on it or ctx
	burns   int
	ejects  int
}

func newFake() *fakeBurner {
	return &fakeBurner{discs: map[string][]byte{}, corrupt: map[string]int{}}
}

func (f *fakeBurner) Burn(ctx context.Context, d drive.Drive, iso string, size int64, progress func(int64)) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	data, err := os.ReadFile(iso)
	if err != nil {
		return err
	}
	progress(size / 2)
	progress(size)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.burns++
	if f.corrupt[d.ID] > 0 {
		f.corrupt[d.ID]--
		data = append([]byte(nil), data...)
		data[0] ^= 0xff
	}
	f.discs[d.ID] = data
	return nil
}

func (f *fakeBurner) OpenDisc(ctx context.Context, d drive.Drive) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.discs[d.ID]
	if !ok {
		return nil, errors.New("no disc")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeBurner) Eject(ctx context.Context, d drive.Drive) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ejects++
	delete(f.discs, d.ID)
	return nil
}

var testDrives = []drive.Drive{{ID: "1", Model: "BDR"}, {ID: "2", Model: "BW"}}

func newSession(t *testing.T, copies int, drives []drive.Drive) (*store.Store, int64, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "burns.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	path, data := writeISO(t, 2*verifyChunk)
	id, err := st.CreateSession(context.Background(), []store.Job{{Path: path, Size: int64(len(data)), Copies: copies}}, drives)
	if err != nil {
		t.Fatal(err)
	}
	return st, id, path
}

// waitFor polls the engine until cond holds.
func waitFor(t *testing.T, e *Engine, what string, cond func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := e.Snapshot()
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func allWaitingOrFinished(s Snapshot) bool {
	for _, d := range s.Drives {
		if d.State != store.DriveWaiting && d.State != store.DriveFinished {
			return false
		}
	}
	return true
}

func TestEngineBurnsAcrossDrivesAndAsksForDiscs(t *testing.T) {
	st, id, _ := newSession(t, 3, testDrives)
	fake := newFake()
	e, err := Start(Config{Store: st, Session: id, Drives: testDrives, Burner: fake, DiscsLoaded: true, OpenRetries: 1})
	if err != nil {
		t.Fatal(err)
	}

	// Both drives burn the loaded discs, then one asks for disc 3 and the
	// other has nothing left.
	s := waitFor(t, e, "first round", func(s Snapshot) bool { return s.Done == 2 && allWaitingOrFinished(s) })
	var waiting DriveStatus
	for _, d := range s.Drives {
		if d.State == store.DriveWaiting {
			waiting = d
		}
	}
	if waiting.Disc == nil || waiting.Disc.Copy != 3 || waiting.Last == nil || waiting.Last.Status != store.DiscDone || waiting.Completed != 1 {
		t.Fatalf("waiting drive = %+v", waiting)
	}
	if e.Insert("nope") {
		t.Fatal("unknown drive should not accept a disc")
	}
	if !e.Insert(waiting.Drive.ID) {
		t.Fatal("waiting drive should accept a disc")
	}
	<-e.Done()
	s = e.Snapshot()
	if s.Running || s.Err != nil || s.Done != 3 || s.Total != 3 {
		t.Fatalf("final snapshot = %+v", s)
	}
	if fake.burns != 3 || fake.ejects != 3 {
		t.Fatalf("burns=%d ejects=%d", fake.burns, fake.ejects)
	}
	if u, _ := st.Unfinished(context.Background()); u != nil {
		t.Fatal("a completed session should not be resumable")
	}
	discs, _ := st.Discs(context.Background(), id)
	for _, d := range discs {
		if d.Status != store.DiscDone || d.VerifyNote != "" || d.Progress != d.ISOSize {
			t.Fatalf("disc = %+v", d)
		}
	}
	recs, _ := st.Drives(context.Background(), id)
	if recs[0].State != store.DriveFinished || recs[0].Completed+recs[1].Completed != 3 {
		t.Fatalf("drive records = %+v", recs)
	}
}

func TestEngineRetriesBadBurnOnSameDrive(t *testing.T) {
	drives := testDrives[:1]
	st, id, _ := newSession(t, 1, drives)
	fake := newFake()
	fake.corrupt["1"] = 1
	e, err := Start(Config{Store: st, Session: id, Drives: drives, Burner: fake, DiscsLoaded: true, OpenRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, e, "retry prompt", func(s Snapshot) bool { return s.Drives[0].State == store.DriveWaiting })
	d := s.Drives[0]
	if !strings.Contains(d.Err, "differs from the ISO") || d.Disc == nil || d.Disc.Attempts != 1 || s.Done != 0 {
		t.Fatalf("after bad burn = %+v", d)
	}
	e.Insert("1")
	<-e.Done()
	discs, _ := st.Discs(context.Background(), id)
	if discs[0].Status != store.DiscDone || discs[0].Attempts != 2 || e.Snapshot().Drives[0].Err != "" {
		t.Fatalf("disc after retry = %+v", discs[0])
	}
}

func TestEngineNotesUnreadableDisc(t *testing.T) {
	drives := testDrives[:1]
	st, id, _ := newSession(t, 1, drives)
	e, _ := Start(Config{Store: st, Session: id, Drives: drives, Burner: unreadable{newFake()}, DiscsLoaded: true, OpenRetries: 1})
	<-e.Done()
	discs, _ := st.Discs(context.Background(), id)
	if discs[0].Status != store.DiscDone || !strings.Contains(discs[0].VerifyNote, "permission denied") {
		t.Fatalf("disc = %+v", discs[0])
	}
}

type unreadable struct{ *fakeBurner }

func (unreadable) OpenDisc(context.Context, drive.Drive) (io.ReadCloser, error) {
	return nil, os.ErrPermission
}

func TestEngineStopAndResume(t *testing.T) {
	st, id, _ := newSession(t, 2, testDrives)
	fake := newFake()
	fake.block = make(chan struct{})
	e, _ := Start(Config{Store: st, Session: id, Drives: testDrives, Burner: fake, DiscsLoaded: true, OpenRetries: 1})
	waitFor(t, e, "burning", func(s Snapshot) bool {
		return s.Drives[0].State == store.DriveBurning && s.Drives[1].State == store.DriveBurning
	})
	e.Stop()
	discs, _ := st.Discs(context.Background(), id)
	for _, d := range discs {
		if d.Status != store.DiscPending || d.Error != "stopped while burning" {
			t.Fatalf("stopped disc = %+v", d)
		}
	}
	u, _ := st.Unfinished(context.Background())
	if u == nil || u.ID != id {
		t.Fatal("stopped session should be resumable")
	}

	// Resume with one drive: it asks for a disc before every burn.
	fake.block = nil
	if err := st.Resume(context.Background(), id, testDrives[1:]); err != nil {
		t.Fatal(err)
	}
	e, _ = Start(Config{Store: st, Session: id, Drives: testDrives[1:], Burner: fake, OpenRetries: 1})
	for want := 0; want < 2; want++ {
		s := waitFor(t, e, "disc prompt", func(s Snapshot) bool { return s.Drives[0].State == store.DriveWaiting })
		if s.Done != want {
			t.Fatalf("done = %d, want %d", s.Done, want)
		}
		e.Insert("2")
		waitFor(t, e, "burn", func(s Snapshot) bool { return s.Done == want+1 })
	}
	<-e.Done()
	if s := e.Snapshot(); s.Done != 2 || s.Drives[0].Completed != 2 {
		t.Fatalf("after resume = %+v", s)
	}
}
