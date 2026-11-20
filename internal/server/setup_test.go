package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/adminauth"
)

// setupServer is up's builtin instance with no admin yet and the setup
// opened in a temporary data directory, served on a loopback listener.
func setupServer(t *testing.T) (*Server, *httptest.Server, string, *time.Time) {
	t.Helper()
	s := testServer(t)
	if err := adminauth.Configure(&s.admin, fakeEnv(map[string]string{"SWITCHBOARD_ADMIN_AUTH": "builtin", "SWITCHBOARD_INSECURE_COOKIE": "1"})); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 9, 0, 10, 0, time.UTC)
	s.clock = func() time.Time { return now }
	dir := t.TempDir()
	if err := s.openSetup(dir); err != nil {
		t.Fatal(err)
	}
	if s.setup == nil {
		t.Fatal("setup not opened on an instance without an admin")
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return s, srv, dir, &now
}

func setupCode(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, adminauth.SetupCodeFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// setupPost posts body as the setup page does and returns the status and
// the decoded JSON answer.
func setupPost(t *testing.T, c *http.Client, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Switchboard-Dashboard", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func getStatus(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestSetupCreatesFirstAdminAfterTOTPAndCloses(t *testing.T) {
	s, srv, dir, now := setupServer(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	code := setupCode(t, dir)
	if fi, err := os.Stat(filepath.Join(dir, adminauth.SetupCodeFile)); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("code file %v %v", fi, err)
	}

	if st := getStatus(t, c, srv.URL+"/setup"); st != 200 {
		t.Fatalf("GET /setup=%d", st)
	}
	if st, out := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": "AAAA-AAAA-AAAA-AAAA", "name": "owner", "password": "a long enough password"}); st != 403 || out["error"] != "code" {
		t.Fatalf("wrong code=%d %v", st, out)
	}
	if st, out := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": code, "name": "owner", "password": "short"}); st != 400 || out["error"] != "password" {
		t.Fatalf("short password=%d %v", st, out)
	}
	if st, out := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": code, "name": "bad name!", "password": "a long enough password"}); st != 400 || out["error"] != "name" {
		t.Fatalf("bad name=%d %v", st, out)
	}
	// The code is accepted lower case and without dashes, as typed.
	typed := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	st, begun := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": typed, "name": "owner", "password": "a long enough password"})
	if st != 200 {
		t.Fatalf("begin=%d %v", st, begun)
	}
	secret, _ := begun["secret"].(string)
	uri, _ := begun["otpauth_uri"].(string)
	rows, _ := begun["qr"].([]any)
	if secret == "" || !strings.HasPrefix(uri, "otpauth://totp/Switchboard:owner?") || !strings.Contains(uri, "secret="+secret) || len(rows) < 21 || len(rows[0].(string)) != len(rows) {
		t.Fatalf("begin answer %v", begun)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("admin created before the TOTP confirmation: %d %v", n, err)
	}
	if st, out := setupPost(t, c, srv.URL+"/setup/confirm", map[string]string{"code": code, "totp": "000000"}); st != 400 || out["error"] != "totp" {
		t.Fatalf("wrong totp=%d %v", st, out)
	}
	if st, out := setupPost(t, c, srv.URL+"/setup/confirm", map[string]string{"code": "AAAA-AAAA-AAAA-AAAA", "totp": totpAt(t, secret, *now)}); st != 403 || out["error"] != "code" {
		t.Fatalf("confirm with a wrong code=%d %v", st, out)
	}
	if st, out := setupPost(t, c, srv.URL+"/setup/confirm", map[string]string{"code": code, "totp": totpAt(t, secret, *now)}); st != 200 || out["user"] != "owner" {
		t.Fatalf("confirm=%d %v", st, out)
	}
	// Signed in by the confirmation.
	if st := getStatus(t, c, srv.URL+"/"); st != 200 {
		t.Fatalf("dashboard after setup=%d", st)
	}
	// Closed for good.
	for _, p := range []string{"/setup", "/setup/begin"} {
		if st := getStatus(t, c, srv.URL+p); st != 404 {
			t.Errorf("GET %s after setup=%d", p, st)
		}
	}
	if st, _ := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": code, "name": "second", "password": "a long enough password"}); st != 404 {
		t.Errorf("begin after setup=%d", st)
	}
	if _, err := os.Stat(filepath.Join(dir, adminauth.SetupCodeFile)); !os.IsNotExist(err) {
		t.Errorf("code file left after setup: %v", err)
	}
	// The confirmed step counts as used: the same code cannot sign in again.
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: `{"name":"owner","password":"a long enough password","code":"` + totpAt(t, secret, *now) + `"}`}); w.Code != 401 {
		t.Errorf("replayed setup code login=%d", w.Code)
	}
	*now = now.Add(30 * time.Second)
	if w := doAuth(s, authReq{method: "POST", path: "/auth/login", body: `{"name":"owner","password":"a long enough password","code":"` + totpAt(t, secret, *now) + `"}`}); w.Code != 200 {
		t.Errorf("login with the new admin=%d %s", w.Code, w.Body)
	}
}

func TestSetupRefusesNonLoopback(t *testing.T) {
	s, srv, dir, _ := setupServer(t)
	code := setupCode(t, dir)
	// A peer that is not loopback (as behind a port forward).
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/setup", nil)
	r.RemoteAddr = "192.0.2.10:4000"
	s.handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("remote peer=%d", w.Code)
	}
	c := &http.Client{}
	for name, h := range map[string]map[string]string{
		"Host":            {"Host": "pc.example"}, // DNS rebinding to 127.0.0.1
		"X-Forwarded-For": {"X-Forwarded-For": "192.0.2.10"},
		"Forwarded":       {"Forwarded": "for=192.0.2.10"},
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/setup", nil)
		for k, v := range h {
			if k == "Host" {
				req.Host = v
			} else {
				req.Header.Set(k, v)
			}
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("%s=%d", name, resp.StatusCode)
		}
	}
	// The write guard of the dashboard applies too.
	req, _ := http.NewRequest("POST", srv.URL+"/setup/begin", strings.NewReader(`{"code":"`+code+`","name":"owner","password":"a long enough password"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("begin without the dashboard header=%d", resp.StatusCode)
	}
}

func TestSetupWrongCodesReplaceTheCode(t *testing.T) {
	_, srv, dir, _ := setupServer(t)
	c := &http.Client{}
	first := setupCode(t, dir)
	for i := range 5 {
		if st, _ := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": "WRNG-WRNG-WRNG-WRN" + string(rune('A'+i)), "name": "owner", "password": "a long enough password"}); st != 403 {
			t.Fatalf("wrong code %d=%d", i, st)
		}
	}
	second := setupCode(t, dir)
	if second == first {
		t.Fatal("five wrong codes did not replace the code")
	}
	if st, _ := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": first, "name": "owner", "password": "a long enough password"}); st != 403 {
		t.Errorf("old code=%d", st)
	}
	if st, _ := setupPost(t, c, srv.URL+"/setup/begin", map[string]string{"code": second, "name": "owner", "password": "a long enough password"}); st != 200 {
		t.Errorf("new code=%d", st)
	}
}

func TestSetupClosesOnceAnAdminExists(t *testing.T) {
	s, srv, dir, _ := setupServer(t)
	c := &http.Client{}
	// `admin create` from the CLI while the page is open.
	if _, err := s.db.Exec(`INSERT INTO admin_users(name,password_hash,totp_secret,created_at) VALUES('cli','x','y','now')`); err != nil {
		t.Fatal(err)
	}
	if st := getStatus(t, c, srv.URL+"/setup"); st != 404 {
		t.Errorf("GET /setup with an admin=%d", st)
	}
	if _, err := os.Stat(filepath.Join(dir, adminauth.SetupCodeFile)); !os.IsNotExist(err) {
		t.Errorf("code file left: %v", err)
	}
	// The next start of up leaves it closed and removes a stale file.
	if err := os.WriteFile(filepath.Join(dir, adminauth.SetupCodeFile), []byte("STALE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.setup = nil
	if err := s.openSetup(dir); err != nil || s.setup != nil {
		t.Fatalf("reopened with an admin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, adminauth.SetupCodeFile)); !os.IsNotExist(err) {
		t.Errorf("stale code file left: %v", err)
	}
}

// TestSetupNeverOutsideUpBuiltin pins where the setup page exists: the
// plain server (Run, the operations deployment) never opens it, even with
// builtin admin login, hosting on and no admin; neither does up in the oidc
// mode.
func TestSetupNeverOutsideUpBuiltin(t *testing.T) {
	s, _, _ := builtinServer(t)
	if _, err := s.db.Exec(`DELETE FROM admin_users`); err != nil {
		t.Fatal(err)
	}
	s.hosting = true
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	for _, p := range []string{"/setup", "/setup/begin"} {
		if st := getStatus(t, &http.Client{}, srv.URL+p); st != 404 {
			t.Errorf("serve GET %s=%d", p, st)
		}
	}
	if st, _ := setupPost(t, &http.Client{}, srv.URL+"/setup/begin", map[string]string{"code": "x"}); st != 404 {
		t.Errorf("serve POST /setup/begin=%d", st)
	}

	p := newFakeIdP(t)
	o := testServer(t)
	if err := adminauth.Configure(&o.admin, fakeEnv(oidcEnv(p, "admin@example.com"))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := o.openSetup(dir); err != nil || o.setup != nil {
		t.Fatalf("up under oidc opened setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, adminauth.SetupCodeFile)); !os.IsNotExist(err) {
		t.Errorf("up under oidc wrote a code: %v", err)
	}
}

func totpAt(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	c, err := adminauth.TOTPCode(secret, now.Unix()/adminauth.TOTPStep)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
