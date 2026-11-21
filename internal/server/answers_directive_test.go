package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// readJSON is `read ID --json` decoded.
func readJSON(t *testing.T, h *cliHarness, id string) wire.Message {
	t.Helper()
	var m wire.Message
	if err := json.Unmarshal([]byte(h.mustRun(nil, "read", id, "--json")), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The sender of a directive sees, when reading an answer, that the directive
// is still open: one line under the header and answers_directive in the JSON.
// The recipient never does, and neither does the sender once it is done or
// closed (#275).
func TestReadShowsAnsweredDirectiveOpenToSender(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure([]string{"--profile", "b"}, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "build", "--body", "x") // #1
	h.mustRun(nil, "--profile", "b", "reply", "1", "--body", "started")                        // #2

	h.configure(nil, "a")
	text := h.mustRun(nil, "read", "2")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[2] != cli.DirectiveOpenNote(1, 2) || lines[2] != "directive #1 still open: close 2 closes it and completes #2" {
		t.Fatalf("sender read:\n%s", text)
	}
	if m := readJSON(t, h, "2"); m.AnswersDirective == nil || *m.AnswersDirective != (wire.DirectiveRef{ID: 1, State: "open"}) {
		t.Fatalf("sender read JSON: %+v", m.AnswersDirective)
	}
	inbox := h.mustRun(nil, "inbox", "--json")
	if !strings.Contains(inbox, `"answers_directive": {`) || strings.Count(inbox, `"answers_directive"`) != 1 {
		t.Fatalf("inbox --json:\n%s", inbox)
	}
	// The directive itself answers nothing.
	if m := readJSON(t, h, "1"); m.AnswersDirective != nil {
		t.Fatalf("directive read by its sender: %+v", m.AnswersDirective)
	}

	// The recipient reads its own answer and the directive: nothing.
	h.configure(nil, "b")
	for _, id := range []string{"1", "2"} {
		if text := h.mustRun(nil, "read", id); strings.Contains(text, "still open") {
			t.Fatalf("recipient read %s:\n%s", id, text)
		}
		if m := readJSON(t, h, id); m.AnswersDirective != nil {
			t.Fatalf("recipient read %s JSON: %+v", id, m.AnswersDirective)
		}
	}

	// Done: the state is done and no line is printed.
	h.mustRun(nil, "reply", "1", "--done", "--body", "built") // #3
	h.configure(nil, "a")
	for _, id := range []string{"2", "3"} {
		if text := h.mustRun(nil, "read", id); strings.Contains(text, "still open") {
			t.Fatalf("read %s after done:\n%s", id, text)
		}
		if m := readJSON(t, h, id); m.AnswersDirective == nil || m.AnswersDirective.State != "done" {
			t.Fatalf("read %s after done: %+v", id, m.AnswersDirective)
		}
	}

	// Closed by the sender: also done.
	h.mustRun(nil, "send", "--to", "b", "--subject", "test", "--body", "y") // #4
	h.configure(nil, "b")
	h.mustRun(nil, "reply", "4", "--body", "on it") // #5
	h.configure(nil, "a")
	if m := readJSON(t, h, "5"); m.AnswersDirective == nil || m.AnswersDirective.State != "open" || m.AnswersDirective.ID != 4 {
		t.Fatalf("before close: %+v", m.AnswersDirective)
	}
	h.mustRun(nil, "close", "4")
	if text := h.mustRun(nil, "read", "5"); strings.Contains(text, "still open") {
		t.Fatalf("read after close:\n%s", text)
	}
	if m := readJSON(t, h, "5"); m.AnswersDirective == nil || m.AnswersDirective.State != "done" {
		t.Fatalf("after close: %+v", m.AnswersDirective)
	}
}

// A follow-up in the thread (the sender asks for more, the recipient answers
// that) still answers the root directive: the walk goes up the reply chain to
// the nearest directive.
func TestAnswerToFollowUpNamesRootDirective(t *testing.T) {
	t.Parallel()
	s := testServer(t)
	d := sendMail(t, s, "secret-a", "b", "build")
	r1 := replyAs(t, s, "secret-b", d)
	ask := replyAs(t, s, "secret-a", r1) // a plain reply: no new directive
	r2 := replyAs(t, s, "secret-b", ask)
	var m wire.Message
	w := call(s, "GET", "/api/messages/"+itoa(r2), "secret-a", nil, "")
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m.AnswersDirective == nil || m.AnswersDirective.ID != d || m.AnswersDirective.State != "open" {
		t.Fatalf("answer to a follow-up: %s", w.Body)
	}
	// b reading the follow-up from a: b did not send the directive.
	w = call(s, "GET", "/api/messages/"+itoa(ask), "secret-b", nil, "")
	if strings.Contains(w.Body.String(), "answers_directive") {
		t.Fatalf("recipient sees answers_directive: %s", w.Body)
	}
}

func replyAs(t *testing.T, s *Server, token string, id int64) int64 {
	t.Helper()
	w := call(s, "POST", "/api/messages/"+itoa(id)+"/reply", token, map[string]any{"body": "re"}, "")
	if w.Code != 201 {
		t.Fatalf("reply: %d %s", w.Code, w.Body)
	}
	var m wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m.ID
}

// The event stream, the long poll and watch carry answers_directive for the
// directive's sender (#275).
func TestEventsCarryAnsweredDirective(t *testing.T) {
	t.Parallel()
	s, base := eventServer(t)
	d := sendMail(t, s, "secret-a", "b", "build")
	st := openEvents(t, base, "secret-a", "a", nil, "")
	if ids, _ := st.untilReady(t); len(ids) != 0 {
		t.Fatalf("backlog %v", ids)
	}
	r := replyAs(t, s, "secret-b", d)
	ev := st.nextMessage(t)
	ref, _ := ev["answers_directive"].(map[string]any)
	if int64(ev["id"].(float64)) != r || ref == nil || ref["id"] != float64(d) || ref["state"] != "open" {
		t.Fatalf("stream event %v", ev)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := poll(t, ctx, base, "secret-a", "a", "?timeout=0", nil)
	if len(p.resp.Events) != 1 || p.resp.Events[0].AnswersDirective == nil || *p.resp.Events[0].AnswersDirective != (wire.DirectiveRef{ID: d, State: "open"}) {
		t.Fatalf("poll %s", p.body)
	}
	// The directive's recipient gets the directive without the field.
	p = poll(t, ctx, base, "secret-b", "b", "?timeout=0", nil)
	if len(p.resp.Events) != 1 || p.resp.Events[0].AnswersDirective != nil {
		t.Fatalf("recipient poll %s", p.body)
	}

	w := call(s, "GET", "/api/watch?after=0", "secret-a", nil, "")
	var wv struct {
		Answers map[string]wire.DirectiveRef `json:"answers_directive"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &wv)
	if wv.Answers[itoa(r)] != (wire.DirectiveRef{ID: d, State: "open"}) {
		t.Fatalf("watch %s", w.Body)
	}
}

// The report count: 진행 중 rests on open_directives > 0 out of directives,
// the linked threads that still exist (#275).
func TestReportCountsOpenDirectives(t *testing.T) {
	t.Parallel()
	s := reportServer(t)
	open := directive(t, s, "build it")
	done := directive(t, s, "ship it")
	rp := mustReport(t, s, map[string]any{"title": "Release", "body": "notes", "mail": []int64{open, done}})
	if rp.Directives != 2 || rp.OpenDirectives != 2 || rp.Status != wire.ReportOpen {
		t.Fatalf("new: %+v", rp)
	}
	workerReply(t, s, open, "started")
	if w := callAs(s, "POST", "/api/messages/"+itoa(done)+"/reply", reportWorkerToken, reportWorkerRole, map[string]any{"body": "shipped", "done": true}); w.Code != 201 {
		t.Fatalf("reply --done: %d %s", w.Code, w.Body)
	}
	d := reportDetail(t, s, rp.ID)
	open1 := 0
	for _, th := range d.Threads {
		if th.Open {
			open1++
		}
	}
	if d.Directives != 2 || d.OpenDirectives != 1 || open1 != 1 || d.Status != wire.ReportOpen {
		t.Fatalf("detail: %+v", d.Report)
	}
	if l := reportList(t, s); l[0].Directives != 2 || l[0].OpenDirectives != 1 {
		t.Fatalf("list: %+v", l[0])
	}
	// The sender closes the open one: 0/2, 완료.
	if w := callAs(s, "POST", "/api/messages/"+itoa(open)+"/close", reportBossToken, reportBossRole, nil); w.Code >= 300 {
		t.Fatalf("close: %d %s", w.Code, w.Body)
	}
	if l := reportList(t, s); l[0].OpenDirectives != 0 || l[0].Status != wire.ReportDone {
		t.Fatalf("after close: %+v", l[0])
	}
}
