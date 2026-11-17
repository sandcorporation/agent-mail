package main

import (
	"sort"
	"testing"
	"time"
)

// answeredFixture writes boss directives to team/worker (#63) and returns the
// ids that must stay open in the progress view, the ids the metrics count as
// answered, and the total number of directives.
func answeredFixture(t *testing.T, s *Server, at time.Time) (open []int64, answered int, total int) {
	t.Helper()
	s.bossRoles = map[string]bool{"hq/boss": true, "hq/chief": true}
	d := func(subject, task, status string, offset time.Duration) int64 {
		total++
		return mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker-1", subject: subject, task: task, status: status, at: at.Add(offset)})
	}
	// A thread reply from another slot of the recipient base answers.
	thread := d("thread reply", "", "read", 0)
	mailAt(t, s, flowRow{sender: "team/worker-2", recipient: "hq/boss", at: at.Add(time.Minute), thread: thread, reply: thread})
	answered++
	// A task_id report to any boss role answers, even outside the thread.
	d("task report", "T-2", "read", time.Minute)
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/chief", task: "T-2", at: at.Add(2 * time.Minute)})
	answered++
	// A reply from another base does not.
	other := d("other base", "", "read", 2*time.Minute)
	mailAt(t, s, flowRow{sender: "ops/lead", recipient: "hq/boss", at: at.Add(3 * time.Minute), thread: other, reply: other})
	// A reply dated before the directive does not.
	before := d("reply before", "", "pending", 3*time.Minute)
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", at: at.Add(2 * time.Minute), thread: before})
	// System mail does not.
	system := d("system", "T-5", "read", 4*time.Minute)
	mailAt(t, s, flowRow{sender: systemSender, recipient: "hq/boss", task: "T-5", at: at.Add(5 * time.Minute), thread: system})
	mailAt(t, s, flowRow{sender: legacySystemSender, recipient: "hq/boss", task: "T-5", at: at.Add(6 * time.Minute), thread: system})
	// A completed directive stays hidden without any reply.
	d("completed", "", "complete", 5*time.Minute)
	return []int64{other, before, system}, answered, total
}

func TestProgressAnsweredDirectivesHidden(t *testing.T) {
	s, clock := slotServer(t)
	open, _, _ := answeredFixture(t, s, clock.now().Add(-time.Hour))
	var got []int64
	for _, d := range serviceNamed(t, mustProgress(t, s), "team").OpenDirectives {
		got = append(got, d.ID)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != len(open) {
		t.Fatalf("open directives %v, want %v", got, open)
	}
	for i := range open {
		if got[i] != open[i] {
			t.Fatalf("open directives %v, want %v", got, open)
		}
	}
}

func TestMetricsFirstReplyFollowsAnsweredRule(t *testing.T) {
	s, clock := slotServer(t)
	_, answered, total := answeredFixture(t, s, clock.now().Add(-time.Hour))
	fr := mustMetrics(t, s, 7).FirstReply
	if fr.Count != total || fr.Answered != answered || fr.Unanswered != total-answered {
		t.Fatalf("first_reply %+v, want count %d answered %d", fr.latencyStats, total, answered)
	}
}

// More directives than one lookup chunk: every replied one is still found.
func TestProgressAnsweredAcrossChunks(t *testing.T) {
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	at := clock.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200; i++ {
		res, err := tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,status) VALUES('hq/boss','team/worker','d','b',?,'read')`, at)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err = tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,thread_id,reply_to,status) VALUES('team/worker','hq/boss','r','b',?,?,?,'read')`, at, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := serviceNamed(t, mustProgress(t, s), "team").OpenDirectives; len(got) != 0 {
		t.Fatalf("%d replied directives still open, first %+v", len(got), got[0])
	}
}
