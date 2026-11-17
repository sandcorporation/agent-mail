package main

// Admin login mode oidc (#75, #71 §1 ②): authorization-code flow with PKCE
// (S256), state and nonce against an OpenID Connect provider. The callback
// checks the ID token (issuer, audience, expiry, nonce, signature from the
// provider's JWKS) and the admin allowlist, then starts the same sb_session
// as the built-in mode.
//
// Libraries: github.com/coreos/go-oidc/v3 (discovery, JWKS, ID token checks)
// and golang.org/x/oauth2 (authorization URL, code exchange, PKCE).

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sandcorporation/agent-mail/internal/config"
	"golang.org/x/oauth2"
)

const (
	adminModeOIDC = "oidc"

	oidcFlowCookie = "sb_oidc"
	// oidcFlowTTL bounds the time between /auth/login and /auth/callback.
	oidcFlowTTL      = 10 * time.Minute
	oidcCallbackPath = "/auth/callback"
	oidcDefaultScope = "openid email profile"
	// oidcFailedText is the one body of every refused callback.
	oidcFailedText = "login failed"
	// oidcSubPrefix marks an allowlist entry (and a session user) that is a
	// provider subject rather than an email.
	oidcSubPrefix = "sub:"
)

// oidcLogin is the configured provider. Discovery runs on first use, so a
// provider that is down at start does not stop the server.
type oidcLogin struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string
	scopes       []string
	emails       map[string]bool // lower-cased
	subs         map[string]bool
	flowKey      []byte // HMAC key of the flow cookie; new on every start
	client       *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

// configureOIDC reads the SWITCHBOARD_OIDC_* settings and SWITCHBOARD_PUBLIC_URL.
// The error names the missing or invalid settings, never their values.
func configureOIDC(getenv func(string) string) (*oidcLogin, error) {
	get := func(name string) string { return strings.TrimSpace(getenv(config.EnvPrefix + name)) }
	o := &oidcLogin{
		issuer:       get("OIDC_ISSUER"),
		clientID:     get("OIDC_CLIENT_ID"),
		clientSecret: get("OIDC_CLIENT_SECRET"),
		emails:       map[string]bool{},
		subs:         map[string]bool{},
		client:       &http.Client{Timeout: 10 * time.Second},
	}
	for _, a := range strings.Split(get("OIDC_ADMINS"), ",") {
		a = strings.TrimSpace(a)
		switch {
		case strings.HasPrefix(a, oidcSubPrefix) && len(a) > len(oidcSubPrefix):
			o.subs[strings.TrimPrefix(a, oidcSubPrefix)] = true
		case strings.Contains(a, "@"):
			o.emails[strings.ToLower(a)] = true
		}
	}
	scope := get("OIDC_SCOPES")
	if scope == "" {
		scope = oidcDefaultScope
	}
	o.scopes = strings.Fields(scope)
	public := strings.TrimRight(get("PUBLIC_URL"), "/")

	var missing, invalid []string
	for name, v := range map[string]string{"OIDC_ISSUER": o.issuer, "OIDC_CLIENT_ID": o.clientID, "OIDC_CLIENT_SECRET": o.clientSecret, "PUBLIC_URL": public} {
		if v == "" {
			missing = append(missing, config.EnvPrefix+name)
		}
	}
	if len(o.emails)+len(o.subs) == 0 {
		missing = append(missing, config.EnvPrefix+"OIDC_ADMINS")
	}
	if u, err := url.Parse(o.issuer); o.issuer != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "") {
		invalid = append(invalid, config.EnvPrefix+"OIDC_ISSUER")
	}
	if u, err := url.Parse(public); public != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "") {
		invalid = append(invalid, config.EnvPrefix+"PUBLIC_URL")
	}
	hasOpenID := false
	for _, sc := range o.scopes {
		hasOpenID = hasOpenID || sc == oidc.ScopeOpenID
	}
	if !hasOpenID {
		invalid = append(invalid, config.EnvPrefix+"OIDC_SCOPES (needs openid)")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, errors.New(config.EnvPrefix + "ADMIN_AUTH=oidc needs " + strings.Join(missing, ", "))
	}
	if len(invalid) > 0 {
		return nil, errors.New(config.EnvPrefix + "ADMIN_AUTH=oidc: invalid " + strings.Join(invalid, ", "))
	}
	o.redirectURL = public + oidcCallbackPath
	o.flowKey = make([]byte, 32)
	if _, err := rand.Read(o.flowKey); err != nil {
		return nil, err
	}
	return o, nil
}

// ctx carries the provider's HTTP client; it outlives requests so the JWKS
// cache can refresh later.
func (o *oidcLogin) ctx(parent context.Context) context.Context {
	return oidc.ClientContext(parent, o.client)
}

// discover returns the provider, fetching its discovery document once.
func (o *oidcLogin) discover() (*oidc.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return o.provider, nil
	}
	p, err := oidc.NewProvider(o.ctx(context.Background()), o.issuer)
	if err != nil {
		return nil, err
	}
	o.provider = p
	return p, nil
}

func (o *oidcLogin) oauth(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{ClientID: o.clientID, ClientSecret: o.clientSecret, Endpoint: p.Endpoint(), RedirectURL: o.redirectURL, Scopes: o.scopes}
}

// allowed reports whether a session user (an email or sub:<id>) is still on
// the allowlist, so removing an admin and restarting ends their sessions.
func (o *oidcLogin) allowed(user string) bool {
	if strings.HasPrefix(user, oidcSubPrefix) {
		return o.subs[strings.TrimPrefix(user, oidcSubPrefix)]
	}
	return o.emails[strings.ToLower(user)]
}

// oidcFlow is what the flow cookie carries from /auth/login to the callback.
type oidcFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Expires  int64  `json:"e"`
}

func (o *oidcLogin) sealFlow(f oidcFlow) (string, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + o.sign(payload), nil
}

func (o *oidcLogin) sign(payload string) string {
	m := hmac.New(sha256.New, o.flowKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (o *oidcLogin) openFlow(v string, now time.Time) (oidcFlow, bool) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(o.sign(payload))) {
		return oidcFlow{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return oidcFlow{}, false
	}
	var f oidcFlow
	if json.Unmarshal(b, &f) != nil || f.State == "" || f.Nonce == "" || f.Verifier == "" || f.Expires <= now.Unix() {
		return oidcFlow{}, false
	}
	return f, true
}

// flowCookie is SameSite=Lax: the provider sends the browser back with a
// cross-site top-level GET, which Strict would strip.
func (s *Server) flowCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: oidcFlowCookie, Value: value, Path: "/auth/", MaxAge: maxAge, HttpOnly: true, Secure: !s.insecureCookie, SameSite: http.SameSiteLaxMode}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// oidcAuth serves GET /auth/login (redirect to the provider) and GET
// /auth/callback in oidc mode.
func (s *Server) oidcAuth(w http.ResponseWriter, r *http.Request) {
	if s.adminMode != adminModeOIDC || s.oidc == nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == oidcCallbackPath {
		s.oidcCallback(w, r)
		return
	}
	o := s.oidc
	p, err := o.discover()
	if err != nil {
		log.Printf("oidc: discovery failed: %v", err)
		http.Error(w, "login unavailable", http.StatusBadGateway)
		return
	}
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	if err1 != nil || err2 != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	f := oidcFlow{State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(), Expires: s.now().Add(oidcFlowTTL).Unix()}
	sealed, err := o.sealFlow(f)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, s.flowCookie(sealed, int(oidcFlowTTL/time.Second)))
	http.Redirect(w, r, o.oauth(p).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

// oidcClaims are the ID token claims the allowlist reads.
type oidcClaims struct {
	Email         string    `json:"email"`
	EmailVerified claimBool `json:"email_verified"`
}

// claimBool accepts true and "true"; some providers send the string.
type claimBool bool

func (b *claimBool) UnmarshalJSON(d []byte) error {
	var v any // a JSON boolean or string
	if err := json.Unmarshal(d, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case bool:
		*b = claimBool(t)
	case string:
		*b = claimBool(t == "true")
	default:
		*b = false
	}
	return nil
}

// oidcCallback finishes the login. Every refusal is the same 403; the reason
// goes to the log without tokens or secrets.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	o := s.oidc
	refuse := func(reason string) {
		log.Printf("oidc: login refused: %q", reason)
		http.Error(w, oidcFailedText, http.StatusForbidden)
	}
	c, err := r.Cookie(oidcFlowCookie)
	http.SetCookie(w, s.flowCookie("", -1))
	if err != nil {
		refuse("no flow cookie")
		return
	}
	f, ok := o.openFlow(c.Value, s.now())
	if !ok {
		refuse("flow cookie invalid or expired")
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		refuse("state mismatch")
		return
	}
	if e := q.Get("error"); e != "" {
		refuse("provider error " + truncate(e, 64))
		return
	}
	code := q.Get("code")
	if code == "" {
		refuse("no code")
		return
	}
	p, err := o.discover()
	if err != nil {
		refuse("discovery failed")
		return
	}
	ctx := o.ctx(r.Context())
	tok, err := o.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		refuse("code exchange failed")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		refuse("no id_token")
		return
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: o.clientID, Now: s.now}).Verify(ctx, raw)
	if err != nil {
		refuse("id token rejected")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		refuse("nonce mismatch")
		return
	}
	var cl oidcClaims
	if err := idt.Claims(&cl); err != nil {
		refuse("claims unreadable")
		return
	}
	user := ""
	switch {
	case o.subs[idt.Subject]:
		user = oidcSubPrefix + idt.Subject
	case cl.Email != "" && o.emails[strings.ToLower(cl.Email)] && bool(cl.EmailVerified):
		user = strings.ToLower(cl.Email)
	case cl.Email != "" && o.emails[strings.ToLower(cl.Email)]:
		refuse("email not verified")
		return
	default:
		refuse("not on the allowlist")
		return
	}
	if err := s.startSession(w, r, user, adminModeOIDC); err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	log.Printf("oidc: admin %q signed in", user)
	// The session cookie is SameSite=Strict and this response ends a
	// cross-site redirect chain, so the browser would not send it on a
	// redirect to /. A same-site page that moves on does.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, oidcDoneHTML)
}

func truncate(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}

const oidcDoneHTML = `<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/"><title>` + productName + `</title></head>
<body><a href="/">` + productName + `</a></body></html>
`
