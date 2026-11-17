package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func adminQuestionList(t *testing.T, s *Server, query string) []questionView {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/questions"+query, "", nil)
	var out questionList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("GET questions%s=%d %s", query, w.Code, w.Body)
	}
	return out.Questions
}

func adminAnswerBody(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"answers": sampleAnswers()})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdminQuestionAnswer(t *testing.T) {
	s := questionServer(t)
	s.adminProxyToken = "proxy-secret"
	v := askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	path := "/api/admin/questions/" + v.ID + "/answer"

	// Without the dashboard guard header the write is refused.
	if w := adminRequest(s, "POST", path, adminAnswerBody(t), map[string]string{"Content-Type": "application/json"}); w.Code != 403 {
		t.Fatalf("unguarded answer=%d", w.Code)
	}
	if w := adminRequest(s, "POST", path, adminAnswerBody(t), map[string]string{"Content-Type": "application/json", "X-Agent-Mail-Dashboard": "1", "Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatalf("cross-site answer=%d", w.Code)
	}
	if w := adminWrite(s, "POST", path, `{"answers":[{"selected":["없음"]},{"selected":["web"]}]}`); w.Code != 400 {
		t.Fatalf("bad label=%d %s", w.Code, w.Body)
	}
	w := adminWrite(s, "POST", path, adminAnswerBody(t))
	if w.Code != 200 {
		t.Fatalf("answer=%d %s", w.Code, w.Body)
	}
	got := decodeQuestion(t, w.Body.Bytes())
	if got.Status != "answered" || got.AnsweredBy != "dashboard" || got.CanAnswer || len(got.Answers) != 2 {
		t.Fatalf("answered view=%+v", got)
	}
	// The asker gets the answer mail.
	m := questionMail(t, s, v.ID)
	if len(m) != 1 || m[0].to != "team/worker-2" || m[0].subject != "[질문 답] "+v.ID+" 배포" || !strings.HasSuffix(m[0].body, "답한 쪽: dashboard") {
		t.Fatalf("answer mail=%+v", m)
	}
	if w := adminWrite(s, "POST", path, adminAnswerBody(t)); w.Code != 409 {
		t.Fatalf("second answer=%d", w.Code)
	}
	if w := adminWrite(s, "POST", "/api/admin/questions/q_none/answer", adminAnswerBody(t)); w.Code != 404 {
		t.Fatalf("unknown answer=%d", w.Code)
	}
	if w := adminRequest(s, "GET", path, "", nil); w.Code != 405 {
		t.Fatalf("GET answer=%d", w.Code)
	}
	if w := adminWrite(s, "POST", "/api/admin/questions/"+v.ID+"/withdraw", "{}"); w.Code != 404 {
		t.Fatalf("withdraw=%d", w.Code)
	}
	// Without the proxy secret nothing is served.
	r := httptest.NewRequest("GET", "/api/admin/questions", nil)
	rw := httptest.NewRecorder()
	s.api(rw, r)
	if rw.Code != 401 {
		t.Fatalf("no proxy secret=%d", rw.Code)
	}
}

func TestAdminQuestionList(t *testing.T) {
	s, clock := permissionServer(t)
	if _, err := s.db.Exec(`UPDATE question_settings SET can_ask=1 WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	s.adminProxyToken = "proxy-secret"
	first := askQuestionAs(t, s, workerToken, "team/worker", 201)
	clock.add(time.Second)
	second := askQuestionAs(t, s, workerToken, "team/worker", 201)
	clock.add(time.Second)
	third := askQuestionAs(t, s, workerToken, "team/worker", 201)
	if w := adminWrite(s, "POST", "/api/admin/questions/"+third.ID+"/answer", adminAnswerBody(t)); w.Code != 200 {
		t.Fatalf("answer=%d", w.Code)
	}
	withdrawAs(t, s, workerToken, "team/worker", second.ID, 200)

	// No status: pending first, then the rest newest first.
	all := adminQuestionList(t, s, "")
	if len(all) != 3 || all[0].ID != first.ID || all[1].ID != third.ID || all[2].ID != second.ID {
		t.Fatalf("order=%v", ids(all))
	}
	if !all[0].CanAnswer || all[1].CanAnswer || all[2].CanAnswer || all[1].AnsweredBy != "dashboard" {
		t.Fatalf("can_answer/answered_by=%+v", all)
	}
	if got := adminQuestionList(t, s, "?status=withdrawn"); len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("withdrawn=%v", ids(got))
	}
	if got := adminQuestionList(t, s, "?limit=1"); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("limit=%v", ids(got))
	}
	for _, q := range []string{"?status=open", "?limit=0"} {
		if w := adminRequest(s, "GET", "/api/admin/questions"+q, "", nil); w.Code != 400 {
			t.Fatalf("GET %s=%d", q, w.Code)
		}
	}
}

func ids(vs []questionView) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.ID
	}
	return out
}

func TestDashboardQuestionsAndSettings(t *testing.T) {
	w := httptest.NewRecorder()
	dashboard(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`id="questions"`, `id="reloadQuestions"`, `id="settingsTable"`, `id="settingsForm"`, `id="setTarget"`, `id="setCanAsk"`, `id="setDiscord"`, `id="setWatchers"`,
		"tabLoaders.questions=", "tabLoaders.settings=", "setTabCount('questions'", "'questions/'+encodeURIComponent(", "/answer'",
		"'X-Switchboard-Dashboard':'1'", "'Content-Type':'application/json'", "이미 답했거나 거둔 질문입니다", "기타", "'radio'", "'checkbox'", "'pre'",
		"question-settings/'", "'DELETE'", "'PUT'",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "준비 중 · 역할별") || strings.Contains(body, "#42 fills") {
		t.Fatal("dashboard still has the #42 placeholders")
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML"} {
		if strings.Contains(body, banned) {
			t.Fatalf("dashboard uses %s", banned)
		}
	}
}
