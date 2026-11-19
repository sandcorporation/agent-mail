package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// openIDs lists the open directive ids of every service, ascending.
func openIDs(t *testing.T, s *Server) []int64 {
	t.Helper()
	var got []int64
	for _, p := range mustProgress(t, s).Services {
		for _, d := range p.OpenDirectives {
			got = append(got, d.ID)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

// unrepliedIDs lists the open directives that no reply has answered yet,
// ascending. Under answered rule 2 (#269) a reply leaves a directive open,
// so tests about which mail counts as a reply (mail.Replies) look here.
func unrepliedIDs(t *testing.T, s *Server) []int64 {
	t.Helper()
	var got []int64
	for _, p := range mustProgress(t, s).Services {
		for _, d := range p.OpenDirectives {
			if d.RepliedAt == nil {
				got = append(got, d.ID)
			}
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

func sameIDs(got, want []int64) bool {
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	return fmt.Sprint(got) == fmt.Sprint(want)
}

// (c) A report sent as new mail names the directives it answers: "#<id>" in
// its subject or body, from the recipient base to the sender base or a boss.
// The metrics count it as an answer (mail.Answers); the progress view does
// not (mail.Replies, #214), so every directive here stays open.
func TestProgressAnsweredByMention(t *testing.T) {
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true, "hq/chief": true}
	at := clock.now().Add(-time.Hour)
	n := 0
	next := func() time.Time { n++; return at.Add(time.Duration(n) * time.Minute) }
	d := func(subject string) int64 {
		return mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker-1", subject: subject, at: next()})
	}
	report := func(from, to, subject, body string) {
		mailAt(t, s, flowRow{sender: from, recipient: to, subject: subject, body: body, at: next()})
	}
	// One report closes two directives, from another slot of the base.
	a, b := d("a"), d("b")
	report("team/worker-2", "hq/boss", fmt.Sprintf("Re #%d·#%d", a, b), "done")
	// The id in the body, to another boss role.
	inBody := d("in body")
	report("team/worker", "hq/chief", "report", fmt.Sprintf("finished (#%d).", inBody))
	// Another number (#<id>0, #<id>a) does not.
	longer := d("longer number")
	report("team/worker", "hq/boss", fmt.Sprintf("Re #%d0", longer), fmt.Sprintf("see #%da", longer))
	// Another base does not.
	otherBase := d("other base")
	report("ops/lead", "hq/boss", fmt.Sprintf("Re #%d", otherBase), "")
	// Mail to a role neither the sender's base nor a boss does not.
	sideways := d("sideways")
	report("team/worker", "ops/lead", fmt.Sprintf("Re #%d", sideways), "")
	// Mail sent before the directive does not.
	early := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "early", at: at.Add(30 * time.Minute)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", early), at: at.Add(29 * time.Minute)})

	if got, want := openIDs(t, s), []int64{a, b, inBody, longer, otherBase, sideways, early}; !sameIDs(got, want) {
		t.Fatalf("open %v, want %v (a mention is no reply)", got, want)
	}
	// Metrics first reply keeps the loose rule: a, b and inBody are answered.
	if fr := mustMetrics(t, s, 7).FirstReply; fr.Answered != 3 {
		t.Fatalf("first_reply %+v, want 3 answered", fr.LatencyStats)
	}
}

// slotsWithRoles is a server with hq/boss (a boss) and team/worker roles.
func slotsWithRoles(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('hq/boss','secret-hq/boss','now')`); err != nil {
		t.Fatal(err)
	}
	return s, clock
}

func tokenFor(role string) string {
	if role == "team/worker" {
		return workerToken
	}
	return "secret-" + role
}

func apiMail(t *testing.T, s *Server, clock *fakeClock, as, path string, body map[string]any) wire.Message {
	t.Helper()
	clock.add(time.Minute)
	w := callAs(s, "POST", path, tokenFor(as), as, body)
	if w.Code != 201 && w.Code != 200 {
		t.Fatalf("%s as %s: %d %s", path, as, w.Code, w.Body)
	}
	var m wire.Message
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// (d) The recipient completing a directive closes it; reading does not.
func TestProgressDirectiveClosedByComplete(t *testing.T) {
	s, clock := slotsWithRoles(t)
	done := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "finish"}).ID
	read := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "look"}).ID
	if w := callAs(s, "GET", fmt.Sprintf("/api/messages/%d", read), workerToken, "team/worker", nil); w.Code != 200 {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	if w := callAs(s, "GET", fmt.Sprintf("/api/messages/%d", done), workerToken, "team/worker", nil); w.Code != 200 {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	if got := openIDs(t, s); !sameIDs(got, []int64{done, read}) {
		t.Fatalf("after read: open %v", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/complete", done), nil)
	if got := openIDs(t, s); !sameIDs(got, []int64{read}) {
		t.Fatalf("after complete: open %v, want %d", got, read)
	}
}

// (e) A reply is not a directive unless sent with directive; a send is.
func TestProgressRepliesAreNotDirectives(t *testing.T) {
	s, clock := slotsWithRoles(t)
	report := apiMail(t, s, clock, "team/worker", "/api/messages", map[string]any{"recipient": "hq/boss", "subject": "report"}).ID
	ack := apiMail(t, s, clock, "hq/boss", fmt.Sprintf("/api/messages/%d/reply", report), map[string]any{"subject": "ok", "body": "thanks"})
	if ack.Directive {
		t.Fatalf("plain reply marked directive: %+v", ack)
	}
	more := apiMail(t, s, clock, "hq/boss", fmt.Sprintf("/api/messages/%d/reply", report), map[string]any{"subject": "next", "body": "also do X", "directive": true})
	if !more.Directive {
		t.Fatalf("reply with directive not marked: %+v", more)
	}
	send := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "new work"})
	if got := openIDs(t, s); !sameIDs(got, []int64{more.ID, send.ID}) {
		t.Fatalf("open %v, want %d %d (not the ack %d)", got, more.ID, send.ID, ack.ID)
	}
	// The stored flag comes back on read; a plain message has no field.
	w := callAs(s, "GET", fmt.Sprintf("/api/messages/%d", more.ID), workerToken, "team/worker", nil)
	if !strings.Contains(w.Body.String(), `"directive":true`) {
		t.Fatalf("read directive reply: %s", w.Body)
	}
	if w = callAs(s, "GET", fmt.Sprintf("/api/messages/%d", send.ID), workerToken, "team/worker", nil); strings.Contains(w.Body.String(), "directive") {
		t.Fatalf("read send: %s", w.Body)
	}
}

func TestCLIReplyDirective(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure(nil, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "report", "--body", "x")
	h.mustRun(nil, "reply", "1", "--subject", "ok", "--body", "thanks")
	h.mustRun(nil, "reply", "1", "--subject", "next", "--body", "do Y", "--directive")
	var sent []wire.Message
	if out := h.mustRun(nil, "outbox", "--json"); json.Unmarshal([]byte(out), &sent) != nil || len(sent) != 2 || !sent[0].Directive || sent[1].Directive {
		t.Fatalf("reply directive flags: %s", out)
	}
	if !strings.Contains(h.mustRun(nil, "reply", "--help"), "--directive") {
		t.Fatal("reply --help lacks --directive")
	}
}

// mixedFixture is 30 messages of directives, reports, acks and completions
// as they occur in use. Under the rule before #138 (thread, reply_to,
// task_id; every boss mail a directive) 15 stay open; under #138's rule 5;
// under the strict reply rule (#214) 11.
func TestProgressMixedFixture(t *testing.T) {
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	at := clock.now().Add(-2 * time.Hour)
	n := 0
	m := func(r flowRow) int64 {
		n++
		r.at = at.Add(time.Duration(n) * time.Minute)
		return mailAt(t, s, r)
	}
	boss := func(to, subject string) int64 {
		return m(flowRow{sender: "hq/boss", recipient: to, subject: subject})
	}
	ack := func(to string, parent int64, subject string) int64 {
		return m(flowRow{sender: "hq/boss", recipient: to, subject: subject, thread: parent, reply: parent})
	}
	d1, d2 := boss("team/worker", "build A"), boss("team/worker", "build B")                                                   // 1, 2
	r3 := m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d·#%d", d1, d2)})                  // 3
	ackA := ack("team/worker", r3, "ok")                                                                                       // 4
	c5 := m(flowRow{sender: "hq/boss", recipient: "team/worker", subject: "task C", task: "T-3"})                              // 5
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "C done", task: "T-3"})                                    // 6
	d4 := boss("team/worker", "fix D")                                                                                         // 7
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "re", thread: d4, reply: d4})                              // 8
	m(flowRow{sender: "hq/boss", recipient: "team/worker", subject: "deploy E", status: "complete"})                           // 9
	d6 := m(flowRow{sender: "hq/boss", recipient: "team/worker", subject: "read only", status: "read"})                        // 10
	d7 := boss("ops/lead", "check F")                                                                                          // 11
	r12 := m(flowRow{sender: "ops/lead", recipient: "hq/boss", subject: "checked", body: fmt.Sprintf("Re #%d", d7)})           // 12
	ackB := ack("ops/lead", r12, "thanks")                                                                                     // 13
	d8 := boss("team/worker", "write G")                                                                                       // 14
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "progress", body: fmt.Sprintf("about #%d0", d8)})          // 15
	d9 := boss("team/worker", "H")                                                                                             // 16
	m(flowRow{sender: "ops/lead", recipient: "hq/boss", subject: fmt.Sprintf("#%d", d9)})                                      // 17
	more := m(flowRow{sender: "hq/boss", recipient: "team/worker", subject: "also I", thread: r3, reply: r3, directive: true}) // 18
	d10 := boss("team/worker-2", "J")                                                                                          // 19
	r20 := m(flowRow{sender: "team/worker-2", recipient: "hq/boss", subject: "done", body: fmt.Sprintf("Re #%d", d10)})        // 20
	ackC := ack("team/worker-2", r20, "ack")                                                                                   // 21
	d11 := boss("team/worker", "K")                                                                                            // 22
	m(flowRow{sender: "team/worker", recipient: "ops/lead", subject: fmt.Sprintf("#%d cc", d11)})                              // 23
	d12 := boss("ops/lead", "L")                                                                                               // 24
	r25 := m(flowRow{sender: "ops/lead", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d, #%d", d12, d7)})                  // 25
	ackD := ack("ops/lead", r25, "ok")                                                                                         // 26
	r27 := m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "status update"})                                   // 27
	ackE := ack("team/worker", r27, "noted")                                                                                   // 28
	d13 := m(flowRow{sender: "hq/boss", recipient: "team/worker", subject: "M", status: "pending"})                            // 29
	if _, err := s.db.Exec(`INSERT INTO role_states VALUES('team/worker',?,'complete','now')`, d13); err != nil {
		t.Fatal(err)
	}
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "noise"}) // 30
	if n != 30 {
		t.Fatalf("fixture has %d messages", n)
	}
	_ = []int64{ackA, ackB, ackC, ackD, ackE} // open under the rule before #138 only
	// Under mail.Replies (#214) only the thread reply 8 answers (7) and the
	// completion closes 9 and 29: reports by task (6) or by #id (3, 12, 20,
	// 25) leave their directives open.
	if got, want := unrepliedIDs(t, s), []int64{d1, d2, c5, d6, d7, d8, d9, more, d10, d11, d12}; !sameIDs(got, want) {
		t.Fatalf("open %v (%d), want %v", got, len(got), want)
	}
}
