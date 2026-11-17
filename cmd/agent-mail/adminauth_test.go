package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/agent-mail/internal/adminauth"
	"github.com/sandcorporation/agent-mail/internal/cli"
	"github.com/sandcorporation/agent-mail/internal/store"
)

// authHandler is the server's real routing for admin pages and endpoints.
func authHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/auth/", s.authAPI)
	mux.HandleFunc("/", s.root)
	return secureHeaders(mux)
}

type authReq struct {
	method, path, body string
	cookie             *http.Cookie
	headers            map[string]string
	ip                 string
}

func doAuth(s *Server, q authReq) *httptest.ResponseRecorder {
	r := httptest.NewRequest(q.method, q.path, strings.NewReader(q.body))
	if q.ip != "" {
		r.RemoteAddr = q.ip + ":4000"
	}
	if q.method == "POST" || q.method == "PUT" || q.method == "DELETE" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Switchboard-Dashboard", "1")
	}
	for k, v := range q.headers {
		r.Header.Set(k, v)
	}
	if q.cookie != nil {
		r.AddCookie(q.cookie)
	}
	w := httptest.NewRecorder()
	authHandler(s).ServeHTTP(w, r)
	return w
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie: %d %s", sessionCookie, w.Code, w.Body)
	return nil
}

// builtinServer is a test server in builtin mode with user alice and a
// clock the test moves.
func builtinServer(t *testing.T) (*Server, string, *time.Time) {
	t.Helper()
	s := testServer(t)
	if err := adminauth.Configure(&s.admin, fakeEnv(map[string]string{"SWITCHBOARD_ADMIN_AUTH": "builtin"})); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 9, 0, 10, 0, time.UTC)
	s.clock = func() time.Time { return now }
	secret := totpEncoding.EncodeToString([]byte("0123456789abcdefghij"))
	hash, err := adminauth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO admin_users(name,password_hash,totp_secret,created_at) VALUES('alice',?,?,'now')`, hash, secret); err != nil {
		t.Fatal(err)
	}
	return s, secret, &now
}

func codeAt(t *testing.T, secret string, at time.Time, offset int64) string {
	t.Helper()
	c, err := adminauth.TOTPCode(secret, at.Unix()/adminauth.TOTPStep+offset)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func loginBody(name, pw, code string) string {
	b, _ := json.Marshal(loginRequest{Name: name, Password: pw, Code: code})
	return string(b)
}

func TestAdminDefaultDeny(t *testing.T) {
	s := testServer(t)
	if err := adminauth.Configure(&s.admin, fakeEnv(nil)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/admin/roles", "/api/admin/me", "/api/admin/messages"} {
		if w := doAuth(s, authReq{method: "GET", path: path}); w.Code != 401 {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
	// Even a request carrying an admin header is refused: no secret is configured.
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/roles", headers: map[string]string{"X-Switchboard-Admin": ""}}); w.Code != 401 {
		t.Fatalf("empty admin header=%d", w.Code)
	}
	w := doAuth(s, authReq{method: "GET", path: "/"})
	body := w.Body.String()
	if w.Code != 401 || !strings.Contains(body, `type="password"`) || !strings.Contains(body, "인증 코드") || strings.Contains(body, "api/admin") {
		t.Fatalf("login page=%d %s", w.Code, body)
	}
	for _, banned := range []string{"http://", "https://", "<link", " src=", "innerHTML"} {
		if strings.Contains(body, banned) {
			t.Fatalf("login page uses %q", banned)
		}
	}
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", "123456")}); w.Code != 401 || strings.TrimSpace(w.Body.String()) != loginFailedText {
		t.Fatalf("login without mode=%d %s", w.Code, w.Body)
	}
}

// TestAdminTransitionRule: no SWITCHBOARD_ADMIN_AUTH but the old proxy secret
// set behaves as before #74: the page is served, the proxy header admits,
// and writes keep the dashboard guard.
func TestAdminTransitionRule(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if err := adminauth.Configure(&s.admin, fakeEnv(map[string]string{"AGENT_MAIL_ADMIN_PROXY_TOKEN": "proxy-secret"})); err != nil {
		t.Fatal(err)
	}
	if s.adminAuth().EffectiveMode() != adminauth.ModeLegacy {
		t.Fatalf("mode=%q", s.adminAuth().EffectiveMode())
	}
	w := doAuth(s, authReq{method: "GET", path: "/"})
	if w.Code != 200 || strings.Contains(w.Body.String(), `type="password"`) || !strings.Contains(w.Body.String(), `id="tabs"`) {
		t.Fatalf("dashboard page=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/roles"}); w.Code != 401 {
		t.Fatalf("no proxy header=%d", w.Code)
	}
	for _, h := range []string{"X-Switchboard-Admin", "X-Agent-Mail-Admin"} {
		if w := doAuth(s, authReq{method: "GET", path: "/api/admin/roles", headers: map[string]string{h: "proxy-secret"}}); w.Code != 200 {
			t.Fatalf("%s=%d", h, w.Code)
		}
	}
	w = doAuth(s, authReq{method: "GET", path: "/api/admin/me", headers: map[string]string{"X-Switchboard-Admin": "proxy-secret"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"proxy","mode":"legacy"`) {
		t.Fatalf("me=%d %s", w.Code, w.Body)
	}
	put := func(guard bool) int {
		h := map[string]string{"X-Switchboard-Admin": "proxy-secret"}
		if !guard {
			h["X-Switchboard-Dashboard"] = ""
		}
		return doAuth(s, authReq{method: "PUT", path: "/api/admin/question-settings/a", body: `{"discord":false}`, headers: h}).Code
	}
	if c := put(false); c != 403 {
		t.Fatalf("write without guard=%d", c)
	}
	if c := put(true); c == 401 || c == 403 {
		t.Fatalf("guarded write=%d", c)
	}
	// A session cookie means nothing here, and there is no built-in login.
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "x", "1")}); w.Code != 401 {
		t.Fatalf("login=%d", w.Code)
	}
}

func TestAdminAuthModeConfig(t *testing.T) {
	for v, ok := range map[string]bool{"builtin": true, "oidc": false, "proxy": false, "Builtin": false} {
		err := adminauth.Configure(&adminauth.Config{}, fakeEnv(map[string]string{"SWITCHBOARD_ADMIN_AUTH": v}))
		if (err == nil) != ok {
			t.Errorf("%s: %v", v, err)
		}
	}
	// A set mode overrides the transition rule: the proxy secret alone no longer admits.
	s, _, _ := builtinServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/roles", headers: map[string]string{"X-Switchboard-Admin": "proxy-secret"}}); w.Code != 401 {
		t.Fatalf("proxy secret under builtin=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/"}); w.Code != 401 {
		t.Fatalf("page under builtin without session=%d", w.Code)
	}
}

func TestBuiltinLoginSessionAndLogout(t *testing.T) {
	s, secret, now := builtinServer(t)
	w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0))})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"alice","mode":"builtin"`) {
		t.Fatalf("login=%d %s", w.Code, w.Body)
	}
	c := sessionFrom(t, w)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie flags %+v", c)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT id_hash FROM admin_sessions WHERE user='alice'`).Scan(&stored); err != nil || stored == c.Value || stored != store.TokenHash(c.Value) {
		t.Fatalf("stored session id %q %v", stored, err)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}); w.Code != 200 || !strings.Contains(w.Body.String(), `"user":"alice"`) {
		t.Fatalf("me=%d %s", w.Code, w.Body)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/", cookie: c}); w.Code != 200 || !strings.Contains(w.Body.String(), `id="logout"`) {
		t.Fatalf("dashboard=%d", w.Code)
	}
	// Writes keep the dashboard guard under a session.
	if w := doAuth(s, authReq{method: "PUT", path: "/api/admin/question-settings/a", body: `{}`, cookie: c, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}}); w.Code != 403 {
		t.Fatalf("cross-site write=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: &http.Cookie{Name: sessionCookie, Value: "forged"}}); w.Code != 401 {
		t.Fatalf("forged cookie=%d", w.Code)
	}
	// Sliding expiry: 11h of activity apart keeps the session; 12h idle ends it.
	*now = now.Add(11 * time.Hour)
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}); w.Code != 200 {
		t.Fatalf("after 11h=%d", w.Code)
	}
	*now = now.Add(11 * time.Hour)
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}); w.Code != 200 {
		t.Fatalf("after 22h with activity=%d", w.Code)
	}
	*now = now.Add(12*time.Hour + time.Second)
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}); w.Code != 401 {
		t.Fatalf("after 12h idle=%d", w.Code)
	}
	// Logout deletes the session and clears the cookie.
	w = doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0))})
	c = sessionFrom(t, w)
	if w := doAuth(s, authReq{method: "POST", path: "/auth/logout", cookie: c, headers: map[string]string{"X-Switchboard-Dashboard": ""}}); w.Code != 403 {
		t.Fatalf("unguarded logout=%d", w.Code)
	}
	w = doAuth(s, authReq{method: "POST", path: "/auth/logout", body: "{}", cookie: c})
	if cleared := sessionFrom(t, w); w.Code != 204 || cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("logout=%d %+v", w.Code, cleared)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}); w.Code != 401 {
		t.Fatalf("after logout=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/auth/login"}); w.Code != 405 {
		t.Fatalf("GET login=%d", w.Code)
	}
}

func TestInsecureCookieFlag(t *testing.T) {
	s, secret, now := builtinServer(t)
	if err := adminauth.Configure(&s.admin, fakeEnv(map[string]string{"SWITCHBOARD_ADMIN_AUTH": "builtin", "SWITCHBOARD_INSECURE_COOKIE": "1"})); err != nil {
		t.Fatal(err)
	}
	c := sessionFrom(t, doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0))}))
	if c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("insecure cookie flags %+v", c)
	}
}

func TestBuiltinLoginFailuresReadTheSame(t *testing.T) {
	s, secret, now := builtinServer(t)
	good := codeAt(t, secret, *now, 0)
	cases := map[string]string{
		"wrong password": loginBody("alice", "wrong password!!", good),
		"wrong code":     loginBody("alice", "correct horse battery", "000000"),
		"no such user":   loginBody("bob", "correct horse battery", good),
		"short code":     loginBody("alice", "correct horse battery", good[:5]),
		"bad json":       `{"name":`,
	}
	ip := 10
	for name, body := range cases {
		ip++ // a fresh IP each, so the rate limit stays out of this test
		w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: body, ip: "192.0.2." + itoa(int64(ip))})
		if w.Code != 401 || w.Body.String() != loginFailedText+"\n" || len(w.Result().Cookies()) != 0 {
			t.Errorf("%s: %d %q", name, w.Code, w.Body)
		}
	}
	// ±1 step is accepted; the same step again is a replay.
	prev := codeAt(t, secret, *now, -1)
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", prev), ip: "192.0.2.50"}); w.Code != 200 {
		t.Fatalf("previous step=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", prev), ip: "192.0.2.51"}); w.Code != 401 || w.Body.String() != loginFailedText+"\n" {
		t.Fatalf("replay=%d %q", w.Code, w.Body)
	}
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 2)), ip: "192.0.2.52"}); w.Code != 401 {
		t.Fatalf("two steps ahead=%d", w.Code)
	}
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", good), ip: "192.0.2.53"}); w.Code != 200 {
		t.Fatalf("current step after previous=%d", w.Code)
	}
	// A disabled user cannot log in and loses existing sessions.
	*now = now.Add(time.Minute)
	c := sessionFrom(t, doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0)), ip: "192.0.2.54"}))
	if _, err := s.db.Exec(`UPDATE admin_users SET disabled_at='now' WHERE name='alice'`); err != nil {
		t.Fatal(err)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}); w.Code != 401 {
		t.Fatalf("disabled user's session=%d", w.Code)
	}
	*now = now.Add(time.Minute)
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0)), ip: "192.0.2.55"}); w.Code != 401 || w.Body.String() != loginFailedText+"\n" {
		t.Fatalf("disabled login=%d", w.Code)
	}
}

func TestBuiltinLoginRateLimit(t *testing.T) {
	s, secret, now := builtinServer(t)
	login := func(name, pw, ip string) int {
		return doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody(name, pw, codeAt(t, secret, *now, 0)), ip: ip}).Code
	}
	// Per name: 5 failures from different IPs block even the right password.
	for i := 0; i < 5; i++ {
		if c := login("alice", "wrong password!!", "198.51.100."+itoa(int64(i+1))); c != 401 {
			t.Fatal(c)
		}
	}
	if c := login("alice", "correct horse battery", "198.51.100.99"); c != 401 {
		t.Fatalf("name limit=%d", c)
	}
	*now = now.Add(loginFailWindow + time.Second)
	if c := login("alice", "correct horse battery", "198.51.100.99"); c != 200 {
		t.Fatalf("after window=%d", c)
	}
	// Per IP: 5 failures over different names block that IP only.
	*now = now.Add(time.Minute)
	for i := 0; i < 5; i++ {
		login("nobody"+itoa(int64(i)), "wrong password!!", "203.0.113.7")
	}
	if c := login("alice", "correct horse battery", "203.0.113.7"); c != 401 {
		t.Fatalf("ip limit=%d", c)
	}
	if c := login("alice", "correct horse battery", "203.0.113.8"); c != 200 {
		t.Fatalf("other ip=%d", c)
	}
	// With SWITCHBOARD_TRUST_FORWARDED the last X-Forwarded-For entry is the client.
	s.admin.TrustForwarded = true
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("X-Forwarded-For", "1.1.1.1, 203.0.113.9")
	if ip := s.adminAuth().ClientIP(r); ip != "203.0.113.9" {
		t.Fatalf("forwarded ip=%q", ip)
	}
}

func adminCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	_, err := adminauth.CLI(args[0], args[1], slices.Contains(args[2:], "--json"), strings.NewReader(stdin), &out, &errOut)
	return out.String(), err
}

func TestAdminCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "mail.db")
	t.Setenv("DB_PATH", path)
	out, err := adminCLI(t, "correct horse battery\n", "create", "alice")
	if err != nil || !strings.Contains(out, "created alice") || !strings.Contains(out, "otpauth://totp/Switchboard:alice?") || !strings.Contains(out, "secret (shown once): ") {
		t.Fatalf("create: %v %s", err, out)
	}
	if _, err = adminCLI(t, "correct horse battery\n", "create", "alice"); err == nil {
		t.Fatal("duplicate create accepted")
	}
	if _, err = adminCLI(t, "short\n", "create", "bob"); err == nil || !strings.Contains(err.Error(), "at least 12") {
		t.Fatalf("short password: %v", err)
	}
	out, err = adminCLI(t, "another long password\n", "create", "bob", "--json")
	var created map[string]string
	if err != nil || json.Unmarshal([]byte(out), &created) != nil || created["name"] != "bob" || created["secret"] == "" || !strings.HasPrefix(created["otpauth_uri"], "otpauth://totp/") {
		t.Fatalf("create --json: %v %s", err, out)
	}
	var ue cli.UsageError
	if _, err = adminCLI(t, "", "rename", "bob"); !errors.As(err, &ue) {
		t.Fatalf("unknown action: %v", err)
	}
	if _, err = adminCLI(t, "", "disable", "bad name"); !errors.As(err, &ue) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err = adminCLI(t, "", "disable", "carol"); err == nil {
		t.Fatal("disable of missing user accepted")
	}

	// A server on the same database signs bob in; each CLI change ends his sessions.
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, admin: adminauth.Config{Mode: adminauth.ModeBuiltin}}
	now := time.Now()
	s.clock = func() time.Time { return now }
	login := func(pw, secret string) (*http.Cookie, int) {
		now = now.Add(time.Minute)
		w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("bob", pw, codeAt(t, secret, now, 0))})
		if w.Code != 200 {
			return nil, w.Code
		}
		return sessionFrom(t, w), 200
	}
	alive := func(c *http.Cookie) bool {
		return doAuth(s, authReq{method: "GET", path: "/api/admin/me", cookie: c}).Code == 200
	}
	c, code := login("another long password", created["secret"])
	if code != 200 || !alive(c) {
		t.Fatalf("login with created user=%d", code)
	}
	if out, err = adminCLI(t, "a changed password\n", "passwd", "bob"); err != nil || alive(c) {
		t.Fatalf("passwd: %v %s alive=%v", err, out, alive(c))
	}
	if _, code = login("another long password", created["secret"]); code != 401 {
		t.Fatalf("old password=%d", code)
	}
	c, code = login("a changed password", created["secret"])
	if code != 200 {
		t.Fatalf("new password=%d", code)
	}
	out, err = adminCLI(t, "", "totp-reset", "bob", "--json")
	var reset map[string]string
	if err != nil || json.Unmarshal([]byte(out), &reset) != nil || reset["secret"] == "" || reset["secret"] == created["secret"] || alive(c) {
		t.Fatalf("totp-reset: %v %s", err, out)
	}
	if _, code = login("a changed password", created["secret"]); code != 401 {
		t.Fatalf("old secret=%d", code)
	}
	c, code = login("a changed password", reset["secret"])
	if code != 200 {
		t.Fatalf("new secret=%d", code)
	}
	if out, err = adminCLI(t, "", "disable", "bob"); err != nil || !strings.Contains(out, "disabled bob") || alive(c) {
		t.Fatalf("disable: %v %s", err, out)
	}
	if _, code = login("a changed password", reset["secret"]); code != 401 {
		t.Fatalf("disabled login=%d", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestAdminCommandNeedsNoClientConfig: admin runs without a profile or
// endpoint, and its usage errors go through the usual CLI path.
func TestAdminCommandNeedsNoClientConfig(t *testing.T) {
	if code := cli.Run([]string{"admin", "create"}, version, adminauth.CLI); code != cli.ExitUsage {
		t.Fatalf("missing name: %v", code)
	}
	if code := cli.Run([]string{"admin", "frobnicate", "alice"}, version, adminauth.CLI); code != cli.ExitUsage {
		t.Fatalf("bad action: %v", code)
	}
}
