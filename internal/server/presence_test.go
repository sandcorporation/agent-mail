package server

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/presence"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// presenceServer is slotServer with the lease derived from presence settings:
// 10s heartbeats, 3 may be missed, so the lease (and offline) is 30s.
func presenceServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := slotServer(t)
	s.presenceCfg.SlotLease = 0
	s.presenceCfg.Interval = 10 * time.Second
	s.presenceCfg.Missed = 3
	return s, clock
}

func int64p(v int64) *int64 { return &v }

func report(machine, dir, state string, tokens *int64, last string) wire.SlotRequest {
	return wire.SlotRequest{Machine: machine, Directory: dir, State: state, ContextTokens: tokens, LastTurnAt: last}
}

// presenceAs reads GET /api/presence?role=filter as role a.
func presenceAs(t *testing.T, s *Server, filter string) (wire.PresenceList, string) {
	t.Helper()
	path := "/api/presence"
	if filter != "" {
		path += "?role=" + filter
	}
	w := callAs(s, "GET", path, "secret-a", "a", nil)
	if w.Code != 200 {
		t.Fatalf("presence %q: %d %s", filter, w.Code, w.Body)
	}
	var out wire.PresenceList
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out, w.Body.String()
}

func onlySession(t *testing.T, s *Server, filter string) wire.PresenceView {
	t.Helper()
	out, _ := presenceAs(t, s, filter)
	if len(out.Sessions) != 1 {
		t.Fatalf("presence %q: want one session, got %+v", filter, out.Sessions)
	}
	return out.Sessions[0]
}

func TestPresenceReportAndQuery(t *testing.T) {
	s, clock := presenceServer(t)
	slotDo(t, s, "register", wire.SlotRequest{Machine: "m", Directory: "/a", SessionID: "sess-1", Label: "issue-21", Parent: "team/lead", State: "idle"})
	clock.add(5 * time.Second)
	slotDo(t, s, "heartbeat", wire.SlotRequest{Machine: "m", Directory: "/a", SessionID: "sess-1", Label: "issue-21", Parent: "team/lead",
		State: "busy", ContextTokens: int64p(12345), LastTurnAt: "2026-11-17T08:59:00+09:00"})
	clock.add(4 * time.Second)

	out, body := presenceAs(t, s, "team/worker")
	if out.IntervalSeconds != 10 || out.Missed != 3 || out.LeaseSeconds != 30 || out.RetainSeconds != 86400 || !out.Now.Equal(clock.now()) {
		t.Fatalf("presence settings: %+v", out)
	}
	v := out.Sessions[0]
	if len(out.Sessions) != 1 || v.Role != "team/worker-1" || !v.Online || v.Status != "busy" || v.State != "busy" ||
		v.ContextTokens == nil || *v.ContextTokens != 12345 || v.LastTurnAt == nil || !v.LastTurnAt.Equal(time.Date(2026, 11, 16, 23, 59, 0, 0, time.UTC)) ||
		v.Machine != "m" || v.Directory != "/a" || v.SessionID != "sess-1" || v.Label != "issue-21" || v.Parent != "team/lead" ||
		v.ReportedAt == nil || clock.now().Sub(*v.ReportedAt) != 4*time.Second || v.OfflineSince != nil {
		t.Fatalf("session: %+v", out.Sessions)
	}
	if strings.Contains(body, workerToken) || strings.Contains(body, "secret-a") || strings.Contains(body, "token\"") {
		t.Fatalf("presence must carry no secrets: %s", body)
	}

	// A heartbeat without presence fields keeps what was reported.
	slotDo(t, s, "heartbeat", ident("m", "/a"))
	if v = onlySession(t, s, "team/worker-1"); v.Status != "busy" || *v.ContextTokens != 12345 || v.LastTurnAt == nil {
		t.Fatalf("plain heartbeat changed presence: %+v", v)
	}
	// A new register starts from what it sends.
	slotDo(t, s, "register", ident("m", "/a"))
	if v = onlySession(t, s, "team/worker-1"); v.Status != "unknown" || v.State != "" || v.ContextTokens != nil || v.LastTurnAt != nil {
		t.Fatalf("register kept old presence: %+v", v)
	}

	// Filters: base, numbered role, prefix; -2 is empty until registered.
	slotDo(t, s, "register", report("m", "/b", "compacting", int64p(0), ""))
	for filter, want := range map[string]string{"": "team/worker-1,team/worker-2", "team/worker": "team/worker-1,team/worker-2",
		"team/worker-2": "team/worker-2", "team/*": "team/worker-1,team/worker-2", "team/worker-3": "", "team/w": "", "other*": ""} {
		out, _ := presenceAs(t, s, filter)
		got := []string{}
		for _, v := range out.Sessions {
			got = append(got, v.Role)
		}
		if strings.Join(got, ",") != want {
			t.Fatalf("filter %q: %v, want %s", filter, got, want)
		}
	}
	if v = onlySession(t, s, "team/worker-2"); v.Status != "compacting" || v.ContextTokens == nil || *v.ContextTokens != 0 {
		t.Fatalf("zero tokens: %+v", v)
	}
	// A slot session reads presence too, through its base token.
	if w := callAs(s, "GET", "/api/presence", workerToken, "team/worker-2", nil); w.Code != 200 {
		t.Fatalf("slot reads presence: %d", w.Code)
	}
}

func TestPresenceRejectsBadFields(t *testing.T) {
	s, _ := presenceServer(t)
	slotDo(t, s, "register", ident("m", "/a"))
	for name, body := range map[string]any{
		"state":           report("m", "/a", "sleeping", nil, ""),
		"state case":      report("m", "/a", "Busy", nil, ""),
		"bare error":      report("m", "/a", "error", nil, ""),
		"empty kind":      report("m", "/a", "error:", nil, ""),
		"empty blocked":   report("m", "/a", "blocked:", nil, ""),
		"long kind":       report("m", "/a", "error:"+strings.Repeat("x", 65), nil, ""),
		"kind space":      report("m", "/a", "error:Stop Failure", nil, ""),
		"kind colon":      report("m", "/a", "blocked:a:b", nil, ""),
		"kind slash":      report("m", "/a", "blocked:a/b", nil, ""),
		"prefix case":     report("m", "/a", "Error:x", nil, ""),
		"other prefix":    report("m", "/a", "failed:x", nil, ""),
		"negative tokens": report("m", "/a", "idle", int64p(-1), ""),
		"float tokens":    map[string]any{"machine": "m", "directory": "/a", "context_tokens": 1.5},
		"string tokens":   map[string]any{"machine": "m", "directory": "/a", "context_tokens": "12"},
		"last turn":       report("m", "/a", "idle", nil, "yesterday"),
	} {
		for _, action := range []string{"register", "heartbeat", "takeover"} {
			if w := callAs(s, "POST", "/api/slots/"+action, workerToken, "team/worker", body); w.Code != 400 {
				t.Fatalf("%s with bad %s: %d %s", action, name, w.Code, w.Body)
			}
		}
	}
	if v := onlySession(t, s, ""); v.Status != "unknown" {
		t.Fatalf("rejected reports changed presence: %+v", v)
	}
}

func TestPresenceKindStates(t *testing.T) {
	s, clock := presenceServer(t)
	slotDo(t, s, "register", report("m", "/a", "idle", nil, ""))
	for _, state := range []string{"error:StopFailure", "blocked:permission_prompt", "error:a.b-c_9", "blocked:" + strings.Repeat("K", 64), "busy"} {
		for _, action := range []string{"heartbeat", "takeover", "register"} {
			slotDo(t, s, action, report("m", "/a", state, nil, ""))
			out, _ := presenceAs(t, s, "")
			if v := out.Sessions[0]; len(out.Sessions) != 1 || v.State != state || v.Status != state {
				t.Fatalf("%s %q: %+v", action, state, out.Sessions)
			}
		}
	}
	w := callAs(s, "POST", "/api/slots/heartbeat", workerToken, "team/worker", report("m", "/a", "error:", nil, ""))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "error:<kind>") || !strings.Contains(w.Body.String(), "blocked:<kind>") {
		t.Fatalf("bad kind message: %d %s", w.Code, w.Body)
	}
	slotDo(t, s, "heartbeat", report("m", "/a", "blocked:permission_prompt", nil, ""))
	clock.add(30 * time.Second)
	if v := onlySession(t, s, ""); v.Online || v.Status != "offline" || v.State != "blocked:permission_prompt" {
		t.Fatalf("offline wins over blocked state: %+v", v)
	}
}

func TestPresenceOfflineAfterMissedHeartbeats(t *testing.T) {
	s, clock := presenceServer(t)
	slotDo(t, s, "register", report("m", "/a", "busy", int64p(500), ""))
	clock.add(29 * time.Second) // two heartbeats missed, the third not yet
	if v := onlySession(t, s, ""); !v.Online || v.Status != "busy" {
		t.Fatalf("before the third missed heartbeat: %+v", v)
	}
	if got, _ := s.presence().OfflineUnnoticed(); len(got) != 0 {
		t.Fatalf("online session listed as offline: %+v", got)
	}
	clock.add(time.Second)
	v := onlySession(t, s, "")
	if v.Online || v.Status != "offline" || v.State != "busy" || v.OfflineSince == nil || !v.OfflineSince.Equal(v.LeaseExpiresAt) {
		t.Fatalf("after three missed heartbeats: %+v", v)
	}
	if got := slotTable(t, s); len(got) != 0 {
		t.Fatalf("offline session still holds its lease: %v", got)
	}

	// The offline transition shows up once until handled.
	got, err := s.presence().OfflineUnnoticed()
	if err != nil || len(got) != 1 || got[0].Role != "team/worker-1" {
		t.Fatalf("offline transition: %+v %v", got, err)
	}
	if ok, err := s.presence().MarkOfflineNoticed(got[0]); !ok || err != nil {
		t.Fatalf("mark noticed: %v %v", ok, err)
	}
	if got, _ = s.presence().OfflineUnnoticed(); len(got) != 0 {
		t.Fatalf("handled transition listed again: %+v", got)
	}

	// Reporting again brings the session back on the same number.
	g := slotDo(t, s, "heartbeat", report("m", "/a", "idle", nil, ""))
	if g.Number != 1 {
		t.Fatalf("back online: %+v", g)
	}
	if v = onlySession(t, s, ""); !v.Online || v.Status != "idle" || v.OfflineSince != nil {
		t.Fatalf("after reporting again: %+v", v)
	}
	// A transition read before a renewal cannot be marked afterwards.
	clock.add(30 * time.Second)
	got, _ = s.presence().OfflineUnnoticed()
	slotDo(t, s, "heartbeat", ident("m", "/a"))
	if ok, _ := s.presence().MarkOfflineNoticed(got[0]); ok {
		t.Fatal("marked a session that reported again")
	}

	// Once offline, the number is free: a newcomer takes it, and the old
	// identity is told to register again and gets the next number.
	clock.add(30 * time.Second)
	if g = slotDo(t, s, "register", ident("m", "/b")); g.Number != 1 || g.Reclaimed {
		t.Fatalf("freed number: %+v", g)
	}
	if w := callAs(s, "POST", "/api/slots/heartbeat", workerToken, "team/worker", ident("m", "/a")); w.Code != 404 {
		t.Fatalf("heartbeat after the number was taken: %d", w.Code)
	}
	if g = slotDo(t, s, "register", report("m", "/a", "idle", nil, "")); g.Number != 2 {
		t.Fatalf("re-register: %+v", g)
	}

	// Release is an immediate offline.
	slotDo(t, s, "release", ident("m", "/b"))
	if v = onlySession(t, s, "team/worker-1"); v.Online {
		t.Fatalf("released session online: %+v", v)
	}
}

func TestPresenceRetention(t *testing.T) {
	s, clock := presenceServer(t)
	retain := time.Hour
	s.presenceCfg.Retain = &retain
	slotDo(t, s, "register", ident("m", "/a"))
	clock.add(30*time.Second + time.Hour - time.Millisecond)
	if v := onlySession(t, s, ""); v.Online {
		t.Fatalf("still online: %+v", v)
	}
	clock.add(time.Millisecond)
	if out, _ := presenceAs(t, s, ""); len(out.Sessions) != 0 {
		t.Fatalf("offline past retention still listed: %+v", out.Sessions)
	}
	if got, _ := s.presence().OfflineUnnoticed(); len(got) != 0 {
		t.Fatalf("offline past retention still pending: %+v", got)
	}
}

func TestPresenceTakeoverMovesPresence(t *testing.T) {
	s, _ := presenceServer(t)
	slotDo(t, s, "register", report("m", "/a", "busy", int64p(7000), ""))
	slotDo(t, s, "takeover", report("m", "/b", "idle", nil, ""))
	out, _ := presenceAs(t, s, "")
	if len(out.Sessions) != 2 || out.Sessions[0].Directory != "/b" || out.Sessions[0].Status != "idle" ||
		out.Sessions[1].Role != "team/worker-2" || out.Sessions[1].Status != "busy" || *out.Sessions[1].ContextTokens != 7000 {
		t.Fatalf("after takeover: %+v", out.Sessions)
	}
}

func TestPresenceAuth(t *testing.T) {
	s, _ := presenceServer(t)
	s.admin.ProxyToken = "proxy-secret"
	slotDo(t, s, "register", report("m", "/a", "idle", nil, ""))
	// Another base's token cannot act as or report for team/worker-N.
	if w := callAs(s, "POST", "/api/slots/heartbeat", "secret-a", "team/worker-1", report("m", "/a", "busy", nil, "")); w.Code != 401 {
		t.Fatalf("heartbeat with another base's token: %d", w.Code)
	}
	if w := callAs(s, "GET", "/api/presence", "secret-a", "team/worker-1", nil); w.Code != 401 {
		t.Fatalf("presence with another base's token: %d", w.Code)
	}
	if w := callAs(s, "GET", "/api/presence", "", "", nil); w.Code != 401 {
		t.Fatalf("presence without a token: %d", w.Code)
	}
	if w := callAs(s, "POST", "/api/presence", "secret-a", "a", nil); w.Code != 405 {
		t.Fatalf("POST presence: %d", w.Code)
	}
	if v := onlySession(t, s, ""); v.Status != "idle" {
		t.Fatalf("rejected report applied: %+v", v)
	}
	for secret, code := range map[string]int{"": 401, "wrong-secret": 401, "proxy-secret": 200} {
		r := httptest.NewRequest("GET", "/api/admin/presence?role=team/worker", nil)
		if secret != "" {
			r.Header.Set("X-Switchboard-Admin", secret)
		}
		w := httptest.NewRecorder()
		s.api(w, r)
		if w.Code != code || code == 200 && !strings.Contains(w.Body.String(), `"role":"team/worker-1"`) {
			t.Fatalf("admin presence with %q: %d %s", secret, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), workerToken) {
			t.Fatal("token leaked")
		}
	}
}

func TestPresenceSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	s := &Server{}
	if err := presence.Configure(&s.presenceCfg, env(nil)); err != nil || s.presence().LeaseLength() != 3*time.Minute || s.presence().OfflineRetention() != 24*time.Hour {
		t.Fatalf("defaults: %v %v %v", err, s.presence().LeaseLength(), s.presence().OfflineRetention())
	}
	s = &Server{}
	if err := presence.Configure(&s.presenceCfg, env(map[string]string{"AGENT_MAIL_PRESENCE_INTERVAL": "40s", "AGENT_MAIL_PRESENCE_MISSED": "2", "AGENT_MAIL_PRESENCE_RETAIN": "0"})); err != nil ||
		s.presence().LeaseLength() != 80*time.Second || s.presence().OfflineRetention() != 0 {
		t.Fatalf("configured: %v %v %v", err, s.presence().LeaseLength(), s.presence().OfflineRetention())
	}
	s.presenceCfg.SlotLease = 5 * time.Minute // AGENT_MAIL_SLOT_LEASE overrides the product
	if s.presence().LeaseLength() != 5*time.Minute {
		t.Fatalf("override: %v", s.presence().LeaseLength())
	}
	for k, v := range map[string]string{"AGENT_MAIL_PRESENCE_INTERVAL": "500ms", "AGENT_MAIL_PRESENCE_MISSED": "0", "AGENT_MAIL_PRESENCE_RETAIN": "-1h"} {
		if err := presence.Configure(&presence.Config{}, env(map[string]string{k: v})); err == nil {
			t.Fatalf("%s=%s accepted", k, v)
		}
	}
}

// slotsSchemaV20 is the slots table as created before presence.
const slotsSchemaV20 = `CREATE TABLE slots(base TEXT NOT NULL,number INTEGER NOT NULL,machine TEXT NOT NULL,directory TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',label TEXT NOT NULL DEFAULT '',parent TEXT NOT NULL DEFAULT '',registered_at INTEGER NOT NULL,lease_until INTEGER NOT NULL,PRIMARY KEY(base,number),UNIQUE(base,machine,directory));`

func TestPresenceSchemaOnSlotsDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "slots.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	if _, err = db.Exec(legacySchema + `ALTER TABLE messages ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal';` + slotsSchemaV20 +
		`INSERT INTO roles VALUES('team/worker','` + workerToken + `','now'); INSERT INTO slots VALUES('team/worker',1,'m','/a','','old','',` + itoa(now.UnixMilli()) + `,` + itoa(now.Add(time.Minute).UnixMilli()) + `);`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = store.Init(db); err != nil {
			t.Fatalf("schema run %d: %v", i, err)
		}
	}
	s := &Server{db: db, tokens: map[string]string{}, clock: func() time.Time { return now }}
	v := onlyPresence(t, s)
	if !v.Online || v.Status != "unknown" || v.Label != "old" || v.ContextTokens != nil || v.ReportedAt != nil || v.LastTurnAt != nil {
		t.Fatalf("pre-presence row: %+v", v)
	}
	slotDo(t, s, "heartbeat", report("m", "/a", "busy", int64p(1), ""))
	if v = onlyPresence(t, s); v.Status != "busy" || v.ReportedAt == nil {
		t.Fatalf("heartbeat on migrated db: %+v", v)
	}
}

func onlyPresence(t *testing.T, s *Server) wire.PresenceView {
	t.Helper()
	out, err := s.presence().List("")
	if err != nil || len(out.Sessions) != 1 {
		t.Fatalf("presence: %+v %v", out, err)
	}
	return out.Sessions[0]
}

func TestStatusCommand(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "b")
	if out := h.mustRun(nil, "status"); strings.TrimSpace(out) != "no sessions" {
		t.Fatalf("empty status: %q", out)
	}
	h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/a", "--label", "issue-21", "--parent", "team/lead", "--state", "idle")
	h.mustRun(nil, "slot", "heartbeat", "--machine", "m", "--directory", "/a", "--label", "issue-21", "--parent", "team/lead",
		"--state", "busy", "--context-tokens", "45200", "--last-turn-at", "2026-11-17T09:00:00Z")
	h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/b", "--context-tokens", "950")
	out := h.mustRun(nil, "status")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ROLE") || !strings.Contains(lines[0], "REPORTED") {
		t.Fatalf("status table: %q", out)
	}
	for _, want := range []string{"b-1", "busy", "45k", "m", "/a", "issue-21", "team/lead", "s ago"} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("status row 1 missing %q: %q", want, lines[1])
		}
	}
	if f := strings.Fields(lines[2]); f[0] != "b-2" || f[1] != "unknown" || f[2] != "0.9k" || f[3] != "-" {
		t.Fatalf("status row 2: %q", lines[2])
	}
	if out = h.mustRun(nil, "status", "b-2", "--json"); !strings.Contains(out, `"role":"b-2"`) || strings.Contains(out, `"role":"b-1"`) || strings.Contains(out, "secret-b") {
		t.Fatalf("status b-2 --json: %q", out)
	}
	h.mustRun(nil, "slot", "heartbeat", "--machine", "m", "--directory", "/a", "--state", "error:StopFailure")
	h.mustRun(nil, "slot", "heartbeat", "--machine", "m", "--directory", "/b", "--state", "blocked:permission_prompt")
	out = h.mustRun(nil, "status")
	if lines = strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 3 || strings.Fields(lines[1])[1] != "error:StopFailure" || strings.Fields(lines[2])[1] != "blocked:permission_prompt" {
		t.Fatalf("status with kind states: %q", out)
	}
	if out = h.mustRun(nil, "status", "a"); strings.TrimSpace(out) != "no sessions" {
		t.Fatalf("status a: %q", out)
	}
	for _, args := range [][]string{
		{"slot", "heartbeat", "--state", "asleep"},
		{"slot", "heartbeat", "--state", "error:"},
		{"slot", "heartbeat", "--context-tokens", "-5"},
		{"slot", "heartbeat", "--last-turn-at", "noon"},
		{"status", "b", "c"},
	} {
		if r := h.run(nil, args...); r.code != cli.ExitUsage {
			t.Fatalf("%v: %d %s", args, r.code, r.stderr)
		}
	}
	if out = h.mustRun(nil, "help", "slot"); !strings.Contains(out, "error:<kind>") || !strings.Contains(out, "blocked:<kind>") {
		t.Fatalf("slot help: %q", out)
	}
	if out = h.mustRun(nil, "help", "status"); !strings.Contains(out, "switchboard status [ROLE]") {
		t.Fatalf("status help: %q", out)
	}
}
