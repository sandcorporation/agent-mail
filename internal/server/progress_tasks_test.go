package server

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// waitingTasks lists "service:task" for every task the progress view shows.
func waitingTasks(t *testing.T, s *Server) []string {
	t.Helper()
	var got []string
	for _, p := range mustProgress(t, s).Services {
		for _, x := range p.OpenTasks {
			got = append(got, p.Service+":"+x.TaskID)
		}
	}
	sort.Strings(got)
	return got
}

// oldOpenTasks counts task ids under the rule before #144: any non-system
// mail with the task id whose status is not complete.
func oldOpenTasks(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT task_id) FROM messages WHERE task_id<>'' AND status NOT IN ('complete','completed') AND sender NOT LIKE 'switchboard/%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every waiting task has an open directive with its task id in the same
// service, and every open directive with a task id in the window waits there.
func checkTasksMatchDirectives(t *testing.T, s *Server) {
	t.Helper()
	r := mustProgress(t, s)
	directives := map[string]bool{}
	for _, p := range r.Services {
		for _, d := range p.OpenDirectives {
			var task string
			if err := s.db.QueryRow(`SELECT COALESCE(task_id,'') FROM messages WHERE id=?`, d.ID).Scan(&task); err != nil {
				t.Fatal(err)
			}
			if task != "" {
				directives[p.Service+":"+task] = true
			}
		}
	}
	tasks := map[string]bool{}
	for _, p := range r.Services {
		for _, x := range p.OpenTasks {
			k := p.Service + ":" + x.TaskID
			tasks[k] = true
			if !directives[k] {
				t.Errorf("task %s waits without an open directive", k)
			}
		}
	}
	for k := range directives {
		if !tasks[k] {
			t.Errorf("open directive on %s but the task does not wait", k)
		}
	}
}

func TestProgressTasksFollowFlow(t *testing.T) {
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

	// T-A: directive, then a "Re #id" report without the task id: done.
	a := boss("team/worker", "T-A", "build A")
	m(flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", a)})
	// T-B: directive the recipient only read: waiting.
	boss("team/worker", "T-B", "build B")
	// T-C: two directives, only the first answered by #id: waiting.
	c1 := boss("team/worker", "T-C", "C part 1")
	boss("team/worker", "T-C", "C part 2")
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
	// T-G: directive answered by a report with the task id; the boss's plain
	// ack is not a directive: done.
	boss("ops/lead", "T-G", "check G")
	g := m(flowRow{sender: "ops/lead", recipient: "hq/boss", task: "T-G", subject: "checked"})
	m(flowRow{sender: "hq/boss", recipient: "ops/lead", task: "T-G", subject: "thanks", thread: g, reply: g})
	// T-H: report, then a reply --directive asking for more, unanswered: waiting.
	h := m(flowRow{sender: "ops/lead", recipient: "hq/boss", task: "T-H", subject: "H draft"})
	m(flowRow{sender: "hq/boss", recipient: "ops/lead", task: "T-H", subject: "also tests", thread: h, reply: h, directive: true})

	want := []string{"ops:T-H", "team:T-B", "team:T-C"}
	if got := waitingTasks(t, s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("waiting tasks %v, want %v", got, want)
	}
	if old := oldOpenTasks(t, s); old != 8 {
		t.Fatalf("old rule open tasks %d, want 8", old)
	}
	checkTasksMatchDirectives(t, s)

	// Answering the rest closes them.
	m(flowRow{sender: "team/worker", recipient: "hq/boss", task: "T-B", subject: "B done"})
	m(flowRow{sender: "team/worker", recipient: "hq/boss", task: "T-C", subject: "C done"})
	m(flowRow{sender: "ops/lead", recipient: "hq/boss", task: "T-H", subject: "tests added"})
	if got := waitingTasks(t, s); len(got) != 0 {
		t.Fatalf("waiting tasks after answers %v", got)
	}
	checkTasksMatchDirectives(t, s)
}

// The #139 mixed fixture: its one task (T-3) was answered by a task_id report.
func TestProgressMixedFixtureTasks(t *testing.T) {
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	at := clock.now().Add(-time.Hour)
	mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "task C", task: "T-3", at: at})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "C done", task: "T-3", at: at.Add(time.Minute)})
	if old := oldOpenTasks(t, s); old != 1 {
		t.Fatalf("old rule %d", old)
	}
	if got := waitingTasks(t, s); len(got) != 0 {
		t.Fatalf("waiting %v", got)
	}
}
