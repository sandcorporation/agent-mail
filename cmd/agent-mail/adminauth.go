package main

// Admin login (#71 §1). Every /api/admin/* request and the dashboard page go
// through adminIdentify. SWITCHBOARD_ADMIN_AUTH picks the mode:
//
//   - unset, no legacy secret: default deny. /api/admin/* answers 401 and the
//     dashboard shows the login page, which cannot sign anyone in.
//   - unset, legacy ADMIN_PROXY_TOKEN set (transition rule): exactly the old
//     behaviour. The reverse proxy injects X-Switchboard-Admin, the dashboard
//     page is served as is, and no login page or session is involved.
//   - builtin: name + password + TOTP from admin_users (adminusers.go).
//   - oidc: an OpenID Connect provider and an allowlist (oidc.go).
//
// Every mode other than the transition rule ends in the same session
// (admin_sessions, cookie sb_session). A new mode (oidc #75, proxy #76) only
// adds the step that proves who the user is and then calls startSession; the
// session check below stays the same. Register it in adminModes.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/agent-mail/internal/config"
	"github.com/sandcorporation/agent-mail/internal/store"
)

const (
	adminModeBuiltin = "builtin"
	adminModeLegacy  = "legacy" // transition rule: unset mode + ADMIN_PROXY_TOKEN

	sessionCookie     = "sb_session"
	defaultSessionTTL = 12 * time.Hour
	// sessionSlide is how stale expires_at may get before a request extends
	// it, so a busy dashboard does not write on every call.
	sessionSlide = time.Minute

	loginFailLimit  = 5
	loginFailWindow = 15 * time.Minute
	loginFailedText = "login failed"
)

// adminModes lists the SWITCHBOARD_ADMIN_AUTH values this build can sign
// users in with. proxy (#76) is added with its login step.
var adminModes = map[string]bool{adminModeBuiltin: true, adminModeOIDC: true}

// adminIdentity is who made an admin request.
type adminIdentity struct {
	User string `json:"user"`
	Mode string `json:"mode"`
}

// configureAdminAuth reads SWITCHBOARD_ADMIN_AUTH, SWITCHBOARD_INSECURE_COOKIE
// and SWITCHBOARD_TRUST_FORWARDED, and logs which admin access is in force.
func configureAdminAuth(s *Server, getenv func(string) string) error {
	mode := strings.TrimSpace(getenv(config.EnvPrefix + "ADMIN_AUTH"))
	if mode != "" && !adminModes[mode] {
		return errors.New(config.EnvPrefix + "ADMIN_AUTH must be builtin or oidc (proxy is not available in this version)")
	}
	if mode == adminModeOIDC {
		o, err := configureOIDC(getenv)
		if err != nil {
			return err
		}
		s.oidc = o
	}
	s.adminMode = mode
	s.insecureCookie = getenv(config.EnvPrefix+"INSECURE_COOKIE") == "1"
	s.trustForwarded = getenv(config.EnvPrefix+"TRUST_FORWARDED") == "1"
	switch {
	case mode == "" && s.adminProxyToken != "":
		log.Printf("deprecated: admin access through the proxy secret (ADMIN_PROXY_TOKEN) without %sADMIN_AUTH; set %sADMIN_AUTH to move to admin login", config.EnvPrefix, config.EnvPrefix)
	case mode == "":
		log.Printf("admin access denied: %sADMIN_AUTH is not set", config.EnvPrefix)
	case s.adminProxyToken != "":
		log.Printf("%sADMIN_AUTH=%s: the proxy secret (ADMIN_PROXY_TOKEN) is ignored", config.EnvPrefix, mode)
	}
	return nil
}

// effectiveAdminMode is the mode in force: the configured one, the transition
// rule, or "" (deny).
func (s *Server) effectiveAdminMode() string {
	if s.adminMode == "" && s.adminProxyToken != "" {
		return adminModeLegacy
	}
	return s.adminMode
}

// adminIdentify returns who is making an admin request, or false. It slides
// the session's expiry.
func (s *Server) adminIdentify(r *http.Request) (adminIdentity, bool) {
	switch s.effectiveAdminMode() {
	case "":
		return adminIdentity{}, false
	case adminModeLegacy:
		provided := s.header(r, "Admin")
		if len(provided) != len(s.adminProxyToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.adminProxyToken)) != 1 {
			return adminIdentity{}, false
		}
		return adminIdentity{User: "proxy", Mode: adminModeLegacy}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return adminIdentity{}, false
	}
	return s.lookupSession(c.Value)
}

func (s *Server) sessionTTL() time.Duration {
	if s.adminSessionTTL > 0 {
		return s.adminSessionTTL
	}
	return defaultSessionTTL
}

// startSession stores a new session for user and sets the cookie. Only the
// SHA-256 of the cookie value is kept.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user, mode string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if _, err := s.db.Exec(`DELETE FROM admin_sessions WHERE expires_at<=?`, now.Unix()); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO admin_sessions(id_hash,user,mode,created_at,expires_at,ip,user_agent) VALUES(?,?,?,?,?,?,?)`,
		store.TokenHash(id), user, mode, now.Unix(), now.Add(s.sessionTTL()).Unix(), s.clientIP(r), ua); err != nil {
		return err
	}
	http.SetCookie(w, s.sessionCookie(id, 0))
	return nil
}

func (s *Server) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: !s.insecureCookie, SameSite: http.SameSiteStrictMode}
}

// lookupSession finds a live session for the cookie value. A session of a
// built-in user ends when that user is disabled or gone.
func (s *Server) lookupSession(id string) (adminIdentity, bool) {
	h := store.TokenHash(id)
	now := s.now()
	var who adminIdentity
	var expires int64
	err := s.db.QueryRow(`SELECT s.user,s.mode,s.expires_at FROM admin_sessions s
LEFT JOIN admin_users u ON s.mode='builtin' AND u.name=s.user
WHERE s.id_hash=? AND (s.mode<>'builtin' OR (u.name IS NOT NULL AND u.disabled_at IS NULL))`, h).Scan(&who.User, &who.Mode, &expires)
	if err != nil || expires <= now.Unix() || who.Mode != s.adminMode {
		return adminIdentity{}, false
	}
	if who.Mode == adminModeOIDC && (s.oidc == nil || !s.oidc.allowed(who.User)) {
		return adminIdentity{}, false
	}
	next := now.Add(s.sessionTTL()).Unix()
	if next-expires >= int64(sessionSlide/time.Second) {
		_, _ = s.db.Exec(`UPDATE admin_sessions SET expires_at=? WHERE id_hash=?`, next, h)
	}
	return who, true
}

// clientIP is the peer address, or with SWITCHBOARD_TRUST_FORWARDED=1 the
// last X-Forwarded-For entry (the one the nearest proxy added).
func (s *Server) clientIP(r *http.Request) string {
	if s.trustForwarded {
		if xf := r.Header.Values("X-Forwarded-For"); len(xf) > 0 {
			parts := strings.Split(xf[len(xf)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter counts failed logins per key (name or IP) in a sliding window.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (l *loginLimiter) prune(key string, now time.Time) []time.Time {
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if now.Sub(t) < loginFailWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

func (l *loginLimiter) blocked(now time.Time, keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if len(l.prune(k, now)) >= loginFailLimit {
			return true
		}
	}
	return false
}

func (l *loginLimiter) fail(now time.Time, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string][]time.Time{}
	}
	for _, k := range keys {
		l.fails[k] = append(l.prune(k, now), now)
	}
}

func (l *loginLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// authAPI serves POST /auth/login and POST /auth/logout. Both need the same
// write guard as admin writes. GET /auth/login and GET /auth/callback belong
// to the oidc mode.
func (s *Server) authAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/auth/login" || r.URL.Path == oidcCallbackPath) {
		s.oidcAuth(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/auth/login", "/auth/logout":
	default:
		http.NotFound(w, r)
		return
	}
	if !adminWriteAllowed(r, s.header(r, "Dashboard")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/auth/logout" {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			_, _ = s.db.Exec(`DELETE FROM admin_sessions WHERE id_hash=?`, store.TokenHash(c.Value))
		}
		http.SetCookie(w, s.sessionCookie("", -1))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.login(w, r)
}

type loginRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

// login checks name, password and TOTP for the built-in mode. Every failure,
// including the rate limit and a mode without built-in login, answers the
// same 401 body.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	fail := func() { http.Error(w, loginFailedText, http.StatusUnauthorized) }
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Name == "" || len(req.Name) > 64 || len(req.Password) > 1024 {
		fail()
		return
	}
	if s.adminMode != adminModeBuiltin {
		fail()
		return
	}
	now := s.now()
	nameKey, ipKey := "name:"+req.Name, "ip:"+s.clientIP(r)
	if s.loginLimits.blocked(now, nameKey, ipKey) {
		fail()
		return
	}
	if !s.checkBuiltinLogin(req, now) {
		s.loginLimits.fail(now, nameKey, ipKey)
		fail()
		return
	}
	s.loginLimits.clear(nameKey)
	if err := s.startSession(w, r, req.Name, adminModeBuiltin); err != nil {
		http.Error(w, "session failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, adminIdentity{User: req.Name, Mode: adminModeBuiltin})
}

// root serves the dashboard to a signed-in admin (or anyone under the
// transition rule, where the proxy guards the page) and the login page with
// 401 otherwise.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.effectiveAdminMode() == adminModeLegacy {
		dashboard(w, r)
		return
	}
	if _, ok := s.adminIdentify(r); ok {
		w.Header().Set("Cache-Control", "no-store")
		dashboard(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	if s.adminMode == adminModeOIDC {
		_, _ = io.WriteString(w, oidcLoginHTML)
		return
	}
	_, _ = io.WriteString(w, loginHTML)
}

// loginHead is the head shared by the login pages.
const loginHead = `<!doctype html>
<html lang="ko">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>` + productName + ` 로그인</title>
<style>
:root{color-scheme:light dark;--bg:#f4f6f9;--surface:#fff;--border:#dfe4ec;--border-strong:#c5cdd9;--text:#18202e;--muted:#5f6b7c;--accent:#2457c5;--accent-fg:#fff;--error:#c0302b;--error-bg:#fceaea;--r:8px;--r-sm:5px}
@media (prefers-color-scheme:dark){:root{--bg:#0f1319;--surface:#171c24;--border:#2b3340;--border-strong:#3b4555;--text:#e4e8ef;--muted:#97a1b0;--accent:#7aa5ff;--accent-fg:#0f1319;--error:#ff7b74;--error-bg:#3a1c1c}}
*,*::before,*::after{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:flex-start;justify-content:center;padding:12vh 1rem 2rem;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans KR","Apple SD Gothic Neo","Malgun Gothic",sans-serif}
form,.box{width:100%;max-width:22rem;background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:1.5rem;display:flex;flex-direction:column;gap:.9rem}
h1{font-size:1.2rem;font-weight:650;margin:0 0 .25rem}
label{display:flex;flex-direction:column;gap:.3rem;font-size:.9rem;color:var(--muted)}
input{font:inherit;color:var(--text);padding:.5rem .6rem;border:1px solid var(--border-strong);border-radius:var(--r-sm);background:var(--surface);width:100%;min-width:0}
button{font:inherit;padding:.55rem .75rem;border:1px solid var(--accent);border-radius:var(--r-sm);background:var(--accent);color:var(--accent-fg);cursor:pointer}
button:disabled{opacity:.6;cursor:default}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
#err{margin:0;padding:.5rem .6rem;border-radius:var(--r-sm);background:var(--error-bg);color:var(--error);font-size:.9rem}
#err:empty{display:none}
a.button{display:block;text-align:center;text-decoration:none;font:inherit;padding:.55rem .75rem;border:1px solid var(--accent);border-radius:var(--r-sm);background:var(--accent);color:var(--accent-fg)}
</style>
</head>
`

// oidcLoginHTML is the login page of the oidc mode: one link to the provider.
const oidcLoginHTML = loginHead + `<body>
<main class="box">
<h1>` + productName + `</h1>
<a class="button" href="/auth/login">로그인</a>
</main>
</body>
</html>
`

// loginHTML is the login page. It loads nothing from outside the page.
const loginHTML = loginHead + `<body>
<form id="login" autocomplete="on">
<h1>` + productName + `</h1>
<label>이름<input id="name" name="username" autocomplete="username" required maxlength="64" autocapitalize="none" spellcheck="false"></label>
<label>비밀번호<input id="password" name="password" type="password" autocomplete="current-password" required></label>
<label>인증 코드<input id="code" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required></label>
<p id="err" role="alert"></p>
<button type="submit" id="submit">로그인</button>
</form>
<script>
(function(){
const $=function(id){return document.getElementById(id)},f=$('login'),err=$('err'),btn=$('submit');
f.addEventListener('submit',async function(ev){
  ev.preventDefault();err.textContent='';btn.disabled=true;
  const body={name:$('name').value,password:$('password').value,code:$('code').value};
  try{
    const r=await fetch('/auth/login',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-Switchboard-Dashboard':'1'},body:JSON.stringify(body)});
    if(r.ok){location.replace('/'+location.hash);return}
    err.textContent='로그인하지 못했습니다';
  }catch(e){err.textContent='네트워크 오류'}
  $('code').value='';btn.disabled=false;
});
})();
</script>
</body>
</html>
`
