package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/store"
)

// legacyRolesDB is a database written by a binary from before hashed tokens:
// a three-column roles table holding plain tokens, plus one row an earlier
// run of the migration already hashed.
func legacyRolesDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO roles VALUES('done',?,'then')`, store.TokenHash("secret-done")); err != nil {
		t.Fatal(err)
	}
	return db
}

func roleTokens(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT role,token FROM ` + table + ` ORDER BY role`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var role, token string
		if err = rows.Scan(&role, &token); err != nil {
			t.Fatal(err)
		}
		out[role] = token
	}
	return out
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n == 1
}

func TestRoleTokenMigrationHashesBacksUpAndKeepsClients(t *testing.T) {
	db := legacyRolesDB(t)
	var backedUp string
	// Every start runs the migration; the second run must change nothing.
	for run := 0; run < 2; run++ {
		if err := store.Init(db); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got := roleTokens(t, db, "roles")
		want := map[string]string{"a": store.TokenHash("secret-a"), "b": store.TokenHash("secret-b"), "done": store.TokenHash("secret-done")}
		if len(got) != len(want) {
			t.Fatalf("run %d roles=%v", run, got)
		}
		for role, hash := range want {
			if got[role] != hash {
				t.Fatalf("run %d %s=%q want %q", run, role, got[role], hash)
			}
		}
		backup := roleTokens(t, db, "roles_plain_backup")
		if len(backup) != 2 || backup["a"] != "secret-a" || backup["b"] != "secret-b" {
			t.Fatalf("run %d backup=%v", run, backup)
		}
		var at string
		_ = db.QueryRow(`SELECT backed_up_at FROM roles_plain_backup WHERE role='a'`).Scan(&at)
		if run == 0 {
			backedUp = at
		} else if at != backedUp {
			t.Fatalf("second run rewrote the backup: %q then %q", backedUp, at)
		}
	}
	s := &Server{db: db}
	if w := call(s, "GET", "/api/messages", "secret-b", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "old mail") {
		t.Fatalf("old client with plain token: %d %s", w.Code, w.Body)
	}
	if w := callAs(s, "GET", "/api/messages", "secret-done", "done", nil); w.Code != 200 {
		t.Fatalf("already hashed row: %d", w.Code)
	}
	if w := callAs(s, "GET", "/api/messages", store.TokenHash("secret-b"), "b", nil); w.Code != 401 {
		t.Fatalf("the stored hash itself must not authenticate: %d", w.Code)
	}
	if w := callAs(s, "GET", "/api/messages", "secret-a", "b", nil); w.Code != 401 {
		t.Fatalf("another role's token: %d", w.Code)
	}
}

func TestRoleTokenMigrationRollsBackOnFailure(t *testing.T) {
	db := legacyRolesDB(t)
	// The last role's update fails after the backup and the first hash.
	if _, err := db.Exec(`CREATE TRIGGER fail_b BEFORE UPDATE ON roles WHEN NEW.role='b' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(db); err == nil {
		t.Fatal("migration should fail")
	}
	got := roleTokens(t, db, "roles")
	if got["a"] != "secret-a" || got["b"] != "secret-b" || got["done"] != store.TokenHash("secret-done") {
		t.Fatalf("failed migration left roles=%v", got)
	}
	if tableExists(db, "roles_plain_backup") {
		t.Fatal("failed migration left a backup table")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_b`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(db); err != nil {
		t.Fatal(err)
	}
	if got = roleTokens(t, db, "roles"); got["a"] != store.TokenHash("secret-a") || got["b"] != store.TokenHash("secret-b") {
		t.Fatalf("retry roles=%v", got)
	}
}

func TestRegisterAndSeedStoreHashes(t *testing.T) {
	s := testServer(t)
	t.Setenv("SWITCHBOARD_ENROLLMENT_TOKEN", "enrollment-secret")
	cred := "worker-credential-long-enough"
	body := map[string]string{"role": "team/worker", "credential": cred}
	w := callAs(s, "POST", "/api/register", "enrollment-secret", "", body)
	if w.Code != 201 || strings.Contains(w.Body.String(), cred) || strings.Contains(w.Body.String(), "sha256") {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	if got := roleTokens(t, s.db, "roles")["team/worker"]; got != store.TokenHash(cred) {
		t.Fatalf("stored %q", got)
	}
	if w = callAs(s, "POST", "/api/register", "", "", body); w.Code != 200 || !strings.Contains(w.Body.String(), "resumed") {
		t.Fatalf("resume: %d %s", w.Code, w.Body)
	}
	if w = callAs(s, "POST", "/api/register", "", "", map[string]string{"role": "team/worker", "credential": store.TokenHash(cred)}); w.Code != 409 {
		t.Fatalf("resume with the hash: %d", w.Code)
	}
	s.tokens = map[string]string{"seeded": "seeded-secret", "a": "changed"}
	if err := migrateConfiguredRoles(s); err != nil {
		t.Fatal(err)
	}
	got := roleTokens(t, s.db, "roles")
	if got["seeded"] != store.TokenHash("seeded-secret") || got["a"] != store.TokenHash("secret-a") {
		t.Fatalf("seed: %v", got)
	}
	if w = callAs(s, "GET", "/api/messages", "seeded-secret", "seeded", nil); w.Code != 200 {
		t.Fatalf("seeded role: %d", w.Code)
	}
}

func TestRevokeRefusesRoleAndSlotsUntilRestored(t *testing.T) {
	s, _ := slotServer(t)
	s.admin.ProxyToken = "proxy-secret"
	t.Setenv("SWITCHBOARD_ENROLLMENT_TOKEN", "enrollment-secret")
	slotDo(t, s, "register", ident("m", "/one"))
	two := slotDo(t, s, "register", ident("m", "/two"))
	if two.Role != "team/worker-2" {
		t.Fatalf("second slot=%+v", two)
	}
	if w := adminRequest(s, "POST", "/api/admin/roles/team/worker/revoke", "", nil); w.Code != 403 {
		t.Fatalf("revoke without write guard: %d", w.Code)
	}
	if w := adminWrite(s, "POST", "/api/admin/roles/team/nobody/revoke", ""); w.Code != 404 {
		t.Fatalf("revoke unknown role: %d", w.Code)
	}
	if w := adminWrite(s, "GET", "/api/admin/roles/team/worker/revoke", ""); w.Code != 405 {
		t.Fatalf("GET revoke: %d", w.Code)
	}
	w := adminWrite(s, "POST", "/api/admin/roles/team/worker/revoke", "")
	var v identity.RoleView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Role != "team/worker" || v.RevokedAt == nil {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	for _, name := range []string{"team/worker", "team/worker-1", "team/worker-2", "team/worker-7"} {
		if w := callAs(s, "GET", "/api/messages", workerToken, name, nil); w.Code != 401 {
			t.Fatalf("%s after revoke: %d", name, w.Code)
		}
	}
	if w := callAs(s, "POST", "/api/slots/heartbeat", workerToken, "team/worker", ident("m", "/one")); w.Code != 401 {
		t.Fatalf("heartbeat after revoke: %d", w.Code)
	}
	if live, err := s.presence().ActiveSlots("team/worker"); err != nil || len(live) != 0 {
		t.Fatalf("live slots after revoke: %v %v", live, err)
	}
	if w := call(s, "GET", "/api/presence", "secret-a", nil, ""); w.Code != 200 || strings.Count(w.Body.String(), `"status":"offline"`) != 2 {
		t.Fatalf("presence after revoke: %d %s", w.Code, w.Body)
	}
	if w := callAs(s, "POST", "/api/register", "", "", map[string]string{"role": "team/worker", "credential": workerToken}); w.Code != 401 {
		t.Fatalf("resume a revoked role: %d", w.Code)
	}
	if w := call(s, "GET", "/api/messages", "secret-a", nil, ""); w.Code != 200 {
		t.Fatalf("other role after revoke: %d", w.Code)
	}
	first := *v.RevokedAt
	if w = adminWrite(s, "POST", "/api/admin/roles/team/worker/revoke", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || *v.RevokedAt != first {
		t.Fatalf("second revoke: %d %s", w.Code, w.Body)
	}

	w = adminRequest(s, "GET", "/api/admin/roles", "", nil)
	var list []identity.RoleView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("roles: %d %s", w.Code, w.Body)
	}
	for _, r := range list {
		if (r.Role == "team/worker") != (r.RevokedAt != nil) {
			t.Fatalf("roles list: %s", w.Body)
		}
	}

	if w = adminWrite(s, "POST", "/api/admin/roles/team/worker/restore", ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.RevokedAt != nil {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	if w := callAs(s, "GET", "/api/messages", workerToken, "team/worker-2", nil); w.Code != 200 {
		t.Fatalf("slot after restore: %d", w.Code)
	}
	if g := slotDo(t, s, "register", ident("m", "/one")); g.Number != 1 || !g.Reclaimed {
		t.Fatalf("register after restore: %+v", g)
	}
}

func TestRoleHashesNeverLeaveTheServer(t *testing.T) {
	s, _ := slotServer(t)
	s.admin.ProxyToken = "proxy-secret"
	slotDo(t, s, "register", ident("m", "/one"))
	bodies := []string{
		adminRequest(s, "GET", "/api/admin/roles", "", nil).Body.String(),
		adminRequest(s, "GET", "/api/admin/slots", "", nil).Body.String(),
		adminRequest(s, "GET", "/api/admin/presence", "", nil).Body.String(),
		adminWrite(s, "POST", "/api/admin/roles/team/worker/revoke", "").Body.String(),
		adminWrite(s, "POST", "/api/admin/roles/team/worker/restore", "").Body.String(),
		callAs(s, "GET", "/api/presence", workerToken, "team/worker", nil).Body.String(),
		callAs(s, "GET", "/api/slots", workerToken, "team/worker", nil).Body.String(),
	}
	for _, b := range bodies {
		if strings.Contains(b, "sha256") || strings.Contains(b, workerToken) || strings.Contains(b, store.TokenHash(workerToken)[7:]) {
			t.Fatalf("secret in response: %s", b)
		}
	}
}
