package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"time"
)

// Names and bodies the admin login tests check, as the client sees them.
const (
	sessionCookie   = "sb_session"
	loginFailedText = "login failed"
	oidcFlowCookie  = "sb_oidc"
	oidcFailedText  = "login failed"
	loginFailWindow = 15 * time.Minute
	loginFailLimit  = 5
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// loginRequest is the body of POST /auth/login.
type loginRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
