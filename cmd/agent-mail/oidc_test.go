package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/agent-mail/internal/adminauth"
)

// fakeIdP is an OpenID Connect provider on httptest: discovery, JWKS and a
// token endpoint that checks the client secret and PKCE. The test decides
// what the next ID token says through issue.
type fakeIdP struct {
	srv          *httptest.Server
	key          *rsa.PrivateKey
	clientID     string
	clientSecret string

	mu        sync.Mutex
	codes     map[string]idpGrant
	exchanges int
}

// idpGrant is what the provider remembers about a code it handed out.
type idpGrant struct {
	challenge   string
	redirectURI string
	claims      map[string]any // JSON claims of the ID token
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{key: key, clientID: "switchboard", clientSecret: "client-secret-value", codes: map[string]idpGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := p.srv.URL
		writeJSON(w, map[string]any{
			"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token", "jwks_uri": u + "/jwks",
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		e := big.NewInt(int64(key.E)).Bytes()
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	})
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	p.mu.Lock()
	p.exchanges++
	g, found := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case id != p.clientID || secret != p.clientSecret:
		http.Error(w, `{"error":"invalid_client"}`, 401)
		return
	case !found || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirectURI,
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 300, "id_token": p.sign(g.claims)})
}

// sign makes an RS256 JWT with kid k1.
func (p *fakeIdP) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// issue plays the user signing in at the provider: it takes the
// authorization URL our server redirected to and returns a code whose ID
// token has the standard claims (from the request) overridden by extra.
func (p *fakeIdP) issue(t *testing.T, authURL string, now time.Time, extra map[string]any) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, p.srv.URL+"/authorize?") {
		t.Fatalf("auth url %q", authURL)
	}
	q := u.Query()
	claims := map[string]any{
		"iss": p.srv.URL, "aud": p.clientID, "sub": "user-1", "nonce": q.Get("nonce"),
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"email": "admin@example.com", "email_verified": true,
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	code, _ = randomToken()
	p.mu.Lock()
	p.codes[code] = idpGrant{challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), claims: claims}
	p.mu.Unlock()
	return code, q.Get("state")
}

func oidcEnv(p *fakeIdP, admins string) map[string]string {
	return map[string]string{
		"SWITCHBOARD_ADMIN_AUTH":         "oidc",
		"SWITCHBOARD_OIDC_ISSUER":        p.srv.URL,
		"SWITCHBOARD_OIDC_CLIENT_ID":     p.clientID,
		"SWITCHBOARD_OIDC_CLIENT_SECRET": p.clientSecret,
		"SWITCHBOARD_OIDC_ADMINS":        admins,
		"SWITCHBOARD_PUBLIC_URL":         "https://mail.example/",
	}
}

func oidcServer(t *testing.T, p *fakeIdP, admins string) (*Server, *time.Time) {
	t.Helper()
	s := testServer(t)
	if err := adminauth.Configure(&s.admin, fakeEnv(oidcEnv(p, admins))); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	return s, &now
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startOIDC is GET /auth/login: the redirect target and the flow cookie.
func startOIDC(t *testing.T, s *Server) (string, *http.Cookie) {
	t.Helper()
	w := doAuth(s, authReq{method: "GET", path: "/auth/login"})
	if w.Code != http.StatusFound {
		t.Fatalf("login=%d %s", w.Code, w.Body)
	}
	c := cookieNamed(w, oidcFlowCookie)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 || c.MaxAge > 600 {
		t.Fatalf("flow cookie %+v", c)
	}
	return w.Header().Get("Location"), c
}

func callback(s *Server, code, state string, flow *http.Cookie) *httptest.ResponseRecorder {
	return doAuth(s, authReq{method: "GET", path: "/auth/callback?" + url.Values{"code": {code}, "state": {state}}.Encode(), cookie: flow})
}

func TestOIDCLoginRedirect(t *testing.T) {
	p := newFakeIdP(t)
	s, _ := oidcServer(t, p, "admin@example.com")
	loc, _ := startOIDC(t, s)
	u, _ := url.Parse(loc)
	q := u.Query()
	want := map[string]string{"response_type": "code", "client_id": p.clientID, "redirect_uri": "https://mail.example/auth/callback", "scope": "openid email profile", "code_challenge_method": "S256"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s=%q want %q", k, q.Get(k), v)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(k)) < 40 {
			t.Errorf("%s=%q", k, q.Get(k))
		}
	}
	if q.Get("client_secret") != "" || strings.Contains(loc, p.clientSecret) {
		t.Fatal("client secret in the redirect")
	}
	// A second login gets fresh state and nonce.
	loc2, _ := startOIDC(t, s)
	u2, _ := url.Parse(loc2)
	if u2.Query().Get("state") == q.Get("state") || u2.Query().Get("nonce") == q.Get("nonce") {
		t.Fatal("state or nonce reused")
	}
}

func TestOIDCLoginPage(t *testing.T) {
	p := newFakeIdP(t)
	s, _ := oidcServer(t, p, "admin@example.com")
	w := doAuth(s, authReq{method: "GET", path: "/"})
	body := w.Body.String()
	if w.Code != 401 || !strings.Contains(body, `href="/auth/login"`) || strings.Contains(body, "password") || strings.Contains(body, "<form") {
		t.Fatalf("page=%d %s", w.Code, body)
	}
	// The password login is not open in oidc mode.
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", "123456")}); w.Code != 401 {
		t.Fatalf("password login=%d", w.Code)
	}
}

func TestOIDCSuccessStartsSession(t *testing.T) {
	p := newFakeIdP(t)
	s, now := oidcServer(t, p, "Admin@Example.com, sub:other")
	loc, flow := startOIDC(t, s)
	code, state := p.issue(t, loc, *now, nil)
	w := callback(s, code, state, flow)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `url=/`) {
		t.Fatalf("callback=%d %s", w.Code, w.Body)
	}
	if c := cookieNamed(w, oidcFlowCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("flow cookie not cleared: %+v", c)
	}
	sess := sessionFrom(t, w)
	if !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie %+v", sess)
	}
	me := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: sess})
	if me.Code != 200 || !strings.Contains(me.Body.String(), `"user":"admin@example.com","mode":"oidc"`) {
		t.Fatalf("me=%d %s", me.Code, me.Body)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/", cookie: sess}); w.Code != 200 {
		t.Fatalf("dashboard=%d", w.Code)
	}
	// The code and the flow are single use.
	if w := callback(s, code, state, flow); w.Code != 403 {
		t.Fatalf("replay=%d", w.Code)
	}
	// Logout ends the session.
	if w := doAuth(s, authReq{method: "POST", path: "/auth/logout", body: "{}", cookie: sess}); w.Code != 204 {
		t.Fatalf("logout=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: sess}); w.Code != 401 {
		t.Fatalf("me after logout=%d", w.Code)
	}
}

func TestOIDCSubjectAllowlist(t *testing.T) {
	p := newFakeIdP(t)
	s, now := oidcServer(t, p, "sub:user-9")
	loc, flow := startOIDC(t, s)
	// A subject entry needs no email at all.
	code, state := p.issue(t, loc, *now, map[string]any{"sub": "user-9", "email": nil, "email_verified": nil})
	w := callback(s, code, state, flow)
	me := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: sessionFrom(t, w)})
	if !strings.Contains(me.Body.String(), `"user":"sub:user-9"`) {
		t.Fatalf("me=%s", me.Body)
	}
}

func TestOIDCCallbackRefusals(t *testing.T) {
	p := newFakeIdP(t)
	cases := map[string]struct {
		admins string
		claims map[string]any
	}{
		"bad nonce":         {"admin@example.com", map[string]any{"nonce": "not-the-nonce"}},
		"no nonce":          {"admin@example.com", map[string]any{"nonce": nil}},
		"bad audience":      {"admin@example.com", map[string]any{"aud": "another-client"}},
		"bad issuer":        {"admin@example.com", map[string]any{"iss": "https://idp.example"}},
		"expired":           {"admin@example.com", map[string]any{"exp": int64(1)}},
		"not on allowlist":  {"admin@example.com", map[string]any{"email": "someone@example.com"}},
		"unverified email":  {"admin@example.com", map[string]any{"email_verified": false}},
		"no email_verified": {"admin@example.com", map[string]any{"email_verified": nil}},
		"email as sub only": {"sub:admin@example.com", nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, now := oidcServer(t, p, tc.admins)
			loc, flow := startOIDC(t, s)
			code, state := p.issue(t, loc, *now, tc.claims)
			w := callback(s, code, state, flow)
			if w.Code != 403 || strings.TrimSpace(w.Body.String()) != oidcFailedText || cookieNamed(w, sessionCookie) != nil {
				t.Fatalf("callback=%d %q", w.Code, w.Body)
			}
		})
	}
}

func TestOIDCFlowCookieRefusals(t *testing.T) {
	p := newFakeIdP(t)
	s, now := oidcServer(t, p, "admin@example.com")
	refused := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 403 || strings.TrimSpace(w.Body.String()) != oidcFailedText || cookieNamed(w, sessionCookie) != nil {
			t.Fatalf("%s: callback=%d %q", name, w.Code, w.Body)
		}
	}
	loc, flow := startOIDC(t, s)
	code, state := p.issue(t, loc, *now, nil)
	before := p.exchanges
	refused("no cookie", callback(s, code, state, nil))
	refused("wrong state", callback(s, code, "other-state", flow))
	tampered := *flow
	tampered.Value = strings.Replace(flow.Value, ".", "x.", 1)
	refused("tampered cookie", callback(s, code, state, &tampered))
	refused("provider error", doAuth(s, authReq{method: "GET", path: "/auth/callback?error=access_denied&state=" + url.QueryEscape(state), cookie: flow}))
	if p.exchanges != before {
		t.Fatalf("code exchanged %d times before the flow was checked", p.exchanges-before)
	}

	// The flow lasts ten minutes.
	*now = now.Add(11 * time.Minute)
	refused("expired flow", callback(s, code, state, flow))

	// A flow from before a restart (another signing key) is refused.
	loc, flow = startOIDC(t, s)
	code, state = p.issue(t, loc, *now, nil)
	s2, _ := oidcServer(t, p, "admin@example.com")
	s2.clock = s.clock
	refused("other key", callback(s2, code, state, flow))

	// A wrong client secret fails the exchange.
	wrongSecret := oidcEnv(p, "admin@example.com")
	wrongSecret["SWITCHBOARD_OIDC_CLIENT_SECRET"] = "wrong"
	if err := adminauth.Configure(&s.admin, fakeEnv(wrongSecret)); err != nil {
		t.Fatal(err)
	}
	loc, flow = startOIDC(t, s)
	code, state = p.issue(t, loc, *now, nil)
	refused("bad client secret", callback(s, code, state, flow))
}

func TestOIDCSessionEndsWhenRemovedFromAllowlist(t *testing.T) {
	p := newFakeIdP(t)
	s, now := oidcServer(t, p, "admin@example.com")
	loc, flow := startOIDC(t, s)
	code, state := p.issue(t, loc, *now, nil)
	sess := sessionFrom(t, callback(s, code, state, flow))
	// Restart with a new allowlist on the same database.
	if err := adminauth.Configure(&s.admin, fakeEnv(oidcEnv(p, "other@example.com"))); err != nil {
		t.Fatal(err)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: sess}); w.Code != 401 {
		t.Fatalf("me=%d", w.Code)
	}
}

func TestOIDCConfig(t *testing.T) {
	p := newFakeIdP(t)
	full := oidcEnv(p, "admin@example.com")
	if err := adminauth.Configure(&adminauth.Config{}, fakeEnv(full)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SWITCHBOARD_OIDC_ISSUER", "SWITCHBOARD_OIDC_CLIENT_ID", "SWITCHBOARD_OIDC_CLIENT_SECRET", "SWITCHBOARD_OIDC_ADMINS", "SWITCHBOARD_PUBLIC_URL"} {
		env := map[string]string{}
		for k, v := range full {
			if k != name {
				env[k] = v
			}
		}
		err := adminauth.Configure(&adminauth.Config{}, fakeEnv(env))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), p.clientSecret) {
			t.Errorf("secret in error: %v", err)
		}
	}
	bad := map[string]string{"SWITCHBOARD_OIDC_ADMINS": "nobody", "SWITCHBOARD_PUBLIC_URL": "mail.example", "SWITCHBOARD_OIDC_SCOPES": "email profile"}
	for k, v := range bad {
		env := map[string]string{}
		for k2, v2 := range full {
			env[k2] = v2
		}
		env[k] = v
		if err := adminauth.Configure(&adminauth.Config{}, fakeEnv(env)); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
	// GET /auth/login and /auth/callback are not served outside oidc mode.
	s, _, _ := builtinServer(t)
	for _, path := range []string{"/auth/login", "/auth/callback?code=x&state=y"} {
		if w := doAuth(s, authReq{method: "GET", path: path}); w.Code != 405 {
			t.Errorf("%s under builtin=%d", path, w.Code)
		}
	}
}

func TestOIDCLogsNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	p := newFakeIdP(t)
	s, now := oidcServer(t, p, "admin@example.com")
	for _, extra := range []map[string]any{nil, {"nonce": "x"}, {"email": "someone@example.com"}} {
		loc, flow := startOIDC(t, s)
		code, state := p.issue(t, loc, *now, extra)
		callback(s, code, state, flow)
	}
	wrongSecret := oidcEnv(p, "admin@example.com")
	wrongSecret["SWITCHBOARD_OIDC_CLIENT_SECRET"] = "wrong-secret-value"
	if err := adminauth.Configure(&s.admin, fakeEnv(wrongSecret)); err != nil {
		t.Fatal(err)
	}
	loc, flow := startOIDC(t, s)
	code, state := p.issue(t, loc, *now, nil)
	callback(s, code, state, flow)
	out := buf.String()
	for _, secret := range []string{p.clientSecret, "wrong-secret-value", "eyJ", "access_token"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log has %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "nonce mismatch") || !strings.Contains(out, "not on the allowlist") {
		t.Fatalf("log:\n%s", out)
	}
}
