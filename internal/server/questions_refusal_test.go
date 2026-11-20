package server

import (
	"encoding/json"
	"strings"
	"testing"
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
	s := groupServer(t)
	putSettings(t, s, "default", `{"can_ask":false}`)
	if err := configureStall(s, fakeEnv(map[string]string{"SWITCHBOARD_BOSS_ROLES": "team/boss, hq/boss"})); err != nil {
		t.Fatal(err)
	}
	wantCanAskOff(t, refusalAs(t, s, workerToken, "team/worker"), "team/boss")
}

func TestQuestionForbiddenHasCode(t *testing.T) {
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
