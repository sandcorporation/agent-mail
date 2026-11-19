package mail

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
)

// goldenFingerprints pins what the answered rule decides on goldenCases, per
// AnsweredRuleVersion. Never change a past entry: a rule change adds the next
// version and its fingerprint (see TestAnsweredRuleGolden).
var goldenFingerprints = map[int]string{
	1: "11ba0e5b476d42f9", // #214: reply_to or the thread only (Replies); #id and task_id for Answers
}

var goldenNumber = regexp.MustCompile(`-[0-9]+$`)

func goldenBase(name string) string { return goldenNumber.ReplaceAllString(name, "") }

func goldenIsBoss(name string) bool {
	b := goldenBase(name)
	return b == "hq/boss" || b == "ops/boss"
}

// goldenCase: does m answer the directive d under Replies (strict) and
// Answers (loose)?
type goldenCase struct {
	name             string
	d, m             FlowMail
	replies, answers bool
}

func goldenCases() []goldenCase {
	t0 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	d := FlowMail{ID: 10, Sender: "hq/boss", Recipient: "team/worker", Subject: "build", Created: t0, TaskID: "task-1"}
	inThread := FlowMail{ID: 20, Sender: "hq/boss", Recipient: "team/worker", Subject: "next", Created: t0, ThreadID: 5, ReplyTo: 5, Directive: true}
	at := t0.Add(time.Minute)
	m := func(id int64, sender, recipient, subject string, thread, reply int64, task string) FlowMail {
		return FlowMail{ID: id, Sender: sender, Recipient: recipient, Subject: subject, Body: "b", Created: at, ThreadID: thread, ReplyTo: reply, TaskID: task}
	}
	earlier := m(11, "team/worker", "hq/boss", "re", 10, 10, "")
	earlier.Created = t0.Add(-time.Minute)
	return []goldenCase{
		{"reply_to the directive", d, m(11, "team/worker", "hq/boss", "re", 10, 10, ""), true, true},
		{"reply in its thread to another message", d, m(11, "team/worker", "hq/boss", "re", 10, 12, ""), true, true},
		{"numbered roles of both bases", d, m(11, "team/worker-2", "hq/boss-1", "re", 10, 10, ""), true, true},
		{"directive inside a thread, reply in that thread", inThread, m(21, "team/worker", "hq/boss", "re", 5, 5, ""), true, true},
		{"send mentioning #id to the sender", d, m(11, "team/worker", "hq/boss", "Re #10", 0, 0, ""), false, true},
		{"send mentioning #id to another boss", d, m(11, "team/worker", "ops/boss", "Re #10", 0, 0, ""), false, true},
		{"send mentioning #id to a non-boss", d, m(11, "team/worker", "ops/lead", "Re #10", 0, 0, ""), false, false},
		{"send with the directive's task to the sender", d, m(11, "team/worker", "hq/boss", "done", 0, 0, "task-1"), false, true},
		{"send with another task", d, m(11, "team/worker", "hq/boss", "done", 0, 0, "task-2"), false, false},
		{"mention of a longer id", d, m(11, "team/worker", "hq/boss", "Re #100 and #10a", 0, 0, ""), false, false},
		{"reply from another base", d, m(11, "team/reviewer", "hq/boss", "re", 10, 10, ""), false, false},
		{"reply to another base", d, m(11, "team/worker", "ops/lead", "re", 10, 10, ""), false, false},
		{"reply sent before the directive", d, earlier, false, false},
		{"system mail in the thread", d, m(11, identity.SystemSender, "hq/boss", "re", 10, 10, ""), false, false},
		{"the directive itself", d, d, false, false},
	}
}

// goldenFingerprint hashes what the rule as built decides on goldenCases.
func goldenFingerprint() string {
	h := sha256.New()
	for _, c := range goldenCases() {
		fmt.Fprintf(h, "%s|%t|%t\n", c.name, Replies(c.d, c.m, goldenBase), Answers(c.d, c.m, goldenBase, goldenIsBoss))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// TestAnsweredRuleGolden pins the answered rule (Answers, Replies) to
// AnsweredRuleVersion. A change to what either decides fails it until the
// version is bumped: add the cases, add the new version's fingerprint to
// goldenFingerprints and a line to the release notes. CI and release run it
// as their own step (scripts/ci/answered_golden.sh).
func TestAnsweredRuleGolden(t *testing.T) {
	var wrong []string
	for _, c := range goldenCases() {
		r, a := Replies(c.d, c.m, goldenBase), Answers(c.d, c.m, goldenBase, goldenIsBoss)
		if r != c.replies || a != c.answers {
			wrong = append(wrong, fmt.Sprintf("  %s: Replies %t Answers %t, table says %t %t", c.name, r, a, c.replies, c.answers))
		}
	}
	for v := 1; v <= AnsweredRuleVersion; v++ {
		if goldenFingerprints[v] == "" {
			t.Errorf("goldenFingerprints has no entry for version %d", v)
		}
	}
	got, want := goldenFingerprint(), goldenFingerprints[AnsweredRuleVersion]
	if len(wrong) == 0 && got == want {
		return
	}
	t.Fatalf("the answered rule no longer decides as version %d does (fingerprint %s, version %d is %s):\n%s\n"+
		"A rule change reopens or closes stored directives. Bump mail.AnsweredRuleVersion to %d, update the cases, "+
		"add goldenFingerprints[%d] = %q and a line in the release notes; start-up then marks the directives the new rule opens (presence.MigrateAnsweredRule).",
		AnsweredRuleVersion, got, AnsweredRuleVersion, want, strings.Join(wrong, "\n"), AnsweredRuleVersion+1, AnsweredRuleVersion+1, got)
}
