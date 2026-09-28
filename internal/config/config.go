// Package config holds build-time settings shared across iso-burner.
package config

// RxAuth sign-in settings. The redirect URI must be on the OAuth client's
// allow-list in the RxAuth admin.
const (
	RxAuthIssuer      = "https://auth.rxlab.app"
	RxAuthClientID    = "client_15471181c4b343aca050b52a2bf7f27f"
	RxAuthRedirectURI = "http://127.0.0.1:53682/callback"
	// RxAuthStoreName names the token file in the user config directory.
	RxAuthStoreName = "iso-burner"
)
