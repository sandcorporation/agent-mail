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
	t     *testing.T
	s     *Server
	url   string
	clock *fakeClock
}

// newSOT is questionServer with an asker partner-a/coordinator and a boss
// team/boss of another base, behind a real HTTP test server.
func newSOT(t *testing.T) *sotHarness {
	t.Helper()
	s, clock := permissionServer(t)
	if _, err := s.db.Exec(`UPDATE question_settings SET can_ask=1 WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	s.admin.ProxyToken = "proxy-secret"
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/boss',?,'now'),('partner-a/coordinator',?,'now')`, sotBossToken, sotAskerToken); err != nil {
		t.Fatal(err)
	}
	s.bossRoles = map[string]bool{sotBoss: true}
	ts := httptest.NewServer(secureHeaders(http.HandlerFunc(s.api)))
	t.Cleanup(ts.Close)
	return &sotHarness{t: t, s: s, url: ts.URL, clock: clock}
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

// slot registers a session of the asker's base at dir with label.
func (h *sotHarness) slot(dir, label string) wire.SlotGrant {
	h.t.Helper()
	return h.session("register", sotAsker, dir, label, "")
}

// session posts a register or heartbeat of session id at dir with label as
// role and returns the grant.
func (h *sotHarness) session(action, role, dir, label, id string) wire.SlotGrant {
	h.t.Helper()
	code, body := h.as("POST", "/api/slots/"+action, sotAskerToken, role, wire.SlotRequest{Machine: "box-1", Directory: dir, Label: label, SessionID: id})
	if code != 200 {
		h.fail("%s %s: %d %s", action, dir, code, body)
	}
	var g wire.SlotGrant
	if err := json.Unmarshal(body, &g); err != nil {
		h.t.Fatal(err)
	}
	return g
}

// leave releases the slot of the session at dir.
func (h *sotHarness) leave(g wire.SlotGrant) {
	h.t.Helper()
	if code, body := h.as("POST", "/api/slots/release", sotAskerToken, g.Role, wire.SlotRequest{Machine: "box-1", Directory: g.Directory}); code != 200 {
		h.fail("release %s: %d %s", g.Role, code, body)
	}
}

// noAnswerMail wants no answer mail for id in role's inbox.
func (h *sotHarness) noAnswerMail(role, id string) {
	h.t.Helper()
	if n := h.answerMail(sotAskerToken, role, id); n != 0 {
		h.fail("answer mail delivered %d times to %s, want none", n, role)
	}
}

// delivery is the delivery field of question id in the admin list.
func (h *sotHarness) delivery(id string) string {
	h.t.Helper()
	code, body := h.admin("GET", "/api/admin/questions", nil)
	if code != 200 {
		h.fail("admin questions: %d %s", code, body)
	}
	var out wire.QuestionList
	if err := json.Unmarshal(body, &out); err != nil {
		h.t.Fatal(err)
	}
	for _, v := range out.Questions {
		if v.ID == id {
			return v.Delivery
		}
	}
	h.fail("admin list misses %s", id)
	return ""
}

// 8. The asking session left and came back under another number (another
// session took its old one): the answer reaches the new number, not the old.
func TestQuestionSingleSourceOfTruth_AskerBackUnderAnotherNumber(t *testing.T) {
	h := newSOT(t)
	h.slot("/work/a", "")
	q := h.slot("/work/q", "")
	v := h.ask(sotAskerToken, q.Role)
	h.leave(q)
	other := h.slot("/work/other", "")
	if other.Number != q.Number {
		h.fail("other session got %s, want the asker's old number %s", other.Role, q.Role)
	}
	back := h.slot("/work/q", "")
	if back.Number == q.Number {
		h.fail("asking session came back as %s, want another number", back.Role)
	}
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.noAnswerMail(other.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, back.Role, v.ID, 1)
}

// 9. The asking session is gone and two sessions of its base are live: only
// the most recently registered one gets the answer.
func TestQuestionSingleSourceOfTruth_NewestLiveSlotGetsAnswer(t *testing.T) {
	h := newSOT(t)
	q := h.slot("/work/q", "")
	v := h.ask(sotAskerToken, q.Role)
	h.leave(q)
	older := h.slot("/work/a", "")
	newer := h.slot("/work/b", "")
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.noAnswerMail(older.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, newer.Role, v.ID, 1)
}

// 10. No session of the asking base is live: the answer waits (the admin list
// says so) and the next session that registers gets it, once.
func TestQuestionSingleSourceOfTruth_WaitingAnswerGoesToNextSessionOnce(t *testing.T) {
	h := newSOT(t)
	a := h.slot("/work/a", "")
	q := h.slot("/work/q", "")
	v := h.ask(sotAskerToken, q.Role)
	h.leave(a)
	h.leave(q)
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	if d := h.delivery(v.ID); d != wire.DeliveryWaiting {
		h.fail("admin list delivery %q before any session, want %q", d, wire.DeliveryWaiting)
	}
	first := h.slot("/work/b", "")
	second := h.slot("/work/c", "")
	h.noAnswerMail(second.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, first.Role, v.ID, 1)
	if d := h.delivery(v.ID); d != "" {
		h.fail("admin list delivery %q after delivery, want none", d)
	}
}

// 11. A live session of the base with another label (another task) never
// gets the answer; it waits for a session with the asker's label.
func TestQuestionSingleSourceOfTruth_OtherLabelNeverGetsAnswer(t *testing.T) {
	h := newSOT(t)
	busy := h.slot("/work/a", "issue-2")
	q := h.slot("/work/q", "issue-1")
	v := h.ask(sotAskerToken, q.Role)
	h.leave(q)
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.noAnswerMail(busy.Role, v.ID)
	if d := h.delivery(v.ID); d != wire.DeliveryWaiting {
		h.fail("admin list delivery %q with only another label live, want %q", d, wire.DeliveryWaiting)
	}
	taker := h.slot("/work/d", "issue-2")
	h.noAnswerMail(taker.Role, v.ID)
	same := h.slot("/work/e", "issue-1")
	h.noAnswerMail(busy.Role, v.ID)
	h.noAnswerMail(taker.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, same.Role, v.ID, 1)
}

// 12. The asking session is still live: it gets the answer even when a newer
// session of its base registered after it asked.
func TestQuestionSingleSourceOfTruth_LiveAskerKeepsAnswer(t *testing.T) {
	h := newSOT(t)
	h.slot("/work/a", "")
	q := h.slot("/work/q", "")
	v := h.ask(sotAskerToken, q.Role)
	newer := h.slot("/work/b", "")
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.noAnswerMail(newer.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, q.Role, v.ID, 1)
	if d := h.delivery(v.ID); d != "" {
		h.fail("admin list delivery %q for a delivered answer, want none", d)
	}
}

// crashed is a session of the asker's base at /work/q (session s1, label)
// that asked and then died without a release, and its restart in the same
// folder: the plugin marks the directory with the new session id because the
// old session still holds /work/q, so the restart gets another number.
func (h *sotHarness) crashed(label string) (asked, restarted wire.SlotGrant, v wire.QuestionView) {
	h.t.Helper()
	h.slot("/work/a", "")
	asked = h.session("register", sotAsker, "/work/q", label, "1a2b3c4d-s1")
	v = h.ask(sotAskerToken, asked.Role)
	restarted = h.session("register", sotAsker, "/work/q #52abcdef", label, "52abcdef-s2")
	if restarted.Number == asked.Number {
		h.fail("restarted session got %s, want another number than %s", restarted.Role, asked.Role)
	}
	return asked, restarted, v
}

// outliveLease moves the clock past the lease of every slot that did not
// heartbeat, keeping g live with heartbeats on the way.
func (h *sotHarness) outliveLease(g wire.SlotGrant, label string) {
	h.t.Helper()
	half := h.s.presenceCfg.SlotLease/2 + time.Minute
	h.clock.add(half)
	h.session("heartbeat", g.Role, g.Directory, label, "")
	h.clock.add(half)
	h.session("heartbeat", g.Role, g.Directory, label, "")
}

// 13. The asking session crashed and restarted in the same folder under a
// session-marked directory: while the dead lease lives the answer stays in
// the asking slot; once it ends the unread answer moves to the restarted
// session, once.
func TestQuestionSingleSourceOfTruth_CrashedAskerAnswerMovesAfterLease(t *testing.T) {
	h := newSOT(t)
	asked, restarted, v := h.crashed("")
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.wantAnswerMail(sotAskerToken, asked.Role, v.ID, 1)
	h.outliveLease(restarted, "")
	h.noAnswerMail(asked.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, restarted.Role, v.ID, 1)
	h.session("heartbeat", restarted.Role, restarted.Directory, "", "")
	h.wantAnswerMail(sotAskerToken, restarted.Role, v.ID, 1)
}

// read marks the answer mail for id in role's inbox read.
func (h *sotHarness) read(role, id string) {
	h.t.Helper()
	code, body := h.as("GET", "/api/messages?box=inbox", sotAskerToken, role, nil)
	if code != 200 {
		h.fail("inbox of %s: %d %s", role, code, body)
	}
	var msgs []wire.Message
	if err := json.Unmarshal(body, &msgs); err != nil {
		h.t.Fatal(err)
	}
	for _, m := range msgs {
		if m.TaskID == id {
			if code, body := h.as("GET", fmt.Sprintf("/api/messages/%d", m.ID), sotAskerToken, role, nil); code != 200 {
				h.fail("read %d as %s: %d %s", m.ID, role, code, body)
			}
			return
		}
	}
	h.fail("no answer mail for %s in %s to read", id, role)
}

// 14. The crashed session read the answer before it died: the answer stays
// where it was read and the restarted session gets no copy.
func TestQuestionSingleSourceOfTruth_ReadAnswerStaysAfterLease(t *testing.T) {
	h := newSOT(t)
	asked, restarted, v := h.crashed("")
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.read(asked.Role, v.ID)
	h.outliveLease(restarted, "")
	h.noAnswerMail(restarted.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, asked.Role, v.ID, 1)
}

// 15. Two live sessions in the same folder (the second under a
// session-marked directory): the server cannot tell this from a crash, so
// the asking session gets the answer and keeps it while it heartbeats.
func TestQuestionSingleSourceOfTruth_LiveSessionsInOneFolderAskerKeepsAnswer(t *testing.T) {
	h := newSOT(t)
	asked, sibling, v := h.crashed("")
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	half := h.s.presenceCfg.SlotLease/2 + time.Minute
	for i := 0; i < 3; i++ {
		h.clock.add(half)
		h.session("heartbeat", asked.Role, asked.Directory, "", "")
		h.session("heartbeat", sibling.Role, sibling.Directory, "", "")
	}
	h.noAnswerMail(sibling.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, asked.Role, v.ID, 1)
}

// 16. A session in the asker's folder (session-marked) with another label is
// another task: when the asking lease ends it does not get the answer; the
// next session with the asker's label does.
func TestQuestionSingleSourceOfTruth_OtherLabelInSameFolderNeverGetsAnswer(t *testing.T) {
	h := newSOT(t)
	asked := h.session("register", sotAsker, "/work/q", "issue-1", "1a2b3c4d-s1")
	v := h.ask(sotAskerToken, asked.Role)
	other := h.session("register", sotAsker, "/work/q #52abcdef", "issue-2", "52abcdef-s2")
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.outliveLease(other, "issue-2")
	h.noAnswerMail(asked.Role, v.ID)
	h.noAnswerMail(other.Role, v.ID)
	if d := h.delivery(v.ID); d != wire.DeliveryWaiting {
		h.fail("admin list delivery %q after the asking lease ended, want %q", d, wire.DeliveryWaiting)
	}
	same := h.slot("/work/e", "issue-1")
	h.noAnswerMail(other.Role, v.ID)
	h.wantAnswerMail(sotAskerToken, same.Role, v.ID, 1)
}

// hostedAsker is the asker partner-a/coordinator as a hosted session on a
// node (machine pc-1) in /work/q, its plugin registered with the record's
// conversation.
func (h *sotHarness) hostedAsker() (*hostedRig, wire.SlotGrant) {
	h.t.Helper()
	r := newHostedRig(h.t, h.s, h.url, h.clock, sotAsker, sotAskerToken, "/work/q")
	return r, r.plugin("register", sotAsker, "/work/q", r.record().AgentSessionID)
}

// heartbeatsPastLease keeps the hosted asker's slot g live for three half
// leases, so every lease it no longer renews ends.
func (h *sotHarness) heartbeatsPastLease(r *hostedRig, g wire.SlotGrant, sid string) {
	h.t.Helper()
	half := h.s.presenceCfg.SlotLease/2 + time.Minute
	for i := 0; i < 3; i++ {
		h.clock.add(half)
		r.plugin("heartbeat", g.Role, g.Directory, sid)
	}
	r.sync()
}

// 17. A hosted session asks, then its process is relaunched (resume): the
// answer, given before the relaunched session registers, reaches the
// relaunched session once under its old number and stays once.
func TestQuestionSingleSourceOfTruth_HostedRelaunchGetsAnswerOnce(t *testing.T) {
	h := newSOT(t)
	r, asked := h.hostedAsker()
	sid := r.record().AgentSessionID
	v := h.ask(sotAskerToken, asked.Role)
	h.clock.add(time.Second)
	r.restart()
	if l := r.launchArgs(); l[len(l)-1] != "sb-"+r.id+" --resume "+sid {
		h.fail("relaunch %q", l)
	}
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	again := r.plugin("register", sotAsker, "/work/q", sid)
	if again.Role != asked.Role {
		h.fail("relaunched session registered as %s, asked as %s", again.Role, asked.Role)
	}
	h.wantAnswerMail(sotAskerToken, again.Role, v.ID, 1)
	h.heartbeatsPastLease(r, again, sid)
	h.wantAnswerMail(sotAskerToken, again.Role, v.ID, 1)
	if got := r.record().AgentSessionID; got != sid {
		h.fail("conversation changed to %s", got)
	}
}

// 18. A hosted session asks, then moves to a new conversation inside its
// terminal (/clear): the host follows it, the answer reaches the session
// once, and after a relaunch into the followed conversation it is still
// once under the same number.
func TestQuestionSingleSourceOfTruth_HostedConversationFollowedGetsAnswerOnce(t *testing.T) {
	h := newSOT(t)
	r, asked := h.hostedAsker()
	v := h.ask(sotAskerToken, asked.Role)
	next := "52abcdef-0000-4000-8000-000000000000"
	cleared := r.clear(asked, "/work/q", next)
	if cleared.Role != asked.Role {
		h.fail("cleared session registered as %s, asked as %s", cleared.Role, asked.Role)
	}
	r.sync()
	if got := r.record().AgentSessionID; got != next {
		h.fail("host did not follow the conversation: %s", got)
	}
	h.panelAnswer(sotBossToken, sotBoss, v.ID, 200)
	h.wantAnswerMail(sotAskerToken, cleared.Role, v.ID, 1)
	h.clock.add(time.Second)
	r.restart()
	if l := r.launchArgs(); l[len(l)-1] != "sb-"+r.id+" --resume "+next {
		h.fail("relaunch %q", l)
	}
	again := r.plugin("register", sotAsker, "/work/q", next)
	if again.Role != asked.Role {
		h.fail("relaunched session registered as %s", again.Role)
	}
	h.wantAnswerMail(sotAskerToken, again.Role, v.ID, 1)
	h.heartbeatsPastLease(r, again, next)
	h.wantAnswerMail(sotAskerToken, again.Role, v.ID, 1)
}
