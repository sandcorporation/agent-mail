package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// joinRemoteNode joins node name over the remote listener and returns its
// token.
func joinRemoteNode(t *testing.T, s *Server, remote, name string) string {
	t.Helper()
	code, _, err := s.hostingService().InviteNode(name)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "node.token")
	if _, err := joinHost(context.Background(), remote, tokenFile, code); err != nil {
		t.Fatal(err)
	}
	token, err := node.ReadTokenFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// attachRemoteNode runs a node that takes role credentials into dir, as
// `switchboard node` does, until the returned stop is called.
func attachRemoteNode(t *testing.T, host, name, token, dir string, backend *termtest.Fake, logf func(string, ...any)) (stop func()) {
	t.Helper()
	n, err := node.New(node.Config{Agent: "/opt/agent/claude", Machine: name + "-machine", Endpoint: host, NodeToken: token}, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l := &node.Link{Node: n, Host: host, Token: token, Interval: 20 * time.Millisecond,
		Hello:    wire.NodeHello{Name: name, Machine: name + "-machine", OS: "linux", Version: "test", Backend: "fake"},
		Profiles: &node.Profiles{Dir: dir, Endpoint: host},
		Logf:     logf}
	go func() { _ = l.Run(ctx); close(done) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

type hostedProfileFile struct {
	Endpoint   string `json:"endpoint"`
	Role       string `json:"role"`
	Token      string `json:"token"`
	Credential string `json:"credential"`
}

func readHostedProfile(dir, id string) (hostedProfileFile, bool) {
	var p hostedProfileFile
	b, err := os.ReadFile(filepath.Join(dir, wire.HostedProfileName(id)+".json"))
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, false
	}
	return p, p.Token != ""
}

func createRecord(t *testing.T, base string, cookie *http.Cookie, body string) wire.HostedSession {
	t.Helper()
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, body)
	var h wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &h); err != nil || got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	return h
}

func launchEnv(f *termtest.Fake, name string) []string {
	var env []string
	for _, l := range f.Launches() {
		if l.Name == name {
			env = l.Env
		}
	}
	return env
}

func TestRemoteNodeSessionsGetARoleCredential(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s, base, cookie := signedInServer(t, true)
	remote := remoteServer(t, s, "100.64.0.10:40000") // leak-guard:allow
	token := joinRemoteNode(t, s, remote, "pc")
	rec := createRecord(t, base, cookie, newRecord)

	dir := t.TempDir()
	fake := termtest.New()
	logf := func(f string, a ...any) { log.Printf(f, a...) }
	stop := attachRemoteNode(t, remote, "pc", token, dir, fake, logf)
	eventually(t, "the record running", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })

	// The node wrote the record's profile, user-only, before launching.
	p, ok := readHostedProfile(dir, rec.ID)
	if !ok || p.Role != "a" || p.Endpoint != remote || p.Credential == "" || p.Token == "secret-a" {
		t.Fatalf("profile %+v %v", p, ok)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(dir, wire.HostedProfileName(rec.ID)+".json")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("profile mode %v %v", fi.Mode(), err)
		}
	}
	// The session runs with that profile; no token is in its environment.
	env := launchEnv(fake, "sb-"+rec.ID)
	if !slices.Contains(env, "SWITCHBOARD_PROFILE="+wire.HostedProfileName(rec.ID)) {
		t.Errorf("session env %q", env)
	}
	for _, kv := range env {
		if strings.Contains(kv, p.Token) {
			t.Errorf("token in the session env: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}

	// With it the session sends and reads its inbox, as the role and as a
	// numbered slot of it, over the remote listener.
	if got := roleCall(t, remote, "POST", "/api/messages", p.Token, "a", map[string]string{"recipient": "b", "subject": "from the remote node", "body": "x"}, nil); got.code != http.StatusCreated {
		t.Fatalf("send = %d %s", got.code, got.body)
	}
	if got := roleCall(t, remote, "POST", "/api/messages", "secret-b", "b", map[string]string{"recipient": "a-2", "subject": "to the slot", "body": "x"}, nil); got.code != http.StatusCreated {
		t.Fatalf("send to a-2 = %d %s", got.code, got.body)
	}
	if got := roleCall(t, remote, "GET", "/api/messages", p.Token, "a-2", nil, nil); got.code != http.StatusOK || !strings.Contains(got.body, "to the slot") {
		t.Fatalf("inbox = %d %s", got.code, got.body)
	}
	// The role's own token is unchanged.
	if got := roleCall(t, remote, "GET", "/api/messages", "secret-a", "a", nil, nil); got.code != http.StatusOK {
		t.Errorf("role token = %d", got.code)
	}
	// It is a credential of role a only.
	if got := roleCall(t, remote, "GET", "/api/messages", p.Token, "b", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("credential as b = %d", got.code)
	}

	// A reconnect keeps the credential the node holds.
	stop()
	stop = attachRemoteNode(t, remote, "pc", token, dir, fake, logf)
	eventually(t, "reattached", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })
	if again, _ := readHostedProfile(dir, rec.ID); again.Credential != p.Credential {
		t.Errorf("credential reissued on reconnect: %s → %s", p.Credential, again.Credential)
	}

	// A node that lost its profile gets a new credential and the old one
	// stops working.
	stop()
	if err := os.Remove(filepath.Join(dir, wire.HostedProfileName(rec.ID)+".json")); err != nil {
		t.Fatal(err)
	}
	stop = attachRemoteNode(t, remote, "pc", token, dir, fake, logf)
	var fresh hostedProfileFile
	eventually(t, "a new credential", func() bool { fresh, ok = readHostedProfile(dir, rec.ID); return ok })
	if fresh.Credential == p.Credential {
		t.Fatalf("same credential after the profile was lost")
	}
	if got := roleCall(t, remote, "GET", "/api/messages", p.Token, "a", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("replaced credential = %d", got.code)
	}
	p = fresh

	// Neither the admin API nor the logs carry a token.
	for _, path := range []string{"/api/admin/terminals", "/api/admin/nodes", "/api/admin/roles", "/api/admin/terminals/" + rec.ID + "/events"} {
		if got := adminCall(t, base, "GET", path, cookie, false, ""); got.code != http.StatusOK || strings.Contains(got.body, p.Token) || strings.Contains(got.body, "sbr_") {
			t.Errorf("%s = %d carries a credential", path, got.code)
		}
	}

	// Deleting the record revokes its credential and the node drops the
	// profile.
	if got := adminCall(t, base, "DELETE", "/api/admin/terminals/"+rec.ID, cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("delete = %d %s", got.code, got.body)
	}
	if got := roleCall(t, remote, "GET", "/api/messages", p.Token, "a", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("after the record was deleted = %d", got.code)
	}
	eventually(t, "the profile removed", func() bool { _, ok := readHostedProfile(dir, rec.ID); return !ok })

	// Revoking the role revokes its credentials; restoring the role does
	// not bring them back.
	second := createRecord(t, base, cookie, `{"node":"pc","base":"a","profile":"team-a","directory":"/work/second"}`)
	var p2 hostedProfileFile
	eventually(t, "the second profile", func() bool { p2, ok = readHostedProfile(dir, second.ID); return ok })
	for _, action := range []string{"revoke", "restore"} {
		if got := adminCall(t, base, "POST", "/api/admin/roles/a/"+action, cookie, true, ""); got.code != http.StatusOK {
			t.Fatalf("%s = %d %s", action, got.code, got.body)
		}
	}
	if got := roleCall(t, remote, "GET", "/api/messages", p2.Token, "a", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("after the role was revoked = %d", got.code)
	}
	// The node gets a new one at the next sync.
	if err := s.hostingHub().Sync(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a credential after restore", func() bool {
		v, ok := readHostedProfile(dir, second.ID)
		if ok && v.Credential != p2.Credential {
			p2 = v
			return true
		}
		return false
	})
	if got := roleCall(t, remote, "GET", "/api/messages", p2.Token, "a", nil, nil); got.code != http.StatusOK {
		t.Errorf("new credential = %d", got.code)
	}

	// Revoking the node revokes the credentials issued to it.
	if got := adminCall(t, base, "DELETE", "/api/admin/nodes/pc", cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("revoke node = %d %s", got.code, got.body)
	}
	if got := roleCall(t, remote, "GET", "/api/messages", p2.Token, "a", nil, nil); got.code != http.StatusUnauthorized {
		t.Errorf("after the node was revoked = %d", got.code)
	}

	for _, secret := range []string{p.Token, p2.Token, "sbr_"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("a credential is in the log")
		}
	}
}

// A node on the host's machine (`switchboard up`) keeps the profiles its
// records name and gets no credential.
func TestLocalNodeKeepsTheRecordProfile(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	rec := createRecord(t, base, cookie, newRecord)
	fake := termtest.New()
	attachNode(t, base, token, fake)
	eventually(t, "running", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })
	if env := launchEnv(fake, "sb-"+rec.ID); !slices.Contains(env, "SWITCHBOARD_PROFILE=team-a") {
		t.Errorf("session env %q", env)
	}
	live, err := s.hostingService().Nodes()
	if err != nil || len(live) != 1 {
		t.Fatal(live, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM role_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials issued to a local node: %d %v", n, err)
	}
}
