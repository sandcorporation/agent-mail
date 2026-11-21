package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

func sweepOnce(t *testing.T, s *Server) sweepResult {
	t.Helper()
	got, err := s.sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// systemMail is the mail from the system sender in role's inbox.
func systemMail(t *testing.T, s *Server, token, role string) []wire.Message {
	t.Helper()
	var out []wire.Message
	for _, m := range listAs(t, s, token, role, "inbox") {
		if m.Sender == identity.SystemSender {
			out = append(out, m)
		}
	}
	return out
}

func withParent(machine, dir, label, parent string) wire.SlotRequest {
	r := labeled(machine, dir, label)
	r.Parent = parent
	r.State = "busy"
	return r
}

func TestStallOfflineNoticeOncePerTransition(t *testing.T) {
	t.Parallel()
	s, clock := presenceServer(t) // lease 30s
	slotDo(t, s, "register", withParent("box-1", "/work/a", "issue-7", "a"))
	if got := sweepOnce(t, s); got.Offline != 0 {
		t.Fatalf("online session noticed: %+v", got)
	}
	wake, stop := s.hub.Subscribe("a")
	defer stop()
	clock.add(31 * time.Second)
	total := 0
	for i := 0; i < 3; i++ {
		total += sweepOnce(t, s).Offline
	}
	if total != 1 {
		t.Fatalf("notices over three sweeps: %d", total)
	}
	select {
	case <-wake:
	default:
		t.Fatal("notice did not wake the parent")
	}
	got := systemMail(t, s, "secret-a", "a")
	if len(got) != 1 {
		t.Fatalf("parent inbox: %+v", got)
	}
	m := got[0]
	if m.Subject != "offline: team/worker-1" || m.Priority != "normal" {
		t.Fatalf("notice: %+v", m)
	}
	for _, want := range []string{"team/worker-1", "label issue-7", "machine box-1", "directory /work/a", "went offline at 2026-11-17T09:00:30Z"} {
		if !strings.Contains(m.Body, want) {
			t.Fatalf("body missing %q: %q", want, m.Body)
		}
	}
	if strings.Contains(m.Body, workerToken) || strings.Contains(m.Body, "secret") {
		t.Fatalf("body leaks a token: %q", m.Body)
	}

	// Back online, then offline again: one more notice.
	slotDo(t, s, "heartbeat", withParent("box-1", "/work/a", "issue-7", "a"))
	if got := sweepOnce(t, s); got.Offline != 0 {
		t.Fatalf("noticed while online again: %+v", got)
	}
	clock.add(31 * time.Second)
	if got := sweepOnce(t, s).Offline + sweepOnce(t, s).Offline; got != 1 {
		t.Fatalf("second transition notices: %d", got)
	}
	if got := systemMail(t, s, "secret-a", "a"); len(got) != 2 {
		t.Fatalf("parent inbox after second transition: %d", len(got))
	}
}

func TestStallReleaseIsNotNoticed(t *testing.T) {
	t.Parallel()
	s, clock := presenceServer(t)
	slotDo(t, s, "register", withParent("m", "/w", "", "a"))
	slotDo(t, s, "release", ident("m", "/w"))
	clock.add(time.Minute)
	if got := sweepOnce(t, s); got.Offline != 0 {
		t.Fatalf("release noticed: %+v", got)
	}
	// A new run after the release is watched again.
	slotDo(t, s, "register", withParent("m", "/w", "", "a"))
	clock.add(time.Minute)
	if got := sweepOnce(t, s); got.Offline != 1 {
		t.Fatalf("expiry after re-register: %+v", got)
	}
}

func TestStallOfflineWithoutUsableParent(t *testing.T) {
	t.Parallel()
	s, clock := presenceServer(t)
	for i, parent := range []string{"team/worker" /* its own mailbox (-1) */, "", "nobody/here", identity.SystemSender} {
		slotDo(t, s, "register", withParent("m", "/w"+itoa(int64(i)), "", parent))
	}
	clock.add(time.Minute)
	if got := sweepOnce(t, s); got.Offline != 0 {
		t.Fatalf("notices without a usable parent: %+v", got)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM slots WHERE offline_noticed_at=0`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left unmarked: %d %v", n, err)
	}
	var mail int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE sender=?`, identity.SystemSender).Scan(&mail)
	if mail != 0 {
		t.Fatalf("system mail stored: %d", mail)
	}
}

func TestStallLabelNoticeToSender(t *testing.T) {
	t.Parallel()
	s, clock, _ := labelServer(t)
	s.labelStall = 10 * time.Minute
	first := sendFrom(t, s, label22, "one", "", 201)
	second := sendFrom(t, s, label22, "two", "", 201)
	sendFrom(t, s, "label:team/worker/23", "three", "", 201)
	clock.add(9 * time.Minute)
	if got := sweepOnce(t, s); got.Label != 0 {
		t.Fatalf("noticed before the limit: %+v", got)
	}
	// Connected in time: no notice for label 24.
	sendFrom(t, s, "label:team/worker/24", "four", "", 201)
	slotDo(t, s, "register", labeled("m", "/w24", "24"))
	clock.add(2 * time.Minute)
	if got := sweepOnce(t, s).Label + sweepOnce(t, s).Label; got != 2 {
		t.Fatalf("label notices: %d", got)
	}
	got := systemMail(t, s, "secret-a", "a")
	if len(got) != 2 {
		t.Fatalf("sender inbox: %+v", got)
	}
	var n22 wire.Message
	for _, m := range got {
		if m.Subject == "label not connected: "+label22 {
			n22 = m
		}
		if strings.Contains(m.Subject, "/24") {
			t.Fatalf("notice for a connected label: %+v", m)
		}
	}
	for _, want := range []string{"team/worker", "label 22", "10m0s", "2 held message(s)", "#" + itoa(first.ID), "#" + itoa(second.ID)} {
		if !strings.Contains(n22.Body, want) {
			t.Fatalf("label 22 notice missing %q: %+v", want, n22)
		}
	}
	if n22.Priority != "normal" {
		t.Fatalf("priority: %s", n22.Priority)
	}
	// New mail to the same label after the notice gets its own notice later.
	sendFrom(t, s, label22, "five", "", 201)
	clock.add(11 * time.Minute)
	if got := sweepOnce(t, s); got.Label != 1 {
		t.Fatalf("later held mail: %+v", got)
	}
	// Mail that was noticed is still delivered when the label connects.
	if g := slotDo(t, s, "register", labeled("m", "/w22", "22")); g.HeldDelivered != 3 {
		t.Fatalf("delivered after notice: %+v", g)
	}
}

func TestSystemSenderIsReserved(t *testing.T) {
	s, _ := slotServer(t)
	// A role row by that name (from an old database) still cannot log in.
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES(?,?,'now')`, identity.SystemSender, "system-secret"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{identity.SystemSender, identity.SystemSender + "-1", identity.SystemSender + "-2"} {
		if w := callAs(s, "GET", "/api/messages", "system-secret", role, nil); w.Code != 401 {
			t.Fatalf("auth as %s: %d", role, w.Code)
		}
	}
	if w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": identity.SystemSender, "subject": "hi"}, ""); w.Code != 403 {
		t.Fatalf("send to system: %d", w.Code)
	}
	t.Setenv("AGENT_MAIL_ENROLLMENT_TOKEN", "enroll-secret-enroll-secret")
	r := callAs(s, "POST", "/api/register", "enroll-secret-enroll-secret", "", map[string]string{"role": identity.SystemSender + "-3", "credential": strings.Repeat("x", 30)})
	if r.Code != 403 {
		t.Fatalf("register reserved name: %d %s", r.Code, r.Body)
	}
	fresh := testServer(t)
	fresh.tokens[identity.SystemSender] = "configured-secret"
	if err := migrateConfiguredRoles(fresh); err != nil || identity.RoleExists(fresh.db, identity.SystemSender) {
		t.Fatalf("configured reserved role imported: %v", err)
	}
	if err := configureStall(&Server{}, func(k string) string {
		if k == "AGENT_MAIL_BOSS_ROLES" {
			return "a," + identity.SystemSender
		}
		return ""
	}); err == nil {
		t.Fatal("reserved boss role accepted")
	}
}

func TestStallSettings(t *testing.T) {
	t.Parallel()
	s := &Server{}
	if err := configureStall(s, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if s.sweepInterval() != 30*time.Second || s.labelStallAfter() != 10*time.Minute || s.asks().PermissionTTL() != 10*time.Minute || len(s.bossRoles) != 0 {
		t.Fatalf("defaults: %v %v %v %v", s.sweepInterval(), s.labelStallAfter(), s.asks().PermissionTTL(), s.bossRoles)
	}
	env := map[string]string{"AGENT_MAIL_SWEEP_INTERVAL": "5s", "AGENT_MAIL_LABEL_STALL": "2m", "AGENT_MAIL_PERMISSION_TTL": "90s", "AGENT_MAIL_BOSS_ROLES": " sandcorp/boss , team/lead ,"}
	if err := configureStall(s, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if s.sweepInterval() != 5*time.Second || s.labelStallAfter() != 2*time.Minute || s.asks().PermissionTTL() != 90*time.Second || !s.bossRoles["sandcorp/boss"] || !s.bossRoles["team/lead"] || len(s.bossRoles) != 2 {
		t.Fatalf("configured: %+v", s)
	}
	for k, v := range map[string]string{"AGENT_MAIL_SWEEP_INTERVAL": "0s", "AGENT_MAIL_LABEL_STALL": "soon", "AGENT_MAIL_PERMISSION_TTL": "500ms", "AGENT_MAIL_BOSS_ROLES": "bad role"} {
		if err := configureStall(&Server{}, func(key string) string {
			if key == k {
				return v
			}
			return ""
		}); err == nil {
			t.Fatalf("%s=%s accepted", k, v)
		}
	}
}

func TestSweeperRunsAndStopsOnShutdown(t *testing.T) {
	t.Parallel()
	s, _ := presenceServer(t)
	s.clock = nil
	s.presenceCfg.SlotLease = time.Second
	s.sweepEvery = 20 * time.Millisecond
	s.done = make(chan struct{})
	slotDo(t, s, "register", withParent("m", "/w", "", "a"))
	wait := s.startSweeper()
	deadline := time.Now().Add(5 * time.Second)
	for len(systemMail(t, s, "secret-a", "a")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sweeper sent no notice")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(s.done)
	stopped := make(chan struct{})
	go func() { wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not stop")
	}
	if n := len(systemMail(t, s, "secret-a", "a")); n != 1 {
		t.Fatalf("notices: %d", n)
	}
}
