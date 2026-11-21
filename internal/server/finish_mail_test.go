package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// stateOf is role's role_states status for message id, "" when none.
func stateOf(t *testing.T, s *Server, role string, id int64) string {
	t.Helper()
	var st string
	if err := s.db.QueryRow(`SELECT COALESCE((SELECT status FROM role_states WHERE role=? AND message_id=?),'')`, role, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// reply --complete is --done under another name, and it completes any mail
// it answers, not only a directive (#327).
func TestReplyCompleteOnNonDirective(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure([]string{"--profile", "b"}, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "build", "--body", "x") // #1
	h.mustRun(nil, "--profile", "b", "reply", "1", "--body", "built")                          // #2: a report, not a directive

	r := h.run(nil, "--profile", "a", "reply", "2", "--complete", "--body", "thanks", "--json") // #3
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("reply --complete: %+v", r)
	}
	if st := stateOf(t, h.s, "a", 2); st != "complete" {
		t.Fatalf("report #2 for a: %q, want complete", st)
	}
	if st := stateOf(t, h.s, "b", 1); st != "" {
		t.Fatalf("directive #1 for b: %q, want untouched", st)
	}
	help := h.mustRun(nil, "help", "reply")
	if !strings.Contains(help, "--complete") || !strings.Contains(help, "the reply is the final word on ID: also mark ID complete") {
		t.Fatalf("help reply:\n%s", help)
	}
}

// send --complete completes mail the sender received, in the send's
// transaction; mail it did not receive or that does not exist refuses the
// whole send; a retry with the same key marks nothing twice (#327).
func TestSendCompleteMarksReceivedMail(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure([]string{"--profile", "b"}, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "one", "--body", "x") // #1
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "two", "--body", "x") // #2
	h.mustRun(nil, "--profile", "b", "send", "--to", "a", "--subject", "own", "--body", "x") // #3: b sent it

	for _, c := range []struct {
		id   string
		code string
	}{{"3", "400"}, {"999", "404"}} {
		r := h.run(nil, "--profile", "b", "send", "--to", "a", "--subject", "done", "--body", "y", "--complete", "1", "--complete", c.id)
		if r.code == 0 || !strings.Contains(r.stderr+r.stdout, c.code) {
			t.Fatalf("send --complete %s: %+v", c.id, r)
		}
	}
	if n := countRows(t, h.s, `SELECT COUNT(*) FROM messages`); n != 3 {
		t.Fatalf("messages %d after refused sends, want 3", n)
	}
	if n := countRows(t, h.s, `SELECT COUNT(*) FROM role_states`); n != 0 {
		t.Fatalf("marks %d after refused sends, want 0", n)
	}

	args := []string{"--profile", "b", "send", "--to", "a", "--subject", "done", "--body", "y", "--complete", "1,2", "--idempotency-key", "k1", "--json"}
	first := h.run(nil, args...)
	if first.code != 0 {
		t.Fatalf("send --complete: %+v", first)
	}
	if stateOf(t, h.s, "b", 1) != "complete" || stateOf(t, h.s, "b", 2) != "complete" {
		t.Fatal("received mail not complete")
	}
	h.mustRun(nil, "--profile", "b", "ack", "2") // a later state change the retry must not undo
	again := h.run(nil, args...)
	if again.code != 0 || again.stdout != first.stdout {
		t.Fatalf("retry: %+v\nfirst: %s", again, first.stdout)
	}
	if n := countRows(t, h.s, `SELECT COUNT(*) FROM messages WHERE subject='done'`); n != 1 {
		t.Fatalf("sends stored %d, want 1", n)
	}
	if st := stateOf(t, h.s, "b", 2); st != "acknowledged" {
		t.Fatalf("retry marked #2 again: %q", st)
	}
}

// report --mail completes the linked mail the boss received and only links
// the rest; a retry with the same key marks nothing twice (#327).
func TestReportMailCompletesReceivedMail(t *testing.T) {
	t.Parallel()
	s := reportServer(t)
	d := directive(t, s, "build")
	r := workerReply(t, s, d, "built")
	body := map[string]any{"title": "R", "body": "b", "mail": []int64{r, d}}
	code, rp, raw := postReport(s, reportBossToken, reportBossRole, body, "rk")
	if code != 201 || len(rp.Mail) != 1 || rp.Mail[0] != d {
		t.Fatalf("report: %d %s", code, raw)
	}
	if st := stateOf(t, s, reportBossRole, r); st != "complete" {
		t.Fatalf("received reply: %q, want complete", st)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM role_states WHERE message_id=`+itoa(d)); n != 0 {
		t.Fatalf("directive the boss sent got %d marks, want 0", n)
	}
	if w := callAs(s, "POST", "/api/messages/"+itoa(r)+"/ack", reportBossToken, reportBossRole, nil); w.Code != 200 {
		t.Fatalf("ack: %d %s", w.Code, w.Body)
	}
	body = map[string]any{"title": "R", "body": "b", "mail": []int64{r, d}}
	if code, again, _ := postReport(s, reportBossToken, reportBossRole, body, "rk"); code != 200 || again.ID != rp.ID {
		t.Fatalf("retry: %d %+v", code, again)
	}
	if st := stateOf(t, s, reportBossRole, r); st != "acknowledged" {
		t.Fatalf("retry marked again: %q", st)
	}
}

// close with an answer's ID closes the directive it answers and completes
// the answer for the caller, in one step; other mail stays 400 (#327).
func TestCloseByAnswerClosesDirectiveAndCompletesAnswer(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure([]string{"--profile", "b"}, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "build", "--body", "x") // #1
	h.mustRun(nil, "--profile", "b", "reply", "1", "--body", "built")                          // #2
	h.mustRun(nil, "--profile", "a", "reply", "2", "--body", "ok")                             // #3: answers a plain reply

	h.configure(nil, "a")
	text := h.mustRun(nil, "read", "2")
	if lines := strings.Split(text, "\n"); len(lines) < 3 || lines[2] != cli.DirectiveOpenNote(1, 2) {
		t.Fatalf("read of the answer:\n%s", text)
	}
	out := h.mustRun(nil, "close", "2")
	if out != "#1 closed; #2 complete" {
		t.Fatalf("close 2: %q", out)
	}
	var noReply bool
	if err := h.s.db.QueryRow(`SELECT no_reply FROM messages WHERE id=1`).Scan(&noReply); err != nil || !noReply {
		t.Fatalf("directive #1 no_reply=%v %v", noReply, err)
	}
	if st := stateOf(t, h.s, "a", 2); st != "complete" {
		t.Fatalf("answer #2 for a: %q", st)
	}
	var c wire.Closed
	if err := json.Unmarshal([]byte(h.mustRun(nil, "close", "2", "--json")), &c); err != nil || c != (wire.Closed{ID: 1, Closed: true, Completed: 2}) {
		t.Fatalf("close again --json: %+v %v", c, err)
	}
	if n := countRows(t, h.s, `SELECT COUNT(*) FROM role_states`); n != 1 {
		t.Fatalf("marks %d, want 1", n)
	}
	// A directive ID still closes as before.
	if out := h.mustRun(nil, "close", "1"); out != "#1 closed" {
		t.Fatalf("close 1: %q", out)
	}
	// #3 answers #2, which is not a directive: refused as before, 403 for
	// its recipient (not the sender), 400 for its sender.
	for prof, want := range map[string]string{"b": "403", "a": "not a directive"} {
		if r := h.run(nil, "--profile", prof, "close", "3"); r.code == 0 || !strings.Contains(r.stderr, want) {
			t.Fatalf("close 3 by %s: %+v", prof, r)
		}
	}
	if st := stateOf(t, h.s, "b", 3); st != "" {
		t.Fatalf("refused close marked #3: %q", st)
	}
}

// read tells the recipient how to finish the mail, on stderr, until it is
// complete; the reply note names the new form (#327).
func TestReadTailNamesFinishCommands(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure(nil, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "build", "--body", "x") // #1

	r := h.run(nil, "read", "1")
	if r.code != 0 || strings.TrimSpace(r.stderr) != cli.ReadNote(1) {
		t.Fatalf("read: %+v", r)
	}
	if note := cli.ReadNote(1); !strings.Contains(note, "reply 1 --complete") || !strings.Contains(note, "complete 1") {
		t.Fatalf("read note %q", note)
	}
	if r := h.run(nil, "read", "1", "--json"); r.stderr != "" {
		t.Fatalf("read --json stderr %q", r.stderr)
	}
	if r := h.run(nil, "--profile", "a", "read", "1"); r.stderr != "" {
		t.Fatalf("sender read stderr %q", r.stderr)
	}
	if note := cli.ReplyOpenNote("1"); !strings.Contains(note, "complete 1") || !strings.Contains(note, "--complete") {
		t.Fatalf("reply note %q", note)
	}
	h.mustRun(nil, "complete", "1")
	if r := h.run(nil, "read", "1"); r.stderr != "" {
		t.Fatalf("read after complete stderr %q", r.stderr)
	}
}
