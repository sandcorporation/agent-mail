package server

import (
	"github.com/sandcorporation/agent-mail/internal/asks"
	"github.com/sandcorporation/agent-mail/internal/dashboard"
	"github.com/sandcorporation/agent-mail/internal/wire"

	"bufio"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const teamOtherToken = "secret-team-other"

// decisionServer is questionServer with its clock and a second base under
// team/ (team/other).
func decisionServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := permissionServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := s.db.Exec(`UPDATE question_settings SET can_ask=1 WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/other',?,'now')`, teamOtherToken); err != nil {
		t.Fatal(err)
	}
	return s, clock
}

// decide asks one question with header as role and has the boss answer it
// with label; it returns the 201 body of the ask.
func decide(t *testing.T, s *Server, token, role, header, label string) asks.AskedQuestion {
	t.Helper()
	a := askOne(t, s, token, role, header)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", a.ID, []wire.QuestionAnswer{{Selected: []string{label}}}, 200)
	return a
}

func askOne(t *testing.T, s *Server, token, role, header string) asks.AskedQuestion {
	t.Helper()
	qs := []wire.QuestionItem{{Header: header, Question: header + " 할까요?", Options: []wire.QuestionOption{{Label: "예", Description: "y"}, {Label: "아니오", Description: "n"}}}}
	w := callAs(s, "POST", "/api/questions", token, role, map[string]any{"questions": qs})
	if w.Code != 201 {
		t.Fatalf("ask as %s: %d %s", role, w.Code, w.Body)
	}
	var a asks.AskedQuestion
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return a
}

func adminDecisions(t *testing.T, s *Server, query string) []wire.Decision {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/decisions"+query, "", nil)
	var out wire.DecisionList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("GET decisions%s=%d %s", query, w.Code, w.Body)
	}
	return out.Decisions
}

func decisionIDs(ds []wire.Decision) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

// seedDecisions: q1 by a on 11-17 (web, api / 천천히), q2 by team/worker-2 on
// 11-19 (예, worker), q3 by team/other on 11-19 (아니오, web), plus a pending
// and a withdrawn question that are not decisions.
func seedDecisions(t *testing.T, s *Server, clock *fakeClock) (q1, q2, q3 wire.QuestionView) {
	t.Helper()
	q1 = askQuestionAs(t, s, "secret-a", "a", 201)
	clock.add(time.Hour)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", q1.ID, sampleAnswers(), 200)
	clock.add(48 * time.Hour)
	q2 = askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", q2.ID, []wire.QuestionAnswer{{Selected: []string{"예"}}, {Selected: []string{"worker"}}}, 200)
	clock.add(time.Minute)
	q3 = askQuestionAs(t, s, teamOtherToken, "team/other", 201)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", q3.ID, []wire.QuestionAnswer{{Selected: []string{"아니오"}}, {Selected: []string{"web"}}}, 200)
	askQuestionAs(t, s, "secret-b", "b", 201)
	w := askQuestionAs(t, s, "secret-a", "a", 201)
	withdrawAs(t, s, "secret-a", "a", w.ID, 200)
	return q1, q2, q3
}

func TestDecisionFilters(t *testing.T) {
	s, clock := decisionServer(t)
	q1, q2, q3 := seedDecisions(t, s, clock)
	all := adminDecisions(t, s, "")
	if got := decisionIDs(all); !slices.Equal(got, []string{q3.ID, q2.ID, q1.ID}) {
		t.Fatalf("all: %v", got)
	}
	d := all[2]
	if d.Asker != "a" || d.AnsweredBy != "sandcorp/boss" || !d.AskedAt.Equal(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)) ||
		!d.AnsweredAt.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) || len(d.Items) != 2 ||
		d.Items[1].Header != "대상" || d.Items[1].Question != "어디에?" || !slices.Equal(d.Items[1].Chosen, []string{"web", "api"}) || d.Items[1].Other != "천천히" {
		t.Fatalf("decision shape: %+v", d)
	}
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"?from=2026-11-18", []string{q3.ID, q2.ID}},
		{"?to=2026-11-17", []string{q1.ID}},
		{"?from=2026-11-17T10:00:00Z&to=2026-11-17T10:00:00Z", []string{q1.ID}},
		{"?to=2026-11-17T09:59:59Z", []string{}},
		{"?role=team/*", []string{q3.ID, q2.ID}},
		{"?role=team/worker", []string{q2.ID}},
		{"?role=team/worker-2", []string{q2.ID}},
		{"?role=a", []string{q1.ID}},
		{"?role=tea/*", []string{}},
		{"?q=API", []string{q1.ID}},
		{"?q=worker", []string{q2.ID}},
		{"?q=아니오", []string{q3.ID}},
		{"?q=어디에", []string{q3.ID, q2.ID, q1.ID}},
		{"?q=web&role=team/*&from=2026-11-19", []string{q3.ID}},
	} {
		if got := decisionIDs(adminDecisions(t, s, c.query)); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v want %v", c.query, got, c.want)
		}
	}
	for _, bad := range []string{"?from=yesterday", "?to=2026-13-01", "?role=bad%20role", "?role=*", "?format=csv"} {
		if w := adminRequest(s, "GET", "/api/admin/decisions"+bad, "", nil); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := adminRequest(s, "GET", "/api/admin/decisions", "", map[string]string{"X-Agent-Mail-Admin": "wrong"}); w.Code != 401 {
		t.Fatalf("wrong admin secret: %d", w.Code)
	}
}

func TestDecisionExportNDJSON(t *testing.T) {
	s, clock := decisionServer(t)
	_, q2, q3 := seedDecisions(t, s, clock)
	w := adminRequest(s, "GET", "/api/admin/decisions?from=2026-11-18&to=2026-11-19&format=ndjson", "", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="decisions-2026-11-18-2026-11-19.ndjson"` {
		t.Fatalf("disposition %q", cd)
	}
	var got []string
	sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for sc.Scan() {
		var d wire.Decision
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		got = append(got, d.ID)
	}
	if !slices.Equal(got, []string{q3.ID, q2.ID}) || !strings.HasSuffix(w.Body.String(), "\n") {
		t.Fatalf("lines %v in %q", got, w.Body)
	}
	// Without from/to the name says start and today.
	w = adminRequest(s, "GET", "/api/admin/decisions?format=ndjson", "", nil)
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="decisions-start-2026-11-19.ndjson"` || strings.Count(w.Body.String(), "\n") != 3 {
		t.Fatalf("unbounded export: %q %q", cd, w.Body)
	}
}

func TestDecisionRoleEndpointVisibility(t *testing.T) {
	s, clock := decisionServer(t)
	q1, q2, q3 := seedDecisions(t, s, clock)
	get := func(token, role, query string) []string {
		t.Helper()
		w := callAs(s, "GET", "/api/decisions"+query, token, role, nil)
		var out wire.DecisionList
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("GET /api/decisions%s as %s: %d %s", query, role, w.Code, w.Body)
		}
		return decisionIDs(out.Decisions)
	}
	if got := get(bossToken, "sandcorp/boss", ""); !slices.Equal(got, []string{q3.ID, q2.ID, q1.ID}) {
		t.Fatalf("boss: %v", got)
	}
	if got := get("secret-a", "a", ""); !slices.Equal(got, []string{q1.ID}) {
		t.Fatalf("a: %v", got)
	}
	if got := get(workerToken, "team/worker-3", "?role=team/*"); !slices.Equal(got, []string{q2.ID}) {
		t.Fatalf("team/worker-3: %v", got)
	}
	if got := get("secret-b", "b", ""); len(got) != 0 {
		t.Fatalf("b: %v", got)
	}
	if w := callAs(s, "POST", "/api/decisions", "secret-a", "a", nil); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	if w := callAs(s, "GET", "/api/decisions", "nope", "a", nil); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
}

func TestRelatedDecisions(t *testing.T) {
	s, clock := decisionServer(t)
	// team/* has its own settings row, so team bases fill from each other;
	// a watches team questions.
	if _, err := s.db.Exec(`INSERT INTO question_settings(target,can_ask,watchers,discord,updated_at) VALUES('team/*',1,'["a"]',0,'now')`); err != nil {
		t.Fatal(err)
	}
	first := askOne(t, s, workerToken, "team/worker-1", "배포")
	if first.Related == nil || len(first.Related) != 0 {
		t.Fatalf("no decisions yet: %+v", first.Related)
	}
	withdrawAs(t, s, workerToken, "team/worker-1", first.ID, 200)
	w1 := decide(t, s, workerToken, "team/worker-1", "X", "예")
	clock.add(time.Minute)
	b1 := decide(t, s, "secret-b", "b", "Deploy", "아니오")
	clock.add(time.Minute)
	decide(t, s, "secret-a", "a", "Z", "예") // matches nothing
	clock.add(time.Minute)

	// Own base, then (no other team base yet) the first header, case-insensitive.
	got := askOne(t, s, workerToken, "team/worker-2", "DEPLOY")
	if ids := decisionIDs(got.Related); !slices.Equal(ids, []string{w1.ID, b1.ID}) {
		t.Fatalf("related: %v", ids)
	}
	withdrawAs(t, s, workerToken, "team/worker-2", got.ID, 200)

	o1 := decide(t, s, teamOtherToken, "team/other", "Y", "예")
	clock.add(time.Minute)
	o2 := decide(t, s, teamOtherToken, "team/other", "Y", "아니오")
	clock.add(time.Minute)
	// Own base first, then the team/* prefix newest first; capped at 3, so
	// the header match (b1) is left out.
	got = askOne(t, s, workerToken, "team/worker-2", "deploy")
	if ids := decisionIDs(got.Related); !slices.Equal(ids, []string{w1.ID, o2.ID, o1.ID}) {
		t.Fatalf("related: %v", ids)
	}
	if r := got.Related[1]; r.Asker != "team/other" || len(r.Items) != 1 || r.Items[0].Header != "Y" || !slices.Equal(r.Items[0].Chosen, []string{"아니오"}) {
		t.Fatalf("related shape: %+v", r)
	}

	// The watcher's create mail carries the same list.
	m := questionMail(t, s, got.ID)
	day := clock.now().Format("2006-01-02")
	want := "\n\n관련 지난 결정:\n- " + w1.ID + " " + day + ": X → 예\n- " + o2.ID + " " + day + ": Y → 아니오\n- " + o1.ID + " " + day + ": Y → 예"
	if len(m) != 1 || m[0].to != "a" || !strings.HasSuffix(m[0].body, want) {
		t.Fatalf("watcher mail: %+v\nwant suffix %q", m, want)
	}

	// The team/* prefix only applies when it is the resolved target: b
	// resolves to default, so it gets its own decision and the header match.
	got = askOne(t, s, "secret-b", "b", "x")
	if ids := decisionIDs(got.Related); !slices.Equal(ids, []string{b1.ID, w1.ID}) {
		t.Fatalf("b related: %v", ids)
	}
	if m := questionMail(t, s, got.ID); len(m) != 0 {
		t.Fatalf("b has no watchers: %+v", m)
	}
}

func TestDashboardDecisionsView(t *testing.T) {
	w := httptest.NewRecorder()
	dashboard.Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`id="viewQuestions"`, `id="viewDecisions"`, `aria-pressed`, `id="decisionsView"`, `id="decPeriod"`, `value="7"`, `value="30"`, `value="90"`, `value="custom"`,
		`id="decFrom" type="date"`, `id="decTo" type="date"`, `id="decRole"`, `id="decSearch"`, `id="decExport"`, "download", "JSON 내보내기",
		"'/api/admin/decisions?'", "p.set('format','ndjson')", "api('decisions?'", "h==='decisions'", "selectTab('decisions')",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML"} {
		if strings.Contains(body, banned) {
			t.Fatalf("dashboard uses %s", banned)
		}
	}
}
