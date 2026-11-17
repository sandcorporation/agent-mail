package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

const (
	partnerBossToken  = "secret-partner-a-boss"
	partnerDevToken   = "secret-partner-a-dev"
	partnerBDevToken  = "secret-partner-b-dev"
	partnerBossRole   = "partner-a/boss"
	partnerDevRole    = "partner-a/dev"
	partnerBDevRole   = "partner-b/dev"
	internalBossRole  = "sandcorp/boss"
	internalWorkerRol = "team/worker"
)

// groupServer is questionServer (global boss sandcorp/boss, internal
// team/worker, a, b) plus partner-a/boss, partner-a/dev and partner-b/dev.
// No group exists yet.
func groupServer(t *testing.T) *Server {
	t.Helper()
	s := questionServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES(?,?,'now'),(?,?,'now'),(?,?,'now')`,
		partnerBossRole, partnerBossToken, partnerDevRole, partnerDevToken, partnerBDevRole, partnerBDevToken); err != nil {
		t.Fatal(err)
	}
	return s
}

func putGroup(t *testing.T, s *Server, prefix, body string) identity.Group {
	t.Helper()
	w := adminWrite(s, "PUT", "/api/admin/groups/"+prefix, body)
	if w.Code != 200 {
		t.Fatalf("PUT group %s: %d %s", prefix, w.Code, w.Body)
	}
	var g identity.Group
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func sendAs(s *Server, token, role, to string) int {
	return callAs(s, "POST", "/api/messages", token, role, map[string]string{"recipient": to, "subject": "hi", "body": "x"}).Code
}

func whoamiAs(t *testing.T, s *Server, token, role string) identity.Whoami {
	t.Helper()
	w := callAs(s, "GET", "/api/whoami", token, role, nil)
	if w.Code != 200 {
		t.Fatalf("whoami %s: %d %s", role, w.Code, w.Body)
	}
	var v identity.Whoami
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func presenceRoles(t *testing.T, s *Server, token, role string) []string {
	t.Helper()
	w := callAs(s, "GET", "/api/presence", token, role, nil)
	if w.Code != 200 {
		t.Fatalf("presence %s: %d %s", role, w.Code, w.Body)
	}
	var out wire.PresenceList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, v := range out.Sessions {
		roles = append(roles, v.Base)
	}
	return roles
}

func registerSessionAs(t *testing.T, s *Server, token, role, dir string) {
	t.Helper()
	if w := callAs(s, "POST", "/api/slots/register", token, role, ident("m", dir)); w.Code != 200 {
		t.Fatalf("register %s: %d %s", role, w.Code, w.Body)
	}
}

func questionPollIDs(t *testing.T, s *Server, token, role string) []string {
	t.Helper()
	var ids []string
	for _, q := range pollAs(t, s, token, role, "?timeout=0").Questions {
		ids = append(ids, q.ID)
	}
	return ids
}

// With no group every role is internal: names that look like a group change
// nothing. Send, presence, questions, permissions and metrics behave as
// before groups.
func TestNoGroupsKeepsBehaviour(t *testing.T) {
	s := groupServer(t)
	for _, to := range []string{"b", internalWorkerRol, partnerBDevRole, internalBossRole} {
		if code := sendAs(s, partnerDevToken, partnerDevRole, to); code != 201 {
			t.Fatalf("send to %s: %d", to, code)
		}
	}
	registerSessionAs(t, s, partnerDevToken, partnerDevRole, "/a")
	registerSessionAs(t, s, workerToken, internalWorkerRol, "/w")
	registerSessionAs(t, s, partnerBDevToken, partnerBDevRole, "/b")
	want := []string{partnerDevRole, partnerBDevRole, internalWorkerRol}
	for _, who := range [][2]string{{partnerDevToken, partnerDevRole}, {workerToken, internalWorkerRol}} {
		if got := presenceRoles(t, s, who[0], who[1]); !slices.Equal(got, want) {
			t.Fatalf("presence as %s = %v, want %v", who[1], got, want)
		}
	}
	q := askQuestionAs(t, s, partnerDevToken, partnerDevRole, 201)
	if got := questionPollIDs(t, s, bossToken, internalBossRole); !slices.Equal(got, []string{q.ID}) {
		t.Fatalf("global boss poll = %v", got)
	}
	if got := questionPollIDs(t, s, partnerBossToken, partnerBossRole); len(got) != 0 {
		t.Fatalf("partner-a/boss is no boss without groups, poll = %v", got)
	}
	answerQuestionAs(t, s, partnerBossToken, partnerBossRole, q.ID, sampleAnswers(), 403)
	answerQuestionAs(t, s, bossToken, internalBossRole, q.ID, sampleAnswers(), 200)
	p := askAs(t, s, partnerDevToken, partnerDevRole, 201)
	if got := listPermissionsAs(t, s, bossToken, internalBossRole, ""); len(got) != 1 || got[0].ID != p.ID || !got[0].CanAnswer {
		t.Fatalf("global boss permissions = %+v", got)
	}
	answerAs(t, s, partnerBossToken, partnerBossRole, p.ID, "allow", 403)
	if w := callAs(s, "GET", "/api/metrics", bossToken, internalBossRole, nil); w.Code != 200 {
		t.Fatalf("metrics: %d", w.Code)
	}
	if v := whoamiAs(t, s, bossToken, internalBossRole); v.Group != "" || !v.GlobalBoss || !v.IsBoss || len(v.BossOf) != 0 {
		t.Fatalf("whoami boss = %+v", v)
	}
	if v := whoamiAs(t, s, partnerBossToken, partnerBossRole); v.Group != "" || v.GlobalBoss || v.IsBoss {
		t.Fatalf("whoami partner-a/boss = %+v", v)
	}
	// A group that covers none of these roles changes no response byte.
	reads := [][2]string{{bossToken, internalBossRole}, {partnerDevToken, partnerDevRole}, {workerToken, internalWorkerRol}}
	paths := []string{"/api/presence", "/api/questions", "/api/questions/poll?timeout=0", "/api/permissions", "/api/decisions", "/api/messages", "/api/metrics", "/api/progress"}
	snapshot := func() []string {
		var out []string
		for _, who := range reads {
			for _, path := range paths {
				w := callAs(s, "GET", path, who[0], who[1], nil)
				out = append(out, itoa(int64(w.Code))+" "+w.Body.String())
			}
		}
		return out
	}
	before := snapshot()
	putGroup(t, s, "partner-z", "")
	if after := snapshot(); !slices.Equal(before, after) {
		t.Fatalf("an unrelated group changed responses:\n%v\n%v", before, after)
	}
	w := adminRequest(s, "GET", "/api/admin/roles", "", nil)
	var roles []identity.RoleView
	_ = json.Unmarshal(w.Body.Bytes(), &roles)
	for _, r := range roles {
		if r.Group != "" {
			t.Fatalf("role %s has group %q", r.Role, r.Group)
		}
	}
}

func TestGroupAdminAPI(t *testing.T) {
	s := groupServer(t)
	g := putGroup(t, s, "partner-a", "")
	if g.Prefix != "partner-a" || g.BossRole != partnerBossRole || g.RevokedAt != nil {
		t.Fatalf("group = %+v", g)
	}
	if g = putGroup(t, s, "partner-b", `{"boss_role":"partner-b/lead"}`); g.BossRole != "partner-b/lead" {
		t.Fatalf("partner-b = %+v", g)
	}
	for _, bad := range []string{"partner-a/", "/x", "x!y"} {
		if w := adminWrite(s, "PUT", "/api/admin/groups/"+bad, ""); w.Code != 400 && w.Code != 404 {
			t.Fatalf("PUT %q: %d", bad, w.Code)
		}
	}
	if w := adminRequest(s, "PUT", "/api/admin/groups/x", "", nil); w.Code != 403 {
		t.Fatalf("PUT without dashboard guard: %d", w.Code)
	}
	if w := adminRequest(s, "GET", "/api/admin/groups/none", "", nil); w.Code != 404 {
		t.Fatalf("GET missing: %d", w.Code)
	}
	w := adminRequest(s, "GET", "/api/admin/roles", "", nil)
	var roles []identity.RoleView
	_ = json.Unmarshal(w.Body.Bytes(), &roles)
	groups := map[string]string{}
	for _, r := range roles {
		groups[r.Role] = r.Group
	}
	if groups[partnerDevRole] != "partner-a" || groups[partnerBDevRole] != "partner-b" || groups["team/worker"] != "" {
		t.Fatalf("role groups = %v", groups)
	}

	// Revoking a group revokes its roles at once and keeps the group's
	// scope; registration under it is refused.
	if w = adminWrite(s, "DELETE", "/api/admin/groups/partner-a", ""); w.Code != 200 {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body)
	}
	if w = callAs(s, "GET", "/api/messages", partnerDevToken, partnerDevRole, nil); w.Code != 401 {
		t.Fatalf("revoked group role: %d", w.Code)
	}
	if w = callAs(s, "GET", "/api/messages", partnerBDevToken, partnerBDevRole, nil); w.Code != 200 {
		t.Fatalf("other group role: %d", w.Code)
	}
	s.db.Exec(`UPDATE roles SET revoked_at=NULL WHERE role=?`, partnerDevRole)
	if code := sendAs(s, partnerDevToken, partnerDevRole, "b"); code != 403 {
		t.Fatalf("role restored alone in a revoked group still scoped: %d", code)
	}
	t.Setenv("SWITCHBOARD_ENROLLMENT_TOKEN", "enroll-secret-enroll-secret")
	r := callAs(s, "POST", "/api/register", "enroll-secret-enroll-secret", "", map[string]string{"role": "partner-a/new", "credential": "new-credential-new-credential"})
	if r.Code != 403 {
		t.Fatalf("register in revoked group: %d %s", r.Code, r.Body)
	}
	if g = putGroup(t, s, "partner-a", ""); g.RevokedAt != nil {
		t.Fatalf("PUT restores the group: %+v", g)
	}
}

func TestGroupSendScope(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	putGroup(t, s, "partner-b", "")
	cases := []struct {
		token, role, to string
		want            int
	}{
		{partnerDevToken, partnerDevRole, partnerBossRole, 201},
		{partnerDevToken, partnerDevRole, partnerDevRole + "-2", 201},
		{partnerDevToken, partnerDevRole, internalBossRole, 201},
		{partnerDevToken, partnerDevRole, "b", 403},
		{partnerDevToken, partnerDevRole, internalWorkerRol, 403},
		{partnerDevToken, partnerDevRole, partnerBDevRole, 403},
		{partnerDevToken, partnerDevRole, partnerBDevRole + "@x", 403},
		{workerToken, internalWorkerRol, partnerDevRole, 201},
		{bossToken, internalBossRole, partnerBDevRole, 201},
		{partnerBDevToken, partnerBDevRole, internalBossRole, 201},
	}
	for _, c := range cases {
		if got := sendAs(s, c.token, c.role, c.to); got != c.want {
			t.Errorf("%s -> %s: %d, want %d", c.role, c.to, got, c.want)
		}
	}
	w := callAs(s, "POST", "/api/messages", partnerDevToken, partnerDevRole, map[string]string{"recipient": "b", "subject": "hi"})
	if w.Code != 403 || w.Body.String() != "outside your group\n" {
		t.Fatalf("body = %d %q", w.Code, w.Body)
	}
	// A group role may answer whoever mailed it.
	var id int64
	_ = s.db.QueryRow(`SELECT id FROM messages WHERE sender=? AND recipient=?`, internalWorkerRol, partnerDevRole).Scan(&id)
	if w = callAs(s, "POST", "/api/messages/"+itoa(id)+"/reply", partnerDevToken, partnerDevRole, map[string]string{"subject": "re", "body": "ok"}); w.Code != 201 {
		t.Fatalf("reply: %d %s", w.Code, w.Body)
	}
}

func TestGroupPresenceScope(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	putGroup(t, s, "partner-b", "")
	registerSessionAs(t, s, partnerDevToken, partnerDevRole, "/a")
	registerSessionAs(t, s, partnerBossToken, partnerBossRole, "/ab")
	registerSessionAs(t, s, workerToken, internalWorkerRol, "/w")
	registerSessionAs(t, s, partnerBDevToken, partnerBDevRole, "/b")
	all := []string{partnerBossRole, partnerDevRole, partnerBDevRole, internalWorkerRol}
	for _, c := range []struct {
		token, role string
		want        []string
	}{
		{partnerDevToken, partnerDevRole, []string{partnerBossRole, partnerDevRole}},
		{partnerBDevToken, partnerBDevRole, []string{partnerBDevRole}},
		{workerToken, internalWorkerRol, all},
		{bossToken, internalBossRole, all},
	} {
		if got := presenceRoles(t, s, c.token, c.role); !slices.Equal(got, c.want) {
			t.Errorf("presence as %s = %v, want %v", c.role, got, c.want)
		}
	}
	w := adminRequest(s, "GET", "/api/admin/presence", "", nil)
	var out wire.PresenceList
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Sessions) != 4 {
		t.Fatalf("admin presence = %d sessions", len(out.Sessions))
	}
}

func TestGroupQuestionsGoToGroupBoss(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	putSettings(t, s, "partner-a/*", `{"can_ask":true,"watchers":["sandcorp/boss"]}`)
	q := askQuestionAs(t, s, partnerDevToken, partnerDevRole, 201)
	internal := askQuestionAs(t, s, workerToken, internalWorkerRol, 201)

	if got := questionPollIDs(t, s, partnerBossToken, partnerBossRole); !slices.Equal(got, []string{q.ID}) {
		t.Fatalf("group boss poll = %v", got)
	}
	if got := questionPollIDs(t, s, bossToken, internalBossRole); !slices.Equal(got, []string{internal.ID}) {
		t.Fatalf("global boss poll = %v", got)
	}
	if got := questionPollIDs(t, s, partnerDevToken, partnerDevRole); !slices.Equal(got, []string{q.ID}) {
		t.Fatalf("asker poll = %v", got)
	}
	if w := callAs(s, "GET", "/api/questions/"+internal.ID, partnerBossToken, partnerBossRole, nil); w.Code == 200 {
		t.Fatal("group boss read an internal question")
	}
	// The watcher setting still mails the global boss.
	var told bool
	for _, m := range questionMail(t, s, q.ID) {
		told = told || m.to == internalBossRole
	}
	if !told {
		t.Fatal("watcher mail did not reach the global boss")
	}
	answerQuestionAs(t, s, bossToken, internalBossRole, q.ID, sampleAnswers(), 403)
	answerQuestionAs(t, s, partnerBossToken, partnerBossRole, internal.ID, sampleAnswers(), 403)
	if v := answerQuestionAs(t, s, partnerBossToken, partnerBossRole, q.ID, sampleAnswers(), 200); v.AnsweredBy != partnerBossRole {
		t.Fatalf("answered by %q", v.AnsweredBy)
	}
	w := callAs(s, "GET", "/api/decisions", partnerDevToken, partnerDevRole, nil)
	var d decisionList
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if len(d.Decisions) != 1 || d.Decisions[0].ID != q.ID {
		t.Fatalf("group decisions = %+v", d.Decisions)
	}

	// The group boss cannot answer its own question, and a group whose boss
	// role is not registered has nobody: 409.
	askQuestionAs(t, s, partnerBossToken, partnerBossRole, 409)
	putGroup(t, s, "partner-b", "")
	askQuestionAs(t, s, partnerBDevToken, partnerBDevRole, 409)
}

func TestGroupPermissionsGoToGroupBoss(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	p := askAs(t, s, partnerDevToken, partnerDevRole, 201)
	internal := askAs(t, s, workerToken, internalWorkerRol, 201)
	var notified []string
	rows, _ := s.db.Query(`SELECT recipient FROM messages WHERE task_id=?`, p.ID)
	for rows.Next() {
		var to string
		_ = rows.Scan(&to)
		notified = append(notified, to)
	}
	rows.Close()
	if !slices.Equal(notified, []string{partnerBossRole}) {
		t.Fatalf("notified = %v", notified)
	}
	ids := func(vs []permissionView) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	if got := ids(listPermissionsAs(t, s, partnerBossToken, partnerBossRole, "")); !slices.Equal(got, []string{p.ID}) {
		t.Fatalf("group boss list = %v", got)
	}
	if got := ids(listPermissionsAs(t, s, bossToken, internalBossRole, "")); !slices.Equal(got, []string{internal.ID}) {
		t.Fatalf("global boss list = %v", got)
	}
	showAs(t, s, bossToken, internalBossRole, p.ID, 403)
	answerAs(t, s, bossToken, internalBossRole, p.ID, "allow", 403)
	answerAs(t, s, partnerBossToken, partnerBossRole, internal.ID, "allow", 403)
	answerAs(t, s, partnerBossToken, partnerBossRole, p.ID, "allow", 200)
	answerAs(t, s, bossToken, internalBossRole, internal.ID, "deny", 200)
	putGroup(t, s, "partner-b", "")
	askAs(t, s, partnerBDevToken, partnerBDevRole, 409)
}

func TestGroupBossIsNotGlobal(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	for _, path := range []string{"/api/metrics", "/api/progress"} {
		if w := callAs(s, "GET", path, partnerBossToken, partnerBossRole, nil); w.Code != 403 {
			t.Errorf("%s as group boss: %d", path, w.Code)
		}
	}
	v := whoamiAs(t, s, partnerBossToken, partnerBossRole)
	if v.Role != partnerBossRole || v.Group != "partner-a" || v.GlobalBoss || !v.IsBoss || !slices.Equal(v.BossOf, []string{"partner-a"}) {
		t.Fatalf("whoami group boss = %+v", v)
	}
	if v = whoamiAs(t, s, partnerDevToken, partnerDevRole); v.Group != "partner-a" || v.IsBoss || len(v.BossOf) != 0 {
		t.Fatalf("whoami group dev = %+v", v)
	}
	if v = whoamiAs(t, s, bossToken, internalBossRole); v.Group != "" || !v.GlobalBoss || !v.IsBoss {
		t.Fatalf("whoami global boss = %+v", v)
	}
}
