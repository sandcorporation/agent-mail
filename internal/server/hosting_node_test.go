package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// nodeToken registers node name on s and returns its token.
func nodeToken(t *testing.T, s *Server, name string) string {
	t.Helper()
	token, err := node.EnsureTokenFile(filepath.Join(t.TempDir(), "node.token"))
	if err == nil {
		err = s.hostingService().RegisterLocalNode(name, token)
	}
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestNodeTokenIsStoredHashedInAPrivateFile(t *testing.T) {
	t.Parallel()
	s, _, _ := signedInServer(t, true)
	path := filepath.Join(t.TempDir(), "data", "node.token")
	ensure := func() (string, error) {
		token, err := node.EnsureTokenFile(path)
		if err == nil {
			err = s.hostingService().RegisterLocalNode("local", token)
		}
		return token, err
	}
	token, err := ensure()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	// Windows has no mode bits: the file's DACL keeps it private there.
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v mode %v", err, fi.Mode())
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM nodes WHERE name='local'`).Scan(&stored); err != nil || stored == token || strings.Contains(stored, token) {
		t.Fatalf("stored %q (%v)", stored, err)
	}
	again, err := ensure()
	if err != nil || again != token {
		t.Fatalf("second start: %q %v", again, err)
	}
	if _, err := s.db.Exec(`UPDATE nodes SET revoked_at=1 WHERE name='local'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ensure(); err == nil {
		t.Fatal("a revoked node started")
	}
}

func TestNodeTokenIsRefusedOutsideTheNodeChannel(t *testing.T) {
	t.Parallel()
	s, base, _ := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	bearer := map[string]string{"Authorization": "Bearer " + token}
	asRole := map[string]string{"Authorization": "Bearer " + token, "X-Switchboard-Role": "pc"}
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/messages"}, {"POST", "/api/messages"}, {"GET", "/api/watch"}, {"GET", "/api/presence"},
		{"GET", "/api/whoami"}, {"GET", "/api/slots"}, {"GET", "/api/questions"},
		{"GET", "/api/admin/terminals"}, {"POST", "/api/admin/terminals"}, {"GET", "/api/admin/nodes"},
		{"DELETE", "/api/admin/nodes/pc"}, {"GET", "/api/admin/roles"}, {"GET", "/api/admin/me"}, {"GET", "/api/admin/live"},
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
	return attachNodeWith(t, base, token, backend, node.Config{})
}

// attachNodeWith is attachNode with cfg's extra settings (AllowChecks,
// Environ).
func attachNodeWith(t *testing.T, base, token string, backend *termtest.Fake, cfg node.Config) (stop func()) {
	t.Helper()
	cfg.Agent, cfg.Machine, cfg.Endpoint, cfg.NodeToken = "/opt/agent/claude", "pc-1", base, token
	n, err := node.New(cfg, backend)
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
	t.Parallel()
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
	nodeToken(t, s, "other")
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
	// New conversation: the session stops and starts with a new id.
	before = len(fake.Launches())
	got = adminCall(t, base, "POST", "/api/admin/terminals/"+second.ID+"/new-conversation", cookie, true, "")
	var renewed wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &renewed); err != nil || got.code != http.StatusOK || renewed.AgentSessionID == second.AgentSessionID {
		t.Fatalf("new conversation = %d %s", got.code, got.body)
	}
	eventually(t, "started in the new conversation", func() bool {
		l := launchArgs(fake)
		return len(l) == before+1 && l[before] == "sb-"+second.ID+" --session-id "+renewed.AgentSessionID
	})
	eventually(t, "new conversation persisted as launched", func() bool { h, _ := s.hostingService().Get(second.ID); return h.Launched })
	second = renewed

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
	t.Parallel()
	for _, path := range []string{"terminals", "terminals/hs_x", "terminals/hs_x/restart", "nodes"} {
		if got := liveKinds(path, true); !slices.Equal(got, []string{"terminals"}) {
			t.Errorf("liveKinds(%q) = %v", path, got)
		}
	}
	if got := liveKinds("terminals", false); got != nil {
		t.Errorf("role path terminals = %v", got)
	}
}

// A node whose token is revoked while it is attached stops within one retry
// with node.ErrRevoked, leaves its hosted terminals as they are, and the
// `node` command maps that to cli.ExitNodeRevoked (#203).
func TestRevokedNodeStopsRetrying(t *testing.T) {
	t.Parallel()
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	fake := termtest.New()
	n, err := node.New(node.Config{Agent: "/opt/agent/claude", Machine: "pc-1", Endpoint: base, NodeToken: token}, fake)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	sleeps := 0
	l := &node.Link{Node: n, Host: base, Token: token, Interval: 20 * time.Millisecond,
		Hello: wire.NodeHello{Name: "pc", Machine: "pc-1", OS: "linux", Version: "test", Backend: "fake"},
		Logf: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			mu.Unlock()
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			sleeps++
			mu.Unlock()
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	eventually(t, "the record running", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })

	if got := adminCall(t, base, "DELETE", "/api/admin/nodes/pc", cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("revoke = %d %s", got.code, got.body)
	}
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked node kept retrying")
	}
	if !errors.Is(err, node.ErrRevoked) {
		t.Fatalf("Run = %v, want node.ErrRevoked", err)
	}
	mu.Lock()
	if sleeps > 1 {
		t.Errorf("%d retries before stopping", sleeps)
	}
	if !slices.ContainsFunc(logs, func(s string) bool { return strings.Contains(s, "revoked") }) {
		t.Errorf("no line naming the revocation: %q", logs)
	}
	mu.Unlock()
	if st := fake.Stops(); len(st) != 0 {
		t.Errorf("stopped %v", st)
	}
	var ce cli.ExitCodeError
	if err := nodeRunError(err); !errors.As(err, &ce) || ce.Code != cli.ExitNodeRevoked {
		t.Errorf("node command error %v, want exit %d", err, cli.ExitNodeRevoked)
	}
}

// start → crash → automatic restart → stop of a hosted session reach the
// host as four events, served newest first on its events route (#252).
func TestHostedSessionHistoryOverTheNodeChannel(t *testing.T) {
	t.Parallel()
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	fake := termtest.New()
	attachNode(t, base, token, fake)
	events := func() []wire.HostedEvent {
		got := adminCall(t, base, "GET", "/api/admin/terminals/"+rec.ID+"/events", cookie, false, "")
		var ev []wire.HostedEvent
		if err := json.Unmarshal([]byte(got.body), &ev); err != nil || got.code != http.StatusOK {
			t.Fatalf("events = %d %s", got.code, got.body)
		}
		return ev
	}
	eventually(t, "the start", func() bool { return len(events()) == 1 })
	if err := fake.Exit("sb-"+rec.ID, 7); err != nil {
		t.Fatal(err)
	}
	// The restart policy relaunches after node.BackoffBase.
	deadline := time.Now().Add(node.BackoffBase + 5*time.Second)
	for len(events()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("no automatic restart: %+v", events())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := adminCall(t, base, "PUT", "/api/admin/terminals/"+rec.ID, cookie, true, `{"desired":"stopped"}`); got.code != http.StatusOK {
		t.Fatalf("stop = %d %s", got.code, got.body)
	}
	eventually(t, "the stop", func() bool { return len(events()) == 4 })
	time.Sleep(100 * time.Millisecond) // the stopped run's exit adds nothing
	ev := events()
	var kinds []string
	for _, e := range ev {
		kinds = append(kinds, e.Kind+"/"+e.Actor)
	}
	want := []string{"stop/admin", "restart/policy", "crash/", "start/node"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events %q, want %q", kinds, want)
	}
	if ev[2].ExitCode == nil || *ev[2].ExitCode != 7 || ev[0].ExitCode != nil {
		t.Errorf("exit codes %v %v", ev[2].ExitCode, ev[0].ExitCode)
	}
	if ev[1].Restarts != 1 || ev[3].Restarts != 0 {
		t.Errorf("restarts %d %d", ev[1].Restarts, ev[3].Restarts)
	}
}
