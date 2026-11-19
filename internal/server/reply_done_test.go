package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
)

// Replies never close a directive; reply --done does, in one step (#269).
func TestReplyDoneClosesDirectiveOnce(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "b")
	if w := call(h.s, "POST", "/api/messages", "secret-a", map[string]string{"recipient": "b", "subject": "build", "body": "x"}, ""); w.Code != 201 {
		t.Fatal(w.Body)
	}
	status := func() string {
		t.Helper()
		var st string
		if err := h.s.db.QueryRow(`SELECT COALESCE((SELECT status FROM role_states WHERE role='b' AND message_id=1),'')`).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	plain := h.run(nil, "reply", "1", "--body", "started", "--json")
	if plain.code != 0 || !strings.Contains(plain.stdout, `"directive_state": "open"`) || strings.TrimSpace(plain.stderr) != cli.ReplyOpenNote("1") {
		t.Fatalf("plain reply: %+v", plain)
	}
	if st := status(); st != "" {
		t.Fatalf("status after a plain reply %q, want none", st)
	}

	args := []string{"reply", "1", "--done", "--body", "built", "--idempotency-key", "final-1", "--json"}
	done := h.run(nil, args...)
	if done.code != 0 || !strings.Contains(done.stdout, `"directive_state": "done"`) || done.stderr != "" {
		t.Fatalf("reply --done: %+v", done)
	}
	if st := status(); st != "complete" {
		t.Fatalf("status after reply --done %q", st)
	}
	again := h.run(nil, args...)
	if again.code != 0 || again.stdout != done.stdout {
		t.Fatalf("retry: %+v\nfirst: %s", again, done.stdout)
	}
	if n := countRows(t, h.s, `SELECT COUNT(*) FROM messages WHERE sender='b'`); n != 2 {
		t.Fatalf("replies stored %d, want 2", n)
	}
	if n := countRows(t, h.s, `SELECT COUNT(*) FROM role_states WHERE message_id=1`); n != 1 {
		t.Fatalf("done marks %d, want 1", n)
	}

	help := h.mustRun(nil, "help", "reply")
	if i, j := strings.Index(help, "--done"), strings.Index(help, "--subject"); i < 0 || j < 0 || i > j {
		t.Fatalf("help reply does not lead with --done:\n%s", help)
	}
}

// A plain reply leaves the directive open, and the reminders go on: the next
// is due SWITCHBOARD_REMIND_AFTER after the reply (#269).
func TestPlainReplyKeepsReminding(t *testing.T) {
	s, clock := slotsWithRoles(t)
	remindOn(s)
	sweepOnce(t, s)
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "build"})
	readAs(t, s, "team/worker", d.ID)
	clock.add(30 * time.Minute)
	reply := apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "started"})
	if reply.DirectiveState != "open" {
		t.Fatalf("reply directive_state %q", reply.DirectiveState)
	}
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID}) {
		t.Fatalf("open %v after a plain reply", got)
	}
	if n := sweepAt(t, s, clock, reply.CreatedAt.Add(remindGap-time.Second)); n != 0 {
		t.Fatalf("sent %d before a gap after the reply", n)
	}
	if n := sweepAt(t, s, clock, reply.CreatedAt.Add(remindGap)); n != 1 {
		t.Fatalf("sent %d a gap after the reply, want the reminder", n)
	}
	if got := remindKeys(t, s); fmt.Sprint(got) != fmt.Sprintf("[team/worker remind:%d:1]", d.ID) {
		t.Fatalf("reminders %v", got)
	}
}
