package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// directiveViews is what each view that reads the answered rule's Open says
// about directive id: open in 진행 (open_directives), in 대기 as an old
// directive (open over the pending threshold) or as 멈춤 (stalled), open in
// the report linking it, and reminded by the next due sweep.
type directiveViews struct{ progress, waitOld, waitStalled, report, reminded bool }

func viewsOf(t *testing.T, s *Server, clock *fakeClock, report, id int64) directiveViews {
	t.Helper()
	var v directiveViews
	for _, p := range mustProgress(t, s).Services {
		for _, d := range p.OpenDirectives {
			if d.ID == id {
				v.progress, v.waitOld = true, d.OverThreshold
			}
		}
		for _, d := range p.Stalled {
			if d.ID == id {
				v.waitStalled = true
			}
		}
	}
	for _, th := range reportDetail(t, s, report).Threads {
		if th.Directive.ID == id {
			v.report = th.Open
		}
	}
	// The reminder sweep, far enough on that any reminder is due.
	before := len(remindKeys(t, s))
	sweepAt(t, s, clock, clock.now().Add(8*remindGap))
	workerSession(t, s, "idle")
	v.reminded = len(remindKeys(t, s)) > before
	return v
}

// 진행, 대기, the reminder sweep and the report agree on one fixture (#269):
// unanswered (open everywhere), a plain reply with the session idle (still
// open everywhere, and a 멈춤 row) and reply --done (closed everywhere).
func TestDirectiveViewsAgree(t *testing.T) {
	s, clock := slotsWithRoles(t)
	s.admin.ProxyToken = "proxy-secret"
	slotDo(t, s, "register", ident("m", "/a"))
	workerSession(t, s, "busy")
	remindOn(s)
	sweepOnce(t, s) // reminders on from here
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "build"})
	readAs(t, s, "team/worker", d.ID)
	code, rp, raw := postReport(s, tokenFor("hq/boss"), "hq/boss", map[string]any{"title": "Release", "body": "b", "mail": []int64{d.ID}}, "")
	if code != 201 {
		t.Fatalf("report: %d %s", code, raw)
	}
	clock.add(3 * time.Hour) // over the default pending threshold (2h)
	workerSession(t, s, "busy")
	if got := viewsOf(t, s, clock, rp.ID, d.ID); got != (directiveViews{progress: true, waitOld: true, report: true, reminded: true}) {
		t.Fatalf("unanswered: %+v", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "started"})
	workerSession(t, s, "idle")
	clock.add(time.Hour)
	if got := viewsOf(t, s, clock, rp.ID, d.ID); got != (directiveViews{progress: true, waitOld: true, waitStalled: true, report: true, reminded: true}) {
		t.Fatalf("plain reply, idle: %+v", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "built", "done": true})
	if got := viewsOf(t, s, clock, rp.ID, d.ID); got != (directiveViews{}) {
		t.Fatalf("reply --done: %+v", got)
	}
}

// Idle open directives (#268, #269): an open directive (a plain reply leaves
// it open) whose recipient session sits idle.

// workerSession registers team/worker's slot 1 (the team/worker mailbox)
// with a lease long enough for the test and reports state.
func workerSession(t *testing.T, s *Server, state string) {
	t.Helper()
	s.presenceCfg.SlotLease = 24 * time.Hour
	slotDo(t, s, "heartbeat", report("m", "/a", state, nil, ""))
}

func stalledIDs(t *testing.T, s *Server) []int64 {
	t.Helper()
	got := []int64{}
	for _, p := range mustProgress(t, s).Services {
		for _, d := range p.Stalled {
			got = append(got, d.ID)
		}
	}
	return got
}

func TestIdleOpenShowsInWaitAndTellsSender(t *testing.T) {
	s, clock := slotsWithRoles(t)
	slotDo(t, s, "register", ident("m", "/a"))
	workerSession(t, s, "busy")
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "build"})
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "started"})
	workerSession(t, s, "idle")
	idleFrom := clock.now()
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID}) {
		t.Fatalf("open %v: a plain reply leaves it open", got)
	}
	clock.add(time.Hour - time.Second)
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v before SWITCHBOARD_REMIND_IDLE", got)
	}
	clock.add(time.Second)
	r := mustProgress(t, s)
	st := serviceNamed(t, r, "team").Stalled
	if r.IdleAfterS != 3600 || len(st) != 1 || st[0].ID != d.ID || st[0].To != "team/worker" || st[0].From != "hq/boss" || st[0].IdleS != 3600 || !st[0].IdleSince.Equal(idleFrom) || st[0].AnsweredAt == nil || fmt.Sprint(st[0].Sessions) != "[team/worker-1]" {
		t.Fatalf("stalled %+v (idle_after_s %d)", st, r.IdleAfterS)
	}

	// Reminders off: the 대기 row only. Switching on sends nothing at once,
	// then one notice to the sender SWITCHBOARD_REMIND_IDLE later.
	if n := sweepOnce(t, s).Remind; n != 0 {
		t.Fatalf("off: sent %d", n)
	}
	remindOn(s)
	if n := sweepOnce(t, s).Remind; n != 0 {
		t.Fatalf("switch-on sent %d", n)
	}
	workerSession(t, s, "idle")
	if n := sweepAt(t, s, clock, clock.now().Add(time.Hour)); n != 1 {
		t.Fatalf("sent %d, want the sender notice", n)
	}
	for range 3 {
		sweepAt(t, s, clock, clock.now().Add(time.Hour))
		workerSession(t, s, "idle")
	}
	var notices []wire.Message
	for _, m := range systemMail(t, s, "secret-hq/boss", "hq/boss") {
		if strings.HasPrefix(m.Subject, "[멈춤]") {
			notices = append(notices, m)
		}
	}
	if len(notices) != 1 || notices[0].Subject != fmt.Sprintf("[멈춤] #%d build", d.ID) || !strings.Contains(notices[0].Body, "team/worker-1") {
		t.Fatalf("notices %+v", notices)
	}

	// SWITCHBOARD_REMIND_IDLE_MAX=2: one more notice, a threshold later.
	s.presenceCfg.Remind.IdleMax = 2
	sweepAt(t, s, clock, clock.now().Add(time.Hour))
	workerSession(t, s, "idle")
	sweepAt(t, s, clock, clock.now().Add(time.Hour))
	if got := idleKeys(t, s); fmt.Sprint(got) != fmt.Sprintf("[idle-after:%d idle-after:%d:2]", d.ID, d.ID) {
		t.Fatalf("idle notice keys %v", got)
	}

	// Busy again: no longer stalled. complete ends it for good.
	workerSession(t, s, "busy")
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v while busy", got)
	}
	workerSession(t, s, "idle")
	clock.add(2 * time.Hour)
	if got := stalledIDs(t, s); !sameIDs(got, []int64{d.ID}) {
		t.Fatalf("stalled %v after idle again", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/complete", d.ID), nil)
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v after complete", got)
	}
}

// idleKeys lists the idle notice keys stored so far, in send order.
func idleKeys(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT idem FROM messages WHERE idem LIKE 'idle-after:%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err = rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// Only the recipient's newest open directive counts, replied to or not, a
// closed one never, and SWITCHBOARD_REMIND_IDLE=0 turns the check off.
func TestIdleOpenNewestDirectiveOnly(t *testing.T) {
	s, clock := slotsWithRoles(t)
	slotDo(t, s, "register", ident("m", "/a"))
	workerSession(t, s, "busy")
	old := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "old"})
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", old.ID), map[string]any{"subject": "re", "body": "ok"})
	newer := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "new"})
	workerSession(t, s, "idle")
	clock.add(3 * time.Hour)
	if got := stalledIDs(t, s); !sameIDs(got, []int64{newer.ID}) {
		t.Fatalf("stalled %v, want only the newest #%d", got, newer.ID)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", newer.ID), map[string]any{"subject": "re", "body": "ok"})
	clock.add(3 * time.Hour)
	if got := stalledIDs(t, s); !sameIDs(got, []int64{newer.ID}) {
		t.Fatalf("stalled %v after a plain reply, want only #%d", got, newer.ID)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/complete", newer.ID), nil)
	if got := stalledIDs(t, s); !sameIDs(got, []int64{old.ID}) {
		t.Fatalf("stalled %v after #%d is done, want the open #%d", got, newer.ID, old.ID)
	}
	s.presenceCfg.Remind.Idle = -1
	if got, r := stalledIDs(t, s), mustProgress(t, s); len(got) != 0 || r.IdleAfterS != 0 {
		t.Fatalf("off: stalled %v, idle_after_s %d", got, r.IdleAfterS)
	}
	s.presenceCfg.Remind.Idle = 0
	apiMail(t, s, clock, "hq/boss", fmt.Sprintf("/api/messages/%d/close", old.ID), nil)
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v after close", got)
	}
}
