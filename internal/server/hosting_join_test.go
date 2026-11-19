package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// dialNode opens the node channel with token and reports the HTTP status of
// a refusal (0 when the socket opened).
func dialNode(t *testing.T, base, token string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/node/socket", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err == nil {
		ws.CloseNow()
		return 0
	}
	if res == nil {
		t.Fatalf("dial: %v", err)
	}
	return res.StatusCode
}

func TestNodeInviteJoinAndRevoke(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	code, _, err := s.hostingService().InviteNode("pc2")
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "cfg", "node.token")
	name, err := joinHost(context.Background(), base, tokenFile, code)
	if err != nil || name != "pc2" {
		t.Fatalf("join = %q %v", name, err)
	}
	fi, err := os.Stat(tokenFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v mode %v", err, fi.Mode())
	}
	token, err := node.ReadTokenFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM nodes WHERE name='pc2'`).Scan(&stored); err != nil || strings.Contains(stored, token) {
		t.Fatalf("stored %q (%v)", stored, err)
	}

	// The code works once; an existing token file is never replaced.
	other := filepath.Join(t.TempDir(), "node.token")
	if _, err := joinHost(context.Background(), base, other, code); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("second join: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("a refused join wrote %s", other)
	}
	if _, err := joinHost(context.Background(), base, tokenFile, code); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("join over an existing token file: %v", err)
	}

	// The joined node attaches and runs its records.
	rec := `{"node":"pc2","base":"a","profile":"team-a","directory":"/work/app"}`
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, rec)
	var h wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &h); err != nil || got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	attachNode(t, base, token, termtest.New())
	eventually(t, "the joined node's record running", func() bool { return terminals(t, base, cookie)[h.ID].Runtime.State == "running" })

	// A node token cannot revoke; an admin without the write guard cannot.
	if got := send(t, base, "DELETE", "/api/admin/nodes/pc2", nil, map[string]string{"Authorization": "Bearer " + token}); got.code != http.StatusUnauthorized {
		t.Fatalf("revoke with the node token = %d", got.code)
	}
	if got := adminCall(t, base, "DELETE", "/api/admin/nodes/pc2", cookie, false, ""); got.code != http.StatusForbidden {
		t.Fatalf("revoke without the write guard = %d", got.code)
	}
	if got := adminCall(t, base, "DELETE", "/api/admin/nodes/nobody", cookie, true, ""); got.code != http.StatusNotFound {
		t.Fatalf("revoke an unknown node = %d", got.code)
	}

	got = adminCall(t, base, "DELETE", "/api/admin/nodes/pc2", cookie, true, "")
	var n wire.NodeView
	if err := json.Unmarshal([]byte(got.body), &n); err != nil || got.code != http.StatusOK || n.Name != "pc2" || n.RevokedAt == nil {
		t.Fatalf("revoke = %d %s", got.code, got.body)
	}
	if strings.Contains(got.body, stored) || strings.Contains(got.body, token) {
		t.Fatal("the revoke answer carries the token or its hash")
	}
	eventually(t, "the revoked node's sessions offline", func() bool { return terminals(t, base, cookie)[h.ID].Runtime.State == "node offline" })
	if code := dialNode(t, base, token); code != http.StatusUnauthorized {
		t.Fatalf("revoked node reattach = %d", code)
	}
	if got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, strings.Replace(rec, "/work/app", "/work/b", 1)); got.code != http.StatusBadRequest || !strings.Contains(got.body, "revoked") {
		t.Fatalf("record on a revoked node = %d %s", got.code, got.body)
	}
	var seen int64
	if err := s.db.QueryRow(`SELECT COALESCE(last_seen_at,0) FROM nodes WHERE name='pc2'`).Scan(&seen); err != nil || seen == 0 {
		t.Fatalf("last_seen_at %d %v", seen, err)
	}
}

func TestRevokedInDatabaseIsDroppedAtSync(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord)
	var h wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &h); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	attachNode(t, base, token, termtest.New())
	eventually(t, "running", func() bool { return terminals(t, base, cookie)[h.ID].Runtime.State == "running" })
	if _, err := s.db.Exec(`UPDATE nodes SET revoked_at=1 WHERE name='pc'`); err != nil {
		t.Fatal(err)
	}
	if err := s.hostingHub().Sync(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "offline", func() bool { return terminals(t, base, cookie)[h.ID].Runtime.State == "node offline" })
}

func TestJoinRouteAnswers(t *testing.T) {
	_, base, _ := signedInServer(t, true)
	for body, want := range map[string]int{`{}`: 401, `{"code":"sbj_nope"}`: 401, `nope`: 400} {
		req, _ := http.NewRequest("POST", base+"/api/node/join", strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("join %s = %d, want %d", body, res.StatusCode, want)
		}
	}
	if got := send(t, base, "GET", "/api/node/join", nil, nil); got.code != http.StatusMethodNotAllowed {
		t.Errorf("GET join = %d", got.code)
	}
}

func TestNodeRefusesANonLoopbackHost(t *testing.T) {
	dir := t.TempDir()
	for _, host := range []string{"http://192.0.2.10:8090", "http://example.invalid:8090", "https://[2001:db8::1]:8090"} {
		err := Node("test", host, filepath.Join(dir, "node.token"), "sbj_x")
		if err == nil || !strings.Contains(err.Error(), "not a loopback address") || !strings.Contains(err.Error(), "access scope") {
			t.Errorf("%s: %v", host, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "node.token")); !os.IsNotExist(err) {
		t.Error("a refused host wrote a token file")
	}
	if err := Node("test", "ftp://127.0.0.1", filepath.Join(dir, "node.token"), ""); err == nil || !strings.Contains(err.Error(), "not an http") {
		t.Errorf("ftp host: %v", err)
	}
}

func TestNodeInviteCLI(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pc.db")
	code, out, stderr := binaryCLI(t, dbPath, "node", "invite", "laptop")
	if code != 0 || !strings.HasPrefix(out, "sbj_") {
		t.Fatalf("invite = %d %q %s", code, out, stderr)
	}
	invite := strings.SplitN(out, "\n", 2)[0]
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored string
	if err := db.QueryRow(`SELECT code_hash FROM node_invites WHERE name='laptop'`).Scan(&stored); err != nil || strings.Contains(stored, invite) {
		t.Fatalf("stored %q (%v)", stored, err)
	}
	if code, _, stderr = binaryCLI(t, "/data/mail.db", "node", "invite", "laptop"); code != 1 || !strings.Contains(stderr, "container default") {
		t.Errorf("invite against the container database = %d %s", code, stderr)
	}
	if code, _, stderr = binaryCLI(t, dbPath, "node", "invite", "../x"); code != 1 || !strings.Contains(stderr, "invalid name") {
		t.Errorf("invalid name = %d %s", code, stderr)
	}
	if code, _, stderr = binaryCLI(t, dbPath, "node", "evict", "laptop"); code != 2 {
		t.Errorf("unknown subcommand = %d %s", code, stderr)
	}
	if code, _, stderr = binaryCLI(t, dbPath, "node"); code != 2 || !strings.Contains(stderr, "requires --host") {
		t.Errorf("node without --host = %d %s", code, stderr)
	}
}
