package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func lostMail(t *testing.T, s *Server, to string) []wire.Message {
	t.Helper()
	var out []wire.Message
	for _, m := range jobMail(t, s, to) {
		if strings.HasSuffix(m.Subject, " 잃음") {
			out = append(out, m)
		}
	}
	return out
}

func reportJob(t *testing.T, s *Server, token string, id int64, q map[string]any) wire.Job {
	t.Helper()
	w := call(s, "POST", "/api/jobs/"+itoa(id)+"/report", token, q, "")
	if w.Code != 200 {
		t.Fatalf("report: %d %s", w.Code, w.Body)
	}
	var j wire.Job
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	return j
}

// A machine reporting in without a running job of its role (it rebooted)
// makes that job lost with one urgent mail; jobs it lists, jobs younger than
// the grace, other machines' and other roles' jobs stay.
func TestJobLostWhenTheMachineReportsInWithoutIt(t *testing.T) {
	t.Parallel()
	s := testServer(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	job := func(token, machine, label string) int64 {
		j, code := createJob(t, s, token, map[string]any{"command": "x", "label": label, "cwd": "/w", "machine": machine}, "")
		if code != 201 {
			t.Fatal(code)
		}
		reportJob(t, s, token, j.ID, map[string]any{"state": "running"})
		return j.ID
	}
	gone := job("secret-a", "partner-a", "build")
	kept := job("secret-a", "partner-a", "kept")
	elsewhere := job("secret-a", "partner-b", "far")
	other := job("secret-b", "partner-a", "theirs")
	now = now.Add(2 * time.Minute)
	young := job("secret-a", "partner-a", "young")
	machine := func() int {
		t.Helper()
		w := call(s, "POST", "/api/jobs/machine", "secret-a", map[string]any{"machine": "partner-a", "live": []int64{kept}}, "")
		if w.Code != 200 {
			t.Fatalf("machine: %d %s", w.Code, w.Body)
		}
		var r struct{ Lost int }
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return r.Lost
	}
	if n := machine(); n != 1 {
		t.Fatalf("lost %d, want 1", n)
	}
	if n := machine(); n != 0 {
		t.Fatalf("again: lost %d", n)
	}
	for id, want := range map[int64]string{gone: wire.JobLost, kept: wire.JobRunning, elsewhere: wire.JobRunning, other: wire.JobRunning, young: wire.JobRunning} {
		if j, _ := s.jobs().Get(id); j.State != want {
			t.Fatalf("job %d (%s): %s, want %s", id, j.Label, j.State, want)
		}
	}
	m := lostMail(t, s, "a")
	if len(m) != 1 || m[0].Subject != "[작업 끝남] build 잃음" || m[0].Priority != "urgent" {
		t.Fatalf("lost mail: %+v", m)
	}
	if w := call(s, "POST", "/api/jobs/machine", "secret-a", map[string]any{"live": []int64{}}, ""); w.Code != 400 {
		t.Fatalf("no machine: %d", w.Code)
	}
}

// A live job whose runner stays silent past the limit is lost at a sweep,
// with one mail; a runner that reports again takes it back, and its end is
// recorded and told.
func TestJobLostWhenItsRunnerStaysSilent(t *testing.T) {
	t.Parallel()
	s := testServer(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	sweep := func() {
		t.Helper()
		if _, err := s.sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	j, _ := createJob(t, s, "secret-a", map[string]any{"command": "x", "label": "sync", "cwd": "/w", "machine": "partner-a"}, "")
	handed, _ := createJob(t, s, "secret-a", map[string]any{"command": "y", "label": "never", "cwd": "/w", "machine": "partner-a"}, "")
	reportJob(t, s, "secret-a", j.ID, map[string]any{"state": "running"})
	// Heartbeats keep it.
	for i := 0; i < 3; i++ {
		now = now.Add(wire.JobSilentAfter - time.Minute)
		reportJob(t, s, "secret-a", j.ID, map[string]any{"state": "running"})
		sweep()
	}
	if got, _ := s.jobs().Get(j.ID); got.State != wire.JobRunning {
		t.Fatalf("reporting job: %+v", got)
	}
	// A handed job that never started counts from its creation.
	if got, _ := s.jobs().Get(handed.ID); got.State != wire.JobLost {
		t.Fatalf("silent handed job: %+v", got)
	}
	now = now.Add(wire.JobSilentAfter)
	sweep()
	sweep()
	got, _ := s.jobs().Get(j.ID)
	if got.State != wire.JobLost || got.EndedAt == nil || got.Error == "" {
		t.Fatalf("silent job: %+v", got)
	}
	if m := lostMail(t, s, "a"); len(m) != 2 {
		t.Fatalf("lost mails: %+v", m)
	}
	// The server was away, not the runner: it reports again.
	if got = reportJob(t, s, "secret-a", j.ID, map[string]any{"state": "running"}); got.State != wire.JobRunning || got.EndedAt != nil || got.Error != "" {
		t.Fatalf("revived: %+v", got)
	}
	got = reportJob(t, s, "secret-a", j.ID, map[string]any{"state": "exited", "exit_code": 0})
	if got.State != wire.JobExited {
		t.Fatalf("end after revival: %+v", got)
	}
	m := jobMail(t, s, "a")
	if last := m[len(m)-1]; last.Subject != "[작업 끝남] sync exit 0" {
		t.Fatalf("end mail: %+v", m)
	}
}
