package server

import (
	"github.com/sandcorporation/switchboard/internal/insight"

	"sort"
	"strings"
	"testing"
	"time"
)

// alertServer is slotServer with a long lease (sessions stay online without
// heartbeats), default thresholds and a as the boss. The clock starts at
// 2026-11-17 09:00 UTC.
func alertServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := slotServer(t)
	s.presenceCfg.SlotLease = 72 * time.Hour
	s.alerts = &insight.Thresholds{Pending: 2 * time.Hour, Error: 10 * time.Minute, Blocked: 30 * time.Minute}
	s.bossRoles = map[string]bool{"a": true}
	return s, clock
}

// alertSubjects are the subjects of the alerts in role's inbox, sorted.
func alertSubjects(t *testing.T, s *Server, token, role string) []string {
	t.Helper()
	var out []string
	for _, m := range systemMail(t, s, token, role) {
		if strings.HasPrefix(m.Subject, "[지표] ") {
			out = append(out, m.Subject)
		}
	}
	sort.Strings(out)
	return out
}

func TestAlertsEachKindOncePerDay(t *testing.T) {
	t.Parallel()
	s, clock := alertServer(t)
	look := sendMail(t, s, "secret-b", "team/worker", "please look")
	slotDo(t, s, "register", report("m", "/e", "error:StopFailure", nil, ""))
	slotDo(t, s, "register", report("m", "/b", "blocked:permission_prompt", nil, ""))

	clock.add(9 * time.Minute)
	if got := sweepOnce(t, s).Alerts; got != 0 {
		t.Fatalf("alerts below every threshold: %d", got)
	}
	clock.add(time.Minute) // error 10m
	if got := sweepOnce(t, s).Alerts; got != 1 {
		t.Fatalf("error alert: %d", got)
	}
	clock.add(20 * time.Minute) // blocked 30m
	if got := sweepOnce(t, s).Alerts; got != 1 {
		t.Fatalf("blocked alert: %d", got)
	}
	wake, stop := s.hub.Subscribe("a")
	defer stop()
	clock.add(100 * time.Minute) // pending 2h10m
	if got := sweepOnce(t, s).Alerts; got != 1 {
		t.Fatalf("pending alert: %d", got)
	}
	select {
	case <-wake:
	default:
		t.Fatal("alert did not wake the boss")
	}
	for i := 0; i < 3; i++ {
		clock.add(time.Hour)
		if got := sweepOnce(t, s).Alerts; got != 0 {
			t.Fatalf("same day again: %d", got)
		}
	}

	want := []string{
		"[지표] blocked team/worker-2 blocked:permission_prompt, 30m째",
		"[지표] error team/worker-1 error:StopFailure, 10m째",
		"[지표] pending team/worker 1통, 가장 오래 2h10m",
	}
	got := alertSubjects(t, s, "secret-a", "a")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("alerts:\n%s", strings.Join(got, "\n"))
	}
	for _, m := range systemMail(t, s, "secret-a", "a") {
		if m.Priority != "normal" || !strings.Contains(m.Body, "switchboard metrics") || !strings.Contains(m.Body, "#metrics") {
			t.Fatalf("alert mail: %+v", m)
		}
		if strings.HasPrefix(m.Subject, "[지표] pending") {
			// The body names the pending mail: ID, sender, subject (#354).
			if m.TaskID != "alert:pending:team/worker:2026-11-17" || !strings.Contains(m.Body, "1통") || !strings.Contains(m.Body, "2h") ||
				!strings.Contains(m.Body, "#"+itoa(look)+" b — please look") {
				t.Fatalf("pending alert: %+v", m)
			}
		}
	}

	// UTC day boundary: 23:59:59 still today, 00:00 the next day fires again.
	clock.add(time.Date(2026, 11, 17, 23, 59, 59, 0, time.UTC).Sub(clock.now()))
	if got := sweepOnce(t, s).Alerts; got != 0 {
		t.Fatalf("before midnight: %d", got)
	}
	clock.add(time.Second)
	if got := sweepOnce(t, s).Alerts; got != 3 {
		t.Fatalf("next day: %d", got)
	}
	if got := sweepOnce(t, s).Alerts; got != 0 {
		t.Fatalf("next day again: %d", got)
	}
	var tasks []string
	for _, m := range systemMail(t, s, "secret-a", "a") {
		if strings.HasSuffix(m.TaskID, ":2026-11-18") {
			tasks = append(tasks, m.TaskID)
		}
	}
	if len(tasks) != 3 {
		t.Fatalf("next-day alert tasks: %v", tasks)
	}
}

func TestAlertsZeroDisablesKind(t *testing.T) {
	t.Parallel()
	s, clock := alertServer(t)
	s.alerts = &insight.Thresholds{Pending: 0, Error: 0, Blocked: time.Minute}
	sendMail(t, s, "secret-b", "team/worker", "old")
	slotDo(t, s, "register", report("m", "/e", "error:StopFailure", nil, ""))
	slotDo(t, s, "register", report("m", "/b", "blocked:permission_prompt", nil, ""))
	clock.add(5 * time.Hour)
	if got := sweepOnce(t, s).Alerts; got != 1 {
		t.Fatalf("only blocked should alert: %d", got)
	}
	got := alertSubjects(t, s, "secret-a", "a")
	if len(got) != 1 || !strings.HasPrefix(got[0], "[지표] blocked ") {
		t.Fatalf("alerts: %v", got)
	}
}

func TestAlertsOfflineSessionDoesNotAlert(t *testing.T) {
	t.Parallel()
	s, clock := alertServer(t)
	s.presenceCfg.SlotLease = 5 * time.Minute
	slotDo(t, s, "register", report("m", "/e", "error:StopFailure", nil, ""))
	clock.add(time.Hour)
	if got := sweepOnce(t, s).Alerts; got != 0 {
		t.Fatalf("offline session alerted: %d", got)
	}
}

func TestAlertsSkipBossOwnPending(t *testing.T) {
	t.Parallel()
	s, clock := alertServer(t)
	s.bossRoles = map[string]bool{"a": true, "b": true}
	sendMail(t, s, "secret-b", "a", "to boss a")
	sendMail(t, s, "secret-a", "b", "to boss b")
	sendMail(t, s, "secret-a", "team/worker", "to worker")
	clock.add(3 * time.Hour)
	if got := sweepOnce(t, s).Alerts; got != 3 {
		t.Fatalf("alerts: %d", got)
	}
	ga, gb := alertSubjects(t, s, "secret-a", "a"), alertSubjects(t, s, "secret-b", "b")
	wantA := []string{"[지표] pending b 1통, 가장 오래 3h", "[지표] pending team/worker 1통, 가장 오래 3h"}
	wantB := []string{"[지표] pending a 1통, 가장 오래 3h", "[지표] pending team/worker 1통, 가장 오래 3h"}
	if strings.Join(ga, "|") != strings.Join(wantA, "|") || strings.Join(gb, "|") != strings.Join(wantB, "|") {
		t.Fatalf("a: %v\nb: %v", ga, gb)
	}
}

func TestAlertsNoBossNoRecord(t *testing.T) {
	t.Parallel()
	s, clock := alertServer(t)
	s.bossRoles = map[string]bool{}
	sendMail(t, s, "secret-b", "team/worker", "old")
	clock.add(3 * time.Hour)
	if got := sweepOnce(t, s).Alerts; got != 0 {
		t.Fatalf("alerts without a boss: %d", got)
	}
	s.bossRoles = map[string]bool{"a": true}
	if got := sweepOnce(t, s).Alerts; got != 1 {
		t.Fatalf("boss configured later: %d", got)
	}
}

func TestAlertsPrunedAfter30Days(t *testing.T) {
	t.Parallel()
	s, _ := alertServer(t)
	if _, err := s.db.Exec(`INSERT INTO alerts VALUES('pending','x','2026-10-17',0),('pending','y','2026-10-18',0)`); err != nil {
		t.Fatal(err)
	}
	sweepOnce(t, s)
	var keys []string
	rows, err := s.db.Query(`SELECT key FROM alerts ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		keys = append(keys, k)
	}
	if strings.Join(keys, ",") != "y" {
		t.Fatalf("kept: %v", keys)
	}
}
