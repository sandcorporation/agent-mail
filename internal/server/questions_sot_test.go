package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/asks"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// The server is the single source of truth for a question: a session asks
// (POST /api/questions), a boss panel (POST /api/questions/{id}/answer) or
// the dashboard (POST /api/admin/questions/{id}/answer) answers, and the
// asker learns the answer from exactly one mail. These tests drive that whole
// path over real HTTP and count the asker's answer mail in every case.
// Release and CI run them as a named step (scripts/ci/question_sot.sh), which
// fails when the selection runs no test, so a rename cannot skip them.

const (
	sotBoss       = "team/boss"
	sotBossToken  = "secret-example-boss"
	sotAsker      = "partner-a/coordinator"
	sotAskerToken = "secret-example-coordinator"
)

type sotHarness struct {
	t   *testing.T
	s   *Server
	url string
}

// newSOT is questionServer with an asker partner-a/coordinator and a boss
// team/boss of another base, behind a real HTTP test server.
func newSOT(t *testing.T) *sotHarness {
	t.Helper()
	s := questionServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/boss',?,'now'),('partner-a/coordinator',?,'now')`, sotBossToken, sotAskerToken); err != nil {
		t.Fatal(err)
	}
	s.bossRoles = map[string]bool{sotBoss: true}
	ts := httptest.NewServer(secureHeaders(http.HandlerFunc(s.api)))
	t.Cleanup(ts.Close)
	return &sotHarness{t: t, s: s, url: ts.URL}
}

func (h *sotHarness) fail(format string, a ...any) {
	h.t.Helper()
	h.t.Fatalf("question source of truth: "+format, a...)
}

func (h *sotHarness) do(method, path string, headers map[string]string, body any) (int, []byte) {
	h.t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			h.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, h.url+path, &b)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.fail("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.StatusCode, out
}

func (h *sotHarness) as(method, path, token, role string, body any) (int, []byte) {
	h.t.Helper()
	return h.do(method, path, map[string]string{"Authorization": "Bearer " + token, "X-Switchboard-Role": role}, body)
}

func (h *sotHarness) admin(method, path string, body any) (int, []byte) {
	h.t.Helper()
	headers := map[string]string{"X-Switchboard-Admin": "proxy-secret"}
	for k, v := range dashboardHeaders {
		headers[k] = v
	}
	return h.do(method, path, headers, body)
}

func (h *sotHarness) ask(token, role string) wire.QuestionView {
	h.t.Helper()
	code, body := h.as("POST", "/api/questions", token, role, map[string]any{"questions": sampleQuestions()})
	if code != 201 {
		h.fail("ask as %s: %d %s", role, code, body)
	}
	return decodeQuestion(h.t, body)
}

// panelAnswer answers as a session (a boss panel, or the asker of an owner
// question) and wants the status code want.
func (h *sotHarness) panelAnswer(token, role, id string, want int) {
	h.t.Helper()
	if code, body := h.as("POST", "/api/questions/"+id+"/answer", token, role, map[string]any{"answers": sampleAnswers()}); code != want {
		h.fail("panel answer %s as %s: status %d, want %d (%s)", id, role, code, want, body)
	}
}

func (h *sotHarness) dashboardAnswer(id string, want int) {
	h.t.Helper()
	if code, body := h.admin("POST", "/api/admin/questions/"+id+"/answer", map[string]any{"answers": sampleAnswers()}); code != want {
		h.fail("dashboard answer %s: status %d, want %d (%s)", id, code, want, body)
	}
}

func (h *sotHarness) withdraw(token, role, id string, want int) {
	h.t.Helper()
	if code, body := h.as("POST", "/api/questions/"+id+"/withdraw", token, role, nil); code != want {
		h.fail("withdraw %s as %s: status %d, want %d (%s)", id, role, code, want, body)
	}
}

// stored is the question as the server holds it, read by its asker.
func (h *sotHarness) stored(token, role, id string) wire.QuestionView {
	h.t.Helper()
	code, body := h.as("GET", "/api/questions/"+id, token, role, nil)
	if code != 200 {
		h.fail("read %s as %s: %d %s", id, role, code, body)
	}
	return decodeQuestion(h.t, body)
}

func (h *sotHarness) wantStored(token, role, id, status, by string) wire.QuestionView {
	h.t.Helper()
	v := h.stored(token, role, id)
	if v.Status != status || v.AnsweredBy != by {
		h.fail("stored %s is %s by %q, want %s by %q", id, v.Status, v.AnsweredBy, status, by)
	}
	return v
}

// answerMail counts the answer mail for id in role's inbox, read over HTTP.
func (h *sotHarness) answerMail(token, role, id string) int {
	h.t.Helper()
	code, body := h.as("GET", "/api/messages?box=inbox", token, role, nil)
	if code != 200 {
		h.fail("inbox of %s: %d %s", role, code, body)
	}
	var msgs []wire.Message
	if err := json.Unmarshal(body, &msgs); err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.TaskID == id && strings.HasPrefix(m.Subject, "[질문 답] "+id) {
			n++
		}
	}
	return n
}

// wantAnswerMail checks that role's inbox holds want answer mails for id and
// that the server sent no other answer mail for it anywhere.
func (h *sotHarness) wantAnswerMail(token, role, id string, want int) {
	h.t.Helper()
	if n := h.answerMail(token, role, id); n != want {
		h.fail("answer mail delivered %d times to %s, want exactly %d", n, role, want)
	}
	total := 0
	for _, m := range questionMail(h.t, h.s, id) {
		if strings.HasPrefix(m.subject, "[질문 답] ") {
			total++
		}
	}
	if total != want {
		h.fail("answer mail sent %d times in all, want exactly %d", total, want)
	}
}

func (h *sotHarness) poll(query string) asks.QuestionPoll {
	h.t.Helper()
	code, body := h.as("GET", "/api/questions/poll"+query, sotBossToken, sotBoss, nil)
	if code != 200 {
		h.fail("boss poll %s: %d %s", query, code, body)
	}
	var out asks.QuestionPoll
	if err := json.Unmarshal(body, &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// 1. A boss panel answers: answered by that boss role, one answer mail.
func TestQuestionSingleSourceOfTruth_PanelAnswer(t *testing.T) {
	h := newSOT(t)
	v := h.ask(sotAskerToken, sotAsker)
	h.wantAnswerMail(sotAskerToken, sotAsker, v.ID, 0)
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	got := h.wantStored(sotAskerToken, sotAsker, v.ID, "answered", sotBoss)
	if len(got.Answers) != 2 || got.AnsweredAt == nil {
		h.fail("stored answer %+v", got)
	}
	h.wantAnswerMail(sotAskerToken, sotAsker, v.ID, 1)
}

// 2. The dashboard answers: answered by "dashboard", one answer mail.
func TestQuestionSingleSourceOfTruth_DashboardAnswer(t *testing.T) {
	h := newSOT(t)
	v := h.ask(sotAskerToken, sotAsker)
	h.dashboardAnswer(v.ID, 200)
	h.wantStored(sotAskerToken, sotAsker, v.ID, "answered", asks.QuestionByDashboard)
	h.wantAnswerMail(sotAskerToken, sotAsker, v.ID, 1)
}

// 3. Panel and dashboard both answer, in either order: the second gets 409,
// the first answer stays, and the asker is told once.
func TestQuestionSingleSourceOfTruth_FirstAnswerWins(t *testing.T) {
	t.Run("panel then dashboard", func(t *testing.T) {
		h := newSOT(t)
		v := h.ask(sotAskerToken, sotAsker)
		h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
		h.dashboardAnswer(v.ID, 409)
		h.wantStored(sotAskerToken, sotAsker, v.ID, "answered", sotBoss)
		h.wantAnswerMail(sotAskerToken, sotAsker, v.ID, 1)
	})
	t.Run("dashboard then panel", func(t *testing.T) {
		h := newSOT(t)
		v := h.ask(sotAskerToken, sotAsker)
		h.dashboardAnswer(v.ID, 200)
		h.panelAnswer(sotBossToken, sotBoss, v.ID, 409)
		h.wantStored(sotAskerToken, sotAsker, v.ID, "answered", asks.QuestionByDashboard)
		h.wantAnswerMail(sotAskerToken, sotAsker, v.ID, 1)
	})
}

// 4. A withdrawn question takes no answer from either side (409) and nobody
// is mailed an answer.
func TestQuestionSingleSourceOfTruth_WithdrawBeforeAnswer(t *testing.T) {
	h := newSOT(t)
	v := h.ask(sotAskerToken, sotAsker)
	h.withdraw(sotAskerToken, sotAsker, v.ID, 200)
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 409)
	h.dashboardAnswer(v.ID, 409)
	if got := h.wantStored(sotAskerToken, sotAsker, v.ID, "withdrawn", ""); got.WithdrawnAt == nil || got.Answers != nil {
		h.fail("withdrawn question %+v", got)
	}
	h.wantAnswerMail(sotAskerToken, sotAsker, v.ID, 0)
	if m := questionMail(t, h.s, v.ID); len(m) != 0 {
		h.fail("withdraw mailed %d times, want none: %+v", len(m), m)
	}
}

// 5. The asking session restarts before the answer: its slot goes offline
// (release) and a new session registers under the same role. The answer
// reaches the role address once and the restarted session's inbox shows it.
func TestQuestionSingleSourceOfTruth_AskerSessionRestarts(t *testing.T) {
	register := func(h *sotHarness, dir, session string) wire.SlotGrant {
		t.Helper()
		code, body := h.as("POST", "/api/slots/register", sotAskerToken, sotAsker, wire.SlotRequest{Machine: "box-1", Directory: dir, SessionID: session})
		if code != 200 {
			h.fail("register %s: %d %s", dir, code, body)
		}
		var g wire.SlotGrant
		if err := json.Unmarshal(body, &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	release := func(h *sotHarness, role, dir string) {
		t.Helper()
		if code, body := h.as("POST", "/api/slots/release", sotAskerToken, role, wire.SlotRequest{Machine: "box-1", Directory: dir}); code != 200 {
			h.fail("release %s: %d %s", dir, code, body)
		}
	}
	for _, tc := range []struct {
		name   string
		before []string // directories registered before the asker's
	}{
		{"slot 1 (the role address)", nil},
		{"numbered slot", []string{"/work/other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSOT(t)
			for i, dir := range tc.before {
				register(h, dir, fmt.Sprintf("s-other-%d", i))
			}
			first := register(h, "/work/q", "s-old")
			v := h.ask(sotAskerToken, first.Role)
			release(h, first.Role, "/work/q")
			again := register(h, "/work/q", "s-new")
			if again.Role != first.Role {
				h.fail("restarted session registered as %s, asked as %s", again.Role, first.Role)
			}
			h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
			h.wantStored(sotAskerToken, again.Role, v.ID, "answered", sotBoss)
			h.wantAnswerMail(sotAskerToken, again.Role, v.ID, 1)
		})
	}
}

// 6. A boss long poll sees create, answer and withdraw once each, so a boss
// panel closes items answered or withdrawn elsewhere.
func TestQuestionSingleSourceOfTruth_BossPollSeesEachChangeOnce(t *testing.T) {
	h := newSOT(t)
	cursor := h.poll("?after=0&timeout=0").Cursor
	once := func(step, id, status string) {
		t.Helper()
		got := h.poll(fmt.Sprintf("?after=%d&timeout=0", cursor))
		if len(got.Questions) != 1 || got.Questions[0].ID != id || got.Questions[0].Status != status {
			h.fail("boss poll after %s saw %+v, want %s %s once", step, got.Questions, id, status)
		}
		cursor = got.Cursor
		if again := h.poll(fmt.Sprintf("?after=%d&timeout=0", cursor)); len(again.Questions) != 0 {
			h.fail("boss poll after %s saw %d changes again", step, len(again.Questions))
		}
	}
	answered := h.ask(sotAskerToken, sotAsker)
	once("create", answered.ID, "pending")

	// A waiting boss poll wakes on a dashboard answer.
	done := make(chan asks.QuestionPoll, 1)
	go func() {
		_, body := h.as("GET", fmt.Sprintf("/api/questions/poll?after=%d&timeout=20", cursor), sotBossToken, sotBoss, nil)
		var out asks.QuestionPoll
		_ = json.Unmarshal(body, &out)
		done <- out
	}()
	waitSubscribers(t, h.s, "#questions", 1)
	h.dashboardAnswer(answered.ID, 200)
	select {
	case got := <-done:
		if len(got.Questions) != 1 || got.Questions[0].ID != answered.ID || got.Questions[0].Status != "answered" {
			h.fail("woken boss poll saw %+v, want %s answered", got.Questions, answered.ID)
		}
	case <-time.After(10 * time.Second):
		h.fail("boss poll was not woken by a dashboard answer")
	}
	once("answer", answered.ID, "answered")
	h.wantAnswerMail(sotAskerToken, sotAsker, answered.ID, 1)

	withdrawn := h.ask(sotAskerToken, sotAsker)
	once("create", withdrawn.ID, "pending")
	h.withdraw(sotAskerToken, sotAsker, withdrawn.ID, 200)
	once("withdraw", withdrawn.ID, "withdrawn")
	h.wantAnswerMail(sotAskerToken, sotAsker, withdrawn.ID, 0)
}

// 7. An owner question (no boss of another base) is answered by its asker,
// stored as "panel", with one answer mail.
func TestQuestionSingleSourceOfTruth_OwnerQuestionPanelAnswer(t *testing.T) {
	h := newSOT(t)
	v := h.ask(sotBossToken, sotBoss)
	if v.Answerer != wire.AnswererOwner {
		h.fail("boss question answerer %q, want %q", v.Answerer, wire.AnswererOwner)
	}
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.wantStored(sotBossToken, sotBoss, v.ID, "answered", asks.QuestionByPanel)
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 409)
	h.dashboardAnswer(v.ID, 409)
	h.wantAnswerMail(sotBossToken, sotBoss, v.ID, 1)
}
