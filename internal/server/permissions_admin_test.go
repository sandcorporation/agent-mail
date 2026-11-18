package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func adminPermissionList(t *testing.T, s *Server, query string) []wire.AdminPermission {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/permissions"+query, "", nil)
	var out wire.AdminPermissionList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("GET permissions%s=%d %s", query, w.Code, w.Body)
	}
	return out.Permissions
}

// permissionRow is the stored answer of one request.
func permissionRow(t *testing.T, s *Server, id string) (decision, note, by string, at int64) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT decision,note,answered_by,answered_at FROM permissions WHERE id=?`, id).Scan(&decision, &note, &by, &at); err != nil {
		t.Fatal(err)
	}
	return
}

// A dashboard answer and a boss's `permission answer` go through one server
// function: the same row, the same mail and the same JSON, except who answered.
func TestAdminPermissionAnswerMatchesBossAnswer(t *testing.T) {
	s, _ := permissionServer(t)
	s.admin.ProxyToken = "proxy-secret"
	registerWorker(t, s, "/w", "a")
	byBoss := askAs(t, s, workerToken, "team/worker-1", 201)
	byAdmin := askAs(t, s, workerToken, "team/worker-1", 201)

	w := callAs(s, "POST", "/api/permissions/"+byBoss.ID+"/answer", bossToken, "sandcorp/boss", map[string]string{"decision": "deny", "note": "<b>not now</b>"})
	if w.Code != 200 {
		t.Fatalf("boss answer: %d %s", w.Code, w.Body)
	}
	bossJSON := w.Body.String()

	path := "/api/admin/permissions/" + byAdmin.ID + "/answer"
	body := `{"decision":"deny","note":"<b>not now</b>"}`
	if w := adminRequest(s, "POST", path, body, map[string]string{"Content-Type": "application/json"}); w.Code != 403 {
		t.Fatalf("without the dashboard header: %d", w.Code)
	}
	if w := adminRequest(s, "POST", path, body, map[string]string{"Content-Type": "application/json", "X-Switchboard-Dashboard": "1", "Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatalf("cross-site: %d", w.Code)
	}
	if w := adminRequestWith(s, "POST", path, body, dashboardHeaders); w.Code != 401 {
		t.Fatalf("without admin auth: %d", w.Code)
	}
	w = adminWrite(s, "POST", path, body)
	if w.Code != 200 {
		t.Fatalf("admin answer: %d %s", w.Code, w.Body)
	}
	adminJSON := w.Body.String()

	norm := func(js, id, by string) string {
		return strings.NewReplacer(id, "ID", `"answered_by":"`+by+`"`, `"answered_by":"BY"`).Replace(js)
	}
	var a, b wire.PermissionView
	_ = json.Unmarshal([]byte(bossJSON), &a)
	_ = json.Unmarshal([]byte(adminJSON), &b)
	strip := func(js string, v wire.PermissionView) string {
		// created/expires/answered times differ only by the request order.
		for _, t := range []string{v.CreatedAt.Format(time.RFC3339Nano), v.ExpiresAt.Format(time.RFC3339Nano), v.AnsweredAt.Format(time.RFC3339Nano)} {
			js = strings.ReplaceAll(js, t, "T")
		}
		return js
	}
	if got, want := strip(norm(adminJSON, byAdmin.ID, "dashboard"), b), strip(norm(bossJSON, byBoss.ID, "sandcorp/boss"), a); got != want {
		t.Fatalf("responses differ:\nadmin %s\nboss  %s", got, want)
	}
	if b.Status != "denied" || b.AnsweredBy != "dashboard" || b.CanAnswer {
		t.Fatalf("admin answer view: %+v", b)
	}
	d1, n1, _, at1 := permissionRow(t, s, byBoss.ID)
	d2, n2, by2, at2 := permissionRow(t, s, byAdmin.ID)
	if d1 != d2 || n1 != n2 || by2 != "dashboard" || at1 == 0 || at2 == 0 {
		t.Fatalf("rows differ: %q %q / %q %q %q", d1, n1, d2, n2, by2)
	}
	told := systemMail(t, s, workerToken, "team/worker")
	if len(told) != 2 || told[0].Subject != told[1].Subject || told[0].TaskID != byAdmin.ID || told[0].Priority != "urgent" ||
		strings.ReplaceAll(told[0].Body, byAdmin.ID+" (Bash) was denied by dashboard", "X") != strings.ReplaceAll(told[1].Body, byBoss.ID+" (Bash) was denied by sandcorp/boss", "X") {
		t.Fatalf("requester mail: %+v", told)
	}

	// Answered once: both paths now get the same 409.
	if w := adminWrite(s, "POST", path, `{"decision":"allow"}`); w.Code != 409 {
		t.Fatalf("second admin answer: %d", w.Code)
	}
	answerAs(t, s, bossToken, "sandcorp/boss", byAdmin.ID, "allow", 409)
	for _, bad := range []string{`{"decision":"maybe"}`, `not json`} {
		if w := adminWrite(s, "POST", path, bad); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if w := adminWrite(s, "POST", "/api/admin/permissions/perm_nope/answer", `{"decision":"allow"}`); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
}

func TestAdminPermissionListAndDetail(t *testing.T) {
	s, clock := permissionServer(t)
	s.admin.ProxyToken = "proxy-secret"
	s.asksCfg.PermissionTTL = time.Minute
	registerWorker(t, s, "/w", "a")
	expired := askAs(t, s, workerToken, "team/worker-1", 201)
	clock.add(2 * time.Minute)
	answered := askAs(t, s, workerToken, "team/worker-1", 201)
	answerAs(t, s, bossToken, "sandcorp/boss", answered.ID, "allow", 200)
	clock.add(time.Second)
	pending := askAs(t, s, workerToken, "team/worker-1", 201)

	if got := adminPermissionList(t, s, "?status=pending"); len(got) != 1 || got[0].ID != pending.ID || !got[0].CanAnswer {
		t.Fatalf("pending: %+v", got)
	}
	got := adminPermissionList(t, s, "?status=closed")
	if len(got) != 2 || got[0].ID != answered.ID || got[1].ID != expired.ID || got[0].CanAnswer || got[1].Status != "expired" {
		t.Fatalf("closed: %+v", got)
	}
	if got[0].AnsweredBy != "sandcorp/boss" || got[0].AnsweredAt == nil || got[0].Note != "ok by me" || got[0].Decision != "allow" {
		t.Fatalf("history row: %+v", got[0])
	}
	if all := adminPermissionList(t, s, "?limit=2"); len(all) != 2 || all[0].ID != pending.ID {
		t.Fatalf("limit: %+v", all)
	}
	for _, q := range []string{"?status=nope", "?limit=0"} {
		if w := adminRequest(s, "GET", "/api/admin/permissions"+q, "", nil); w.Code != 400 {
			t.Fatalf("%s: %d", q, w.Code)
		}
	}

	w := adminRequest(s, "GET", "/api/admin/permissions/"+pending.ID, "", nil)
	if w.Code != 200 {
		t.Fatalf("detail: %d %s", w.Code, w.Body)
	}
	for _, want := range []string{`"requester":"team/worker"`, `"requester_base":"team/worker"`, `"tool":"Bash"`, `"summary":"rm -rf build/"`, `"parent":"a"`, `"parent_base":"a"`, `"declared_parent":"a"`, `"created_at"`, `"expires_at"`, `"status":"pending"`, `"can_answer":true`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("detail missing %s: %s", want, w.Body)
		}
	}
	if w := adminRequest(s, "GET", "/api/admin/permissions/perm_nope", "", nil); w.Code != 404 {
		t.Fatalf("unknown detail: %d", w.Code)
	}
	if w := adminRequestWith(s, "GET", "/api/admin/permissions", "", nil); w.Code != 401 {
		t.Fatalf("list without admin auth: %d", w.Code)
	}
	// The expired one cannot be answered from the dashboard either.
	if w := adminWrite(s, "POST", "/api/admin/permissions/"+expired.ID+"/answer", `{"decision":"allow"}`); w.Code != 410 {
		t.Fatalf("expired answer: %d", w.Code)
	}
}
