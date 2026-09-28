package auth

import "context"

// AccessToken returns a valid access token for the signed-in account,
// refreshing it first if it has expired. Servers that trust RxAuth, such as
// rxstorage, accept it as a Bearer token.
func (a *RxAuth) AccessToken(ctx context.Context) (string, error) {
	au, err := a.authenticator(nil)
	if err != nil {
		return "", err
	}
	token, err := au.Client().AccessToken(ctx)
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}
