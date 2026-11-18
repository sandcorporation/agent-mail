package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/asks"
	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// questionServer: permissionServer (roles a, b, team/worker, boss
// sandcorp/boss) with can_ask on for every role.
func questionServer(t *testing.T) *Server {
	t.Helper()
	s, _ := permissionServer(t)
	if _, err := s.db.Exec(`UPDATE question_settings SET can_ask=1 WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleQuestions() []wire.QuestionItem {
	return []wire.QuestionItem{
		{Header: "배포", Question: "지금 배포할까요?", Options: []wire.QuestionOption{{Label: "예", Description: "바로"}, {Label: "아니오", Description: "내일"}}},
		{Header: "대상", Question: "어디에?", MultiSelect: true, Options: []wire.QuestionOption{{Label: "web", Description: "w"}, {Label: "api", Description: "a"}, {Label: "worker", Description: "k", Preview: "p"}}},
	}
}

func sampleAnswers() []wire.QuestionAnswer {
	return []wire.QuestionAnswer{{Selected: []string{"예"}}, {Selected: []string{"web", "api"}, Other: "천천히"}}
}

func decodeQuestion(t *testing.T, body []byte) wire.QuestionView {
	t.Helper()
	var v wire.QuestionView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func askQuestionAs(t *testing.T, s *Server, token, role string, want int) wire.QuestionView {
	t.Helper()
	w := callAs(s, "POST", "/api/questions", token, role, map[string]any{"questions": sampleQuestions()})
	if w.Code != want {
		t.Fatalf("ask as %s: %d %s", role, w.Code, w.Body)
	}
	if want != 201 {
		return wire.QuestionView{}
	}
	return decodeQuestion(t, w.Body.Bytes())
}

func answerQuestionAs(t *testing.T, s *Server, token, role, id string, answers any, want int) wire.QuestionView {
	t.Helper()
	w := callAs(s, "POST", "/api/questions/"+id+"/answer", token, role, map[string]any{"answers": answers})
	if w.Code != want {
		t.Fatalf("answer %s as %s: %d %s", id, role, w.Code, w.Body)
	}
	if want != 200 {
		return wire.QuestionView{}
	}
	return decodeQuestion(t, w.Body.Bytes())
}

func withdrawAs(t *testing.T, s *Server, token, role, id string, want int) {
	t.Helper()
	if w := callAs(s, "POST", "/api/questions/"+id+"/withdraw", token, role, nil); w.Code != want {
		t.Fatalf("withdraw %s as %s: %d %s", id, role, w.Code, w.Body)
	}
}

type mailRow struct{ to, subject, body, task, priority string }

func questionMail(t *testing.T, s *Server, id string) []mailRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT recipient,subject,body,COALESCE(task_id,''),priority FROM messages WHERE sender=? AND task_id=? ORDER BY id`, identity.SystemSender, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []mailRow
	for rows.Next() {
		var m mailRow
		if err := rows.Scan(&m.to, &m.subject, &m.body, &m.task, &m.priority); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestQuestionAskRequiresCanAsk(t *testing.T) {
	s := questionServer(t)
	if _, err := s.db.Exec(`UPDATE question_settings SET can_ask=0`); err != nil {
		t.Fatal(err)
	}
	askQuestionAs(t, s, workerToken, "team/worker", 403)
	if _, err := s.db.Exec(`INSERT INTO question_settings(target,can_ask,watchers,discord,updated_at) VALUES('team/*',1,'[]',0,'now')`); err != nil {
		t.Fatal(err)
	}
	v := askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	if !strings.HasPrefix(v.ID, "q_") || v.Asker != "team/worker-2" || v.Status != "pending" || v.CanAnswer || v.Answers != nil || v.AnsweredAt != nil || v.Seq != 1 || len(v.Questions) != 2 || v.Answerer != "" {
		t.Fatalf("created: %+v", v)
	}
	// With no boss of another base (the asker's own base being a boss does
	// not count) the question is stored for the dashboard.
	s.bossRoles = map[string]bool{"team/worker": true}
	if v = askQuestionAs(t, s, workerToken, "team/worker", 201); v.Answerer != wire.AnswererOwner {
		t.Fatalf("own-base boss: %+v", v)
	}
	s.bossRoles = nil
	if v = askQuestionAs(t, s, workerToken, "team/worker", 201); v.Answerer != wire.AnswererOwner {
		t.Fatalf("no boss: %+v", v)
	}
}

// A boss role's question has nobody to answer it but a person on the
// dashboard: it is stored (201, answerer "owner") and answered there.
func TestQuestionByBossGoesToOwner(t *testing.T) {
	s := questionServer(t)
	s.admin.ProxyToken = "proxy-secret"

	// An ordinary question carries no answerer field at all.
	w := callAs(s, "POST", "/api/questions", workerToken, "team/worker", map[string]any{"questions": sampleQuestions()})
	if w.Code != 201 || strings.Contains(w.Body.String(), `"answerer"`) {
		t.Fatalf("ordinary ask: %d %s", w.Code, w.Body)
	}

	w = callAs(s, "POST", "/api/questions", bossToken, "sandcorp/boss", map[string]any{"questions": sampleQuestions()})
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"answerer":"owner"`) {
		t.Fatalf("boss ask: %d %s", w.Code, w.Body)
	}
	v := decodeQuestion(t, w.Body.Bytes())
	if v.Status != "pending" || !v.CanAnswer || v.Asker != "sandcorp/boss" {
		t.Fatalf("boss question: %+v", v)
	}
	// Only the asker's exact role answers from its panel; a numbered slot of
	// the same base and an ordinary role do not.
	answerQuestionAs(t, s, bossToken, "sandcorp/boss-2", v.ID, sampleAnswers(), 403)
	answerQuestionAs(t, s, workerToken, "team/worker", v.ID, sampleAnswers(), 403)

	var listed bool
	for _, q := range adminQuestionList(t, s, "") {
		listed = listed || (q.ID == v.ID && q.Answerer == wire.AnswererOwner)
	}
	if !listed {
		t.Fatal("owner question missing from the admin list")
	}

	// The asker's long poll wakes on the dashboard answer.
	cursor := pollAs(t, s, bossToken, "sandcorp/boss", "?after=0").Cursor
	done := make(chan asks.QuestionPoll, 1)
	go func() { done <- pollAs(t, s, bossToken, "sandcorp/boss", fmt.Sprintf("?after=%d&timeout=20", cursor)) }()

	if w := adminWrite(s, "POST", "/api/admin/questions/"+v.ID+"/answer", adminAnswerBody(t)); w.Code != 200 {
		t.Fatalf("dashboard answer: %d %s", w.Code, w.Body)
	}
	select {
	case got := <-done:
		if len(got.Questions) != 1 || got.Questions[0].ID != v.ID || got.Questions[0].Status != "answered" || got.Questions[0].Answerer != wire.AnswererOwner {
			t.Fatalf("woken poll: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("poll was not woken")
	}
	r := callAs(s, "GET", "/api/questions/"+v.ID, bossToken, "sandcorp/boss", nil)
	if got := decodeQuestion(t, r.Body.Bytes()); r.Code != 200 || got.Status != "answered" || got.AnsweredBy != "dashboard" || len(got.Answers) != 2 {
		t.Fatalf("asker GET: %d %+v", r.Code, got)
	}
	var mailed bool
	for _, m := range systemMail(t, s, bossToken, "sandcorp/boss") {
		mailed = mailed || (m.TaskID == v.ID && strings.HasPrefix(m.Subject, "[질문 답] "+v.ID))
	}
	if !mailed {
		t.Fatal("answer mail did not reach the asker's inbox")
	}
}

// The asker of an owner question answers it itself from its session panel:
// the answer is stored as "panel", mailed to the asker and kept as a decision.
func TestOwnerQuestionAnsweredFromPanel(t *testing.T) {
	s := questionServer(t)
	s.admin.ProxyToken = "proxy-secret"
	v := askQuestionAs(t, s, bossToken, "sandcorp/boss", 201)
	if v.Answerer != wire.AnswererOwner {
		t.Fatalf("not an owner question: %+v", v)
	}
	r := callAs(s, "GET", "/api/questions/"+v.ID, bossToken, "sandcorp/boss", nil)
	if got := decodeQuestion(t, r.Body.Bytes()); !got.CanAnswer {
		t.Fatalf("asker cannot answer: %+v", got)
	}
	r = callAs(s, "GET", "/api/questions/"+v.ID, bossToken, "sandcorp/boss-2", nil)
	if got := decodeQuestion(t, r.Body.Bytes()); got.CanAnswer {
		t.Fatalf("sibling slot can answer: %+v", got)
	}
	answerQuestionAs(t, s, workerToken, "team/worker", v.ID, sampleAnswers(), 403)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss-2", v.ID, sampleAnswers(), 403)

	got := answerQuestionAs(t, s, bossToken, "sandcorp/boss", v.ID, sampleAnswers(), 200)
	if got.Status != "answered" || got.AnsweredBy != "panel" || len(got.Answers) != 2 {
		t.Fatalf("panel answer: %+v", got)
	}
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", v.ID, sampleAnswers(), 409)

	var decided bool
	for _, d := range adminDecisions(t, s, "") {
		decided = decided || (d.ID == v.ID && d.AnsweredBy == "panel")
	}
	if !decided {
		t.Fatal("panel answer missing from decisions")
	}
	var mailed bool
	for _, m := range systemMail(t, s, bossToken, "sandcorp/boss") {
		mailed = mailed || (m.TaskID == v.ID && strings.HasPrefix(m.Subject, "[질문 답] "+v.ID))
	}
	if !mailed {
		t.Fatal("answer mail did not reach the asker's inbox")
	}
}

// An ordinary question is never answered by its asker.
func TestOrdinaryQuestionAskerCannotAnswer(t *testing.T) {
	s := questionServer(t)
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	if v.Answerer != "" {
		t.Fatalf("not ordinary: %+v", v)
	}
	answerQuestionAs(t, s, workerToken, "team/worker", v.ID, sampleAnswers(), 403)
}

func TestQuestionAskValidation(t *testing.T) {
	s := questionServer(t)
	long := strings.Repeat("x", 1001)
	bad := map[string]func([]wire.QuestionItem) []wire.QuestionItem{
		"none": func([]wire.QuestionItem) []wire.QuestionItem { return nil },
		"five": func(q []wire.QuestionItem) []wire.QuestionItem { return append(q, q[0], q[0], q[0]) },
		"header": func(q []wire.QuestionItem) []wire.QuestionItem {
			q[0].Header = "열세글자가넘는헤더입니다아"
			return q
		},
		"empty header": func(q []wire.QuestionItem) []wire.QuestionItem { q[0].Header = ""; return q },
		"question":     func(q []wire.QuestionItem) []wire.QuestionItem { q[0].Question = long; return q },
		"one option":   func(q []wire.QuestionItem) []wire.QuestionItem { q[0].Options = q[0].Options[:1]; return q },
		"dup label": func(q []wire.QuestionItem) []wire.QuestionItem {
			q[0].Options[1].Label = q[0].Options[0].Label
			return q
		},
		"label":       func(q []wire.QuestionItem) []wire.QuestionItem { q[0].Options[0].Label = long[:101]; return q },
		"description": func(q []wire.QuestionItem) []wire.QuestionItem { q[0].Options[0].Description = long[:501]; return q },
		"preview": func(q []wire.QuestionItem) []wire.QuestionItem {
			q[0].Options[0].Preview = strings.Repeat("x", 4001)
			return q
		},
	}
	for name, f := range bad {
		if w := callAs(s, "POST", "/api/questions", workerToken, "team/worker", map[string]any{"questions": f(sampleQuestions())}); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	big := sampleQuestions()
	big[0].Options[0].Description = strings.Repeat("x", 33<<10)
	if w := callAs(s, "POST", "/api/questions", workerToken, "team/worker", map[string]any{"questions": big}); w.Code != 400 {
		t.Errorf("over 32 KiB: %d", w.Code)
	}
	ok := sampleQuestions()
	ok[0].Header = "열두글자까지의헤더입니다"
	if w := callAs(s, "POST", "/api/questions", workerToken, "team/worker", map[string]any{"questions": ok}); w.Code != 201 {
		t.Errorf("12-rune header: %d %s", w.Code, w.Body)
	}
}

func TestQuestionAnswerRules(t *testing.T) {
	s := questionServer(t)
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	answerQuestionAs(t, s, "secret-a", "a", v.ID, sampleAnswers(), 403) // not a boss
	answerQuestionAs(t, s, "secret-a", "a", "q_unknown", sampleAnswers(), 404)
	got := answerQuestionAs(t, s, bossToken, "sandcorp/boss-2", v.ID, sampleAnswers(), 200)
	if got.Status != "answered" || got.AnsweredBy != "sandcorp/boss-2" || got.AnsweredAt == nil || got.CanAnswer || got.Seq != 2 || len(got.Answers) != 2 || got.Answers[1].Other != "천천히" {
		t.Fatalf("answered: %+v", got)
	}
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", v.ID, sampleAnswers(), 409) // second answer

	// The asker's base never answers, even when it is a boss.
	s.bossRoles["team/worker"] = true
	v2 := askQuestionAs(t, s, workerToken, "team/worker", 201)
	answerQuestionAs(t, s, workerToken, "team/worker-3", v2.ID, sampleAnswers(), 403)

	// Withdrawn questions take no answer.
	withdrawAs(t, s, workerToken, "team/worker", v2.ID, 200)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", v2.ID, sampleAnswers(), 409)
	withdrawAs(t, s, workerToken, "team/worker", v2.ID, 409)
}

func TestQuestionWithdrawOnlyByAskerBase(t *testing.T) {
	s := questionServer(t)
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	withdrawAs(t, s, "secret-a", "a", v.ID, 403)
	withdrawAs(t, s, bossToken, "sandcorp/boss", v.ID, 403)
	withdrawAs(t, s, workerToken, "team/worker", "q_nope", 404)
	w := callAs(s, "POST", "/api/questions/"+v.ID+"/withdraw", workerToken, "team/worker-2", nil)
	got := decodeQuestion(t, w.Body.Bytes())
	if w.Code != 200 || got.Status != "withdrawn" || got.WithdrawnAt == nil || got.Seq != 2 {
		t.Fatalf("withdraw: %d %+v", w.Code, got)
	}
	if m := questionMail(t, s, v.ID); len(m) != 0 {
		t.Fatalf("withdraw mailed: %+v", m)
	}
}

func TestQuestionAnswerValidation(t *testing.T) {
	s := questionServer(t)
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	cases := map[string][]wire.QuestionAnswer{
		"unknown label":    {{Selected: []string{"아마도"}}, {Selected: []string{"web"}}},
		"two for single":   {{Selected: []string{"예", "아니오"}}, {Selected: []string{"web"}}},
		"empty entry":      {{Selected: []string{"예"}}, {}},
		"missing entry":    {{Selected: []string{"예"}}},
		"too many entries": {{Selected: []string{"예"}}, {Selected: []string{"web"}}, {Other: "x"}},
		"long other":       {{Other: strings.Repeat("x", 1001)}, {Selected: []string{"web"}}},
		"dup label":        {{Selected: []string{"예"}}, {Selected: []string{"web", "web"}}},
	}
	for name, a := range cases {
		if w := callAs(s, "POST", "/api/questions/"+v.ID+"/answer", bossToken, "sandcorp/boss", map[string]any{"answers": a}); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	got := answerQuestionAs(t, s, bossToken, "sandcorp/boss", v.ID, []wire.QuestionAnswer{{Other: "모르겠다"}, {Selected: []string{"worker"}}}, 200)
	if got.Answers[0].Selected == nil || len(got.Answers[0].Selected) != 0 {
		t.Fatalf("selected should be an empty list: %+v", got.Answers)
	}
}

func TestQuestionMail(t *testing.T) {
	s := questionServer(t)
	// a watches; team/worker (the asker's base) and an unknown role are skipped.
	if _, err := s.db.Exec(`UPDATE question_settings SET watchers='["a","team/worker-2","ghost","a"]'`); err != nil {
		t.Fatal(err)
	}
	v := askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	m := questionMail(t, s, v.ID)
	if len(m) != 1 || m[0].to != "a" || m[0].subject != "[질문] "+v.ID+" team/worker-2 배포" || m[0].priority != "normal" ||
		!strings.Contains(m[0].body, "지금 배포할까요?") || !strings.Contains(m[0].body, "- 아니오: 내일") || !strings.Contains(m[0].body, "- worker: k") {
		t.Fatalf("create mail: %+v", m)
	}
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", v.ID, sampleAnswers(), 200)
	m = questionMail(t, s, v.ID)[1:]
	want := "배포: 지금 배포할까요? = 예\n대상: 어디에? = web, api / 기타: 천천히\n답한 쪽: sandcorp/boss"
	if len(m) != 2 {
		t.Fatalf("answer mail: %+v", m)
	}
	if m[0].to != "team/worker-2" || m[0].subject != "[질문 답] "+v.ID+" 배포" || m[0].body != want || m[0].priority != "normal" {
		t.Fatalf("asker mail: %+v", m[0])
	}
	if m[1].to != "a" || m[1].subject != "[질문 답] "+v.ID+" team/worker-2 배포" || m[1].body != want {
		t.Fatalf("watcher mail: %+v", m[1])
	}
	var bossMail int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient='sandcorp/boss'`).Scan(&bossMail); err != nil || bossMail != 0 {
		t.Fatalf("boss mailed %d %v", bossMail, err)
	}
}

func TestQuestionReadAndList(t *testing.T) {
	s := questionServer(t)
	v1 := askQuestionAs(t, s, workerToken, "team/worker", 201)
	v2 := askQuestionAs(t, s, "secret-b", "b", 201)
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", v2.ID, sampleAnswers(), 200)

	for _, c := range []struct {
		token, role string
		code        int
	}{{workerToken, "team/worker-4", 200}, {bossToken, "sandcorp/boss", 200}, {"secret-a", "a", 403}} {
		if w := callAs(s, "GET", "/api/questions/"+v1.ID, c.token, c.role, nil); w.Code != c.code {
			t.Errorf("show as %s: %d", c.role, w.Code)
		}
	}
	if w := callAs(s, "GET", "/api/questions/q_missing", bossToken, "sandcorp/boss", nil); w.Code != 404 {
		t.Errorf("unknown: %d", w.Code)
	}
	list := func(token, role, query string, want int) []wire.QuestionView {
		t.Helper()
		w := callAs(s, "GET", "/api/questions"+query, token, role, nil)
		if w.Code != want {
			t.Fatalf("list %s %s: %d %s", role, query, w.Code, w.Body)
		}
		var out wire.QuestionList
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.Questions
	}
	all := list(bossToken, "sandcorp/boss", "", 200)
	if len(all) != 2 || all[0].ID != v2.ID || all[1].ID != v1.ID || !all[1].CanAnswer || all[0].CanAnswer {
		t.Fatalf("boss list: %+v", all)
	}
	if got := list(bossToken, "sandcorp/boss", "?status=pending", 200); len(got) != 1 || got[0].ID != v1.ID {
		t.Fatalf("pending: %+v", got)
	}
	if got := list(bossToken, "sandcorp/boss", "?limit=1", 200); len(got) != 1 || got[0].ID != v2.ID {
		t.Fatalf("limit: %+v", got)
	}
	if got := list(workerToken, "team/worker", "", 200); len(got) != 1 || got[0].ID != v1.ID || got[0].CanAnswer {
		t.Fatalf("worker list: %+v", got)
	}
	if got := list("secret-a", "a", "", 200); len(got) != 0 {
		t.Fatalf("bystander list: %+v", got)
	}
	list(bossToken, "sandcorp/boss", "?status=open", 400)
	list(bossToken, "sandcorp/boss", "?limit=0", 400)
}

func pollAs(t *testing.T, s *Server, token, role, query string) asks.QuestionPoll {
	t.Helper()
	w := callAs(s, "GET", "/api/questions/poll"+query, token, role, nil)
	if w.Code != 200 {
		t.Fatalf("poll %s: %d %s", query, w.Code, w.Body)
	}
	var out asks.QuestionPoll
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuestionPoll(t *testing.T) {
	s := questionServer(t)
	v1 := askQuestionAs(t, s, workerToken, "team/worker", 201)
	v2 := askQuestionAs(t, s, "secret-b", "b", 201)
	got := pollAs(t, s, bossToken, "sandcorp/boss", "?after=0")
	if len(got.Questions) != 2 || got.Questions[0].ID != v1.ID || got.Cursor != 2 || !got.Questions[0].CanAnswer {
		t.Fatalf("first poll: %+v", got)
	}
	// Changes move a question past the cursor; it comes back once, as it is now.
	answerQuestionAs(t, s, bossToken, "sandcorp/boss", v1.ID, sampleAnswers(), 200)
	got = pollAs(t, s, bossToken, "sandcorp/boss", "?after=2")
	if len(got.Questions) != 1 || got.Questions[0].ID != v1.ID || got.Questions[0].Status != "answered" || got.Cursor != 3 {
		t.Fatalf("after answer: %+v", got)
	}
	// Non-bosses see only their own base.
	got = pollAs(t, s, "secret-b", "b", "?after=0")
	if len(got.Questions) != 1 || got.Questions[0].ID != v2.ID || got.Cursor != 2 {
		t.Fatalf("b poll: %+v", got)
	}
	// Nothing new: an immediate empty answer with the same cursor...
	if got = pollAs(t, s, bossToken, "sandcorp/boss", "?after=3&timeout=0"); len(got.Questions) != 0 || got.Cursor != 3 {
		t.Fatalf("empty poll: %+v", got)
	}
	// ...or a wait until the timeout.
	start := time.Now()
	if got = pollAs(t, s, bossToken, "sandcorp/boss", "?after=3&timeout=1"); len(got.Questions) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("poll did not wait: %+v after %v", got, time.Since(start))
	}
	// A waiting poll wakes on a change.
	done := make(chan asks.QuestionPoll, 1)
	go func() { done <- pollAs(t, s, bossToken, "sandcorp/boss", "?after=3&timeout=20") }()
	withdrawAs(t, s, "secret-b", "b", v2.ID, 200)
	select {
	case got = <-done:
		if len(got.Questions) != 1 || got.Questions[0].Status != "withdrawn" || got.Cursor != 4 {
			t.Fatalf("woken poll: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("poll was not woken")
	}
	for _, q := range []string{"?after=-1", "?after=x", "?timeout=-1"} {
		if w := callAs(s, "GET", "/api/questions/poll"+q, bossToken, "sandcorp/boss", nil); w.Code != 400 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
}

func TestQuestionConcurrentAnswersOneWins(t *testing.T) {
	s := questionServer(t)
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		role := "sandcorp/boss"
		if i%2 == 1 {
			role = "sandcorp/boss-2"
		}
		go func() {
			codes <- callAs(s, "POST", "/api/questions/"+v.ID+"/answer", bossToken, role, map[string]any{"answers": sampleAnswers()}).Code
		}()
	}
	got := map[int]int{}
	for i := 0; i < 8; i++ {
		got[<-codes]++
	}
	if got[200] != 1 || got[409] != 7 {
		t.Fatalf("answer codes: %v", got)
	}
	if m := questionMail(t, s, v.ID); len(m) != 1 {
		t.Fatalf("asker told %d times", len(m))
	}
}

func TestQuestionDashboardCore(t *testing.T) {
	s := questionServer(t)
	s.bossRoles = map[string]bool{"sandcorp/boss": true}
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	got, err := s.asks().AnswerQuestion(t.Context(), v.ID, asks.QuestionByDashboard, sampleAnswers())
	if err != nil || got.Status != "answered" || got.AnsweredBy != "dashboard" {
		t.Fatalf("dashboard answer: %+v %v", got, err)
	}
	if m := questionMail(t, s, v.ID); len(m) != 1 || !strings.HasSuffix(m[0].body, "답한 쪽: dashboard") {
		t.Fatalf("mail: %+v", m)
	}
	if _, err = s.asks().AnswerQuestion(t.Context(), v.ID, asks.QuestionByDashboard, sampleAnswers()); !errors.Is(err, asks.ErrQuestionNotPending) {
		t.Fatalf("second dashboard answer: %v", err)
	}
	if _, err = s.asks().AnswerQuestion(t.Context(), "q_none", asks.QuestionByDashboard, sampleAnswers()); !errors.Is(err, asks.ErrQuestionNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	v2 := askQuestionAs(t, s, workerToken, "team/worker", 201)
	var in asks.QuestionInputError
	if _, err = s.asks().AnswerQuestion(t.Context(), v2.ID, asks.QuestionByDashboard, nil); !errors.As(err, &in) {
		t.Fatalf("bad answers: %v", err)
	}
	if _, err = s.asks().AnswerQuestion(t.Context(), v2.ID, "a", sampleAnswers()); !errors.Is(err, asks.ErrQuestionForbidden) {
		t.Fatalf("non-boss role through the core: %v", err)
	}
}

func TestQuestionCommand(t *testing.T) {
	h := newCLIHarnessWith(t, func(s *Server) {
		s.bossRoles = map[string]bool{"a": true}
		if _, err := s.db.Exec(`UPDATE question_settings SET can_ask=1`); err != nil {
			t.Fatal(err)
		}
	})
	h.configure([]string{"--profile", "worker"}, "b")
	h.configure([]string{"--profile", "boss"}, "a")
	worker, boss := []string{"AGENT_MAIL_PROFILE=worker"}, []string{"AGENT_MAIL_PROFILE=boss"}
	qf, af := filepath.Join(h.dir, "q.json"), filepath.Join(h.dir, "a.json")
	qb, _ := json.Marshal(map[string]any{"questions": sampleQuestions()})
	ab, _ := json.Marshal(sampleAnswers()) // a bare list works too
	if os.WriteFile(qf, qb, 0600) != nil || os.WriteFile(af, ab, 0600) != nil {
		t.Fatal("write files")
	}
	out := h.mustRun(worker, "question", "ask", "--file", qf)
	id := strings.Fields(out)[0]
	if !strings.HasPrefix(id, "q_") || !strings.Contains(out, "pending  b  배포 | 지금 배포할까요? (+1)") {
		t.Fatalf("ask: %q", out)
	}
	if out = h.mustRun(boss, "question", "list", "--status", "pending"); !strings.Contains(out, id) {
		t.Fatalf("boss list: %q", out)
	}
	if out = h.mustRun(boss, "question", "answer", id, "--file", af); !strings.Contains(out, "answered") || !strings.Contains(out, "by a") {
		t.Fatalf("answer: %q", out)
	}
	if out = h.mustRun(worker, "question", "show", id); !strings.Contains(out, "대상: 어디에? = web, api / 기타: 천천히") {
		t.Fatalf("show: %q", out)
	}
	if out = h.mustRun(worker, "question", "show", id, "--json"); !strings.Contains(out, `"status":"answered"`) {
		t.Fatalf("show json: %q", out)
	}
	if r := h.run(boss, "question", "answer", id, "--file", af); r.code == 0 || !strings.Contains(r.stderr, "409") {
		t.Fatalf("second answer: %d %s", r.code, r.stderr)
	}
	out = h.mustRun(worker, "question", "ask", "--file", qf)
	id2 := strings.Fields(out)[0]
	if out = h.mustRun(worker, "question", "withdraw", id2); !strings.Contains(out, "withdrawn") {
		t.Fatalf("withdraw: %q", out)
	}
	if out = h.mustRun(boss, "question", "list", "--status", "pending"); strings.TrimSpace(out) != "no questions" {
		t.Fatalf("empty list: %q", out)
	}
	for _, args := range [][]string{
		{"question"},
		{"question", "grant"},
		{"question", "show"},
		{"question", "list", "extra"},
		{"question", "ask"},
		{"question", "list", "--status", "open"},
	} {
		if r := h.run(boss, args...); r.code != cli.ExitUsage {
			t.Fatalf("%v: %d %s", args, r.code, r.stderr)
		}
	}
}
