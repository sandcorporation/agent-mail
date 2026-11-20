package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// scheduleClock is a server with a fake clock the test moves.
type scheduleClock struct {
	t   *testing.T
	s   *Server
	now time.Time
}

func newScheduleClock(t *testing.T) *scheduleClock {
	c := &scheduleClock{t: t, s: testServer(t), now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	c.s.clock = func() time.Time { return c.now }
	return c
}

func (c *scheduleClock) at(d time.Duration) {
	c.t.Helper()
	c.now = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC).Add(d)
	if _, err := c.s.sweep(context.Background()); err != nil {
		c.t.Fatal(err)
	}
}

// attach gives role base a live slot until the clock passes until.
func (c *scheduleClock) attach(base string, until time.Duration) {
	c.t.Helper()
	if _, err := c.s.db.Exec(`INSERT OR REPLACE INTO slots(base,number,machine,directory,registered_at,lease_until) VALUES(?,1,'partner-a','/w',?,?)`,
		base, c.now.UnixMilli(), time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC).Add(until).UnixMilli()); err != nil {
		c.t.Fatal(err)
	}
}

func createSchedule(t *testing.T, s *Server, token string, body map[string]any, idem string) (wire.Schedule, int, string) {
	t.Helper()
	if _, ok := body["machine"]; !ok {
		body["machine"] = "partner-a"
	}
	w := call(s, "POST", "/api/schedules", token, body, idem)
	var sc wire.Schedule
	if w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), &sc); err != nil {
			t.Fatal(err)
		}
	}
	return sc, w.Code, w.Body.String()
}

func scheduleMail(t *testing.T, s *Server, to string) []wire.Message {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, subject, body, priority FROM messages WHERE recipient=? AND subject LIKE '[주기%' ORDER BY id`, to)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []wire.Message
	for rows.Next() {
		var m wire.Message
		if err := rows.Scan(&m.ID, &m.Subject, &m.Body, &m.Priority); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func (c *scheduleClock) get(id int64) wire.Schedule {
	c.t.Helper()
	sc, err := c.s.jobs().GetSchedule(id)
	if err != nil {
		c.t.Fatal(err)
	}
	return sc
}

func markRead(t *testing.T, s *Server, role string, id int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO role_states(role,message_id,status,updated_at) VALUES(?,?,'read','now')`, role, id); err != nil {
		t.Fatal(err)
	}
}

// A fire while the owner is attached is one normal mail with the prompt;
// a repeated sweep at the same time sends nothing more.
func TestScheduleAttachedFire(t *testing.T) {
	c := newScheduleClock(t)
	sc, code, body := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "*/10 * * * *", "tz": "Asia/Seoul", "prompt": "check the build", "label": "build"}, "")
	if code != 201 {
		t.Fatalf("create %d %s", code, body)
	}
	if sc.NextFireAt == nil || !sc.NextFireAt.Equal(c.now.Add(10*time.Minute)) || sc.ExpiresAt.Sub(sc.CreatedAt) != wire.DefaultWatchExpiry {
		t.Fatalf("created %+v", sc)
	}
	c.attach("a", 24*time.Hour)
	c.at(5 * time.Minute)
	if m := scheduleMail(t, c.s, "a"); len(m) != 0 {
		t.Fatalf("fired early: %+v", m)
	}
	c.at(10 * time.Minute)
	c.at(10 * time.Minute)
	m := scheduleMail(t, c.s, "a")
	if len(m) != 1 || m[0].Subject != "[주기] build" || m[0].Body != "check the build" || m[0].Priority != "normal" {
		t.Fatalf("fire mail %+v", m)
	}
	got := c.get(sc.ID)
	if got.NextFireAt == nil || !got.NextFireAt.Equal(c.now.Add(10*time.Minute)) || got.Missed != 0 || got.State != wire.ScheduleActive {
		t.Fatalf("after the fire %+v", got)
	}
	// The mail is unread at the next fire: no second mail, the fire waits.
	c.at(20 * time.Minute)
	if m := scheduleMail(t, c.s, "a"); len(m) != 1 {
		t.Fatalf("second mail while the first is unread: %+v", m)
	}
	if got := c.get(sc.ID); got.Missed != 1 {
		t.Fatalf("held fire %+v", got)
	}
	markRead(t, c.s, "a", m[0].ID)
	c.at(21 * time.Minute)
	m = scheduleMail(t, c.s, "a")
	if len(m) != 2 || !strings.Contains(m[1].Body, "밀린 1회, 마지막 2026-11-20 18:20 KST") {
		t.Fatalf("after reading: %+v", m)
	}
}

// Three fires with nobody attached are recorded; the attach brings one mail
// with the prompt and the count.
func TestScheduleMissedFiresMergeOnAttach(t *testing.T) {
	c := newScheduleClock(t)
	sc, _, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 * * * *", "tz": "UTC", "prompt": "hourly look", "label": "hourly"}, "")
	for h := 1; h <= 3; h++ {
		c.at(time.Duration(h)*time.Hour + time.Minute)
	}
	if m := scheduleMail(t, c.s, "a"); len(m) != 0 {
		t.Fatalf("mail with nobody attached: %+v", m)
	}
	got := c.get(sc.ID)
	if got.Missed != 3 || got.MissedLastAt == nil || !got.MissedLastAt.Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("missed %+v", got)
	}
	c.attach("a", 24*time.Hour)
	c.at(3*time.Hour + 30*time.Minute)
	c.at(3*time.Hour + 31*time.Minute)
	m := scheduleMail(t, c.s, "a")
	if len(m) != 1 || m[0].Subject != "[주기] hourly · 밀린 3회" || m[0].Body != "hourly look\n\n밀린 3회, 마지막 2026-11-20 12:00 UTC" {
		t.Fatalf("merged mail %+v", m)
	}
	if got := c.get(sc.ID); got.Missed != 0 || got.MissedLastAt != nil {
		t.Fatalf("after delivery %+v", got)
	}
	// One sweep that finds many fires at once (the server was down) merges
	// them the same way.
	markRead(t, c.s, "a", m[0].ID)
	c.at(7*time.Hour + 5*time.Minute)
	m = scheduleMail(t, c.s, "a")
	if len(m) != 2 || !strings.Contains(m[1].Body, "밀린 4회, 마지막 2026-11-20 16:00 UTC") {
		t.Fatalf("catch-up mail %+v", m)
	}
}

// The schedule lives in the table: a new server on the same database fires
// the next one and never the same fire twice.
func TestScheduleSurvivesServerRestart(t *testing.T) {
	c := newScheduleClock(t)
	sc, _, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "*/30 * * * *", "tz": "UTC", "prompt": "half hour"}, "")
	c.attach("a", 24*time.Hour)
	c.at(30 * time.Minute)
	m := scheduleMail(t, c.s, "a")
	if len(m) != 1 {
		t.Fatalf("first fire %+v", m)
	}
	markRead(t, c.s, "a", m[0].ID)
	// The restarted server repeats the sweep of the same moment, then the next.
	c.s = &Server{db: c.s.db, tokens: c.s.tokens, clock: func() time.Time { return c.now }}
	c.at(30 * time.Minute)
	if m := scheduleMail(t, c.s, "a"); len(m) != 1 {
		t.Fatalf("fired twice across the restart: %+v", m)
	}
	c.at(60 * time.Minute)
	m = scheduleMail(t, c.s, "a")
	if len(m) != 2 || m[1].Subject != "[주기] half hour" {
		t.Fatalf("next fire after restart %+v", m)
	}
	if got := c.get(sc.ID); got.LastFireAt == nil || !got.LastFireAt.Equal(c.now) {
		t.Fatalf("last fire %+v", got)
	}
}

// A once schedule ends after its fire is delivered; missed, it waits for the
// attach.
func TestScheduleOnce(t *testing.T) {
	c := newScheduleClock(t)
	at := c.now.Add(15 * time.Minute)
	sc, code, body := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "once", "at": at, "prompt": "wake up", "label": "wake"}, "")
	if code != 201 {
		t.Fatalf("create %d %s", code, body)
	}
	missedOne, _, _ := createSchedule(t, c.s, "secret-b", map[string]any{"kind": "once", "at": at, "prompt": "b wake"}, "")
	c.attach("a", 24*time.Hour)
	c.at(16 * time.Minute)
	if m := scheduleMail(t, c.s, "a"); len(m) != 1 || m[0].Body != "wake up" {
		t.Fatalf("once mail %+v", m)
	}
	if got := c.get(sc.ID); got.State != wire.ScheduleEnded || got.NextFireAt != nil {
		t.Fatalf("once after its fire %+v", got)
	}
	if got := c.get(missedOne.ID); got.State != wire.ScheduleActive || got.Missed != 1 {
		t.Fatalf("missed once %+v", got)
	}
	c.attach("b", 24*time.Hour)
	c.at(2 * time.Hour)
	c.at(3 * time.Hour)
	if m := scheduleMail(t, c.s, "b"); len(m) != 1 || !strings.Contains(m[0].Body, "b wake\n\n밀린 1회") {
		t.Fatalf("missed once delivered %+v", m)
	}
	if got := c.get(missedOne.ID); got.State != wire.ScheduleEnded {
		t.Fatalf("missed once after delivery %+v", got)
	}
	// A time past the expiry, or long past, is refused.
	if _, code, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "once", "at": c.now.Add(8 * 24 * time.Hour), "prompt": "x"}, ""); code != 400 {
		t.Fatalf("at past the expiry: %d", code)
	}
	if _, code, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "once", "at": c.now.Add(-time.Hour), "prompt": "x"}, ""); code != 400 {
		t.Fatalf("at in the past: %d", code)
	}
}

// With no session attached the schedule expires: one mail that carries the
// missed fires. While attached the expiry moves forward instead.
func TestScheduleExpiryAndAutoExtension(t *testing.T) {
	c := newScheduleClock(t)
	lone, _, _ := createSchedule(t, c.s, "secret-b", map[string]any{"kind": "cron", "cron": "0 * * * *", "tz": "UTC", "prompt": "b hourly", "label": "bh", "expires_in": 7200}, "")
	kept, _, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 * * * *", "tz": "UTC", "prompt": "a hourly", "expires_in": 7200}, "")
	c.attach("a", 24*time.Hour)
	c.at(90 * time.Minute)
	c.at(3 * time.Hour)
	c.at(4 * time.Hour)
	got := c.get(lone.ID)
	if got.State != wire.ScheduleExpired {
		t.Fatalf("lone %+v", got)
	}
	m := scheduleMail(t, c.s, "b")
	if len(m) != 1 || m[0].Subject != "[주기 만료] bh" || !strings.Contains(m[0].Body, "밀린 2회, 마지막 2026-11-20 11:00 UTC") {
		t.Fatalf("expiry mail %+v", m)
	}
	if got := c.get(kept.ID); got.State != wire.ScheduleActive || !got.ExpiresAt.Equal(c.now.Add(2*time.Hour)) {
		t.Fatalf("attached %+v", got)
	}
}

// A no_extend schedule expires at its time although a session is attached,
// with the expiry mail; one without the flag is extended.
func TestScheduleNoExtendExpiresWhileAttached(t *testing.T) {
	c := newScheduleClock(t)
	fixed, _, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 * * * *", "tz": "UTC", "prompt": "fixed", "label": "fx", "expires_in": 7200, "no_extend": true}, "")
	moving, _, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 * * * *", "tz": "UTC", "prompt": "moving", "label": "mv", "expires_in": 7200}, "")
	if fixed.Extends || !moving.Extends {
		t.Fatalf("extends: fixed %v moving %v", fixed.Extends, moving.Extends)
	}
	c.attach("a", 24*time.Hour)
	c.at(90 * time.Minute)
	c.at(3 * time.Hour)
	got := c.get(fixed.ID)
	if got.State != wire.ScheduleExpired || got.Extends {
		t.Fatalf("fixed %+v", got)
	}
	var expiry int
	for _, m := range scheduleMail(t, c.s, "a") {
		if m.Subject == "[주기 만료] fx" {
			expiry++
		}
	}
	if expiry != 1 {
		t.Fatalf("expiry mails %d", expiry)
	}
	if got := c.get(moving.ID); got.State != wire.ScheduleActive || !got.ExpiresAt.Equal(c.now.Add(2*time.Hour)) {
		t.Fatalf("moving %+v", got)
	}
}

func TestScheduleAPI(t *testing.T) {
	c := newScheduleClock(t)
	for _, bad := range []map[string]any{
		{"kind": "cron", "cron": "* * * *", "tz": "UTC", "prompt": "x"},
		{"kind": "cron", "cron": "0 0 30 2 *", "tz": "UTC", "prompt": "x"},
		{"kind": "cron", "cron": "* * * * *", "tz": "Mars/Base", "prompt": "x"},
		{"kind": "cron", "cron": "* * * * *", "tz": "UTC", "prompt": " "},
		{"kind": "once", "prompt": "x"},
		{"kind": "watch", "prompt": "x"},
	} {
		if _, code, body := createSchedule(t, c.s, "secret-a", bad, ""); code != 400 {
			t.Errorf("%v accepted: %d %s", bad, code, body)
		}
	}
	first, code, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 9 * * 1-5", "tz": "+09:00", "prompt": strings.Repeat("long prompt ", 10)}, "k1")
	again, code2, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 9 * * 1-5", "tz": "+09:00", "prompt": "other"}, "k1")
	if code != 201 || code2 != 200 || again.ID != first.ID || len([]rune(first.Label)) != 60 {
		t.Fatalf("idem: %d %d %+v %+v", code, code2, first, again)
	}
	// 09:00 UTC on Friday 2026-11-20 is 18:00 +09:00: next is Monday 09:00 +09:00.
	if want := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC); first.NextFireAt == nil || !first.NextFireAt.Equal(want) {
		t.Fatalf("next %v want %v", first.NextFireAt, want)
	}
	if w := call(c.s, "GET", "/api/schedules/"+itoa(first.ID), "secret-b", nil, ""); w.Code != 404 {
		t.Fatalf("other role show: %d", w.Code)
	}
	if w := call(c.s, "DELETE", "/api/schedules/"+itoa(first.ID), "secret-b", nil, ""); w.Code != 404 {
		t.Fatalf("other role rm: %d", w.Code)
	}
	if w := call(c.s, "GET", "/api/schedules", "secret-b", nil, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("b list %s", w.Body.String())
	}
	w := call(c.s, "GET", "/api/schedules", "secret-a", nil, "")
	var list []wire.Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].Prompt == "" {
		t.Fatalf("a list %s", w.Body.String())
	}
	if w := call(c.s, "DELETE", "/api/schedules/"+itoa(first.ID), "secret-a", nil, ""); w.Code != 200 {
		t.Fatalf("rm %d %s", w.Code, w.Body.String())
	}
	if w := call(c.s, "GET", "/api/schedules/"+itoa(first.ID), "secret-a", nil, ""); w.Code != 404 {
		t.Fatalf("show after rm: %d", w.Code)
	}
}

func TestAdminSchedulesListAndRemove(t *testing.T) {
	c := newScheduleClock(t)
	c.s.admin.ProxyToken = "proxy-secret"
	sc, _, _ := createSchedule(t, c.s, "secret-a", map[string]any{"kind": "cron", "cron": "0 * * * *", "tz": "UTC", "prompt": "secret prompt", "label": "hourly"}, "")
	w := adminRequest(c.s, "GET", "/api/admin/schedules?state=live", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret prompt") || !strings.Contains(w.Body.String(), `"kind":"cron"`) {
		t.Fatalf("admin list %d %s", w.Code, w.Body.String())
	}
	if w := adminRequest(c.s, "DELETE", "/api/admin/schedules/"+itoa(sc.ID), "", nil); w.Code != 403 {
		t.Fatalf("admin rm without the write guard %d", w.Code)
	}
	if w := adminWrite(c.s, "DELETE", "/api/admin/schedules/"+itoa(sc.ID), ""); w.Code != 200 {
		t.Fatalf("admin rm %d %s", w.Code, w.Body.String())
	}
	if _, err := c.s.jobs().GetSchedule(sc.ID); err == nil {
		t.Fatal("still there")
	}
}
