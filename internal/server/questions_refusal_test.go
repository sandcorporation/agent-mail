package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// refusalAs asks as role, expects a JSON 403 and returns its body as a map,
// so a missing ask_via is told apart from an empty one.
func refusalAs(t *testing.T, s *Server, token, role string) map[string]any {
	t.Helper()
	w := callAs(s, "POST", "/api/questions", token, role, map[string]any{"questions": sampleQuestions()})
	if w.Code != 403 {
		t.Fatalf("ask as %s: %d %s", role, w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("ask as %s: Content-Type %q", role, ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("ask as %s: %v: %s", role, err, w.Body)
	}
	return body
}

func wantCanAskOff(t *testing.T, body map[string]any, askVia string) {
	t.Helper()
	if body["error"] != "this role may not ask questions (can_ask is off)" || body["code"] != "can_ask_off" {
		t.Fatalf("refusal = %v", body)
	}
	got, has := body["ask_via"]
	if askVia == "" && has {
		t.Fatalf("ask_via should be omitted: %v", body)
	}
	if askVia != "" && got != askVia {
		t.Fatalf("ask_via = %v, want %s", got, askVia)
	}
}

func TestCanAskRefusalNamesWhoToAsk(t *testing.T) {
	t.Parallel()
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	putSettings(t, s, "default", `{"can_ask":false}`)

	// A group role: its group's boss role.
	wantCanAskOff(t, refusalAs(t, s, partnerDevToken, partnerDevRole), partnerBossRole)
	// An internal role: the global boss.
	wantCanAskOff(t, refusalAs(t, s, workerToken, internalWorkerRol), internalBossRole)
	// The global boss itself has nobody else to ask.
	wantCanAskOff(t, refusalAs(t, s, bossToken, internalBossRole), "")
	// The group boss falls back to the global boss.
	wantCanAskOff(t, refusalAs(t, s, partnerBossToken, partnerBossRole), internalBossRole)

	// No global bosses: an internal role has nobody to ask.
	s.bossRoles = map[string]bool{}
	wantCanAskOff(t, refusalAs(t, s, workerToken, internalWorkerRol), "")

	// can_ask on: the question is posted.
	putSettings(t, s, "default", `{"can_ask":true}`)
	askQuestionAs(t, s, workerToken, internalWorkerRol, 201)
}

// ask_via is the first role SWITCHBOARD_BOSS_ROLES lists, not the first
// in sorted order.
func TestCanAskRefusalFollowsBossRolesOrder(t *testing.T) {
	t.Parallel()
	s := groupServer(t)
	putSettings(t, s, "default", `{"can_ask":false}`)
	if err := configureStall(s, fakeEnv(map[string]string{"SWITCHBOARD_BOSS_ROLES": "team/boss, hq/boss"})); err != nil {
		t.Fatal(err)
	}
	wantCanAskOff(t, refusalAs(t, s, workerToken, "team/worker"), "team/boss")
}

func TestQuestionForbiddenHasCode(t *testing.T) {
	t.Parallel()
	s := questionServer(t)
	v := askQuestionAs(t, s, workerToken, "team/worker", 201)
	w := callAs(s, "POST", "/api/questions/"+v.ID+"/withdraw", "secret-a", "a", nil)
	var body map[string]any
	if w.Code != 403 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["code"] != "question_forbidden" || body["error"] != "not allowed to act on this question" {
		t.Fatalf("withdraw by another base: %d %s", w.Code, w.Body)
	}
	if _, has := body["ask_via"]; has {
		t.Fatalf("ask_via on question_forbidden: %v", body)
	}
}

// refusalMails are the system mails in role's inbox about a can_ask refusal.
func refusalMails(t *testing.T, s *Server, role string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT subject||char(10)||body FROM messages WHERE recipient=? AND sender=? AND subject LIKE '[질문 거절]%' ORDER BY id`, role, identity.SystemSender)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func askWithKey(s *Server, token, role, key string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(map[string]any{"questions": sampleQuestions()})
	r := httptest.NewRequest("POST", "/api/questions", &b)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Switchboard-Role", role)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}

func refusalTarget(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if w.Code != 403 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["code"] != "can_ask_off" {
		t.Fatalf("want a can_ask_off 403: %d %s", w.Code, w.Body)
	}
	target, _ := body["settings_target"].(string)
	return target
}

// A can_ask refusal is logged, mailed once to ask_via with the question,
// names the matched settings target, and shows on the dashboard (#286).
func TestCanAskRefusalIsLoggedMailedAndShown(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", "")
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// The base's own row turns asking off.
	putSettings(t, s, "partner-a/dev", `{"can_ask":false}`)
	if got := refusalTarget(t, askWithKey(s, partnerDevToken, partnerDevRole, "k1")); got != "partner-a/dev" {
		t.Fatalf("settings_target %q", got)
	}
	mails := refusalMails(t, s, partnerBossRole)
	if len(mails) != 1 {
		t.Fatalf("mails to ask_via: %q", mails)
	}
	for _, want := range []string{"[질문 거절] partner-a/dev can_ask 꺼짐", "partner-a/dev", "배포", "지금 배포할까요?", "예", "아니오", "어디에?"} {
		if !strings.Contains(mails[0], want) {
			t.Errorf("mail lacks %q:\n%s", want, mails[0])
		}
	}
	var prio string
	if err := s.db.QueryRow(`SELECT priority FROM messages WHERE recipient=? AND subject LIKE '[질문 거절]%'`, partnerBossRole).Scan(&prio); err != nil || prio != "normal" {
		t.Fatalf("priority %q %v", prio, err)
	}
	// A retry with the same key mails no more; so does the same question without a key, twice.
	refusalTarget(t, askWithKey(s, partnerDevToken, partnerDevRole, "k1"))
	if n := len(refusalMails(t, s, partnerBossRole)); n != 1 {
		t.Fatalf("retry mailed again: %d", n)
	}
	refusalTarget(t, askWithKey(s, partnerDevToken, partnerDevRole, ""))
	refusalTarget(t, askWithKey(s, partnerDevToken, partnerDevRole, ""))
	if n := len(refusalMails(t, s, partnerBossRole)); n != 2 {
		t.Fatalf("keyless retries: %d mails, want 2", n)
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "question refused") {
			line = l
			break
		}
	}
	for _, want := range []string{"role=partner-a/dev", "code=can_ask_off", "target=partner-a/dev", "ask_via=" + partnerBossRole} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q lacks %q", line, want)
		}
	}
	if strings.Contains(logs.String(), "지금 배포할까요?") || strings.Contains(logs.String(), partnerDevToken) {
		t.Error("the log carries the question or a token")
	}

	// A slot of a base resolves through the base exactly as the base does.
	putSettings(t, s, "team/worker", `{"can_ask":false}`)
	if got := refusalTarget(t, askWithKey(s, workerToken, "team/worker-7", "")); got != "team/worker" {
		t.Fatalf("slot settings_target %q", got)
	}
	if got := refusalTarget(t, askWithKey(s, workerToken, internalWorkerRol, "")); got != "team/worker" {
		t.Fatalf("base settings_target %q", got)
	}

	// No matching row and no default row: "none".
	// (The API keeps the default row; an old database may lack it.)
	if _, err := s.db.Exec(`DELETE FROM question_settings WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	if got := refusalTarget(t, askWithKey(s, partnerBDevToken, partnerBDevRole, "")); got != "none" {
		t.Fatalf("no row settings_target %q", got)
	}

	// The dashboard list: refused roles of the last 24 h, with the target and the last time.
	w := adminWrite(s, "GET", "/api/admin/question-refusals", "")
	var list []wire.QuestionRefusal
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("refusals: %d %s", w.Code, w.Body)
	}
	got := map[string]string{}
	for _, r := range list {
		got[r.Role] = r.Target
		if r.At == "" || r.Count < 1 {
			t.Errorf("row %+v", r)
		}
	}
	want := map[string]string{partnerDevRole: "partner-a/dev", "team/worker-7": "team/worker", internalWorkerRol: "team/worker", partnerBDevRole: "none"}
	if len(got) != len(want) {
		t.Fatalf("refusals %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("refusals %v, want %v", got, want)
		}
	}

	// can_ask on: 201, no mail, no refusal line.
	putSettings(t, s, "partner-a/dev", `{"can_ask":true}`)
	logs.Reset()
	before := len(refusalMails(t, s, partnerBossRole))
	if w := askWithKey(s, partnerDevToken, partnerDevRole, "k2"); w.Code != 201 {
		t.Fatalf("allowed ask: %d %s", w.Code, w.Body)
	}
	if len(refusalMails(t, s, partnerBossRole)) != before || strings.Contains(logs.String(), "question refused") {
		t.Fatal("an allowed ask mailed or logged a refusal")
	}
}

// Without ask_via the refusal is only logged.
func TestCanAskRefusalWithoutAskViaOnlyLogs(t *testing.T) {
	t.Parallel()
	s := groupServer(t)
	s.bossRoles = map[string]bool{}
	putSettings(t, s, "default", `{"can_ask":false}`)
	refusalTarget(t, askWithKey(s, workerToken, internalWorkerRol, ""))
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE subject LIKE '[질문 거절]%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("mails %d %v", n, err)
	}
}
