package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func TestJobDefaultExpiryByKind(t *testing.T) {
	s := testServer(t)
	w, code := createJob(t, s, "secret-a", map[string]any{"kind": "watch", "command": "tail -f log", "cwd": "/w", "machine": "partner-a"}, "")
	if code != 201 || w.ExpiresAt.Sub(w.CreatedAt) != wire.DefaultWatchExpiry {
		t.Fatalf("watch default expiry: %d %v", code, w.ExpiresAt.Sub(w.CreatedAt))
	}
	c, _ := createJob(t, s, "secret-a", map[string]any{"command": "make", "cwd": "/w", "machine": "partner-a", "expires_in": 90}, "")
	if c.ExpiresAt.Sub(c.CreatedAt) != 90*time.Second {
		t.Fatalf("--expires: %v", c.ExpiresAt.Sub(c.CreatedAt))
	}
}

// A watch's expiry moves to now + its original duration at each sweep while
// a session of the owner role is attached; a command job's never does.
func TestJobAutoExtendsOnlyWhileAttached(t *testing.T) {
	s := testServer(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	watch, _ := createJob(t, s, "secret-a", map[string]any{"kind": "watch", "command": "tail -f log", "cwd": "/w", "machine": "partner-a", "expires_in": 3600}, "")
	command, _ := createJob(t, s, "secret-a", map[string]any{"command": "make", "cwd": "/w", "machine": "partner-a", "expires_in": 3600}, "")
	other, _ := createJob(t, s, "secret-b", map[string]any{"kind": "watch", "command": "tail -f log", "cwd": "/w", "machine": "partner-a", "expires_in": 3600}, "")
	ended, _ := createJob(t, s, "secret-a", map[string]any{"kind": "watch", "command": "tail -f x", "cwd": "/w", "machine": "partner-a", "expires_in": 3600}, "")
	call(s, "POST", "/api/jobs/"+itoa(ended.ID)+"/report", "secret-a", map[string]any{"state": "exited", "exit_code": 0, "watched": true}, "")
	expires := func(id int64) time.Time {
		t.Helper()
		j, err := s.jobs().Get(id)
		if err != nil {
			t.Fatal(err)
		}
		return j.ExpiresAt
	}
	// a has a live slot; b has none.
	if _, err := s.db.Exec(`INSERT INTO slots(base,number,machine,directory,registered_at,lease_until) VALUES('a',1,'partner-a','/w',?,?)`,
		now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(40 * time.Minute)
	if _, err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := expires(watch.ID); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("attached watch: expires %v want %v", got, now.Add(time.Hour))
	}
	if got := expires(command.ID); !got.Equal(command.ExpiresAt) {
		t.Fatalf("command moved: %v", got)
	}
	if got := expires(other.ID); !got.Equal(other.ExpiresAt) {
		t.Fatalf("unattached watch moved: %v", got)
	}
	if got := expires(ended.ID); !got.Equal(ended.ExpiresAt) {
		t.Fatalf("ended watch moved: %v", got)
	}
	// The lease lapses: no further extension.
	moved := expires(watch.ID)
	now = now.Add(2 * time.Hour)
	if _, err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := expires(watch.ID); !got.Equal(moved) {
		t.Fatalf("watch moved with no session attached: %v want %v", got, moved)
	}
}

// A watch created with no_extend keeps its expiry while a session is
// attached; the same watch without the flag moves, and both say so.
func TestJobNoExtendKeepsExpiry(t *testing.T) {
	s := testServer(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	fixed, _ := createJob(t, s, "secret-a", map[string]any{"kind": "watch", "command": "tail -f log", "cwd": "/w", "machine": "partner-a", "expires_in": 3600, "no_extend": true}, "")
	moving, _ := createJob(t, s, "secret-a", map[string]any{"kind": "watch", "command": "tail -f log", "cwd": "/w", "machine": "partner-a", "expires_in": 3600}, "")
	if fixed.Extends || !moving.Extends {
		t.Fatalf("extends: fixed %v moving %v", fixed.Extends, moving.Extends)
	}
	if _, err := s.db.Exec(`INSERT INTO slots(base,number,machine,directory,registered_at,lease_until) VALUES('a',1,'partner-a','/w',?,?)`,
		now.UnixMilli(), now.Add(24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	if _, err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	f, _ := s.jobs().Get(fixed.ID)
	m, _ := s.jobs().Get(moving.ID)
	if !f.ExpiresAt.Equal(fixed.ExpiresAt) || f.Extends {
		t.Fatalf("fixed watch moved: %v want %v", f.ExpiresAt, fixed.ExpiresAt)
	}
	if !m.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("moving watch: %v", m.ExpiresAt)
	}
}

// An expiry is one urgent mail even when a waiter saw it.
func TestJobExpiryMailsOnce(t *testing.T) {
	s := testServer(t)
	j, _ := createJob(t, s, "secret-a", map[string]any{"command": "sleep 99", "label": "slow", "cwd": "/w", "machine": "partner-a"}, "")
	for i := 0; i < 2; i++ {
		call(s, "POST", "/api/jobs/"+itoa(j.ID)+"/report", "secret-a", map[string]any{"state": "expired", "exit_code": 143, "watched": true}, "")
	}
	got, _ := s.jobs().Get(j.ID)
	m := jobMail(t, s, "a")
	if got.State != wire.JobExpired || len(m) != 1 || m[0].Subject != "[작업 끝남] slow 만료" || m[0].Priority != "urgent" {
		t.Fatalf("expiry: %+v mail %+v", got, m)
	}
}

func TestAdminJobsListStopAndRemove(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	run, _ := createJob(t, s, "secret-a", map[string]any{"command": "sleep 60", "label": "slow", "cwd": "/w", "machine": "partner-a"}, "")
	watch, _ := createJob(t, s, "secret-b", map[string]any{"kind": "watch", "command": "tail -f log", "cwd": "/w", "machine": "partner-b"}, "")
	done, _ := createJob(t, s, "secret-a", map[string]any{"command": "true", "cwd": "/w", "machine": "partner-a"}, "")
	call(s, "POST", "/api/jobs/"+itoa(done.ID)+"/report", "secret-a", map[string]any{"state": "exited", "exit_code": 0, "tail": "one\nlast line", "watched": true}, "")
	list := func(query string) []wire.Job {
		t.Helper()
		w := adminRequest(s, "GET", "/api/admin/jobs"+query, "", nil)
		var out []wire.Job
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("list %s: %d %s", query, w.Code, w.Body)
		}
		return out
	}
	if all := list(""); len(all) != 3 || all[0].ID != done.ID || all[0].Tail != "one\nlast line" || all[0].Command != "" {
		t.Fatalf("all jobs (newest first, no command): %+v", all)
	}
	if got := list("?role=b"); len(got) != 1 || got[0].ID != watch.ID || got[0].Kind != wire.JobWatch {
		t.Fatalf("role filter: %+v", got)
	}
	if got := list("?state=exited"); len(got) != 1 || got[0].ID != done.ID {
		t.Fatalf("state filter: %+v", got)
	}
	if got := list("?state=live"); len(got) != 2 {
		t.Fatalf("live filter: %+v", got)
	}
	if w := adminRequest(s, "POST", "/api/admin/jobs/"+itoa(run.ID)+"/stop", "", nil); w.Code != 403 {
		t.Fatalf("stop without the write guard: %d", w.Code)
	}
	if w := adminWrite(s, "POST", "/api/admin/jobs/"+itoa(run.ID)+"/stop", ""); w.Code != 200 {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if j, _ := s.jobs().Get(run.ID); !j.StopRequested || j.State != wire.JobHanded {
		t.Fatalf("stop not recorded: %+v", j)
	}
	if w := adminWrite(s, "DELETE", "/api/admin/jobs/"+itoa(run.ID), ""); w.Code != 409 {
		t.Fatalf("remove a live job: %d", w.Code)
	}
	if w := adminWrite(s, "DELETE", "/api/admin/jobs/"+itoa(done.ID), ""); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if _, err := s.jobs().Get(done.ID); err == nil {
		t.Fatal("removed job still there")
	}
	if w := adminWrite(s, "POST", "/api/admin/jobs/999/stop", ""); w.Code != 404 {
		t.Fatalf("unknown job: %d", w.Code)
	}
	if w := doAuth(s, authReq{method: "GET", path: "/api/admin/jobs"}); w.Code != 401 {
		t.Fatalf("no admin: %d", w.Code)
	}
}
