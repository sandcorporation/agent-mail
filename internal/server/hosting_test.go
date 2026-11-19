package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hostingPaths are the hosting routes (#173 API) a request may name.
var hostingPaths = []struct{ method, path string }{
	{"GET", "/api/admin/terminals"},
	{"POST", "/api/admin/terminals"},
	{"PUT", "/api/admin/terminals/7"},
	{"POST", "/api/admin/terminals/7/restart"},
	{"POST", "/api/admin/terminals/7/new-conversation"},
	{"DELETE", "/api/admin/terminals/7"},
	{"POST", "/api/admin/terminals/7/ticket"},
	{"GET", "/api/admin/terminals/7/socket"},
	{"GET", "/api/admin/terminals/7/log"},
	{"GET", "/api/admin/nodes"},
	{"DELETE", "/api/admin/nodes/pc"},
	{"GET", "/api/node/socket"},
	{"POST", "/api/node/join"},
}

// unknownTwin is a route nobody serves, under the same prefix as path.
func unknownTwin(path string) string {
	if strings.HasPrefix(path, "/api/node/") {
		return "/api/nowhere/socket"
	}
	return "/api/admin/nowhere" + strings.TrimPrefix(strings.TrimPrefix(path, "/api/admin/terminals"), "/api/admin/nodes")
}

type httpReply struct {
	code int
	body string
}

func send(t *testing.T, base, method, path string, cookie *http.Cookie, headers map[string]string) httpReply {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Switchboard-Dashboard", "1")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return httpReply{res.StatusCode, string(b)}
}

// signedIn is a builtin-mode server served over real HTTP and a session
// cookie for alice.
func signedIn(t *testing.T, hosting bool) (string, *http.Cookie) {
	t.Helper()
	_, base, c := signedInServer(t, hosting)
	return base, c
}

// signedInServer is signedIn with the server itself.
func signedInServer(t *testing.T, hosting bool) (*Server, string, *http.Cookie) {
	t.Helper()
	s, secret, now := builtinServer(t)
	s.hosting = hosting
	c := sessionFrom(t, doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0))}))
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return s, srv.URL, c
}

func TestHostingOffRoutesAre404LikeUnknownRoutes(t *testing.T) {
	base, cookie := signedIn(t, false)
	role := map[string]string{"Authorization": "Bearer secret-a", "X-Switchboard-Role": "a"}
	for _, p := range hostingPaths {
		for _, who := range []struct {
			name    string
			cookie  *http.Cookie
			headers map[string]string
		}{{"anonymous", nil, nil}, {"admin", cookie, nil}, {"role", nil, role}} {
			got := send(t, base, p.method, p.path, who.cookie, who.headers)
			want := send(t, base, p.method, unknownTwin(p.path), who.cookie, who.headers)
			if got != want {
				t.Errorf("%s %s as %s: %d %q, unknown route answers %d %q", p.method, p.path, who.name, got.code, got.body, want.code, want.body)
			}
		}
		// A signed-in admin (or a role, for the node channel) reading one
		// sees a plain 404. Writes answer as unknown admin writes do (405).
		if p.method != "GET" {
			continue
		}
		var got httpReply
		if strings.HasPrefix(p.path, "/api/node/") {
			got = send(t, base, p.method, p.path, nil, role)
		} else {
			got = send(t, base, p.method, p.path, cookie, nil)
		}
		if got.code != http.StatusNotFound {
			t.Errorf("%s %s with hosting off = %d %q, want 404", p.method, p.path, got.code, got.body)
		}
	}
}

// hostingServed are the hosting routes #184, #186, #191 and #192 serve; the rest
// stay stubs.
var hostingServed = map[string]bool{
	"GET /api/admin/terminals": true, "POST /api/admin/terminals": true,
	"PUT /api/admin/terminals/7": true, "DELETE /api/admin/terminals/7": true,
	"POST /api/admin/terminals/7/restart": true, "POST /api/admin/terminals/7/new-conversation": true,
	"GET /api/admin/nodes":               true,
	"DELETE /api/admin/nodes/pc":         true,
	"POST /api/admin/terminals/7/ticket": true, "GET /api/admin/terminals/7/socket": true,
	"GET /api/admin/terminals/7/log": true,
}

func TestHostingOnStubsStayBehindAdminSignIn(t *testing.T) {
	base, cookie := signedIn(t, true)
	for _, p := range hostingPaths {
		if strings.HasPrefix(p.path, "/api/node/") {
			// The node channel answers only a node token.
			for _, who := range []map[string]string{nil, {"Authorization": "Bearer secret-a", "X-Switchboard-Role": "a"}} {
				if got := send(t, base, p.method, p.path, cookie, who); got.code != http.StatusUnauthorized {
					t.Errorf("%s %s without a node token = %d, want 401", p.method, p.path, got.code)
				}
			}
			continue
		}
		if got := send(t, base, p.method, p.path, nil, nil); got.code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, got.code)
		}
		got := send(t, base, p.method, p.path, cookie, nil)
		if served := hostingServed[p.method+" "+p.path]; served != (got.code != http.StatusNotImplemented) {
			t.Errorf("%s %s admin = %d (served: %v)", p.method, p.path, got.code, served)
		}
	}
}

func TestAdminBootstrapReportsHosting(t *testing.T) {
	for _, on := range []bool{false, true} {
		base, cookie := signedIn(t, on)
		got := send(t, base, "GET", "/api/admin/me", cookie, nil)
		var me struct {
			User    string `json:"user"`
			Mode    string `json:"mode"`
			Hosting *bool  `json:"hosting"`
		}
		if err := json.Unmarshal([]byte(got.body), &me); err != nil || got.code != 200 {
			t.Fatalf("me=%d %s %v", got.code, got.body, err)
		}
		if me.User != "alice" || me.Mode != "builtin" || me.Hosting == nil || *me.Hosting != on {
			t.Fatalf("hosting %v: me=%s", on, got.body)
		}
	}
}

func TestHostingStartRefusals(t *testing.T) {
	ok := map[string]string{"SWITCHBOARD_HOSTING": "on", "LISTEN_ADDR": "127.0.0.1:8090", "SWITCHBOARD_ADMIN_AUTH": "builtin", "DB_PATH": "/home/me/switchboard-pc/mail.db"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range ok {
			m[a] = b
		}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		want string // "" means the start is permitted
	}{
		{"permitted", ok, ""},
		{"oidc", with("SWITCHBOARD_ADMIN_AUTH", "oidc"), ""},
		{"loopback 127.0.0.2", with("LISTEN_ADDR", "127.0.0.2:8090"), ""},
		{"loopback ::1", with("LISTEN_ADDR", "[::1]:8090"), ""},
		{"localhost", with("LISTEN_ADDR", "localhost:8090"), ""},
		{"empty host", with("LISTEN_ADDR", ":8090"), "LISTEN_ADDR :8090 is not a loopback address"},
		{"listen unset", with("LISTEN_ADDR", ""), "LISTEN_ADDR :8080 is not a loopback address"},
		{"all interfaces", with("LISTEN_ADDR", "0.0.0.0:8090"), "LISTEN_ADDR 0.0.0.0:8090 is not a loopback address"},
		{"private address", with("LISTEN_ADDR", "192.0.2.10:8090"), "not a loopback address"},
		{"name", with("LISTEN_ADDR", "pc.example:8090"), "not a loopback address"},
		{"admin unset", with("SWITCHBOARD_ADMIN_AUTH", ""), "SWITCHBOARD_ADMIN_AUTH must be builtin or oidc (got unset)"},
		{"admin proxy", with("SWITCHBOARD_ADMIN_AUTH", "proxy"), `SWITCHBOARD_ADMIN_AUTH must be builtin or oidc (got "proxy")`},
		{"db unset", with("DB_PATH", ""), "DB_PATH is unset"},
		{"db container default", with("DB_PATH", "/data/mail.db"), "DB_PATH is the container default /data/mail.db"},
		{"db container default unclean", with("DB_PATH", "/data//mail.db"), "DB_PATH is the container default"},
		{"old name", with("SWITCHBOARD_HOSTING", ""), ""},
	} {
		env := c.env
		if c.name == "old name" {
			env["AGENT_MAIL_HOSTING"] = "on"
			env["LISTEN_ADDR"] = ":8080"
			c.want = "AGENT_MAIL_HOSTING=on refused: LISTEN_ADDR :8080"
		}
		on, err := hostingStart(fakeEnv(env))
		if !on {
			t.Errorf("%s: hosting off", c.name)
		}
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want it to name %q", c.name, err, c.want)
		}
	}
	// Off (the default, or any value but "on") never checks anything.
	for _, v := range []string{"", "off", "true"} {
		if on, err := hostingStart(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING": v, "LISTEN_ADDR": ":8080"})); on || err != nil {
			t.Errorf("SWITCHBOARD_HOSTING=%q: on=%v err=%v", v, on, err)
		}
	}
}

// serverProcess starts the real server (Run) in a child process with env.
func serverProcess(t *testing.T, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(append(os.Environ(), clearNewEnv...), "SWITCHBOARD_HOSTING=", "AGENT_MAIL_HOSTING=", "SWITCHBOARD_ADMIN_AUTH=", "SWITCHBOARD_TEST_CLI=1", "SWITCHBOARD_TEST_ARGS=[]")
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

func TestHostingRefusedStartExitsNonZero(t *testing.T) {
	db := filepath.Join(t.TempDir(), "pc.db")
	out, err := serverProcess(t, "SWITCHBOARD_HOSTING=on", "LISTEN_ADDR=:0", "SWITCHBOARD_ADMIN_AUTH=builtin", "DB_PATH="+db).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "LISTEN_ADDR :0 is not a loopback address") {
		t.Fatalf("refused start: err=%v output=%s", err, out)
	}
	if _, statErr := os.Stat(db); statErr == nil {
		t.Fatal("a refused start created the database")
	}
}

func TestHostingPermittedStartServes(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := serverProcess(t, "SWITCHBOARD_HOSTING=on", "LISTEN_ADDR="+addr, "SWITCHBOARD_ADMIN_AUTH=builtin", "DB_PATH="+filepath.Join(t.TempDir(), "pc.db"))
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("server exited: %v\n%s", err, out.String())
		default:
		}
		if res, err := http.Get("http://" + addr + "/health"); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not answer /health\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := send(t, "http://"+addr, "GET", "/api/admin/terminals", nil, nil); got.code != http.StatusUnauthorized {
		t.Fatalf("terminals anonymous = %d %q, want 401", got.code, got.body)
	}
}
