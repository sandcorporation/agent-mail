package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const bossToken = "secret-boss"

// permissionServer: roles a (the worker's parent), b (a bystander),
// sandcorp/boss (a boss) and team/worker; permission TTL 10m.
func permissionServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := slotServer(t)
	s.slotLease = time.Hour
	if _, err := s.db.Exec(`INSERT INTO roles VALUES('sandcorp/boss',?,'now')`, bossToken); err != nil {
		t.Fatal(err)
	}
	s.bossRoles = map[string]bool{"sandcorp/boss": true}
	return s, clock
}

func askAs(t *testing.T, s *Server, token, role string, want int) permissionView {
	t.Helper()
	w := callAs(s, "POST", "/api/permissions", token, role, map[string]string{"tool": "Bash", "summary": "rm -rf build/"})
	if w.Code != want {
		t.Fatalf("request as %s: %d %s", role, w.Code, w.Body)
	}
	var v permissionView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func answerAs(t *testing.T, s *Server, token, role, id, decision string, want int) permissionView {
	t.Helper()
	w := callAs(s, "POST", "/api/permissions/"+id+"/answer", token, role, map[string]string{"decision": decision, "note": "ok by me"})
	if w.Code != want {
		t.Fatalf("answer %s as %s: %d %s", id, role, w.Code, w.Body)
	}
	var v permissionView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func showAs(t *testing.T, s *Server, token, role, id string, want int) permissionView {
	t.Helper()
	w := callAs(s, "GET", "/api/permissions/"+id, token, role, nil)
	if w.Code != want {
		t.Fatalf("show %s as %s: %d %s", id, role, w.Code, w.Body)
	}
	var v permissionView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return v
}

func listPermissionsAs(t *testing.T, s *Server, token, role, query string) []permissionView {
	t.Helper()
	w := callAs(s, "GET", "/api/permissions"+query, token, role, nil)
	if w.Code != 200 {
		t.Fatalf("list as %s: %d %s", role, w.Code, w.Body)
	}
	var out permissionList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Permissions
}

func registerWorker(t *testing.T, s *Server, dir, parent string) {
	t.Helper()
	r := ident("m", dir)
	r.Parent = parent
	slotDo(t, s, "register", r)
}

func TestPermissionParentAnswers(t *testing.T) {
	s, _ := permissionServer(t)
	registerWorker(t, s, "/w", "a")
	wake, stop := s.hub.subscribe("a")
	defer stop()
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	if !strings.HasPrefix(v.ID, "perm_") || len(v.ID) != 37 || v.Status != "pending" || v.Parent != "a" || v.Requester != "team/worker" ||
		v.Tool != "Bash" || v.Summary != "rm -rf build/" || !v.ExpiresAt.Equal(v.CreatedAt.Add(10*time.Minute)) {
		t.Fatalf("request: %+v", v)
	}
	select {
	case <-wake:
	default:
		t.Fatal("parent not woken")
	}
	notes := systemMail(t, s, "secret-a", "a")
	if len(notes) != 1 || notes[0].Priority != "urgent" || notes[0].TaskID != v.ID || !strings.Contains(notes[0].Body, "rm -rf build/") || !strings.Contains(notes[0].Body, v.ID) {
		t.Fatalf("parent notice: %+v", notes)
	}
	if got := systemMail(t, s, bossToken, "sandcorp/boss"); len(got) != 0 {
		t.Fatalf("boss told although a parent exists: %+v", got)
	}
	if got := listPermissionsAs(t, s, "secret-a", "a", "?pending=1"); len(got) != 1 || got[0].ID != v.ID || !got[0].CanAnswer {
		t.Fatalf("parent pending list: %+v", got)
	}
	if got := listPermissionsAs(t, s, "secret-b", "b", ""); len(got) != 0 {
		t.Fatalf("bystander list: %+v", got)
	}

	// Any number of the parent's base may answer: one token covers them all.
	a := answerAs(t, s, "secret-a", "a-2", v.ID, "allow", 200)
	if a.Status != "allowed" || a.Decision != "allow" || a.AnsweredBy != "a-2" || a.Note != "ok by me" || a.AnsweredAt == nil {
		t.Fatalf("answer: %+v", a)
	}
	got := showAs(t, s, workerToken, "team/worker-1", v.ID, 200)
	if got.Status != "allowed" || got.CanAnswer {
		t.Fatalf("requester view: %+v", got)
	}
	told := systemMail(t, s, workerToken, "team/worker")
	if len(told) != 1 || told[0].Priority != "urgent" || told[0].TaskID != v.ID || told[0].Subject != "permission allowed: Bash" || !strings.Contains(told[0].Body, "ok by me") {
		t.Fatalf("requester notice: %+v", told)
	}
	// A second answer, even the same one, is a conflict.
	answerAs(t, s, "secret-a", "a", v.ID, "deny", 409)
	answerAs(t, s, bossToken, "sandcorp/boss", v.ID, "allow", 409)
	if got := listPermissionsAs(t, s, "secret-a", "a", "?pending=1"); len(got) != 0 {
		t.Fatalf("answered request still pending: %+v", got)
	}
}

func TestPermissionBossAnswers(t *testing.T) {
	s, _ := permissionServer(t)
	registerWorker(t, s, "/w", "a")
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	if got := listPermissionsAs(t, s, bossToken, "sandcorp/boss", "?pending=1"); len(got) != 1 || !got[0].CanAnswer {
		t.Fatalf("boss pending list: %+v", got)
	}
	if a := answerAs(t, s, bossToken, "sandcorp/boss", v.ID, "deny", 200); a.Status != "denied" || a.AnsweredBy != "sandcorp/boss" {
		t.Fatalf("boss answer: %+v", a)
	}
	if told := systemMail(t, s, workerToken, "team/worker"); len(told) != 1 || told[0].Subject != "permission denied: Bash" {
		t.Fatalf("requester notice: %+v", told)
	}

	// Without a parent the bosses are told.
	slotDo(t, s, "release", ident("m", "/w"))
	w := askAs(t, s, "secret-b", "b", 201)
	if w.Parent != "" {
		t.Fatalf("parent without a slot: %+v", w)
	}
	if got := systemMail(t, s, bossToken, "sandcorp/boss"); len(got) != 1 || got[0].TaskID != w.ID || got[0].Priority != "urgent" {
		t.Fatalf("boss notice: %+v", got)
	}
	answerAs(t, s, "secret-a", "a", w.ID, "allow", 403)
	answerAs(t, s, bossToken, "sandcorp/boss-2", w.ID, "allow", 200)
}

func TestPermissionRejectsOtherAnswerers(t *testing.T) {
	s, _ := permissionServer(t)
	registerWorker(t, s, "/w", "a")
	registerWorker(t, s, "/w2", "a")
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	answerAs(t, s, "secret-b", "b", v.ID, "allow", 403)              // bystander
	answerAs(t, s, workerToken, "team/worker-1", v.ID, "allow", 403) // the requester
	answerAs(t, s, workerToken, "team/worker", v.ID, "allow", 403)   // the requester, unnumbered
	answerAs(t, s, workerToken, "team/worker-2", v.ID, "allow", 403) // a sibling slot
	showAs(t, s, "secret-b", "b", v.ID, 403)                         // bystanders cannot read it either
	answerAs(t, s, "secret-b", "team/worker-1", v.ID, "allow", 401)  // token of another base
	answerAs(t, s, "secret-a", "a", "perm_"+strings.Repeat("0", 32), "allow", 404)
	answerAs(t, s, "secret-a", "a", "nonsense", "allow", 404)
	showAs(t, s, "secret-a", "a", "perm_missing", 404)
	answerAs(t, s, "secret-a", "a", v.ID, "maybe", 400)
	if got := showAs(t, s, workerToken, "team/worker-2", v.ID, 200); got.Status != "pending" || got.CanAnswer {
		t.Fatalf("after rejected answers: %+v", got)
	}

	// The requester's base never answers, even when it is a boss.
	s.bossRoles["team/worker"] = true
	answerAs(t, s, workerToken, "team/worker-2", v.ID, "allow", 403)
	answerAs(t, s, "secret-a", "a", v.ID, "allow", 200)
}

func TestPermissionSameBaseParentIsIgnored(t *testing.T) {
	s, _ := permissionServer(t)
	registerWorker(t, s, "/w", "team/worker-2") // a sibling as parent
	registerWorker(t, s, "/w2", "team/worker")  // itself, unnumbered
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	if v.Parent != "" || v.IgnoredParent != "team/worker-2" {
		t.Fatalf("same-base parent: %+v", v)
	}
	answerAs(t, s, workerToken, "team/worker-2", v.ID, "allow", 403)
	if got := systemMail(t, s, bossToken, "sandcorp/boss"); len(got) != 1 {
		t.Fatalf("boss not told: %+v", got)
	}
	answerAs(t, s, bossToken, "sandcorp/boss", v.ID, "allow", 200)

	// With no boss configured, nobody could answer: refused up front.
	s.bossRoles = map[string]bool{}
	if w := callAs(s, "POST", "/api/permissions", workerToken, "team/worker-2", map[string]string{"tool": "Bash", "summary": "x"}); w.Code != 409 {
		t.Fatalf("request nobody can answer: %d %s", w.Code, w.Body)
	}
	// A parent that is not a registered role is ignored too.
	registerWorker(t, s, "/w3", "ghost/lead")
	if w := callAs(s, "POST", "/api/permissions", workerToken, "team/worker-3", map[string]string{"tool": "Bash", "summary": "x"}); w.Code != 409 {
		t.Fatalf("unknown parent: %d %s", w.Code, w.Body)
	}
}

func TestPermissionParentIsFixedAtRequestTime(t *testing.T) {
	s, _ := permissionServer(t)
	registerWorker(t, s, "/w", "a")
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	// The worker repoints its slot to b; the request keeps a.
	r := ident("m", "/w")
	r.Parent = "b"
	slotDo(t, s, "heartbeat", r)
	answerAs(t, s, "secret-b", "b", v.ID, "allow", 403)
	answerAs(t, s, "secret-a", "a", v.ID, "allow", 200)
	// New requests use the new parent.
	w := askAs(t, s, workerToken, "team/worker-1", 201)
	if w.Parent != "b" {
		t.Fatalf("new request parent: %+v", w)
	}
}

func TestPermissionExpires(t *testing.T) {
	s, clock := permissionServer(t)
	s.permissionTTL = time.Minute
	registerWorker(t, s, "/w", "a")
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	done := askAs(t, s, workerToken, "team/worker-1", 201)
	answerAs(t, s, "secret-a", "a", done.ID, "deny", 200)
	clock.add(time.Minute)
	answerAs(t, s, "secret-a", "a", v.ID, "allow", 410)
	answerAs(t, s, "secret-b", "b", v.ID, "allow", 403) // still not theirs
	answerAs(t, s, "secret-a", "a", done.ID, "allow", 409)
	if got := showAs(t, s, workerToken, "team/worker-1", v.ID, 200); got.Status != "expired" || got.Decision != "" {
		t.Fatalf("expired view: %+v", got)
	}
	if got := listPermissionsAs(t, s, "secret-a", "a", "?pending=1"); len(got) != 0 {
		t.Fatalf("expired listed as pending: %+v", got)
	}
	if got := listPermissionsAs(t, s, "secret-a", "a", ""); len(got) != 2 {
		t.Fatalf("parent list: %+v", got)
	}
}

func TestPermissionRequestValidation(t *testing.T) {
	s, _ := permissionServer(t)
	for _, body := range []map[string]string{
		{"summary": "x"},
		{"tool": "Bash"},
		{"tool": strings.Repeat("t", maxPermissionTool+1), "summary": "x"},
		{"tool": "Bash", "summary": strings.Repeat("s", maxPermissionText+1)},
	} {
		if w := callAs(s, "POST", "/api/permissions", "secret-b", "b", body); w.Code != 400 {
			t.Fatalf("%v: %d", body, w.Code)
		}
	}
	v := askAs(t, s, "secret-b", "b", 201)
	w := callAs(s, "POST", "/api/permissions/"+v.ID+"/answer", bossToken, "sandcorp/boss", map[string]string{"decision": "allow", "note": strings.Repeat("n", maxPermissionText+1)})
	if w.Code != 400 {
		t.Fatalf("long note: %d", w.Code)
	}
	if w := callAs(s, "DELETE", "/api/permissions/"+v.ID, bossToken, "sandcorp/boss", nil); w.Code != 405 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := callAs(s, "GET", "/api/permissions", "wrong", "b", nil); w.Code != 401 {
		t.Fatalf("bad token: %d", w.Code)
	}
}

func TestPermissionSchemaOnLegacyDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err = db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = initSchema(db); err != nil {
			t.Fatalf("schema run %d: %v", i, err)
		}
	}
	s := &Server{db: db, tokens: map[string]string{}, bossRoles: map[string]bool{"a": true}}
	v := askAs(t, s, "secret-b", "b", 201)
	answerAs(t, s, "secret-a", "a", v.ID, "allow", 200)
	if got := listAs(t, s, "secret-b", "b", "inbox"); len(got) != 2 || got[1].Subject != "old mail" {
		t.Fatalf("legacy inbox: %+v", got)
	}
}

func TestPermissionCommand(t *testing.T) {
	h := newCLIHarnessWith(t, func(s *Server) { s.bossRoles = map[string]bool{"a": true} })
	h.configure([]string{"--profile", "worker"}, "b")
	h.configure([]string{"--profile", "boss"}, "a")
	worker, boss := []string{"AGENT_MAIL_PROFILE=worker"}, []string{"AGENT_MAIL_PROFILE=boss"}
	out := h.mustRun(worker, "permission", "request", "--tool", "Bash", "--summary", "make deploy")
	id := strings.Fields(out)[0]
	if !strings.HasPrefix(id, "perm_") || !strings.Contains(out, "pending  b  Bash | make deploy") {
		t.Fatalf("request: %q", out)
	}
	if out = h.mustRun(boss, "permission", "list", "--pending"); !strings.Contains(out, id) {
		t.Fatalf("boss list: %q", out)
	}
	if out = h.mustRun(boss, "permission", "answer", id, "--decision", "allow", "--note", "go ahead"); !strings.Contains(out, "allowed") || !strings.Contains(out, "by a") {
		t.Fatalf("answer: %q", out)
	}
	if out = h.mustRun(worker, "permission", "show", id, "--json"); !strings.Contains(out, `"status":"allowed"`) || strings.Contains(out, "secret") {
		t.Fatalf("show: %q", out)
	}
	if r := h.run(boss, "permission", "answer", id, "--decision", "allow"); r.code == 0 || !strings.Contains(r.stderr, "409") {
		t.Fatalf("second answer: %d %s", r.code, r.stderr)
	}
	if out = h.mustRun(boss, "permission", "list", "--pending"); strings.TrimSpace(out) != "no permission requests" {
		t.Fatalf("empty list: %q", out)
	}
	for _, args := range [][]string{
		{"permission"},
		{"permission", "grant"},
		{"permission", "show"},
		{"permission", "list", "extra"},
		{"permission", "request", "--tool", "Bash"},
		{"permission", "answer", id, "--decision", "maybe"},
	} {
		if r := h.run(boss, args...); r.code != exitUsage {
			t.Fatalf("%v: %d %s", args, r.code, r.stderr)
		}
	}
}

func TestPermissionConcurrentAnswersAndSweepsActOnce(t *testing.T) {
	s, clock := permissionServer(t)
	registerWorker(t, s, "/w", "a")
	v := askAs(t, s, workerToken, "team/worker-1", 201)
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		token, role := "secret-a", "a"
		if i%2 == 1 {
			token, role = bossToken, "sandcorp/boss"
		}
		go func() {
			codes <- callAs(s, "POST", "/api/permissions/"+v.ID+"/answer", token, role, map[string]string{"decision": "allow"}).Code
		}()
	}
	got := map[int]int{}
	for i := 0; i < 8; i++ {
		got[<-codes]++
	}
	if got[200] != 1 || got[409] != 7 {
		t.Fatalf("answer codes: %v", got)
	}
	if told := systemMail(t, s, workerToken, "team/worker"); len(told) != 1 {
		t.Fatalf("requester told %d times", len(told))
	}

	s.slotLease = time.Minute
	registerWorker(t, s, "/w2", "a")
	clock.add(2 * time.Hour)
	sent := make(chan int, 6)
	for i := 0; i < 6; i++ {
		go func() {
			r, err := s.sweep(t.Context())
			if err != nil {
				t.Error(err)
			}
			sent <- r.Offline
		}()
	}
	total := 0
	for i := 0; i < 6; i++ {
		total += <-sent
	}
	if total != 2 { // team/worker-1 and team/worker-2, once each
		t.Fatalf("offline notices from concurrent sweeps: %d", total)
	}
}

func TestGlobalRoleOptionActsAsNumberedRole(t *testing.T) {
	h := newCLIHarnessWith(t, func(s *Server) {
		if _, err := s.db.Exec(`INSERT INTO roles VALUES('team/worker',?,'now')`, workerToken); err != nil {
			t.Fatal(err)
		}
	})
	f := filepath.Join(h.dir, "worker.token")
	if err := os.WriteFile(f, []byte(workerToken), 0600); err != nil {
		t.Fatal(err)
	}
	h.tokens["team/worker"] = f
	h.configure(nil, "team/worker")
	h.configure(nil, "a")
	h.mustRun(nil, "send", "--to", "team/worker-2", "--subject", "for two", "--body", "x")
	h.configure(nil, "team/worker")
	out := h.mustRun(nil, "--role", "team/worker-2", "inbox", "--json")
	var msgs []Message
	if err := json.Unmarshal([]byte(out), &msgs); err != nil || len(msgs) != 1 {
		t.Fatalf("inbox as -2: %q", out)
	}
	id := itoa(msgs[0].ID)
	// Without the role the base is refused with a pointer to the right role.
	if r := h.run(nil, "read", id); r.code != 1 || !strings.Contains(r.stderr, "--role team/worker-2") {
		t.Fatalf("read as base: %d %s", r.code, r.stderr)
	}
	h.mustRun([]string{"AGENT_MAIL_ROLE=team/worker-2"}, "read", id)
	if out = h.mustRun(nil, "--role=team/worker-2", "inbox", "--status", "read", "--json"); !strings.Contains(out, "for two") {
		t.Fatalf("state not changed for -2: %q", out)
	}
	// --role wins over the env; both must stay under the profile's base.
	if out = h.mustRun([]string{"AGENT_MAIL_ROLE=team/worker-7"}, "--role", "team/worker-2", "inbox", "--json"); !strings.Contains(out, "for two") {
		t.Fatalf("--role over env: %q", out)
	}
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"--role", "team/other-2", "inbox"}},
		{nil, []string{"--role", "a", "inbox"}},
		{nil, []string{"--role", "team/worker-0", "inbox"}},
		{[]string{"AGENT_MAIL_ROLE=b"}, []string{"inbox"}},
		{nil, []string{"--role"}},
	} {
		if r := h.run(c.env, c.args...); r.code != exitUsage {
			t.Fatalf("%v %v: %d %s", c.env, c.args, r.code, r.stderr)
		}
	}
}
