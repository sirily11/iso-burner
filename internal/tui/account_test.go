package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/auth"
)

type fakeAuth struct {
	user      *auth.User
	loggedOut bool
}

func (f *fakeAuth) Current(context.Context) (auth.User, error) {
	if f.user == nil {
		return auth.User{}, errors.New("no token")
	}
	return *f.user, nil
}

func (f *fakeAuth) Login(_ context.Context, onURL func(string)) (auth.User, error) {
	onURL("https://auth.example/authorize")
	f.user = &auth.User{ID: "u1", Name: "Ada", Email: "ada@example.com"}
	return *f.user, nil
}

func (f *fakeAuth) Logout(context.Context) error {
	f.user, f.loggedOut = nil, true
	return nil
}

// run executes a command and feeds every resulting message back into m.
func run(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = run(t, m, c)
		}
		return m
	}
	if msg == nil {
		return m
	}
	next, cmd := m.Update(msg)
	return run(t, next.(Model), cmd)
}

func TestAccountSignInAndOut(t *testing.T) {
	svc := &fakeAuth{}
	m := New(Options{Auth: svc})
	m = run(t, m, checkAuthCmd(svc))
	if !strings.Contains(m.View(), "Not signed in") {
		t.Fatalf("mode screen should show signed-out status:\n%s", m.View())
	}

	m = send(t, m, key("a"))
	if !strings.Contains(m.View(), "Sign in with RxAuth") {
		t.Fatalf("account screen should offer sign-in:\n%s", m.View())
	}
	next, cmd := m.Update(enter)
	m = next.(Model)
	if !m.signingIn || !strings.Contains(m.View(), "Finish signing in") {
		t.Fatalf("enter should start sign-in:\n%s", m.View())
	}
	m = run(t, m, cmd)
	if m.User() == nil || m.User().Name != "Ada" || m.signingIn {
		t.Fatalf("sign-in should store the user: %+v", m.User())
	}
	if !strings.Contains(m.View(), "ada@example.com") {
		t.Fatalf("account screen should show the user:\n%s", m.View())
	}

	m = send(t, m, enter)
	if !strings.Contains(m.View(), "Sign out of Ada") {
		t.Fatalf("enter should ask to confirm sign-out:\n%s", m.View())
	}
	next, cmd = m.Update(key("y"))
	m = run(t, next.(Model), cmd)
	if m.User() != nil || !svc.loggedOut {
		t.Fatal("y should sign out")
	}

	m = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showAccount || m.cancelled {
		t.Fatal("esc should return to mode selection")
	}
}

func TestAccountRestoresSession(t *testing.T) {
	svc := &fakeAuth{user: &auth.User{ID: "u1", Email: "ada@example.com"}}
	m := run(t, New(Options{Auth: svc}), checkAuthCmd(svc))
	if !strings.Contains(m.View(), "Signed in as ada@example.com") {
		t.Fatalf("stored session should be shown:\n%s", m.View())
	}
}
