package server

import (
	"fmt"
	"slices"
	"strings"
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
	want := fmt.Sprintf("answered rule: version %d stored (none before); 0 directives marked closed (no_reply=1)", mail.AnsweredRuleVersion)
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("log %q, want %q", lines, want)
	}
	if lines = startRule(t, s); len(lines) != 0 {
		t.Fatalf("second start logged %q", lines)
	}
}

// A database with mail from before the version table ran version 1 (#214):
// its first start with this code settles it as version 1 -> current, so a
// directive answered by a reply is closed and the open ones stay open, a
// mention-answered one included (version 1 never counted a mention).
func TestAnsweredRule_ExistingDatabaseWithoutVersionIsVersion1(t *testing.T) {
	s, clock := ruleServer(t)
	now := clock.now()
	open := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "open", at: now.Add(-48 * time.Hour)})
	mention := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "answered by a mention", at: now.Add(-47 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", mention), at: now.Add(-46 * time.Hour)})
	replied := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "replied", at: now.Add(-45 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "re", at: now.Add(-44 * time.Hour), thread: replied, reply: replied})

	lines := startRule(t, s)
	if v := storedRule(t, s); v != mail.AnsweredRuleVersion {
		t.Fatalf("stored version %d", v)
	}
	if got, want := fmt.Sprint(ruleOpenIDs(t, s)), fmt.Sprint([]int64{open, mention}); got != want {
		t.Fatalf("open after %v, want %v", got, want)
	}
	want := "answered rule: version 1 -> 2; 1 directives closed under version 1 but open under version 2 marked closed (no_reply=1); open directives stay open"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("log %q, want %q", lines, want)
	}
}

// Upgrade: a database at version 1 whose directives were answered under
// version 1's rule; this code runs version 2. Version 2 here is the #214
// change replayed (a reply still closes, in both): version 1 counted a plain send that mentions #id, version
// 2 (mail.Replies) does not. A directive open under both stays open (#268).
func looseReplies(d, m mail.FlowMail, base func(string) string) bool {
	return mail.Answers(d, m, base, func(string) bool { return false })
}

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
	// Open under both rules: stays open, not marked.
	stillOpen := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "unanswered", at: now.Add(-17 * time.Hour)})

	replyCloses := mail.AnsweredRules[1].Open
	s.presenceCfg.Rule = mail.AnsweredRule{Version: 2, Replies: mail.Replies, Open: replyCloses}
	s.presenceCfg.Rules = map[int]mail.AnsweredRule{1: {Version: 1, Replies: looseReplies, Open: replyCloses}, 2: s.presenceCfg.Rule}
	lines := startRule(t, s)
	if want := "answered rule: version 1 -> 2; 3 directives closed under version 1 but open under version 2 marked closed (no_reply=1); open directives stay open"; len(lines) != 1 || lines[0] != want {
		t.Fatalf("log %q, want %q", lines, want)
	}
	if v := storedRule(t, s); v != 2 {
		t.Fatalf("stored version %d", v)
	}
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{stillOpen}) {
		t.Fatalf("open after upgrade %v, want [%d]", ids, stillOpen)
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
	// The count and the IDs outlive the log line (#274).
	st, err := s.presence().RuleStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 2 || st.Last == nil || st.Last.From != 1 || st.Last.To != 2 || st.Last.Marked != 3 || fmt.Sprint(st.Last.IDs) != fmt.Sprint(byMention) || st.Last.At == "" {
		t.Fatalf("rule status %+v last %+v, want 1 -> 2 marking %v", st, st.Last, byMention)
	}
	s.admin.ProxyToken = "proxy-secret"
	if w := adminRequest(s, "GET", "/api/admin/settings", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"answered_rule":{"version":2,"last":{"from":1,"to":2,"marked":3,"ids":[`+strings.Trim(strings.Join(strings.Fields(fmt.Sprint(byMention)), ","), "[]")+`]`) {
		t.Fatalf("settings %d %s", w.Code, w.Body.String())
	}

	// Directives sent after the upgrade follow the new rule: a mention does
	// not answer, a reply does.
	clock.add(time.Minute)
	later := clock.now()
	fresh := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "new", at: later})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: fmt.Sprintf("Re #%d", fresh), at: later.Add(time.Second)})
	freshReplied := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "new replied", at: later})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "re", at: later.Add(time.Second), thread: freshReplied, reply: freshReplied})
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{stillOpen, fresh}) {
		t.Fatalf("open after new mail %v, want [%d %d]", ids, stillOpen, fresh)
	}

	// The next start at the same version marks nothing.
	if lines = startRule(t, s); len(lines) != 0 {
		t.Fatalf("restart logged %q", lines)
	}
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{stillOpen, fresh}) {
		t.Fatalf("open after restart %v", ids)
	}
}

// Version 1 -> 2 (#269), the rule this build runs: a directive answered by a
// plain reply under version 1 is closed, one with no reply stays open, a
// completed one stays done; after the cut-over a plain reply leaves a new
// directive open.
func TestAnsweredRule_Version2ClosesOnlyRepliedDirectives(t *testing.T) {
	s, clock := ruleServer(t)
	now := clock.now()
	if _, err := s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,1,'2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	replied := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "replied", at: now.Add(-30 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "started", at: now.Add(-29 * time.Hour), thread: replied, reply: replied})
	silent := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "no reply", at: now.Add(-28 * time.Hour)})
	completed := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "completed", at: now.Add(-27 * time.Hour), status: "complete"})

	lines := startRule(t, s)
	want := "answered rule: version 1 -> 2; 1 directives closed under version 1 but open under version 2 marked closed (no_reply=1); open directives stay open"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("log %q, want %q", lines, want)
	}
	state := func(id int64) string {
		t.Helper()
		var noReply bool
		var status string
		if err := s.db.QueryRow(`SELECT m.no_reply,`+mail.StatusExpr+` FROM messages m `+mail.StatusJoin+` WHERE m.id=?`, id).Scan(&noReply, &status); err != nil {
			t.Fatal(err)
		}
		if noReply || mail.IsCompleteStatus(status) {
			return "closed"
		}
		return "open"
	}
	if got := []string{state(replied), state(silent), state(completed)}; fmt.Sprint(got) != "[closed open closed]" {
		t.Fatalf("after start-up %v, want [closed open closed]", got)
	}
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{silent}) {
		t.Fatalf("open %v, want [%d]", ids, silent)
	}

	clock.add(time.Minute)
	fresh := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "new", at: clock.now()})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "started", at: clock.now().Add(time.Second), thread: fresh, reply: fresh})
	if ids := ruleOpenIDs(t, s); fmt.Sprint(ids) != fmt.Sprint([]int64{silent, fresh}) {
		t.Fatalf("open after a plain reply %v, want [%d %d]", ids, silent, fresh)
	}
	if lines = startRule(t, s); len(lines) != 0 {
		t.Fatalf("restart logged %q", lines)
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

// A stored version this build does not know (none in mail.AnsweredRules):
// every open directive is marked, as before #268.
func TestAnsweredRule_UnknownStoredVersionMarksEveryOpenDirective(t *testing.T) {
	s, clock := ruleServer(t)
	if _, err := s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,1,'2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	d := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "open", at: clock.now().Add(-time.Hour)})
	s.presenceCfg.Rule = mail.AnsweredRule{Version: 2, Replies: mail.Replies, Open: mail.OpenUntilDone}
	s.presenceCfg.Rules = map[int]mail.AnsweredRule{2: s.presenceCfg.Rule}
	if lines := startRule(t, s); len(lines) != 1 || lines[0] != "answered rule: version 1 -> 2; version 1 unknown to this build, 1 open directives marked closed (no_reply=1)" {
		t.Fatalf("log %q", lines)
	}
	if ids := ruleOpenIDs(t, s); len(ids) != 0 {
		t.Fatalf("open %v, want none (#%d marked)", ids, d)
	}
}
