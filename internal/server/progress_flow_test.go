package server

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// openDirectiveTasks lists "service:task" for every unreplied open
// directive with a task id that the progress view shows.
func openDirectiveTasks(t *testing.T, s *Server) []string {
	t.Helper()
	var got []string
	for _, p := range mustProgress(t, s).Services {
		for _, d := range p.OpenDirectives {
			if d.RepliedAt != nil {
				continue // answered: open until done (#269), but not waiting on a reply
			}
			var task string
			if err := s.db.QueryRow(`SELECT COALESCE(task_id,'') FROM messages WHERE id=?`, d.ID).Scan(&task); err != nil {
				t.Fatal(err)
			}
			if task != "" {
				got = append(got, p.Service+":"+task)
			}
		}
	}
	sort.Strings(got)
	return got
}

func TestProgressDirectivesFollowFlow(t *testing.T) {
	t.Parallel()
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	at := clock.now().Add(-3 * time.Hour)
	n := 0
	m := func(r flowRow) int64 {
		n++
		r.at = at.Add(time.Duration(n) * time.Minute)
		return mailAt(t, s, r)
	}
	boss := func(to, task, subject string) int64 {
		return m(flowRow{sender: "hq/boss", recipient: to, task: task, subject: subject})
	}

	// T-A: directive, then a "Re #id" report without the task id: a mention
	// is no reply (#214), waiting.
	a := boss("team/worker", "T-A", "build A")
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", a)})
	// T-B: directive the recipient only read: waiting.
	b := boss("team/worker", "T-B", "build B")
	// T-C: two directives, the first named by #id: both waiting.
	c1 := boss("team/worker", "T-C", "C part 1")
	c2 := boss("team/worker", "T-C", "C part 2")
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "part 1 done", body: fmt.Sprintf("#%d", c1)})
	// T-D: directive completed by its recipient (no answer mail): done.
	m(flowRow{sender: "hq/boss", recipient: "team/worker", task: "T-D", subject: "deploy D", status: "complete"})
	m(flowRow{sender: "team/worker", recipient: "ops/lead", task: "T-D", subject: "fyi"})
	// T-E: reports only, worker to boss (a daily report): not waiting.
	for i := 0; i < 3; i++ {
		m(flowRow{sender: "team/worker", recipient: "hq/boss", task: "T-E", subject: fmt.Sprintf("daily %d", i)})
	}
	// T-F: directive answered in its thread: done.
	f := boss("team/worker", "T-F", "fix F")
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "fixed", thread: f, reply: f})
	// T-G: a report with the task id is no reply (#214): waiting; the boss's
	// plain ack is not a directive.
	gd := boss("ops/lead", "T-G", "check G")
	g := m(flowRow{sender: "ops/lead", recipient: "hq/boss", task: "T-G", subject: "checked"})
	m(flowRow{sender: "hq/boss", recipient: "ops/lead", task: "T-G", subject: "thanks", thread: g, reply: g})
	// T-H: report, then a reply --directive asking for more, unanswered: waiting.
	h := m(flowRow{sender: "ops/lead", recipient: "hq/boss", task: "T-H", subject: "H draft"})
	hd := m(flowRow{sender: "hq/boss", recipient: "ops/lead", task: "T-H", subject: "also tests", thread: h, reply: h, directive: true})

	want := []string{"ops:T-G", "ops:T-H", "team:T-A", "team:T-B", "team:T-C", "team:T-C"}
	if got := openDirectiveTasks(t, s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("open directives %v, want %v", got, want)
	}

	// Replying to the rest closes them.
	for _, id := range []int64{a, b, c1, c2} {
		m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: "done", thread: id, reply: id})
	}
	m(flowRow{sender: "ops/lead", recipient: "hq/boss", subject: "checked", thread: gd, reply: gd})
	m(flowRow{sender: "ops/lead", recipient: "hq/boss", subject: "tests added", thread: h, reply: hd})
	if got := openDirectiveTasks(t, s); len(got) != 0 {
		t.Fatalf("open directives after answers %v", got)
	}
}

// The #139 mixed fixture: its one directive (T-3) got a task_id report,
// which is no reply (#214); a reply in its thread closes it.
func TestProgressMixedFixtureDirectives(t *testing.T) {
	t.Parallel()
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	at := clock.now().Add(-time.Hour)
	d := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "task C", task: "T-3", at: at})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "C done", task: "T-3", at: at.Add(time.Minute)})
	if got := openDirectiveTasks(t, s); strings.Join(got, ",") != "team:T-3" {
		t.Fatalf("waiting %v, want team:T-3", got)
	}
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "re", thread: d, reply: d, at: at.Add(2 * time.Minute)})
	if got := openDirectiveTasks(t, s); len(got) != 0 {
		t.Fatalf("waiting %v after the reply", got)
	}
}
