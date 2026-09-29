package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sirily11/iso-burner/internal/drive"
)

var drives = []drive.Drive{{ID: "1", Vendor: "PIONEER", Model: "BDR"}, {ID: "2", Vendor: "ASUS", Model: "BW"}}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "burns.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestCreateSessionAndClaim(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	id, err := s.CreateSession(ctx, []Job{{Path: "/a.iso", Size: 10, Copies: 2}, {Path: "/b.iso", Size: 20, Copies: 1}}, drives)
	if err != nil {
		t.Fatal(err)
	}
	discs, err := s.Discs(ctx, id)
	if err != nil || len(discs) != 3 {
		t.Fatalf("discs = %v, %v", discs, err)
	}
	if d := discs[1]; d.ISOPath != "/a.iso" || d.Copy != 2 || d.Copies != 2 || d.Status != DiscPending {
		t.Fatalf("second disc = %+v", d)
	}

	a, _ := s.Claim(ctx, id, "1")
	b, _ := s.Claim(ctx, id, "2")
	if a == nil || b == nil || a.ID == b.ID || a.Status != DiscAssigned || a.DriveID != "1" {
		t.Fatalf("claims = %+v, %+v", a, b)
	}

	// A failed disc stays with its drive and is claimed again first.
	if err := s.FailDisc(ctx, a.ID, errors.New("bad disc")); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Claim(ctx, id, "1")
	if again == nil || again.ID != a.ID || again.Error != "bad disc" {
		t.Fatalf("retry claim = %+v", again)
	}

	if err := s.StartPhase(ctx, a.ID, DiscBurning, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDisc(ctx, a.ID, ""); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Claim(ctx, id, "1")
	if c == nil || c.ISOPath != "/b.iso" {
		t.Fatalf("third claim = %+v", c)
	}
	if none, err := s.Claim(ctx, id, "1"); none == nil && err == nil {
		// drive 1 still holds c, so it gets c back rather than nothing
		t.Fatal("claim should return the drive's assigned disc")
	}

	done, _ := s.Disc(ctx, a.ID)
	if done.Status != DiscDone || done.Attempts != 1 || done.Progress != 10 || done.FinishedAt.IsZero() {
		t.Fatalf("finished disc = %+v", done)
	}
	sess, err := s.Session(ctx, id)
	if err != nil || sess.Total != 3 || sess.Done != 1 || len(sess.DriveIDs) != 2 {
		t.Fatalf("session = %+v, %v", sess, err)
	}
}

func TestUnfinishedAndResume(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	if u, err := s.Unfinished(ctx); u != nil || err != nil {
		t.Fatalf("empty store: %v, %v", u, err)
	}
	id, _ := s.CreateSession(ctx, []Job{{Path: "/a.iso", Size: 10, Copies: 3}}, drives)
	burning, _ := s.Claim(ctx, id, "1")
	s.StartPhase(ctx, burning.ID, DiscBurning, 10)
	s.SetProgress(ctx, burning.ID, 4)
	waiting, _ := s.Claim(ctx, id, "2")
	s.SetDrive(ctx, id, "1", DriveBurning, burning.ID, "")
	s.Close()

	// Reopening the database finds the session where it was left.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.Unfinished(ctx)
	if err != nil || u == nil || u.ID != id || u.Total != 3 || u.Done != 0 {
		t.Fatalf("unfinished = %+v, %v", u, err)
	}
	recs, _ := s.Drives(ctx, id)
	if recs[0].State != DriveBurning || recs[0].DiscID != burning.ID {
		t.Fatalf("drive record = %+v", recs[0])
	}
	d, _ := s.Disc(ctx, burning.ID)
	if d.Progress != 4 || d.Total != 10 {
		t.Fatalf("saved progress = %+v", d)
	}

	if err := s.Resume(ctx, id, drives[1:]); err != nil {
		t.Fatal(err)
	}
	discs, _ := s.Discs(ctx, id)
	for _, d := range discs {
		if d.Status != DiscPending || d.DriveID != "" || d.Progress != 0 {
			t.Fatalf("after resume = %+v", d)
		}
	}
	if discs[0].Error != "interrupted" || discs[1].Error != "" {
		t.Fatalf("interrupted burn should be noted: %q, %q (waiting disc %d)", discs[0].Error, discs[1].Error, waiting.ID)
	}
	if recs, _ := s.Drives(ctx, id); len(recs) != 1 || recs[0].DriveID != "2" {
		t.Fatalf("drives after resume = %+v", recs)
	}

	s.SetSessionStatus(ctx, id, SessionDiscarded)
	if u, _ := s.Unfinished(ctx); u != nil {
		t.Fatal("discarded session should not be resumable")
	}
}

func TestSkipISOPreservesOtherDriveAndResume(t *testing.T) {
	ctx := t.Context()
	s, _ := open(t)
	id, err := s.CreateSession(ctx, []Job{{Path: "/a.iso", Size: 10, Copies: 3}, {Path: "/b.iso", Size: 20, Copies: 1}}, drives)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Claim(ctx, id, "1")
	b, _ := s.Claim(ctx, id, "2")
	if n, err := s.SkipISO(ctx, a.ID); err != nil || n != 0 {
		t.Fatalf("an unfailed disc must not be skipped: %d, %v", n, err)
	}
	s.FailDisc(ctx, a.ID, errors.New("bad disc"))
	if n, err := s.SkipISO(ctx, a.ID); err != nil || n != 2 {
		t.Fatalf("skip = %d, %v; want failed and pending copies", n, err)
	}
	other, err := s.Disc(ctx, b.ID)
	if err != nil || other.Status != DiscAssigned || other.DriveID != "2" {
		t.Fatalf("other drive's disc = %+v, %v", other, err)
	}
	// Stop/restart retains the skipped rows, their failure, and the counts.
	if err := s.ReleaseDisc(ctx, a.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(ctx, id, drives[:1]); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Session(ctx, id)
	if err != nil || sess.Total != 4 || sess.Done != 0 || sess.Skipped != 2 {
		t.Fatalf("resumed session = %+v, %v", sess, err)
	}
	skipped, _ := s.Disc(ctx, a.ID)
	if skipped.Status != DiscSkipped || skipped.Error != "bad disc" {
		t.Fatalf("skipped row after resume = %+v", skipped)
	}
	claimed, _ := s.Claim(ctx, id, "1")
	if claimed.ID != b.ID {
		t.Fatalf("resume should claim the other unfinished copy: %+v", claimed)
	}
}
