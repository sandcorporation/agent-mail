package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// nodeToken registers node name on s and returns its token.
func nodeToken(t *testing.T, s *Server, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.token")
	token, err := s.hostingService().EnsureNodeToken(name, path)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestNodeTokenIsStoredHashedInAPrivateFile(t *testing.T) {
	s, _, _ := signedInServer(t, true)
	path := filepath.Join(t.TempDir(), "data", "node.token")
	token, err := s.hostingService().EnsureNodeToken("local", path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v mode %v", err, fi.Mode())
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM nodes WHERE name='local'`).Scan(&stored); err != nil || stored == token || strings.Contains(stored, token) {
		t.Fatalf("stored %q (%v)", stored, err)
	}
	again, err := s.hostingService().EnsureNodeToken("local", path)
	if err != nil || again != token {
		t.Fatalf("second start: %q %v", again, err)
	}
	if _, err := s.db.Exec(`UPDATE nodes SET revoked_at=1 WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.hostingService().EnsureNodeToken("local", path); err == nil {
		t.Fatal("a revoked node started")
	}
}

func TestNodeTokenIsRefusedOutsideTheNodeChannel(t *testing.T) {
	s, base, _ := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	bearer := map[string]string{"Authorization": "Bearer " + token}
	asRole := map[string]string{"Authorization": "Bearer " + token, "X-Switchboard-Role": "pc"}
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/messages"}, {"POST", "/api/messages"}, {"GET", "/api/watch"}, {"GET", "/api/presence"},
		{"GET", "/api/whoami"}, {"GET", "/api/slots"}, {"GET", "/api/questions"},
		{"GET", "/api/admin/terminals"}, {"POST", "/api/admin/terminals"}, {"GET", "/api/admin/nodes"},
		{"GET", "/api/admin/roles"}, {"GET", "/api/admin/me"}, {"GET", "/api/admin/live"},
	} {
		for _, h := range []map[string]string{bearer, asRole} {
			if got := send(t, base, p.method, p.path, nil, h); got.code != http.StatusUnauthorized {
				t.Errorf("%s %s with a node token = %d %q, want 401", p.method, p.path, got.code, got.body)
			}
		}
	}
	for name, h := range map[string]map[string]string{
		"missing": nil,
		"wrong":   {"Authorization": "Bearer sbn_wrong"},
		"role":    {"Authorization": "Bearer secret-a", "X-Switchboard-Role": "a"},
	} {
		if got := send(t, base, "GET", "/api/node/socket", nil, h); got.code != http.StatusUnauthorized {
			t.Errorf("node socket with %s token = %d", name, got.code)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/node/socket", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatalf("node socket with the node token: %v", err)
	}
	ws.CloseNow()
	if _, err := s.db.Exec(`UPDATE nodes SET revoked_at=1 WHERE name='pc'`); err != nil {
		t.Fatal(err)
	}
	if got := send(t, base, "GET", "/api/node/socket", nil, bearer); got.code != http.StatusUnauthorized {
		t.Errorf("revoked node socket = %d", got.code)
	}
}

// terminals is GET /api/admin/terminals by id.
func terminals(t *testing.T, base string, cookie *http.Cookie) map[string]wire.HostedSession {
	t.Helper()
	got := adminCall(t, base, "GET", "/api/admin/terminals", cookie, false, "")
	var list []wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &list); err != nil {
		t.Fatalf("terminals = %d %s", got.code, got.body)
	}
	out := map[string]wire.HostedSession{}
	for _, h := range list {
		out[h.ID] = h
	}
	return out
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// attachNode runs a node over backend attached to base until the returned
// stop is called.
func attachNode(t *testing.T, base, token string, backend *termtest.Fake) (stop func()) {
	t.Helper()
	n, err := node.New(node.Config{Agent: "/opt/agent/claude", Machine: "pc-1", Endpoint: base, NodeToken: token}, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l := &node.Link{Node: n, Host: base, Token: token, Interval: 20 * time.Millisecond,
		Hello: wire.NodeHello{Name: "pc", Machine: "pc-1", OS: "linux", Version: "test", Backend: "fake"},
		Logf:  func(string, ...any) {}}
	go func() { _ = l.Run(ctx); close(done) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func launchArgs(f *termtest.Fake) []string {
	var out []string
	for _, l := range f.Launches() {
		out = append(out, l.Name+" "+strings.Join(l.Argv[1:3], " "))
	}
	return out
}

func TestNodeChannelAssignsReportsAndGoesOffline(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord)
	var first wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &first); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	if rt := terminals(t, base, cookie)[first.ID].Runtime; rt.State != "node offline" {
		t.Errorf("before the node attaches: %+v", rt)
	}

	fake := termtest.New()
	stop := attachNode(t, base, token, fake)
	eventually(t, "the first record running", func() bool { return terminals(t, base, cookie)[first.ID].Runtime.State == "running" })
	if l := launchArgs(fake); len(l) != 1 || l[0] != "sb-"+first.ID+" --session-id "+first.AgentSessionID {
		t.Fatalf("launches %q", l)
	}
	rt := terminals(t, base, cookie)[first.ID].Runtime
	if rt.Since == nil || rt.Run != 1 {
		t.Errorf("runtime %+v", rt)
	}
	eventually(t, "launched persisted", func() bool { h, _ := s.hostingService().Get(first.ID); return h.Launched })
	nodes := adminCall(t, base, "GET", "/api/admin/nodes", cookie, false, "")
	if !strings.Contains(nodes.body, `"machine":"pc-1"`) || !strings.Contains(nodes.body, `"os":"linux"`) {
		t.Errorf("nodes after hello: %s", nodes.body)
	}

	// A record the CLI writes straight into the database reaches the node
	// at the next sync.
	second, err := s.hostingService().Create(wire.HostedSessionCreate{Node: "pc", Base: "b", Profile: "team-b", Directory: "/work/b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.hostingHub().Sync(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the CLI record running", func() bool { return terminals(t, base, cookie)[second.ID].Runtime.State == "running" })
	// So does a record for another node: never.
	if _, err := s.hostingService().Create(wire.HostedSessionCreate{Node: "other", Base: "b", Profile: "p", Directory: "/w"}); err != nil {
		t.Fatal(err)
	}
	_ = s.hostingHub().Sync()

	// Stop, start and restart from the dashboard.
	if got := adminCall(t, base, "PUT", "/api/admin/terminals/"+first.ID, cookie, true, `{"desired":"stopped"}`); got.code != http.StatusOK {
		t.Fatalf("stop = %d %s", got.code, got.body)
	}
	eventually(t, "stopped", func() bool { return terminals(t, base, cookie)[first.ID].Runtime.State == "stopped" })
	if got := adminCall(t, base, "POST", "/api/admin/terminals/"+first.ID+"/restart", cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("restart a stopped session = %d %s", got.code, got.body)
	}
	eventually(t, "started again", func() bool { return terminals(t, base, cookie)[first.ID].Runtime.State == "running" })
	before := len(fake.Launches())
	if got := adminCall(t, base, "POST", "/api/admin/terminals/"+second.ID+"/restart", cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("restart = %d %s", got.code, got.body)
	}
	eventually(t, "relaunched with resume", func() bool {
		l := launchArgs(fake)
		return len(l) == before+1 && l[before] == "sb-"+second.ID+" --resume "+second.AgentSessionID
	})
	if got := adminCall(t, base, "POST", "/api/admin/terminals/hs_00000000000000000000000000000000/restart", cookie, true, ""); got.code != http.StatusNotFound {
		t.Errorf("restart unknown = %d", got.code)
	}

	// The node goes away: its sessions show "node offline" and keep running.
	stop()
	eventually(t, "node offline", func() bool { return terminals(t, base, cookie)[first.ID].Runtime.State == "node offline" })
	if got := adminCall(t, base, "POST", "/api/admin/terminals/"+first.ID+"/restart", cookie, true, ""); got.code != http.StatusConflict {
		t.Errorf("restart while offline = %d", got.code)
	}
	launches := len(fake.Launches())

	// A new node process over the same backend adopts them without
	// relaunching.
	attachNode(t, base, token, fake)
	eventually(t, "adopted", func() bool {
		ts := terminals(t, base, cookie)
		return ts[first.ID].Runtime.State == "running" && ts[second.ID].Runtime.State == "running"
	})
	if n := len(fake.Launches()); n != launches {
		t.Errorf("re-attach launched %d more sessions", n-launches)
	}
}

func TestTerminalsLiveKind(t *testing.T) {
	for _, path := range []string{"terminals", "terminals/hs_x", "terminals/hs_x/restart", "nodes"} {
		if got := liveKinds(path, true); !slices.Equal(got, []string{"terminals"}) {
			t.Errorf("liveKinds(%q) = %v", path, got)
		}
	}
	if got := liveKinds("terminals", false); got != nil {
		t.Errorf("role path terminals = %v", got)
	}
}
