package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func watchMail(t *testing.T, s *Server, to string) []wire.Message {
	t.Helper()
	rows, err := s.db.Query(`SELECT subject, body, priority FROM messages WHERE recipient=? AND subject LIKE '[감시]%' ORDER BY id`, to)
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

// A batch of watch lines is one urgent mail, once per batch number; only a
// running watch takes lines.
func TestWatchLinesMailOncePerBatch(t *testing.T) {
	s := testServer(t)
	w, code := createJob(t, s, "secret-a", map[string]any{"kind": "watch", "command": "tail -f app.log", "label": "app log", "cwd": "/w", "machine": "partner-a"}, "")
	if code != 201 || w.Kind != wire.JobWatch || w.ExpiresAt.Sub(w.CreatedAt) != wire.DefaultWatchExpiry {
		t.Fatalf("create watch: %d %+v", code, w)
	}
	path := "/api/jobs/" + itoa(w.ID)
	batch := map[string]any{"seq": 1, "count": 5, "lines": "e1\ne2\ne3\ne4\ne5"}
	if r := call(s, "POST", path+"/lines", "secret-a", batch, ""); r.Code != 409 {
		t.Fatalf("lines before running: %d", r.Code)
	}
	call(s, "POST", path+"/report", "secret-a", map[string]any{"state": "running"}, "")
	for range 2 { // a retried batch sends no second mail
		if r := call(s, "POST", path+"/lines", "secret-a", batch, ""); r.Code != 200 {
			t.Fatalf("lines: %d %s", r.Code, r.Body)
		}
	}
	m := watchMail(t, s, "a")
	if len(m) != 1 || m[0].Subject != "[감시] app log: 5줄" || m[0].Priority != "urgent" || !strings.HasSuffix(m[0].Body, "e1\ne2\ne3\ne4\ne5") {
		t.Fatalf("watch mail: %+v", m)
	}
	j, _ := s.jobs().Get(w.ID)
	if j.Tail != "e1\ne2\ne3\ne4\ne5" {
		t.Fatalf("tail after lines: %q", j.Tail)
	}
	// More lines than the cap: the body keeps the first ones and says how
	// many were left out; the subject counts them all.
	long := strings.Repeat("x\n", wire.MaxJobTailLines+20)
	call(s, "POST", path+"/lines", "secret-a", map[string]any{"seq": 2, "count": wire.MaxJobTailLines + 20, "lines": long}, "")
	m = watchMail(t, s, "a")
	if len(m) != 2 || m[1].Subject != "[감시] app log: 70줄" || !strings.HasSuffix(m[1].Body, "(20줄 생략)") || strings.Count(m[1].Body, "x\n") != wire.MaxJobTailLines {
		t.Fatalf("capped mail: %+v", m[1])
	}
	if r := call(s, "POST", path+"/lines", "secret-b", map[string]any{"seq": 3, "count": 1, "lines": "y"}, ""); r.Code != 404 {
		t.Fatalf("another role's lines: %d", r.Code)
	}
	cmd, _ := createJob(t, s, "secret-a", map[string]any{"command": "make", "cwd": "/w", "machine": "partner-a"}, "")
	call(s, "POST", "/api/jobs/"+itoa(cmd.ID)+"/report", "secret-a", map[string]any{"state": "running"}, "")
	if r := call(s, "POST", "/api/jobs/"+itoa(cmd.ID)+"/lines", "secret-a", map[string]any{"seq": 1, "count": 1, "lines": "y"}, ""); r.Code != 400 {
		t.Fatalf("lines for a command job: %d", r.Code)
	}
}

// A running report carries the output tail, so the dashboard shows the last
// line before the job ends; the job stays running.
func TestRunningReportUpdatesTheTail(t *testing.T) {
	s := testServer(t)
	j, _ := createJob(t, s, "secret-a", map[string]any{"command": "make", "cwd": "/w", "machine": "partner-a"}, "")
	path := "/api/jobs/" + itoa(j.ID) + "/report"
	call(s, "POST", path, "secret-a", map[string]any{"state": "running"}, "")
	first, _ := s.jobs().Get(j.ID)
	r := call(s, "POST", path, "secret-a", map[string]any{"state": "running", "tail": "step 1\nstep 2\n"}, "")
	var got wire.Job
	_ = json.Unmarshal(r.Body.Bytes(), &got)
	if got.State != wire.JobRunning || got.Tail != "step 1\nstep 2" || !got.StartedAt.Equal(*first.StartedAt) {
		t.Fatalf("running with tail: %+v", got)
	}
	call(s, "POST", path, "secret-a", map[string]any{"state": "exited", "exit_code": 0, "tail": "done", "watched": true}, "")
	call(s, "POST", path, "secret-a", map[string]any{"state": "running", "tail": "late"}, "")
	if end, _ := s.jobs().Get(j.ID); end.State != wire.JobExited || end.Tail != "done" {
		t.Fatalf("a late running report changed an ended job: %+v", end)
	}
}
