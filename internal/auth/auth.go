// Package auth signs the user in with RxAuth through the system browser and
// keeps the session in the user config directory.
package auth

import (
	"context"

	rxauth "github.com/rxtech-lab/RxAuthGo"
	"github.com/rxtech-lab/RxAuthGo/rxauthcli"

	"github.com/sirily11/iso-burner/internal/config"
)

// User is the signed-in RxAuth account.
type User struct {
	ID    string
	Name  string
	Email string
}

// Label is how the account is shown to the user.
func (u User) Label() string {
	switch {
	case u.Name != "" && u.Email != "":
		return u.Name + " <" + u.Email + ">"
	case u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	}
	return u.ID
}

// Service signs users in and out.
type Service interface {
	// Current returns the stored session's user without opening a browser.
	Current(ctx context.Context) (User, error)
	// Login opens the browser for sign-in; onURL receives the sign-in URL
	// in case the browser cannot be opened.
	Login(ctx context.Context, onURL func(string)) (User, error)
	// Logout forgets the stored session.
	Logout(ctx context.Context) error
}

// RxAuth is the Service backed by RxAuth.
type RxAuth struct {
	store rxauth.TokenStore
}

// New opens the token store in the user config directory.
func New() (*RxAuth, error) {
	store, err := rxauthcli.NewDefaultFileTokenStore(config.RxAuthStoreName)
	if err != nil {
		return nil, err
	}
	return &RxAuth{store: store}, nil
}

func (a *RxAuth) authenticator(open rxauthcli.BrowserOpener) (*rxauthcli.Authenticator, error) {
	return rxauthcli.NewAuthenticator(rxauthcli.Options{
		Config: rxauth.Config{
			Issuer:      config.RxAuthIssuer,
			ClientID:    config.RxAuthClientID,
			RedirectURI: config.RxAuthRedirectURI,
			Scopes:      rxauth.DefaultScopes,
		},
		Store:       a.store,
		BrowserOpen: open,
	})
}

func (a *RxAuth) Current(ctx context.Context) (User, error) {
	au, err := a.authenticator(nil)
	if err != nil {
		return User{}, err
	}
	info, err := au.Client().CheckExistingAuth(ctx)
	if err != nil {
		return User{}, err
	}
	return toUser(info), nil
}

func (a *RxAuth) Login(ctx context.Context, onURL func(string)) (User, error) {
	au, err := a.authenticator(func(url string) error {
		if onURL != nil {
			onURL(url)
		}
		// The URL is shown in the app, so a missing browser is not fatal.
		_ = rxauthcli.OpenBrowser(url)
		return nil
	})
	if err != nil {
		return User{}, err
	}
	if _, err := au.Login(ctx); err != nil {
		return User{}, err
	}
	info, err := au.Client().UserInfo(ctx)
	if err != nil {
		return User{}, err
	}
	return toUser(info), nil
}

func (a *RxAuth) Logout(ctx context.Context) error {
	return a.store.Clear(ctx)
}

func toUser(info rxauth.UserInfo) User {
	name := info.Name
	if name == "" {
		name = info.PreferredUsername
	}
	return User{ID: info.ID, Name: name, Email: info.Email}
}
