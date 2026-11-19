package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

const (
	reportBossRole    = "team/boss"
	reportBossToken   = "secret-team-boss"
	reportWorkerRole  = "team/worker"
	reportWorkerToken = "secret-team-worker"
)

// reportServer has the boss role team/boss and the worker team/worker.
func reportServer(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES(?,?,'now'),(?,?,'now')`,
		reportBossRole, reportBossToken, reportWorkerRole, reportWorkerToken); err != nil {
		t.Fatal(err)
	}
	s.bossRoles = map[string]bool{reportBossRole: true}
	return s
}

func directive(t *testing.T, s *Server, subject string) int64 {
	t.Helper()
	w := callAs(s, "POST", "/api/messages", reportBossToken, reportBossRole, map[string]string{"recipient": reportWorkerRole, "subject": subject, "body": "do it"})
	if w.Code != 201 {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	var m wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m.ID
}

func workerReply(t *testing.T, s *Server, id int64, body string) int64 {
	t.Helper()
	w := callAs(s, "POST", "/api/messages/"+itoa(id)+"/reply", reportWorkerToken, reportWorkerRole, map[string]string{"subject": "re", "body": body})
	if w.Code != 201 {
		t.Fatalf("reply: %d %s", w.Code, w.Body)
	}
	var m wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m.ID
}

func postReport(s *Server, token, role string, body map[string]any, idem string) (int, wire.Report, string) {
	w := callAsIdem(s, "POST", "/api/reports", token, role, body, idem)
	var rp wire.Report
	_ = json.Unmarshal(w.Body.Bytes(), &rp)
	return w.Code, rp, w.Body.String()
}

func callAsIdem(s *Server, method, path, token, role string, body any, idem string) *httptest.ResponseRecorder {
	if idem == "" {
		return callAs(s, method, path, token, role, body)
	}
	m, _ := body.(map[string]any)
	m["idempotency_key"] = idem
	return callAs(s, method, path, token, role, m)
}

func mustReport(t *testing.T, s *Server, body map[string]any) wire.Report {
	t.Helper()
	code, rp, raw := postReport(s, reportBossToken, reportBossRole, body, "")
	if code != 201 {
		t.Fatalf("report: %d %s", code, raw)
	}
	return rp
}

func reportDetail(t *testing.T, s *Server, id int64) wire.ReportDetail {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/reports/"+itoa(id), "", nil)
	if w.Code != 200 {
		t.Fatalf("detail: %d %s", w.Code, w.Body)
	}
	var d wire.ReportDetail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func reportList(t *testing.T, s *Server) []wire.Report {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/reports", "", nil)
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var l wire.ReportList
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Reports
}

func TestReportNonBossForbidden(t *testing.T) {
	s := reportServer(t)
	code, _, raw := postReport(s, reportWorkerToken, reportWorkerRole, map[string]any{"title": "t", "body": "b"}, "")
	if code != 403 {
		t.Fatalf("worker: %d %s", code, raw)
	}
	if w := callAs(s, "GET", "/api/reports", reportBossToken, reportBossRole, nil); w.Code != 405 {
		t.Fatalf("GET: %d", w.Code)
	}
	if w := callAs(s, "POST", "/api/reports", "wrong", reportBossRole, map[string]any{"title": "t", "body": "b"}); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}

func TestReportValidation(t *testing.T) {
	s := reportServer(t)
	for _, body := range []map[string]any{
		{"body": "b"},
		{"title": "t"},
		{"title": "two\nlines", "body": "b"},
		{"title": "t", "request": "two\nlines", "body": "b"},
		{"title": "t", "body": "b", "mail": []int64{0}},
	} {
		if code, _, raw := postReport(s, reportBossToken, reportBossRole, body, ""); code != 400 {
			t.Errorf("%v: %d %s", body, code, raw)
		}
	}
}

func TestReportIdempotent(t *testing.T) {
	s := reportServer(t)
	body := func() map[string]any { return map[string]any{"title": "Weekly", "body": "all done"} }
	code, first, raw := postReport(s, reportBossToken, reportBossRole, body(), "k1")
	if code != 201 {
		t.Fatalf("first: %d %s", code, raw)
	}
	code, again, raw := postReport(s, reportBossToken, reportBossRole, body(), "k1")
	if code != 200 || again.ID != first.ID {
		t.Fatalf("retry: %d %s, want id %d", code, raw, first.ID)
	}
	// The header works as well as the body field.
	w := call(s, "POST", "/api/reports", reportBossToken, body(), "k1")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":`+itoa(first.ID)) {
		t.Fatalf("header retry: %d %s", w.Code, w.Body)
	}
	if code, other, _ := postReport(s, reportBossToken, reportBossRole, body(), "k2"); code != 201 || other.ID == first.ID {
		t.Fatalf("new key: %d id %d", code, other.ID)
	}
	if n := len(reportList(t, s)); n != 2 {
		t.Fatalf("%d reports, want 2", n)
	}
}

func TestReportWithoutMailIsDone(t *testing.T) {
	s := reportServer(t)
	rp := mustReport(t, s, map[string]any{"title": "Alone", "request": "tidy up", "body": "done by myself"})
	if rp.Status != wire.ReportDone || len(rp.Mail) != 0 || rp.Author != reportBossRole || rp.Request != "tidy up" {
		t.Fatalf("report %+v", rp)
	}
	l := reportList(t, s)
	if len(l) != 1 || l[0].Status != wire.ReportDone || l[0].Body != "" || l[0].LastReplyAt != nil {
		t.Fatalf("list %+v", l)
	}
}

func TestReportStatusFollowsAnsweredRule(t *testing.T) {
	s := reportServer(t)
	answered := directive(t, s, "build it")
	completed := directive(t, s, "ship it")
	rp := mustReport(t, s, map[string]any{"title": "Release", "body": "notes", "mail": []int64{answered, completed}})
	if rp.Status != wire.ReportOpen {
		t.Fatalf("new: %s", rp.Status)
	}
	// An acknowledgement from the worker to the boss in the thread answers it.
	reply := workerReply(t, s, answered, "built")
	d := reportDetail(t, s, rp.ID)
	if d.Status != wire.ReportOpen || len(d.Threads) != 2 {
		t.Fatalf("after one reply: %+v", d)
	}
	if th := d.Threads[0]; !th.Answered || th.Open || len(th.Replies) != 1 || th.Replies[0].ID != reply || !th.Replies[0].Answers || th.Directive.Body != "do it" {
		t.Fatalf("answered thread %+v", th)
	}
	if th := d.Threads[1]; th.Answered || !th.Open || th.Completed {
		t.Fatalf("open thread %+v", th)
	}
	// The recipient completing the second directive closes the report.
	if w := callAs(s, "POST", "/api/messages/"+itoa(completed)+"/complete", reportWorkerToken, reportWorkerRole, nil); w.Code >= 300 {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	d = reportDetail(t, s, rp.ID)
	if d.Status != wire.ReportDone || !d.Threads[1].Completed || d.Threads[1].Open {
		t.Fatalf("after complete: %+v", d)
	}
	l := reportList(t, s)
	if l[0].Status != wire.ReportDone || l[0].LastReplyAt == nil || !slices.Equal(l[0].Mail, []int64{answered, completed}) {
		t.Fatalf("list %+v", l[0])
	}
}

func TestReportLinkResolvesReplyToRoot(t *testing.T) {
	s := reportServer(t)
	root := directive(t, s, "root")
	reply := workerReply(t, s, root, "first")
	rp := mustReport(t, s, map[string]any{"title": "R", "body": "b", "mail": []int64{reply, root}})
	if !slices.Equal(rp.Mail, []int64{root}) {
		t.Fatalf("mail %v, want [%d]", rp.Mail, root)
	}
	d := reportDetail(t, s, rp.ID)
	if len(d.Threads) != 1 || d.Threads[0].Directive.ID != root || len(d.Threads[0].Replies) != 1 {
		t.Fatalf("threads %+v", d.Threads)
	}
}

func TestReportBodyMentionIsNotALink(t *testing.T) {
	s := reportServer(t)
	open := directive(t, s, "still open")
	rp := mustReport(t, s, map[string]any{"title": "R", "body": "see #" + itoa(open) + " and issue #12"})
	if len(rp.Mail) != 0 || rp.Status != wire.ReportDone {
		t.Fatalf("report %+v", rp)
	}
}

func TestReportUnknownMailFailsWholeCall(t *testing.T) {
	s := reportServer(t)
	ok := directive(t, s, "real")
	code, _, raw := postReport(s, reportBossToken, reportBossRole, map[string]any{"title": "R", "body": "b", "mail": []int64{ok, 9998, 9999}}, "")
	if code != 400 || !strings.Contains(raw, "9998, 9999") {
		t.Fatalf("%d %s", code, raw)
	}
	if n := len(reportList(t, s)); n != 0 {
		t.Fatalf("%d reports stored", n)
	}
	if w := adminRequest(s, "GET", "/api/admin/reports/12345", "", nil); w.Code != 404 {
		t.Fatalf("missing detail: %d", w.Code)
	}
}

// A group's boss role may report, but only on mail its group can see.
func TestReportGroupBossScope(t *testing.T) {
	s := groupServer(t)
	putGroup(t, s, "partner-a", `{"boss_role":"`+partnerBossRole+`"}`)
	putGroup(t, s, "partner-b", "")
	if code := sendAs(s, partnerBossToken, partnerBossRole, partnerDevRole); code != 201 {
		t.Fatalf("send in group: %d", code)
	}
	if code := sendAs(s, bossToken, internalBossRole, partnerBDevRole); code != 201 {
		t.Fatalf("send outside: %d", code)
	}
	var inside, outside int64
	_ = s.db.QueryRow(`SELECT id FROM messages WHERE recipient=?`, partnerDevRole).Scan(&inside)
	_ = s.db.QueryRow(`SELECT id FROM messages WHERE recipient=?`, partnerBDevRole).Scan(&outside)
	w := callAs(s, "POST", "/api/reports", partnerBossToken, partnerBossRole, map[string]any{"title": "R", "body": "b", "mail": []int64{inside, outside}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), itoa(outside)) || strings.Contains(w.Body.String(), itoa(inside)+",") {
		t.Fatalf("group boss: %d %s", w.Code, w.Body)
	}
	if w = callAs(s, "POST", "/api/reports", partnerBossToken, partnerBossRole, map[string]any{"title": "R", "body": "b", "mail": []int64{inside}}); w.Code != 201 {
		t.Fatalf("group boss in scope: %d %s", w.Code, w.Body)
	}
	if w = callAs(s, "POST", "/api/reports", partnerDevToken, partnerDevRole, map[string]any{"title": "R", "body": "b"}); w.Code != 403 {
		t.Fatalf("group dev: %d %s", w.Code, w.Body)
	}
}

func TestReportLiveKinds(t *testing.T) {
	if got := liveKinds("reports", false); !slices.Equal(got, []string{"reports"}) {
		t.Fatalf("POST /api/reports: %v", got)
	}
	for _, admin := range []bool{false, true} {
		if got := liveKinds("messages/1/reply", admin); !slices.Contains(got, "reports") {
			t.Fatalf("messages (admin %v): %v", admin, got)
		}
	}
	s := reportServer(t)
	sub, _, cancel, _ := s.live.Subscribe("", 0)
	defer cancel()
	mustReport(t, s, map[string]any{"title": "R", "body": "b"})
	if ev := <-sub.C; !slices.Contains(ev.Kinds, "reports") {
		t.Fatalf("event %+v", ev)
	}
}

func TestReportCLI(t *testing.T) {
	h := newCLIHarnessWith(t, func(s *Server) { s.bossRoles = map[string]bool{"a": true} })
	h.configure(nil, "a")
	for _, subject := range []string{"one", "two", "three"} {
		if w := call(h.s, "POST", "/api/messages", "secret-a", map[string]string{"recipient": "b", "subject": subject, "body": "x"}, ""); w.Code != 201 {
			t.Fatal(w.Body)
		}
	}
	if out := h.mustRun(nil, "report", "--title", "Plain", "--body", "done"); out != "1" {
		t.Fatalf("plain report printed %q", out)
	}
	body := filepath.Join(h.dir, "body.md")
	if err := os.WriteFile(body, []byte("# Done\n\nSee #2.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"report", "--title", "Linked", "--request", "do two things", "--mail", "1,2", "--mail", "#3", "--body-file", body, "--idem", "k1", "--json"}
	out := h.mustRun(nil, args...)
	var rp wire.Report
	if err := json.Unmarshal([]byte(out), &rp); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if rp.ID != 2 || rp.Title != "Linked" || rp.Request != "do two things" || rp.Body != "# Done\n\nSee #2.\n" || !slices.Equal(rp.Mail, []int64{1, 2, 3}) || rp.Status != wire.ReportOpen {
		t.Fatalf("report %+v", rp)
	}
	if again := h.mustRun(nil, args...); !strings.Contains(again, `"id": 2`) && !strings.Contains(again, `"id":2`) {
		t.Fatalf("retry: %s", again)
	}
	if r := h.run(nil, "report", "--title", "Bad", "--mail", "99", "--body", "x"); r.code == 0 || !strings.Contains(r.stderr, "99") {
		t.Fatalf("unknown mail: %+v", r)
	}
	if r := h.run(nil, "report", "--body", "x"); r.code == 0 || !strings.Contains(r.stderr, "--title") {
		t.Fatalf("no title: %+v", r)
	}
	h.configure(nil, "b")
	if r := h.run(nil, "report", "--title", "T", "--body", "x"); r.code == 0 || !strings.Contains(r.stderr, "403") {
		t.Fatalf("non-boss: %+v", r)
	}
	if help := h.mustRun(nil, "help", "report"); !strings.Contains(help, "--mail") || !strings.Contains(help, "--idem") {
		t.Fatalf("help: %s", help)
	}
}
