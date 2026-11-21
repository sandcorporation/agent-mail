package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/node"
)

func TestHostingRemoteListenRefusals(t *testing.T) {
	t.Parallel()
	ok := map[string]string{"SWITCHBOARD_HOSTING": "on", "LISTEN_ADDR": "127.0.0.1:8090", "SWITCHBOARD_ADMIN_AUTH": "builtin", "DB_PATH": "/home/me/switchboard-pc/mail.db"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for a, b := range ok {
			m[a] = b
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	const listen = "SWITCHBOARD_HOSTING_REMOTE_LISTEN"
	for _, c := range []struct {
		name string
		env  map[string]string
		want string // "" means the start is permitted
	}{
		{"unset", ok, ""},
		{"tailscale ipv4", with(listen, "100.64.0.10:8091"), ""}, // leak-guard:allow
		{"tailscale ipv6", with(listen, "[fd7a:115c:a1e9::10]:8091"), ""},
		{"own range", with(listen, "192.0.2.10:8091", "SWITCHBOARD_HOSTING_REMOTE_ALLOW", "192.0.2.0/24"), ""},
		{"outside the range", with(listen, "192.0.2.10:8091"), listen + " 192.0.2.10:8091 is outside SWITCHBOARD_HOSTING_REMOTE_ALLOW (100.64.0.0/10,fd7a:115c:a1e9::/48)"}, // leak-guard:allow
		{"all ipv4", with(listen, "0.0.0.0:8091"), listen + " 0.0.0.0:8091 binds every interface"},
		{"all ipv6", with(listen, "[::]:8091"), listen + " [::]:8091 binds every interface"},
		{"empty host", with(listen, ":8091"), listen + " :8091 binds every interface"},
		{"host name", with(listen, "pc.example:8091"), listen + " pc.example:8091 names a host; give an IP address"},
		{"loopback", with(listen, "127.0.0.1:8091"), listen + " 127.0.0.1:8091 is a loopback address"},
		{"localhost", with(listen, "localhost:8091"), listen + " localhost:8091 is a loopback address"},
		{"no port", with(listen, "100.64.0.10"), listen + ` "100.64.0.10" is not IP:port`},                                                                                  // leak-guard:allow
		{"bad port", with(listen, "100.64.0.10:x"), listen + ` "100.64.0.10:x" is not IP:port`},                                                                             // leak-guard:allow
		{"bad range", with(listen, "100.64.0.10:8091", "SWITCHBOARD_HOSTING_REMOTE_ALLOW", "nonsense"), `SWITCHBOARD_HOSTING_REMOTE_ALLOW: "nonsense" is not a CIDR range`}, // leak-guard:allow
		{"listen not loopback", with(listen, "100.64.0.10:8091", "LISTEN_ADDR", "100.64.0.10:8090"), "LISTEN_ADDR 100.64.0.10:8090 is not a loopback address"},              // leak-guard:allow
	} {
		on, err := hostingStart(fakeEnv(c.env))
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
	// Two failures are named in one refusal, as the other conditions are.
	_, err := hostingStart(fakeEnv(with(listen, "0.0.0.0:8091", "DB_PATH", "")))
	if err == nil || !strings.Contains(err.Error(), "binds every interface (give one private-network address); DB_PATH is unset") {
		t.Errorf("two failures: %v", err)
	}
	if r, err := hostingRemote(fakeEnv(with(listen, "100.64.0.10:8091"))); err != nil || r.addr != "100.64.0.10:8091" || len(r.allow) != 2 { // leak-guard:allow
		t.Errorf("settings = %+v %v", r, err)
	}
}

// peerAs serves h as if every request came from peer: the remote listener
// tests run on loopback, where CI cannot bind a private-network address.
func peerAs(peer string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = peer
		h.ServeHTTP(w, r)
	})
}

func remoteServer(t *testing.T, s *Server, peer string) string {
	t.Helper()
	srv := httptest.NewServer(peerAs(peer, s.remoteHandler([]netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e9::/48")}))) // leak-guard:allow
	t.Cleanup(srv.Close)
	return srv.URL
}

// roleCall is one role API call with a JSON body (nil: none).
func roleCall(t *testing.T, base, method, path, token, role string, body any, extra map[string]string) httpReply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Switchboard-Role", role)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return httpReply{res.StatusCode, string(b)}
}

func TestHostingRemoteListenerServesNodesAndRolesOnly(t *testing.T) {
	t.Parallel()
	s, _, cookie := signedInServer(t, true)
	remote := remoteServer(t, s, "100.64.0.10:40000") // leak-guard:allow

	// A node on another machine joins with an invite code made on the host,
	// then its channel attaches.
	code, _, err := s.hostingService().InviteNode("pc")
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "node.token")
	if name, err := joinHost(context.Background(), remote, tokenFile, code); err != nil || name != "pc" {
		t.Fatalf("join over the remote listener = %q %v", name, err)
	}
	token, err := node.ReadTokenFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(remote, "http")+"/api/node/socket", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatalf("node socket on the remote listener: %v", err)
	}
	ws.CloseNow()

	// A role token sends and reads its inbox.
	if got := roleCall(t, remote, "POST", "/api/messages", "secret-a", "a", map[string]string{"recipient": "b", "subject": "over the private network", "body": "x"}, nil); got.code != http.StatusCreated {
		t.Fatalf("send = %d %s", got.code, got.body)
	}
	if got := roleCall(t, remote, "GET", "/api/messages", "secret-b", "b", nil, nil); got.code != http.StatusOK || !strings.Contains(got.body, "over the private network") {
		t.Fatalf("inbox = %d %s", got.code, got.body)
	}
	if got := roleCall(t, remote, "GET", "/api/messages", "wrong", "b", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", got.code)
	}

	// Admin, dashboard, setup, sign-in, role registration and the browser
	// terminal are not there, even for a signed-in admin.
	for _, p := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/index.html"}, {"GET", "/setup"}, {"POST", "/auth/login"},
		{"GET", "/api/admin/settings"}, {"GET", "/api/admin/terminals"}, {"POST", "/api/admin/terminals"},
		{"GET", "/api/admin/terminals/t1/socket"}, {"GET", "/api/admin/live"}, {"GET", "/api/admin/nodes"},
		{"GET", "/api/admin"}, {"POST", "/api/register"}, {"POST", "/api/join"}, {"GET", "/assets/xterm.js"},
		{"GET", "/health"}, {"GET", "/api/../api/admin/settings"}, {"GET", "/api//admin/settings"},
	} {
		if got := send(t, remote, p.method, p.path, cookie, nil); got.code != http.StatusNotFound {
			t.Errorf("%s %s = %d %q, want 404", p.method, p.path, got.code, got.body)
		}
	}

	// A forwarded request is refused, whoever it names.
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
		if got := roleCall(t, remote, "GET", "/api/messages", "secret-b", "b", nil, map[string]string{h: "100.64.0.11"}); got.code != http.StatusForbidden { // leak-guard:allow
			t.Errorf("%s = %d %s", h, got.code, got.body)
		}
	}
}

func TestHostingRemoteListenerRefusesPeersOutsideTheRange(t *testing.T) {
	t.Parallel()
	s, _, _ := signedInServer(t, true)
	if got := roleCall(t, remoteServer(t, s, "[fd7a:115c:a1e9::11]:40000"), "GET", "/api/messages", "secret-b", "b", nil, nil); got.code != http.StatusOK {
		t.Errorf("ipv6 peer in range = %d %s", got.code, got.body)
	}
	for _, peer := range []string{"192.0.2.5:40000", "127.0.0.1:40000", "[::1]:40000", "[2001:db8::1]:40000", "garbage"} {
		remote := remoteServer(t, s, peer)
		for _, path := range []string{"/api/messages", "/api/node/socket", "/api/admin/settings"} {
			if got := roleCall(t, remote, "GET", path, "secret-b", "b", nil, nil); got.code != http.StatusForbidden {
				t.Errorf("peer %s %s = %d %s", peer, path, got.code, got.body)
			}
		}
	}
}

// TestCheckNodeHost: a node attaches to a loopback host or to a host whose
// every address is inside the allowed ranges.
func TestCheckNodeHost(t *testing.T) {
	t.Parallel()
	allow := []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e9::/48")} // leak-guard:allow
	names := map[string][]string{
		"pc.example":      {"100.64.0.10", "fd7a:115c:a1e9::10"}, // leak-guard:allow
		"mixed.example":   {"100.64.0.10", "192.0.2.10"},         // leak-guard:allow
		"public.example":  {"192.0.2.10"},
		"lo.example":      {"127.0.0.1"},
		"failing.example": nil,
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		addrs, ok := names[host]
		if !ok || addrs == nil {
			return nil, errors.New("no such host")
		}
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	for _, c := range []struct {
		url  string
		want string // "" means accepted
	}{
		{"http://127.0.0.1:8090", ""},
		{"http://localhost:8090", ""},
		{"http://[::1]:8090", ""},
		{"http://100.64.0.10:8091", ""}, // leak-guard:allow
		{"https://[fd7a:115c:a1e9::10]:8091", ""},
		{"http://pc.example:8091", ""},
		{"http://lo.example:8091", ""},
		{"http://192.0.2.10:8091", "node: --host 192.0.2.10 is neither loopback nor inside SWITCHBOARD_HOSTING_REMOTE_ALLOW (100.64.0.0/10,fd7a:115c:a1e9::/48)"}, // leak-guard:allow
		{"http://0.0.0.0:8091", "is neither loopback nor inside"},
		{"http://mixed.example:8091", "node: --host mixed.example resolves to 192.0.2.10, outside SWITCHBOARD_HOSTING_REMOTE_ALLOW"},
		{"http://public.example:8091", "resolves to 192.0.2.10, outside"},
		{"http://failing.example:8091", "node: --host failing.example: cannot resolve"},
		{"ftp://100.64.0.10", "is not an http:// or https:// URL"}, // leak-guard:allow
		{"100.64.0.10:8091", "is not an http:// or https:// URL"},  // leak-guard:allow
	} {
		err := checkNodeHost(c.url, allow, lookup)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.url, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want %q", c.url, err, c.want)
		}
	}
}
