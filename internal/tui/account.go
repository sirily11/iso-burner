package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sirily11/iso-burner/internal/auth"
)

// authCheckTimeout bounds the startup check of a stored session.
const authCheckTimeout = 10 * time.Second

// authCheckedMsg reports the stored session found at startup.
type authCheckedMsg struct{ user *auth.User }

// signInURLMsg carries the URL the browser was sent to.
type signInURLMsg struct{ url string }

// signInDoneMsg ends a browser sign-in.
type signInDoneMsg struct {
	user auth.User
	err  error
}

// signedOutMsg ends a sign-out.
type signedOutMsg struct{ err error }

func checkAuthCmd(svc auth.Service) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), authCheckTimeout)
		defer cancel()
		user, err := svc.Current(ctx)
		if err != nil {
			return authCheckedMsg{}
		}
		return authCheckedMsg{user: &user}
	}
}

// waitSignInURL delivers the sign-in URL, or nothing if sign-in ended first.
func waitSignInURL(urls <-chan string) tea.Cmd {
	return func() tea.Msg {
		url, ok := <-urls
		if !ok {
			return nil
		}
		return signInURLMsg{url: url}
	}
}

// startSignIn opens the browser and waits for the RxAuth callback.
func (m Model) startSignIn() (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	urls := make(chan string, 1)
	m.signingIn, m.signInURL, m.signInCancel, m.authErr = true, "", cancel, nil
	svc := m.auth
	login := func() tea.Msg {
		defer close(urls)
		user, err := svc.Login(ctx, func(url string) { urls <- url })
		return signInDoneMsg{user: user, err: err}
	}
	return m, tea.Batch(login, waitSignInURL(urls))
}

// updateAuthMsg handles auth results, which may arrive on any screen.
func (m Model) updateAuthMsg(msg tea.Msg) (Model, bool) {
	switch msg := msg.(type) {
	case authCheckedMsg:
		m.authChecking, m.user = false, msg.user
	case signInURLMsg:
		m.signInURL = msg.url
	case signInDoneMsg:
		if m.signInCancel != nil {
			m.signInCancel()
		}
		m.signingIn, m.signInCancel, m.signInURL = false, nil, ""
		if msg.err != nil {
			if msg.err != context.Canceled {
				m.authErr = msg.err
			}
			break
		}
		m.user, m.authErr = &msg.user, nil
	case signedOutMsg:
		if msg.err != nil {
			m.authErr = msg.err
			break
		}
		m.user = nil
	default:
		return m, false
	}
	return m, true
}

// updateAccount handles keys on the account screen.
func (m Model) updateAccount(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Type == tea.KeyCtrlC {
		if m.signInCancel != nil {
			m.signInCancel()
		}
		m.cancelled = true
		return m, tea.Quit
	}
	if m.signingIn {
		if key.Type == tea.KeyEsc {
			m.signInCancel()
		}
		return m, nil
	}
	if m.confirmSignOut {
		m.confirmSignOut = false
		if key.String() == "y" {
			svc := m.auth
			return m, func() tea.Msg { return signedOutMsg{err: svc.Logout(context.Background())} }
		}
		return m, nil
	}
	switch key.String() {
	case "esc", "q":
		m.showAccount, m.authErr, m.uploadAfterSignIn = false, nil, false
	case "enter":
		if m.auth == nil || m.authChecking {
			return m, nil
		}
		if m.user != nil {
			m.confirmSignOut = true
			return m, nil
		}
		return m.startSignIn()
	}
	return m, nil
}

// accountSummary is the one-line account status on the mode screen.
func (m Model) accountSummary() string {
	switch {
	case m.auth == nil:
		return dimStyle.Render("Sign-in unavailable")
	case m.authChecking:
		return dimStyle.Render("Checking sign-in…")
	case m.user != nil:
		return okStyle.Render("✓ Signed in as " + m.user.Label())
	}
	return dimStyle.Render("Not signed in")
}

func (m Model) accountView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("ISO Burner · Account") + "\n\n")
	switch {
	case m.auth == nil:
		b.WriteString(errorStyle.Render("Sign-in is unavailable: the token store could not be opened.") + "\n")
	case m.signingIn:
		b.WriteString(labelStyle.Render("Signing in with RxAuth…") + "\n\n")
		b.WriteString("Finish signing in in your browser.\n")
		if m.signInURL != "" {
			b.WriteString(dimStyle.Render("If it did not open, visit the link below.") + "\n")
		}
	case m.authChecking:
		b.WriteString(dimStyle.Render("Checking sign-in…") + "\n")
	case m.confirmSignOut:
		b.WriteString(labelStyle.Render("Sign out of "+m.user.Label()+"?") + "\n\n")
		b.WriteString("Press y to sign out, any other key to keep the session.\n")
	case m.user != nil:
		b.WriteString(labelStyle.Render("Signed in") + "\n\n")
		if m.user.Name != "" {
			b.WriteString("Name:   " + m.user.Name + "\n")
		}
		if m.user.Email != "" {
			b.WriteString("Email:  " + m.user.Email + "\n")
		}
		b.WriteString("\n" + selectedStyle.Render("› Sign out") + "\n")
	default:
		b.WriteString(labelStyle.Render("Not signed in") + "\n\n")
		if m.uploadAfterSignIn {
			b.WriteString("Uploading content needs your rxstorage account.\n")
		}
		b.WriteString("Sign in with your RxLab account through the browser.\n")
		b.WriteString("\n" + selectedStyle.Render("› Sign in with RxAuth") + "\n")
	}
	if m.authErr != nil {
		b.WriteString("\n" + errorStyle.Render("✗ "+m.authErr.Error()) + "\n")
	}
	help := "enter: select · esc: back"
	switch {
	case m.signingIn:
		help = "esc: cancel sign-in"
	case m.confirmSignOut:
		help = "y: sign out · any key: cancel"
	}
	b.WriteString("\n" + dimStyle.Render(help))
	view := panelStyle.Render(b.String()) + "\n"
	if m.signingIn && m.signInURL != "" {
		// Outside the panel, hard-wrapped because longer lines get cut off.
		view += "\n" + wrapURL(m.signInURL, m.width) + "\n"
	}
	return view
}

// wrapURL splits url into lines of at most width characters.
func wrapURL(url string, width int) string {
	if width <= 0 || len(url) <= width {
		return url
	}
	var lines []string
	for len(url) > width {
		lines = append(lines, url[:width])
		url = url[width:]
	}
	return strings.Join(append(lines, url), "\n")
}
