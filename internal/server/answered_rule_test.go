package server

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/mail"
)

// startRule runs the start-up step and returns the lines it logged.
func startRule(t *testing.T, s *Server) []string {
	t.Helper()
	var lines []string
	if err := s.migrateAnsweredRule(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }); err != nil {
		t.Fatal(err)
	}
	return lines
}

func storedRule(t *testing.T, s *Server) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`SELECT version FROM answered_rule WHERE id=1`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ruleOpenIDs(t *testing.T, s *Server) []int64 {
	t.Helper()
	var ids []int64
	for _, p := range mustProgress(t, s).Services {
		for _, d := range p.OpenDirectives {
			ids = append(ids, d.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func ruleServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	return s, clock
}

// A new database starts at the current version with nothing to mark.
func TestAnsweredRule_NewDatabaseStartsAtCurrentVersion(t *testing.T) {
	s, _ := ruleServer(t)
	lines := startRule(t, s)
	if v := storedRule(t, s); v != mail.AnsweredRuleVersion {
		t.Fatalf("stored version %d, want %d", v, mail.AnsweredRuleVersion)
	}
	want := fmt.Sprintf("answered rule: version %d stored (none before); 0 directives marked as needing no reply", mail.AnsweredRuleVersion)
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("log %q, want %q", lines, want)
	}
	if lines = startRule(t, s); len(lines) != 0 {
		t.Fatalf("second start logged %q", lines)
	}
}

// A database from before the version table already runs the current rule
// (version 1, #214): its first start with this code stores the version and
// leaves every directive as it is, open ones included.
func TestAnsweredRule_ExistingDatabaseWithoutVersionKeepsDirectives(t *testing.T) {
	s, clock := ruleServer(t)
	now := clock.now()
	open := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "open", at: now.Add(-48 * time.Hour)})
	mention := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "answered by a mention", at: now.Add(-47 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", mention), at: now.Add(-46 * time.Hour)})
	before := ruleOpenIDs(t, s)
	if fmt.Sprint(before) != fmt.Sprint([]int64{open, mention}) {
		t.Fatalf("open before %v", before)
	}

	lines := startRule(t, s)
	if v := storedRule(t, s); v != mail.AnsweredRuleVersion {
		t.Fatalf("stored version %d", v)
	}
	if fmt.Sprint(ruleOpenIDs(t, s)) != fmt.Sprint(before) {
		t.Fatalf("open after %v, want %v", ruleOpenIDs(t, s), before)
	}
	var marked int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE no_reply=1`).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("marked %d %v", marked, err)
	}
	if len(lines) != 1 {
		t.Fatalf("log %q", lines)
	}
}

// Upgrade: a database at version 1 whose directives were answered under
// version 1's rule; this code runs version 2. Version 2 here is the #214
// change replayed: version 1 counted a plain send that mentions #id, version
// 2 (mail.Replies) does not.
func TestAnsweredRule_UpgradeMarksDirectivesTheNewRuleOpens(t *testing.T) {
	s, clock := ruleServer(t)
	now := clock.now()
	if _, err := s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,1,'2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Answered by plain sends under version 1.
	var byMention []int64
	for i := range 3 {
		d := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "old", at: now.Add(time.Duration(-30+i) * time.Hour)})
		mailAt(t, s, flowRow{sender: "team/worker-1", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", d), at: now.Add(time.Duration(-29+i) * time.Hour)})
		byMention = append(byMention, d)
	}
	// Answered by a reply under both rules: not touched.
	replied := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "replied", at: now.Add(-20 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "re", at: now.Add(-19 * time.Hour), thread: replied, reply: replied})
	// Completed: not open, not touched.
	mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "done", at: now.Add(-18 * time.Hour), status: "complete"})

	s.presenceCfg.Rule = mail.AnsweredRule{Version: 2, Replies: mail.Replies}
	lines := startRule(t, s)
	if want := "answered rule: version 1 -> 2; 3 directives opened by the new rule marked as needing no reply"; len(lines) != 1 || lines[0] != want {
		t.Fatalf("log %q, want %q", lines, want)
	}
	if v := storedRule(t, s); v != 2 {
		t.Fatalf("stored version %d", v)
	}
	if ids := ruleOpenIDs(t, s); len(ids) != 0 {
		t.Fatalf("open after upgrade %v", ids)
	}
	var marked []int64
	rows, err := s.db.Query(`SELECT id FROM messages WHERE no_reply=1 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		marked = append(marked, id)
	}
	rows.Close()
	if fmt.Sprint(marked) != fmt.Sprint(byMention) {
		t.Fatalf("marked %v, want %v", marked, byMention)
	}

	// Directives sent after the upgrade follow the new rule: a mention does
	// not answer, a reply does.
	clock.add(time.Minute)
	later := clock.now()
	fresh := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "new", at: later})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", fresh), at: later.Add(time.Second)})
	freshReplied := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "new replied", at: later})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "re", at: later.Add(time.Second), thread: freshReplied, reply: freshReplied})
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{fresh}) {
		t.Fatalf("open after new mail %v, want [%d]", ids, fresh)
	}

	// The next start at the same version marks nothing.
	if lines = startRule(t, s); len(lines) != 0 {
		t.Fatalf("restart logged %q", lines)
	}
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{fresh}) {
		t.Fatalf("open after restart %v", ids)
	}
}

// An older build started on a database a newer one settled leaves it as is.
func TestAnsweredRule_NewerStoredVersionIsKept(t *testing.T) {
	s, clock := ruleServer(t)
	if _, err := s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,?,'2026-11-01T00:00:00Z')`, mail.AnsweredRuleVersion+1); err != nil {
		t.Fatal(err)
	}
	d := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "open", at: clock.now().Add(-time.Hour)})
	if lines := startRule(t, s); len(lines) != 0 {
		t.Fatalf("log %q", lines)
	}
	if v := storedRule(t, s); v != mail.AnsweredRuleVersion+1 {
		t.Fatalf("stored version %d", v)
	}
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{d}) {
		t.Fatalf("open %v", ids)
	}
}
