package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func createJob(t *testing.T, s *Server, token string, body map[string]any, idem string) (wire.Job, int) {
	t.Helper()
	w := call(s, "POST", "/api/jobs", token, body, idem)
	var j wire.Job
	if w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
	}
	return j, w.Code
}

func jobMail(t *testing.T, s *Server, to string) []wire.Message {
	t.Helper()
	rows, err := s.db.Query(`SELECT subject, body, priority FROM messages WHERE recipient=? AND subject LIKE '[작업 끝남]%' ORDER BY id`, to)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []wire.Message
	for rows.Next() {
		var m wire.Message
		if err := rows.Scan(&m.Subject, &m.Body, &m.Priority); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestJobCreateIsIdempotentAndOwnerOnly(t *testing.T) {
	s := testServer(t)
	body := map[string]any{"command": "make build", "cwd": "/work", "machine": "partner-a"}
	j, code := createJob(t, s, "secret-a", body, "k1")
	if code != 201 || j.State != wire.JobHanded || j.Role != "a" || j.Label != "make build" || j.Kind != wire.JobCommand {
		t.Fatalf("create: %d %+v", code, j)
	}
	if got := j.ExpiresAt.Sub(j.CreatedAt); got != wire.DefaultJobExpiry {
		t.Fatalf("default expiry %v", got)
	}
	again, code := createJob(t, s, "secret-a", body, "k1")
	if code != 200 || again.ID != j.ID {
		t.Fatalf("idem retry: %d id %d want %d", code, again.ID, j.ID)
	}
	if other, code := createJob(t, s, "secret-b", body, "k1"); code != 201 || other.ID == j.ID {
		t.Fatalf("the key is per role: %d %d", code, other.ID)
	}
	if _, code := createJob(t, s, "secret-a", map[string]any{"command": " ", "cwd": "/w", "machine": "m"}, ""); code != 400 {
		t.Fatalf("empty command: %d", code)
	}
	path := "/api/jobs/" + itoa(j.ID)
	if w := call(s, "GET", path, "secret-b", nil, ""); w.Code != 404 {
		t.Fatalf("another role's job: %d", w.Code)
	}
	if w := call(s, "POST", path+"/report", "secret-b", map[string]any{"state": "running"}, ""); w.Code != 404 {
		t.Fatalf("another role reports: %d", w.Code)
	}
	if w := call(s, "POST", path+"/stop", "secret-b", nil, ""); w.Code != 404 {
		t.Fatalf("another role stops: %d", w.Code)
	}
	var list []wire.Job
	w := call(s, "GET", "/api/jobs", "secret-b", nil, "")
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Role != "b" {
		t.Fatalf("b lists only its own: %s", w.Body)
	}
	if w := call(s, "GET", "/api/jobs?role=a", "secret-b", nil, ""); w.Code != 403 {
		t.Fatalf("non-boss lists another role: %d", w.Code)
	}
	s.bossRoles = map[string]bool{"b": true}
	w = call(s, "GET", "/api/jobs", "secret-b", nil, "")
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("boss lists all: %s", w.Body)
	}
}

func TestJobEndMailOnceAndOnlyWhenUnwatched(t *testing.T) {
	s := testServer(t)
	report := func(id int64, q map[string]any) wire.Job {
		t.Helper()
		w := call(s, "POST", "/api/jobs/"+itoa(id)+"/report", "secret-a", q, "")
		if w.Code != 200 {
			t.Fatalf("report: %d %s", w.Code, w.Body)
		}
		var j wire.Job
		_ = json.Unmarshal(w.Body.Bytes(), &j)
		return j
	}
	job := func(label string) int64 {
		j, code := createJob(t, s, "secret-a", map[string]any{"command": "x", "label": label, "cwd": "/w", "machine": "partner-a"}, "")
		if code != 201 {
			t.Fatal(code)
		}
		return j.ID
	}
	// Watched: a waiter saw the end, no mail.
	watched := job("watched")
	report(watched, map[string]any{"state": "running"})
	if j := report(watched, map[string]any{"state": "exited", "exit_code": 0, "watched": true}); j.State != wire.JobExited || *j.ExitCode != 0 || j.StartedAt == nil || j.EndedAt == nil {
		t.Fatalf("watched end: %+v", j)
	}
	if m := jobMail(t, s, "a"); len(m) != 0 {
		t.Fatalf("mail for a watched end: %+v", m)
	}
	// Unwatched failure: one urgent mail with the tail; a repeated end report
	// changes nothing and sends nothing.
	failed := job("build")
	report(failed, map[string]any{"state": "running"})
	report(failed, map[string]any{"state": "exited", "exit_code": 2, "tail": "line one\nerror: boom"})
	if j := report(failed, map[string]any{"state": "exited", "exit_code": 0}); *j.ExitCode != 2 {
		t.Fatalf("end state changed: %+v", j)
	}
	if j := report(failed, map[string]any{"state": "running"}); j.State != wire.JobExited {
		t.Fatalf("running after the end: %+v", j)
	}
	m := jobMail(t, s, "a")
	if len(m) != 1 || m[0].Subject != "[작업 끝남] build exit 2" || m[0].Priority != "urgent" || !strings.HasSuffix(m[0].Body, "line one\nerror: boom") {
		t.Fatalf("end mail: %+v", m)
	}
	// Unwatched success: normal priority.
	ok := job("tests")
	report(ok, map[string]any{"state": "exited", "exit_code": 0})
	if m = jobMail(t, s, "a"); len(m) != 2 || m[1].Priority != "normal" || m[1].Subject != "[작업 끝남] tests exit 0" {
		t.Fatalf("success mail: %+v", m)
	}
	if w := call(s, "POST", "/api/jobs/"+itoa(ok)+"/report", "secret-a", map[string]any{"state": "exited"}, ""); w.Code != 400 {
		t.Fatalf("exited without a code: %d", w.Code)
	}
}

func TestJobStopIsRecordedUntilTheEnd(t *testing.T) {
	s := testServer(t)
	j, _ := createJob(t, s, "secret-a", map[string]any{"command": "sleep 60", "cwd": "/w", "machine": "partner-a"}, "")
	w := call(s, "POST", "/api/jobs/"+itoa(j.ID)+"/stop", "secret-a", nil, "")
	var got wire.Job
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || !got.StopRequested || got.State != wire.JobHanded {
		t.Fatalf("stop: %d %+v", w.Code, got)
	}
	call(s, "POST", "/api/jobs/"+itoa(j.ID)+"/report", "secret-a", map[string]any{"state": "stopped", "exit_code": 143}, "")
	if m := jobMail(t, s, "a"); len(m) != 1 || m[0].Subject != "[작업 끝남] sleep 60 중지됨" || m[0].Priority != "normal" {
		t.Fatalf("stop mail: %+v", m)
	}
}

func TestJobTailIsCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(strings.Repeat("x", 99) + "\n")
	}
	tail := wire.JobTail(b.String())
	if n := strings.Count(tail, "\n") + 1; n != wire.MaxJobTailLines || len(tail) > wire.MaxJobTailBytes {
		t.Fatalf("tail: %d lines, %d bytes", n, len(tail))
	}
	long := strings.Repeat("y", 20000)
	if got := wire.JobTail(long); len(got) != wire.MaxJobTailBytes {
		t.Fatalf("one long line: %d bytes", len(got))
	}
}
