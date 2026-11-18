package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/agent-mail/internal/cli"
	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/store"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

const joinSecret = "joined-token-0123456789abcdef"

func inviteAs(t *testing.T, s *Server, token, role string, body map[string]string, want int) wire.InviteCreated {
	t.Helper()
	w := callAs(s, "POST", "/api/invites", token, role, body)
	if w.Code != want {
		t.Fatalf("invite %v as %s: %d %s", body, role, w.Code, w.Body)
	}
	var v wire.InviteCreated
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func joinCall(s *Server, code, role, credential string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(wire.JoinRequest{Code: code, Role: role, Credential: credential})
	r := httptest.NewRequest("POST", "/api/join", strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}

func mustJoin(t *testing.T, s *Server, code, role, credential string) {
	t.Helper()
	if w := joinCall(s, code, role, credential); w.Code != 201 {
		t.Fatalf("join %s: %d %s", role, w.Code, w.Body)
	}
}

func TestInviteJoinSendWithinGroup(t *testing.T) {
	s := groupServer(t)
	inv := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c"}, 201)
	if !inv.GroupCreated || len(inv.Code) != 32 || strings.Count(inv.Code, "-") != 6 {
		t.Fatalf("invite = %+v", inv)
	}
	if g, ok := s.ids().GroupOf("partner-c/dev"); !ok || g.BossRole != "partner-c/boss" {
		t.Fatalf("group not created: %+v %v", g, ok)
	}
	// The code is stored hashed only.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM invites WHERE code_hash LIKE '%'||?||'%'`, strings.ReplaceAll(inv.Code, "-", "")).Scan(&n); err != nil || n != 0 {
		t.Fatalf("code stored in clear: %d %v", n, err)
	}
	// Case and dashes do not matter.
	mustJoin(t, s, strings.ToLower(strings.ReplaceAll(inv.Code, "-", "")), "partner-c/dev", joinSecret)
	var stored string
	if err := s.db.QueryRow(`SELECT token FROM roles WHERE role='partner-c/dev'`).Scan(&stored); err != nil || stored != store.TokenHash(joinSecret) {
		t.Fatalf("stored token = %q %v", stored, err)
	}
	boss := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c", "role": "partner-c/boss", "ttl": "7d"}, 201)
	if boss.GroupCreated {
		t.Fatal("second invite created the group again")
	}
	mustJoin(t, s, boss.Code, "", joinSecret+"-boss")

	for to, want := range map[string]int{"partner-c/boss": 201, internalBossRole: 201, internalWorkerRol: 403, partnerDevRole: 403} {
		if got := sendAs(s, joinSecret, "partner-c/dev", to); got != want {
			t.Errorf("partner-c/dev -> %s = %d, want %d", to, got, want)
		}
	}
	if w := callAs(s, "GET", "/api/messages?box=inbox", joinSecret+"-boss", "partner-c/boss", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"sender":"partner-c/dev"`) {
		t.Fatalf("boss inbox: %d %s", w.Code, w.Body)
	}
}

// Creating a group over roles that are already there would restrict them:
// the invite is refused and nothing changes.
func TestInviteRefusesGroupOverExistingRoles(t *testing.T) {
	s := groupServer(t)
	for _, prefix := range []string{"sandcorp", "team", "partner-a"} {
		w := callAs(s, "POST", "/api/invites", bossToken, internalBossRole, map[string]string{"prefix": prefix})
		if w.Code != 409 {
			t.Fatalf("invite prefix=%s: %d %s", prefix, w.Code, w.Body)
		}
		if _, ok := s.ids().GroupOf(prefix + "/x"); ok {
			t.Fatalf("group %s created", prefix)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM invites)+(SELECT COUNT(*) FROM groups)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left behind: %d %v", n, err)
	}
	if !s.ids().Unrestricted(identity.Principal{Role: internalWorkerRol, Base: internalWorkerRol}) {
		t.Fatal("team/worker lost access")
	}
	// An existing group takes invites even though its roles exist.
	putGroup(t, s, "partner-a", `{}`)
	inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-a"}, 201)
	// A revoked group takes none.
	if w := adminWrite(s, "DELETE", "/api/admin/groups/partner-a", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-a"}, 409)
}

func TestInviteOnlyGlobalBoss(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", `{}`)
	inviteAs(t, s, workerToken, internalWorkerRol, map[string]string{"prefix": "partner-c"}, 403)
	inviteAs(t, s, partnerBossToken, partnerBossRole, map[string]string{"prefix": "partner-a"}, 403)
	if w := callAs(s, "POST", "/api/invites", "wrong", internalBossRole, map[string]string{"prefix": "partner-c"}); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
	for _, body := range []map[string]string{
		{"prefix": "partner-c", "ttl": "31d"},
		{"prefix": "partner-c", "ttl": "-1h"},
		{"prefix": "partner-c/"},
		{"prefix": "partner-c", "role": "partner-d/dev"},
	} {
		inviteAs(t, s, bossToken, internalBossRole, body, 400)
	}
}

func TestJoinWrongPrefixAndRole(t *testing.T) {
	s := groupServer(t)
	inv := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c"}, 201)
	for role, want := range map[string]int{"partner-d/dev": 403, "partner-c": 403, "team/x": 403, "": 400} {
		if w := joinCall(s, inv.Code, role, joinSecret); w.Code != want {
			t.Errorf("join role %q: %d %s", role, w.Code, w.Body)
		}
	}
	if w := joinCall(s, inv.Code, "partner-c/dev", "short"); w.Code != 400 {
		t.Errorf("short credential: %d", w.Code)
	}
	pinned := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c", "role": "partner-c/boss"}, 201)
	if w := joinCall(s, pinned.Code, "partner-c/dev", joinSecret); w.Code != 403 {
		t.Errorf("pinned invite, other role: %d", w.Code)
	}
	// Refusals leave the code usable.
	mustJoin(t, s, inv.Code, "partner-c/dev", joinSecret)
	mustJoin(t, s, pinned.Code, "partner-c/boss", joinSecret+"-b")
}

func TestJoinReuseExpiryAndCancel(t *testing.T) {
	s := groupServer(t)
	clock := &fakeClock{t: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	s.clock = clock.now
	used := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c"}, 201)
	mustJoin(t, s, used.Code, "partner-c/one", joinSecret)
	expired := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c", "ttl": "1h"}, 201)
	cancelled := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c"}, 201)
	if w := adminWrite(s, "DELETE", "/api/admin/invites/"+itoa(cancelled.ID), ""); w.Code != 200 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	clock.add(time.Hour)
	var bodies []string
	for _, code := range []string{used.Code, expired.Code, cancelled.Code, "AAAA-BBBB"} {
		w := joinCall(s, code, "partner-c/two", joinSecret)
		if w.Code != 410 {
			t.Fatalf("join %s: %d %s", code, w.Code, w.Body)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("410 bodies differ: %q", bodies)
		}
	}
	if identity.RoleExists(s.db, "partner-c/two") {
		t.Fatal("role created by a dead code")
	}
	// Dead codes are limited per IP.
	if w := joinCall(s, used.Code, "partner-c/two", joinSecret); w.Code != 410 {
		t.Fatalf("fifth dead code: %d", w.Code)
	}
	if w := joinCall(s, used.Code, "partner-c/two", joinSecret); w.Code != 429 {
		t.Fatalf("after %d failures: %d", loginFailLimit, w.Code)
	}
}

func TestAdminInvites(t *testing.T) {
	s := groupServer(t)
	w := adminWrite(s, "POST", "/api/admin/invites", `{"prefix":"partner-c","ttl":"24h"}`)
	if w.Code != 201 {
		t.Fatalf("admin invite: %d %s", w.Code, w.Body)
	}
	var inv wire.InviteCreated
	_ = json.Unmarshal(w.Body.Bytes(), &inv)
	if inv.CreatedBy != "admin:proxy" && !strings.HasPrefix(inv.CreatedBy, "admin:") {
		t.Fatalf("created_by = %q", inv.CreatedBy)
	}
	if w := adminRequest(s, "POST", "/api/admin/invites", `{"prefix":"partner-d"}`, nil); w.Code != 403 {
		t.Fatalf("write without guard: %d", w.Code)
	}
	second := inviteAs(t, s, bossToken, internalBossRole, map[string]string{"prefix": "partner-c"}, 201)
	mustJoin(t, s, second.Code, "partner-c/dev", joinSecret)

	if w := adminRequest(s, "GET", "/api/admin/collaborators", "", nil); w.Code != 404 {
		t.Fatalf("collaborators view: %d", w.Code)
	}
	// Revoking the group cancels its open invites and stops the role at once.
	if w := adminWrite(s, "DELETE", "/api/admin/groups/partner-c", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := joinCall(s, inv.Code, "partner-c/x", joinSecret); w.Code != 410 {
		t.Fatalf("join after group revoke: %d", w.Code)
	}
	if got := sendAs(s, joinSecret, "partner-c/dev", internalBossRole); got != 401 {
		t.Fatalf("send after revoke = %d", got)
	}
	if w := adminWrite(s, "DELETE", "/api/admin/invites/"+itoa(inv.ID), ""); w.Code != 404 {
		t.Fatalf("cancel a cancelled invite: %d", w.Code)
	}
}

// The real CLI: a global boss invites, the collaborator joins into an empty
// config dir, and the new profile works with the other commands.
func TestCLIInviteJoin(t *testing.T) {
	h := newCLIHarnessWith(t, func(s *Server) {
		if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('sandcorp/boss',?,'now')`, bossToken); err != nil {
			t.Fatal(err)
		}
		s.bossRoles = map[string]bool{"sandcorp/boss": true}
	})
	bossFile := filepath.Join(h.dir, "boss.token")
	if err := os.WriteFile(bossFile, []byte(bossToken), 0600); err != nil {
		t.Fatal(err)
	}
	h.mustRun(nil, "--profile", "boss", "config", "set", "--endpoint", h.url, "--role", "sandcorp/boss", "--token-file", bossFile)
	if r := h.run(nil, "--profile", "boss", "invite", "--prefix", "sandcorp"); r.code != 1 || !strings.Contains(r.stderr, "409") {
		t.Fatalf("invite over existing roles: %+v", r)
	}
	out := h.mustRun(nil, "--profile", "boss", "invite", "--prefix", "partner-a", "--ttl", "24h")
	var code string
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Code:"); ok {
			code = strings.TrimSpace(v)
		}
	}
	if code == "" || !strings.Contains(out, "join "+h.url+" "+code+" --role partner-a/NAME") {
		t.Fatalf("invite output:\n%s", out)
	}

	// The collaborator's machine: its own config dir.
	theirs := t.TempDir()
	env := []string{"AGENT_MAIL_CONFIG_DIR=" + theirs}
	r := h.run(env, "join", h.url, code, "--role", "partner-a/dev")
	if r.code != 0 {
		t.Fatalf("join: %+v", r)
	}
	host := cli.HostProfile("127.0.0.1")
	path := filepath.Join(theirs, host+".json")
	if !strings.Contains(r.stdout, "partner-a/dev") || !strings.Contains(r.stdout, "SWITCHBOARD_PROFILE="+host) || !strings.Contains(r.stdout, path) {
		t.Fatalf("join output:\n%s", r.stdout)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("profile %s: %v %v", path, st, err)
	}
	b, _ := os.ReadFile(path)
	var c cli.Config
	if err = json.Unmarshal(b, &c); err != nil || c.Endpoint != h.url || c.Role != "partner-a/dev" || len(c.Token) < 24 {
		t.Fatalf("profile = %s", b)
	}
	if strings.Contains(r.stdout, c.Token) || strings.Contains(r.stderr, c.Token) {
		t.Fatal("join printed the token")
	}
	var stored string
	if err = h.s.db.QueryRow(`SELECT token FROM roles WHERE role='partner-a/dev'`).Scan(&stored); err != nil || stored != store.TokenHash(c.Token) {
		t.Fatalf("server stores %q", stored)
	}
	// The profile works for other commands; group scope applies.
	h.mustRun(env, "--profile", host, "send", "--to", "sandcorp/boss", "--subject", "hello", "--body", "x")
	if r := h.run(env, "--profile", host, "send", "--to", "a", "--subject", "no", "--body", "x"); r.code != 1 || !strings.Contains(r.stderr, "403") {
		t.Fatalf("send outside group: %+v", r)
	}
	if out := h.mustRun(env, "--profile", host, "inbox", "--json"); !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("inbox --json: %s", out)
	}
	// Reuse is refused, and a taken profile name fails before spending a code.
	if r := h.run(env, "join", h.url, code, "--role", "partner-a/two", "--profile", "other"); r.code != 1 || !strings.Contains(r.stderr, "410") || exists(filepath.Join(theirs, "other.json")) {
		t.Fatalf("reuse: %+v", r)
	}
	again := h.mustRun(nil, "--profile", "boss", "invite", "--prefix", "partner-a", "--role", "partner-a/two", "--json")
	var inv wire.InviteCreated
	if err = json.Unmarshal([]byte(again), &inv); err != nil || inv.Code == "" || inv.Role == nil {
		t.Fatalf("invite --json: %s", again)
	}
	if r := h.run(env, "join", h.url, inv.Code); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("taken profile: %+v", r)
	}
	h.mustRun(env, "join", h.url, inv.Code, "--profile", "two")
}
