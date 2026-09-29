package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirily11/iso-burner/internal/store"
)

func TestBurnMenuVerificationLeavesSavedBurnIntact(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "backup.iso")
	st := testStore(t)
	id, err := st.CreateSession(t.Context(), []store.Job{{Path: filepath.Join(root, "backup.iso"), Size: 3, Copies: 1}}, testDrives[:1])
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{Mode: ModeBurn, Folder: root, Store: st})
	if !strings.Contains(m.View(), "1. Burn ISO files") || !strings.Contains(m.View(), "2. Verify disc against ISO") {
		t.Fatalf("missing Burn submenu:\n%s", m.View())
	}
	m = send(t, m, key("v"))
	if !m.verify.choosing || m.resumeOffer != nil || !strings.Contains(m.View(), "Select the original ISO") {
		t.Fatalf("verify must bypass the resume dialog:\n%s", m.View())
	}
	m = send(t, m, key("esc"))
	if !m.burnMenu {
		t.Fatal("escape should return to the submenu")
	}
	u, err := st.Unfinished(t.Context())
	if err != nil || u == nil || u.ID != id {
		t.Fatalf("verification must retain the saved burn: %+v %v", u, err)
	}
	m = send(t, m, key("b"))
	if m.resumeOffer == nil || m.resumeOffer.ID != id {
		t.Fatal("burn action should still offer the saved session")
	}
}

func TestVerifyFlowSelectsOneISOAndDrive(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "match", true: "mismatch"}[mismatch], func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, "a.iso", "b.iso")
			fake := newFakeBurner()
			fake.discs["1"] = []byte("iso")
			fake.discs["2"] = []byte("iso")
			if mismatch {
				fake.discs["2"] = []byte("bad")
			}
			m := New(Options{Mode: ModeBurn, Folder: root, Burner: fake, ListDrives: fixedDrives(testDrives[:2], nil)})
			m = send(t, m, key("2"))
			m = send(t, m, key("down")) // b.iso
			next, cmd := m.Update(enter)
			m = send(t, next.(Model), cmd())
			if m.verify.isoPath != filepath.Join(root, "b.iso") || !strings.Contains(m.View(), "Insert the recorded disc") {
				t.Fatalf("wrong ISO or drive prompt:\n%s", m.View())
			}
			m = send(t, m, key("down")) // second drive
			m = send(t, m, enter)
			if m.verify.run == nil {
				t.Fatalf("verify should start without a burn database: %v", m.verify.err)
			}
			select {
			case <-m.verify.run.Done():
			case <-time.After(5 * time.Second):
				m.verify.run.Cancel()
				t.Fatal("verification did not finish")
			}
			m = send(t, m, burnTickMsg{})
			view := m.View()
			want := "Disc is complete and matches the ISO byte for byte"
			if mismatch {
				want = "Disc does not match the ISO"
			}
			if !strings.Contains(view, want) || !strings.Contains(view, "Drive  2 · ASUS") {
				t.Fatalf("verification result missing %q or drive:\n%s", want, view)
			}
			if snap, ok := m.VerificationResult(); !ok || snap.Running || (snap.Err != nil) != mismatch {
				t.Fatalf("verification result = %+v %v", snap, ok)
			}
			if m.engine != nil || m.burnSession != 0 || len(fake.discs) != 2 {
				t.Fatal("verification must not start a burn or eject either disc")
			}
			m = send(t, m, enter)
			if !m.burnMenu || m.verify.choosing {
				t.Fatal("enter after verification should return to the submenu")
			}
		})
	}
}

func TestVerifyMissingISOCanReturnToPicker(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "a.iso")
	m := New(Options{Mode: ModeBurn, Folder: root, Burner: newFakeBurner(), ListDrives: fixedDrives(testDrives[:1], nil)})
	m = send(t, m, key("v"))
	next, cmd := m.Update(enter)
	m = send(t, next.(Model), cmd())
	if err := os.Remove(filepath.Join(root, "a.iso")); err != nil {
		t.Fatal(err)
	}
	m = send(t, m, enter)
	if m.verify.run != nil || !strings.Contains(m.View(), "open ISO") {
		t.Fatalf("removed ISO should leave an actionable error:\n%s", m.View())
	}
	m = send(t, m, key("esc"))
	if m.verify.isoPath != "" {
		t.Fatal("escape should return to the ISO picker after a start error")
	}
}
