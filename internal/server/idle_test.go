package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// directiveViews is what each view that reads mail.DirectiveState says about
// directive id: open in 진행 (open_directives), in 대기 as an old directive
// (open over the pending threshold) or as 멈춤 (stalled), and open in the
// report linking it.
type directiveViews struct{ progress, waitOld, waitStalled, report bool }

func viewsOf(t *testing.T, s *Server, report, id int64) directiveViews {
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
	return v
}

// 진행, 대기 and the report agree on one fixture: unanswered (open
// everywhere), answered with the session idle (closed, a 멈춤 row) and
// completed (closed, no row).
func TestDirectiveViewsAgree(t *testing.T) {
	s, clock := slotsWithRoles(t)
	s.admin.ProxyToken = "proxy-secret"
	slotDo(t, s, "register", ident("m", "/a"))
	workerSession(t, s, "busy")
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "build"})
	code, rp, raw := postReport(s, tokenFor("hq/boss"), "hq/boss", map[string]any{"title": "Release", "body": "b", "mail": []int64{d.ID}}, "")
	if code != 201 {
		t.Fatalf("report: %d %s", code, raw)
	}
	clock.add(3 * time.Hour) // over the default pending threshold (2h)
	workerSession(t, s, "busy")
	if got := viewsOf(t, s, rp.ID, d.ID); got != (directiveViews{progress: true, waitOld: true, report: true}) {
		t.Fatalf("unanswered: %+v", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "started"})
	workerSession(t, s, "idle")
	clock.add(time.Hour)
	if got := viewsOf(t, s, rp.ID, d.ID); got != (directiveViews{waitStalled: true}) {
		t.Fatalf("answered, idle: %+v", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/complete", d.ID), nil)
	if got := viewsOf(t, s, rp.ID, d.ID); got != (directiveViews{}) {
		t.Fatalf("completed: %+v", got)
	}
}

// Idle after an answer (#268): a directive answered with a plain reply but
// never completed, whose recipient session then sits idle.

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

func TestIdleAfterAnswerShowsInWaitAndTellsSenderOnce(t *testing.T) {
	s, clock := slotsWithRoles(t)
	slotDo(t, s, "register", ident("m", "/a"))
	workerSession(t, s, "busy")
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "build"})
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "started"})
	workerSession(t, s, "idle")
	idleFrom := clock.now()
	if got := openIDs(t, s); len(got) != 0 {
		t.Fatalf("open %v: the reply answers", got)
	}
	clock.add(time.Hour - time.Second)
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v before SWITCHBOARD_REMIND_IDLE", got)
	}
	clock.add(time.Second)
	r := mustProgress(t, s)
	st := serviceNamed(t, r, "team").Stalled
	if r.IdleAfterS != 3600 || len(st) != 1 || st[0].ID != d.ID || st[0].To != "team/worker" || st[0].From != "hq/boss" || st[0].IdleS != 3600 || !st[0].IdleSince.Equal(idleFrom) || fmt.Sprint(st[0].Sessions) != "[team/worker-1]" {
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
	}
	if got, want := remindKeys(t, s), []string{}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reminders %v: none for an answered directive", got)
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

// Only the recipient's newest directive counts, a closed or unanswered one
// never, and SWITCHBOARD_REMIND_IDLE=0 turns the check off.
func TestIdleAfterAnswerNewestDirectiveOnly(t *testing.T) {
	s, clock := slotsWithRoles(t)
	slotDo(t, s, "register", ident("m", "/a"))
	workerSession(t, s, "busy")
	old := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "old"})
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", old.ID), map[string]any{"subject": "re", "body": "ok"})
	newer := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "new"})
	workerSession(t, s, "idle")
	clock.add(3 * time.Hour)
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v: the newest directive is unanswered (a reminder's job)", got)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", newer.ID), map[string]any{"subject": "re", "body": "ok"})
	clock.add(3 * time.Hour)
	if got := stalledIDs(t, s); !sameIDs(got, []int64{newer.ID}) {
		t.Fatalf("stalled %v, want only #%d", got, newer.ID)
	}
	s.presenceCfg.Remind.Idle = -1
	if got, r := stalledIDs(t, s), mustProgress(t, s); len(got) != 0 || r.IdleAfterS != 0 {
		t.Fatalf("off: stalled %v, idle_after_s %d", got, r.IdleAfterS)
	}
	s.presenceCfg.Remind.Idle = 0
	apiMail(t, s, clock, "hq/boss", fmt.Sprintf("/api/messages/%d/close", newer.ID), nil)
	if got := stalledIDs(t, s); len(got) != 0 {
		t.Fatalf("stalled %v after close", got)
	}
}
