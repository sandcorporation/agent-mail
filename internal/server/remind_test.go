package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/presence"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// remindGap is SWITCHBOARD_REMIND_AFTER in these tests (T).
const remindGap = time.Hour

func remindOn(s *Server) {
	s.presenceCfg.Remind = presence.RemindConfig{On: true, After: remindGap, Max: 3}
}

// remindKeys lists the idempotency keys of the reminder mail stored so far,
// in send order, as "<recipient> <key>".
func remindKeys(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT recipient,idem FROM messages WHERE sender=? AND idem LIKE 'remind%' ORDER BY id`, identity.SystemSender)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var to, key string
		if err = rows.Scan(&to, &key); err != nil {
			t.Fatal(err)
		}
		out = append(out, to+" "+key)
	}
	return out
}

func readAs(t *testing.T, s *Server, role string, id int64) {
	t.Helper()
	if w := callAs(s, "GET", fmt.Sprintf("/api/messages/%d", id), tokenFor(role), role, nil); w.Code != 200 {
		t.Fatalf("read %d as %s: %d %s", id, role, w.Code, w.Body)
	}
}

// sweepAt moves the clock to at and sweeps, returning how many reminders
// and notices went out.
func sweepAt(t *testing.T, s *Server, clock *fakeClock, at time.Time) int {
	t.Helper()
	clock.add(at.Sub(clock.now()))
	return sweepOnce(t, s).Remind
}

func countRows(t *testing.T, s *Server, query string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Off (the default): no reminder mail, no remind_state row, however old the
// read directive.
func TestRemindOffSendsAndWritesNothing(t *testing.T) {
	s, clock := slotsWithRoles(t)
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do X"})
	readAs(t, s, "team/worker", d.ID)
	before := countRows(t, s, `SELECT COUNT(*) FROM messages`)
	for _, h := range []time.Duration{time.Hour, 3 * time.Hour, 100 * time.Hour} {
		if n := sweepAt(t, s, clock, d.CreatedAt.Add(h)); n != 0 {
			t.Fatalf("off: sweep sent %d", n)
		}
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM messages`); got != before {
		t.Fatalf("off: messages %d -> %d", before, got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM remind_state`); got != 0 {
		t.Fatalf("off: remind_state has %d rows", got)
	}
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID}) {
		t.Fatalf("off: open %v", got)
	}
}

// On: reminders at T, 3T and 7T, the sender notice at 15T, each exactly once
// across repeated sweeps and a server restart. Unread and --no-reply
// directives are never reminded.
func TestRemindScheduleOnceEach(t *testing.T) {
	s, clock := slotsWithRoles(t)
	remindOn(s)
	sweepOnce(t, s) // records the enable time
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do X", "task_id": "T-1"})
	unread := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "later"})
	fyi := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "fyi", "no_reply": true})
	if !fyi.NoReply {
		t.Fatalf("no_reply not stored: %+v", fyi)
	}
	readAs(t, s, "team/worker", d.ID)
	readAs(t, s, "team/worker", fyi.ID)
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID, unread.ID}) {
		t.Fatalf("open %v, want %d %d (not the --no-reply %d)", got, d.ID, unread.ID, fyi.ID)
	}

	at := d.CreatedAt
	worker := "team/worker " + "remind:"
	steps := []struct {
		at   time.Duration
		sent int
		want []string
	}{
		{remindGap - time.Second, 0, nil},
		{remindGap, 1, []string{fmt.Sprintf("%s%d:1", worker, d.ID)}},
		{remindGap, 0, nil},
		{3*remindGap - time.Second, 0, nil},
		{3 * remindGap, 1, []string{fmt.Sprintf("%s%d:2", worker, d.ID)}},
		{7 * remindGap, 1, []string{fmt.Sprintf("%s%d:3", worker, d.ID)}},
		{15*remindGap - time.Second, 0, nil},
		{15 * remindGap, 1, []string{fmt.Sprintf("hq/boss remind-sender:%d", d.ID)}},
		{100 * remindGap, 0, nil},
	}
	var want []string
	for i, st := range steps {
		if i == 5 {
			// A restart: a new server over the same database.
			s = &Server{db: s.db, tokens: s.tokens, clock: s.clock, bossRoles: s.bossRoles, presenceCfg: s.presenceCfg}
			if n := sweepOnce(t, s); n.Remind != 0 {
				t.Fatalf("restart re-sent %d", n.Remind)
			}
		}
		if n := sweepAt(t, s, clock, at.Add(st.at)); n != st.sent {
			t.Fatalf("step %d at %s: sent %d, want %d (%v)", i, st.at, n, st.sent, remindKeys(t, s))
		}
		want = append(want, st.want...)
		if got := remindKeys(t, s); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("step %d at %s: reminders %v, want %v", i, st.at, got, want)
		}
	}

	var first wire.Message
	for _, m := range systemMail(t, s, workerToken, "team/worker") {
		if strings.HasSuffix(m.Subject, "do X") {
			first = m
			break
		}
	}
	if first.Subject != fmt.Sprintf("[다시 알림] #%d do X", d.ID) || first.Body != fmt.Sprintf("#%d 가 끝났으면 reply --done 으로 보고하고, 아직이면 reply 로 진행을 알려라", d.ID) || first.Status != "pending" || first.TaskID != "T-1" {
		t.Fatalf("reminder %+v", first)
	}
	// Reminders are system mail: never directives themselves.
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID, unread.ID}) {
		t.Fatalf("open after reminders %v", got)
	}
}

// Complete stops the reminders; a reply (reply_to) leaves the directive open
// and only moves the next reminder to a gap after it (#269); new mail that
// only mentions #id (or shares the task) changes nothing.
func TestRemindStopsOnReplyOrComplete(t *testing.T) {
	s, clock := slotsWithRoles(t)
	remindOn(s)
	sweepOnce(t, s)
	replied := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "a"})
	done := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "b"})
	mentioned := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "c", "task_id": "T-9"})
	for _, d := range []wire.Message{replied, done, mentioned} {
		readAs(t, s, "team/worker", d.ID)
	}
	start := mentioned.CreatedAt
	if n := sweepAt(t, s, clock, start.Add(remindGap)); n != 3 {
		t.Fatalf("first round sent %d, want 3", n)
	}
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", replied.ID), map[string]any{"subject": "re", "body": "working on it"})
	apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/complete", done.ID), nil)
	apiMail(t, s, clock, "team/worker", "/api/messages", map[string]any{"recipient": "hq/boss", "subject": fmt.Sprintf("status, see #%d", mentioned.ID), "task_id": "T-9"})
	if got := openIDs(t, s); !sameIDs(got, []int64{replied.ID, mentioned.ID}) {
		t.Fatalf("open %v, want %d and %d (a reply does not close)", got, replied.ID, mentioned.ID)
	}
	if got := unrepliedIDs(t, s); !sameIDs(got, []int64{mentioned.ID}) {
		t.Fatalf("unreplied %v, want only %d (a mention is no answer)", got, mentioned.ID)
	}
	for _, h := range []time.Duration{3, 7, 15, 40} {
		sweepAt(t, s, clock, start.Add(h*remindGap))
	}
	want := []string{
		fmt.Sprintf("team/worker remind:%d:1", replied.ID),
		fmt.Sprintf("team/worker remind:%d:1", done.ID),
		fmt.Sprintf("team/worker remind:%d:1", mentioned.ID),
		fmt.Sprintf("team/worker remind:%d:2", mentioned.ID),
		fmt.Sprintf("team/worker remind:%d:2", replied.ID),
		fmt.Sprintf("team/worker remind:%d:3", mentioned.ID),
		fmt.Sprintf("team/worker remind:%d:3", replied.ID),
		fmt.Sprintf("hq/boss remind-sender:%d", mentioned.ID),
		fmt.Sprintf("hq/boss remind-sender:%d", replied.ID),
	}
	if got := remindKeys(t, s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reminders %v, want %v", got, want)
	}
}

// A directive older than the first sweep with reminders on counts from that
// time (#268): switching on sends nothing at once, the first reminder is due
// at enabled_at + T and none before; switching off and on keeps the first
// enable time.
func TestRemindOlderDirectivesCountFromEnable(t *testing.T) {
	s, clock := slotsWithRoles(t)
	old := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "old"})
	older := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "older"})
	readAs(t, s, "team/worker", old.ID)
	readAs(t, s, "team/worker", older.ID)
	clock.add(5 * remindGap) // both are long past T before reminders exist
	remindOn(s)
	if n := sweepOnce(t, s).Remind; n != 0 {
		t.Fatalf("switch-on sent %d, want no burst", n)
	}
	var enabledMS int64
	if err := s.db.QueryRow(`SELECT enabled_at FROM remind_state`).Scan(&enabledMS); err != nil {
		t.Fatal(err)
	}
	enabled := time.UnixMilli(enabledMS).UTC()
	if n := sweepAt(t, s, clock, enabled.Add(remindGap-time.Second)); n != 0 {
		t.Fatalf("sent %d before enabled_at + T", n)
	}
	s.presenceCfg.Remind.On = false
	newer := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "new"})
	readAs(t, s, "team/worker", newer.ID)
	remindOn(s)
	if n := sweepAt(t, s, clock, enabled.Add(remindGap)); n != 2 {
		t.Fatalf("at enabled_at + T sent %d, want 2", n)
	}
	var again int64
	if err := s.db.QueryRow(`SELECT enabled_at FROM remind_state`).Scan(&again); err != nil || again != enabledMS {
		t.Fatalf("enable time %d -> %d (%v)", enabledMS, again, err)
	}
	sweepAt(t, s, clock, newer.CreatedAt.Add(remindGap))
	// The existing gaps follow: the second reminder 2T after the first.
	sweepAt(t, s, clock, enabled.Add(3*remindGap-time.Second))
	want := []string{
		fmt.Sprintf("team/worker remind:%d:1", old.ID),
		fmt.Sprintf("team/worker remind:%d:1", older.ID),
		fmt.Sprintf("team/worker remind:%d:1", newer.ID),
	}
	if got := remindKeys(t, s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reminders %v, want %v", got, want)
	}
	if n := sweepAt(t, s, clock, enabled.Add(3*remindGap)); n != 2 {
		t.Fatalf("at enabled_at + 3T sent %d, want the second reminder of #%d and #%d", n, old.ID, older.ID)
	}
}

// Directives to a boss role are not reminded.
func TestRemindSkipsBossRecipients(t *testing.T) {
	s, clock := slotsWithRoles(t)
	s.bossRoles["team/worker"] = false
	s.bossRoles["ops/lead"] = true
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('ops/lead','secret-ops/lead','now')`); err != nil {
		t.Fatal(err)
	}
	remindOn(s)
	sweepOnce(t, s)
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "ops/lead", "subject": "peer"})
	readAs(t, s, "ops/lead", d.ID)
	if n := sweepAt(t, s, clock, d.CreatedAt.Add(20*remindGap)); n != 0 {
		t.Fatalf("boss recipient reminded: %v", remindKeys(t, s))
	}
}

func TestRemindSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	var c presence.Config
	if err := presence.Configure(&c, env(nil)); err != nil || c.Remind.On {
		t.Fatalf("default: %+v %v", c.Remind, err)
	}
	if err := presence.Configure(&c, env(map[string]string{"SWITCHBOARD_REMIND": "on", "SWITCHBOARD_REMIND_AFTER": "30m", "SWITCHBOARD_REMIND_MAX": "2"})); err != nil || !c.Remind.On || c.Remind.After != 30*time.Minute || c.Remind.Max != 2 {
		t.Fatalf("on: %+v %v", c.Remind, err)
	}
	for v, want := range map[string]time.Duration{"45m": 45 * time.Minute, "0": -1} {
		var c presence.Config
		if err := presence.Configure(&c, env(map[string]string{"SWITCHBOARD_REMIND_IDLE": v})); err != nil || c.Remind.Idle != want {
			t.Fatalf("idle %s: %+v %v", v, c.Remind, err)
		}
	}
	for k, v := range map[string]string{"SWITCHBOARD_REMIND": "yes", "SWITCHBOARD_REMIND_AFTER": "0s", "SWITCHBOARD_REMIND_MAX": "0", "SWITCHBOARD_REMIND_IDLE": "500ms"} {
		if err := presence.Configure(&presence.Config{}, env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

func TestCLINoReply(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure(nil, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "fyi", "--body", "x", "--no-reply")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "work", "--body", "y")
	h.mustRun(nil, "reply", "1", "--subject", "ok", "--body", "thanks", "--no-reply")
	var sent []wire.Message
	if out := h.mustRun(nil, "--profile", "a", "outbox", "--json"); json.Unmarshal([]byte(out), &sent) != nil || len(sent) != 2 || sent[0].NoReply || !sent[1].NoReply {
		t.Fatalf("send --no-reply: %s", out)
	}
	if out := h.mustRun(nil, "outbox", "--json"); json.Unmarshal([]byte(out), &sent) != nil || len(sent) != 1 || !sent[0].NoReply {
		t.Fatalf("reply --no-reply: %s", out)
	}
	for _, cmd := range []string{"send", "reply"} {
		if !strings.Contains(h.mustRun(nil, cmd, "--help"), "--no-reply") {
			t.Fatalf("%s --help lacks --no-reply", cmd)
		}
	}
}
