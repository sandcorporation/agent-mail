package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/wire"
)

func mustProgress(t *testing.T, s *Server) wire.ProgressReport {
	t.Helper()
	out, err := s.progress()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func serviceNamed(t *testing.T, r wire.ProgressReport, name string) wire.ServiceProgress {
	t.Helper()
	for _, p := range r.Services {
		if p.Service == name {
			return p
		}
	}
	t.Fatalf("no service %q in %+v", name, r.Services)
	return wire.ServiceProgress{}
}

func TestProgressGroupsByServiceAndSortsByActivity(t *testing.T) {
	s, clock := slotServer(t)
	now := clock.now()
	slotDo(t, s, "register", report("m", "/w/a", "busy", nil, ""))
	mailAt(t, s, flowRow{sender: "team/worker-1", recipient: "ops/lead", at: now.Add(-3 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/reviewer", recipient: "ops/lead", at: now.Add(-2 * time.Hour)})
	mailAt(t, s, flowRow{sender: "ops/lead", recipient: "team/worker", at: now.Add(-time.Hour)})
	mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "team/worker", at: now})
	mailAt(t, s, flowRow{sender: identity.LegacySystemSender, recipient: "team/worker", at: now})
	mailAt(t, s, flowRow{sender: "solo", recipient: "ops/lead", at: now.Add(-5 * time.Hour)})

	r := mustProgress(t, s)
	var names []string
	for _, p := range r.Services {
		names = append(names, p.Service)
	}
	if got := strings.Join(names, ","); got != "ops,team,solo" {
		t.Fatalf("services %q, want ops,team,solo (system mail is no activity)", got)
	}
	team := serviceNamed(t, r, "team")
	if team.LastActivityAt == nil || !team.LastActivityAt.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("team last activity %v", team.LastActivityAt)
	}
	if len(team.Sessions) != 1 || team.Sessions[0].Role != "team/worker-1" || team.Sessions[0].State != "busy" || team.Sessions[0].Directory != "/w/a" || !team.Sessions[0].Since.Equal(now) {
		t.Fatalf("team sessions %+v", team.Sessions)
	}
	if team.Badge != "busy" {
		t.Fatalf("badge %q", team.Badge)
	}
	if ops := serviceNamed(t, r, "ops"); len(ops.Sessions) != 0 || ops.Badge != "none" || ops.LastReport != nil {
		t.Fatalf("ops %+v", ops)
	}

	// An offline session is not listed.
	clock.add(2 * time.Minute)
	if team := serviceNamed(t, mustProgress(t, s), "team"); len(team.Sessions) != 0 || team.Badge != "none" {
		t.Fatalf("offline session listed: %+v", team)
	}
}

func TestProgressSessionSinceIsCurrentState(t *testing.T) {
	s, clock := slotServer(t)
	slotDo(t, s, "register", report("m", "/a", "idle", nil, ""))
	clock.add(20 * time.Second)
	changed := clock.now()
	slotDo(t, s, "heartbeat", report("m", "/a", "blocked:permission_prompt", nil, ""))
	clock.add(20 * time.Second)
	slotDo(t, s, "heartbeat", report("m", "/a", "blocked:permission_prompt", nil, ""))
	v := serviceNamed(t, mustProgress(t, s), "team").Sessions[0]
	if v.State != "blocked:permission_prompt" || !v.Since.Equal(changed) || !v.LastSeen.Equal(clock.now()) {
		t.Fatalf("session %+v, want since %v last_seen %v", v, changed, clock.now())
	}
}

func TestProgressLastReportOnlyToBoss(t *testing.T) {
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	now := clock.now()
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "hq/boss", subject: "done #1", at: now.Add(-3 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker-2", recipient: "hq/boss-2", subject: "done #2", at: now.Add(-2 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "ops/lead", subject: "chat", at: now.Add(-time.Hour)})
	team := serviceNamed(t, mustProgress(t, s), "team")
	lr := team.LastReport
	if lr == nil || lr.Subject != "done #2" || lr.From != "team/worker-2" || lr.To != "hq/boss-2" || !lr.At.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("last report %+v", lr)
	}
	if !team.LastActivityAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("last activity %v", team.LastActivityAt)
	}
}

func TestProgressOpenDirectives(t *testing.T) {
	s, clock := slotServer(t)
	s.bossRoles = map[string]bool{"hq/boss": true}
	s.alerts = &insight.Thresholds{Pending: 2 * time.Hour}
	now := clock.now()
	old := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "old", at: now.Add(-3 * time.Hour), status: "read"})
	fresh := mailAt(t, s, flowRow{sender: "hq/boss-2", recipient: "team/worker-2", subject: "fresh", at: now.Add(-time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "done", at: now.Add(-4 * time.Hour), status: "complete"})
	mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "done old spelling", at: now.Add(-4 * time.Hour), status: "completed"})
	// The recipient's role_states row wins over the message status.
	done := mailAt(t, s, flowRow{sender: "hq/boss", recipient: "team/worker", subject: "done by state", at: now.Add(-4 * time.Hour), status: "pending"})
	if _, err := s.db.Exec(`INSERT INTO role_states VALUES('team/worker',?,'complete','now')`, done); err != nil {
		t.Fatal(err)
	}
	mailAt(t, s, flowRow{sender: "ops/lead", recipient: "team/worker", subject: "not a boss", at: now.Add(-5 * time.Hour), status: "pending"})

	r := mustProgress(t, s)
	if r.PendingAlertS != 7200 {
		t.Fatalf("pending_alert_s %d", r.PendingAlertS)
	}
	got := serviceNamed(t, r, "team").OpenDirectives
	want := []wire.ProgressDirective{
		{ID: old, To: "team/worker", Subject: "old", Status: "read", AgeS: 3 * 3600, OverThreshold: true, Recipient: "offline"},
		{ID: fresh, To: "team/worker-2", Subject: "fresh", Status: "pending", AgeS: 3600, Recipient: "offline"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("directives\n got %+v\nwant %+v", got, want)
	}
}

func TestProgressAPI(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if w := call(s, "GET", "/api/progress", "secret-a", nil, ""); w.Code != 403 {
		t.Fatalf("non-boss: %d %s", w.Code, w.Body)
	}
	s.bossRoles = map[string]bool{"a": true}
	w := call(s, "GET", "/api/progress", "secret-a", nil, "")
	if w.Code != 200 {
		t.Fatalf("boss: %d %s", w.Code, w.Body)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"generated_at", "services", "pending_alert_s"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("missing %q: %s", k, w.Body)
		}
	}
	if w := call(s, "POST", "/api/progress", "secret-a", nil, ""); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	mailAt(t, s, flowRow{sender: "b", recipient: "a", subject: "hi", at: time.Now()})
	r := httptest.NewRequest("GET", "/api/admin/progress", nil)
	r.Header.Set("X-Switchboard-Admin", "proxy-secret")
	aw := httptest.NewRecorder()
	s.api(aw, r)
	if aw.Code != 200 {
		t.Fatalf("admin: %d %s", aw.Code, aw.Body)
	}
	for _, want := range []string{`"service":"b"`, `"badge":"none"`, `"sessions":[]`, `"last_report":{"id":1,"from":"b","to":"a","subject":"hi"`, `"open_directives":[]`} {
		if !strings.Contains(aw.Body.String(), want) {
			t.Fatalf("admin missing %s: %s", want, aw.Body)
		}
	}
	if strings.Contains(aw.Body.String(), `"open_tasks"`) {
		t.Fatalf("admin still has open_tasks: %s", aw.Body)
	}
	r = httptest.NewRequest("GET", "/api/admin/progress", nil)
	aw = httptest.NewRecorder()
	s.api(aw, r)
	if aw.Code != 401 {
		t.Fatalf("admin without secret: %d", aw.Code)
	}
}

func TestAdminMessagesTaskAndIDFilter(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	now := time.Now()
	a := mailAt(t, s, flowRow{sender: "a", recipient: "b", task: "T-1", at: now})
	mailAt(t, s, flowRow{sender: "a", recipient: "b", task: "T-10", at: now})
	b := mailAt(t, s, flowRow{sender: "b", recipient: "a", task: "T-1", at: now})
	get := func(q string) []wire.Message {
		r := httptest.NewRequest("GET", "/api/admin/messages?"+q, nil)
		r.Header.Set("X-Switchboard-Admin", "proxy-secret")
		w := httptest.NewRecorder()
		s.api(w, r)
		var out []wire.Message
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v %s", q, err, w.Body)
		}
		return out
	}
	if got := get("task=T-1"); len(got) != 2 || got[0].ID != b || got[1].ID != a {
		t.Fatalf("task=T-1 (exact): %+v", got)
	}
	if got := get(fmt.Sprintf("id=%d", a)); len(got) != 1 || got[0].ID != a {
		t.Fatalf("id: %+v", got)
	}
}

func TestProgressDirectivesIgnoreSystemMail(t *testing.T) {
	s, clock := slotServer(t)
	now := clock.now()
	// System mail (old name and numbered form) is never a directive.
	mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "sandcorp/boss", task: "alert:stall:team/worker:2026-11-17", at: now.Add(-time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.SystemSender + "-2", recipient: "sandcorp/boss", task: "q_abc", at: now.Add(-time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.LegacySystemSender, recipient: "sandcorp/boss", task: "alert:pending:x", at: now.Add(-time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.LegacySystemSender + "-3", recipient: "sandcorp/boss", task: "q_old", at: now.Add(-time.Hour), status: "pending"})
	// A real directive stays open; the system mail on its task id does not
	// answer it.
	s.bossRoles = map[string]bool{"sandcorp/boss": true}
	mailAt(t, s, flowRow{sender: "sandcorp/boss", recipient: "team/worker", task: "REAL", at: now.Add(-2 * time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "sandcorp/boss", task: "REAL", at: now.Add(-time.Minute), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.LegacySystemSender, recipient: "team/worker", task: "REAL", at: now.Add(-time.Minute), status: "pending"})

	if got := openDirectiveTasks(t, s); strings.Join(got, ",") != "team:REAL" {
		t.Fatalf("open directives %v", got)
	}
}
