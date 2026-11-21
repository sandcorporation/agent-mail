package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/mail"
)

type pollResult struct {
	status  int
	header  http.Header
	body    string
	resp    mail.PollResponse
	elapsed time.Duration
}

func poll(t *testing.T, ctx context.Context, base, token, role, query string, header map[string]string) pollResult {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events/poll"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Switchboard-Role", role)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := pollResult{status: resp.StatusCode, header: resp.Header, body: string(b), elapsed: time.Since(start)}
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(b, &r.resp); err != nil {
			t.Fatalf("decode %q: %v", b, err)
		}
	}
	return r
}

func pollIDs(r pollResult) []int64 {
	ids := []int64{}
	for _, ev := range r.resp.Events {
		ids = append(ids, ev.ID)
	}
	return ids
}

// subscribers reports how many long polls or streams wait for role.
func subscribers(s *Server, role string) int {
	return s.hub.Subscribers(role)
}

func waitSubscribers(t *testing.T, s *Server, role string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for subscribers(s, role) != n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers for %s = %d, want %d", role, subscribers(s, role), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pollServer is eventServer with the long-poll cap and a shutdown channel set
// before the server starts.
func pollServer(t *testing.T, max time.Duration) (*Server, string) {
	s := testServer(t)
	s.mailCfg.PollMax = max
	s.done = make(chan struct{})
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('c','secret-c','now')`); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(secureHeaders(http.HandlerFunc(s.api)))
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func TestPollAnswersAtOnceWithPendingAndResumesByCursor(t *testing.T) {
	t.Parallel()
	s, base := eventServer(t)
	first := sendMail(t, s, "secret-a", "b", "one")
	sendMail(t, s, "secret-a", "c", "not-for-b")
	second := sendMail(t, s, "secret-a", "b", "two")
	r := poll(t, context.Background(), base, "secret-b", "b", "?timeout=30", nil)
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d headers=%v body=%s", r.status, r.header, r.body)
	}
	if ids := pollIDs(r); len(ids) != 2 || ids[0] != first || ids[1] != second || r.resp.Cursor != second || r.elapsed > 2*time.Second {
		t.Fatalf("ids=%v cursor=%d elapsed=%s", ids, r.resp.Cursor, r.elapsed)
	}
	// Same event shape as the stream: summary without body for normal mail.
	var raw struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal([]byte(r.body), &raw)
	ev := raw.Events[0]
	if ev["sender"] != "a" || ev["recipient"] != "b" || ev["subject"] != "one" || ev["priority"] != "normal" || ev["task_id"] != "T-1" || ev["created_at"] == nil {
		t.Fatalf("event=%v", ev)
	}
	if _, has := ev["body"]; has {
		t.Fatalf("normal mail leaked body: %v", ev)
	}
	// Cursor resume: ?after= and Last-Event-ID (header wins), timeout 0 answers at once.
	if r := poll(t, context.Background(), base, "secret-b", "b", "?after="+itoa(first)+"&timeout=0", nil); len(r.resp.Events) != 1 || r.resp.Events[0].ID != second || r.resp.Cursor != second {
		t.Fatalf("after=first: %s", r.body)
	}
	if r := poll(t, context.Background(), base, "secret-b", "b", "?after=0&timeout=0", map[string]string{"Last-Event-ID": itoa(second)}); r.status != 200 || len(r.resp.Events) != 0 || r.resp.Cursor != second || r.body != `{"events":[],"cursor":`+itoa(second)+"}\n" {
		t.Fatalf("header cursor: %d %q", r.status, r.body)
	}
	// Polling has no state side effects; read mail drops out of the result.
	call(s, "GET", "/api/messages/"+itoa(first), "secret-b", nil, "")
	if r := poll(t, context.Background(), base, "secret-b", "b", "?timeout=0", nil); len(r.resp.Events) != 1 || r.resp.Events[0].ID != second {
		t.Fatalf("after read: %s", r.body)
	}
	// No cursor and nothing pending: cursor 0.
	if r := poll(t, context.Background(), base, "secret-a", "a", "?timeout=0", nil); r.body != "{\"events\":[],\"cursor\":0}\n" {
		t.Fatalf("empty: %q", r.body)
	}
}

func TestPollWaitsForNewMailThenTimesOut(t *testing.T) {
	t.Parallel()
	s, base := eventServer(t)
	old := sendMail(t, s, "secret-a", "b", "old")
	done := make(chan pollResult, 1)
	go func() {
		done <- poll(t, context.Background(), base, "secret-b", "b", "?after="+itoa(old)+"&timeout=30", nil)
	}()
	waitSubscribers(t, s, "b", 1)
	sendMail(t, s, "secret-a", "c", "other role does not wake b")
	select {
	case r := <-done:
		t.Fatalf("answered before mail for b: %s", r.body)
	case <-time.After(100 * time.Millisecond):
	}
	id := sendMail(t, s, "secret-a", "b", "new")
	select {
	case r := <-done:
		if ids := pollIDs(r); len(ids) != 1 || ids[0] != id || r.resp.Cursor != id {
			t.Fatalf("woken poll: %s", r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("poll not woken by send")
	}
	waitSubscribers(t, s, "b", 0)

	// Timeout: empty list, cursor echoed, after about the requested wait.
	r := poll(t, context.Background(), base, "secret-b", "b", "?after="+itoa(id)+"&timeout=1", nil)
	if r.status != 200 || len(r.resp.Events) != 0 || r.resp.Cursor != id || r.elapsed < 900*time.Millisecond || r.elapsed > 3*time.Second {
		t.Fatalf("timeout poll: %d %s in %s", r.status, r.body, r.elapsed)
	}
}

func TestPollCapsWait(t *testing.T) {
	t.Parallel()
	s, base := pollServer(t, time.Second)
	// A request above the cap (and an absurd one, and the default) waits only up to the cap.
	for _, q := range []string{"?timeout=600", "?timeout=99999999999999", ""} {
		r := poll(t, context.Background(), base, "secret-b", "b", q, nil)
		if r.status != 200 || len(r.resp.Events) != 0 || r.elapsed < 900*time.Millisecond || r.elapsed > 3*time.Second {
			t.Fatalf("%s: %d %s in %s", q, r.status, r.body, r.elapsed)
		}
	}
	waitSubscribers(t, s, "b", 0)
}

func TestPollEndsOnDisconnectAndShutdown(t *testing.T) {
	t.Parallel()
	s, base := pollServer(t, time.Hour)
	// Client disconnect releases the subscription.
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events/poll?timeout=3000", nil)
		req.Header.Set("Authorization", "Bearer secret-b")
		req.Header.Set("X-Switchboard-Role", "b")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	waitSubscribers(t, s, "b", 1)
	cancel()
	<-gone
	waitSubscribers(t, s, "b", 0)
	// Server shutdown answers a waiting poll with an empty list.
	done := make(chan pollResult, 1)
	go func() { done <- poll(t, context.Background(), base, "secret-b", "b", "?timeout=3000", nil) }()
	waitSubscribers(t, s, "b", 1)
	close(s.done)
	select {
	case r := <-done:
		if r.status != 200 || len(r.resp.Events) != 0 {
			t.Fatalf("shutdown: %d %s", r.status, r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("poll not ended by shutdown")
	}
	waitSubscribers(t, s, "b", 0)
}

func TestPollRejectsBadCredentialsCursorAndTimeout(t *testing.T) {
	t.Parallel()
	_, base := eventServer(t)
	if r := poll(t, context.Background(), base, "wrong", "b", "", nil); r.status != 401 {
		t.Fatalf("bad token=%d", r.status)
	}
	if r := poll(t, context.Background(), base, "secret-a", "b", "", nil); r.status != 401 {
		t.Fatalf("other role's token=%d", r.status)
	}
	for _, q := range []string{"?after=x", "?after=-1", "?after=1.5", "?timeout=-1", "?timeout=abc", "?timeout=1.5"} {
		if r := poll(t, context.Background(), base, "secret-b", "b", q, nil); r.status != 400 {
			t.Fatalf("%s=%d %s", q, r.status, r.body)
		}
	}
	if r := poll(t, context.Background(), base, "secret-b", "b", "?after=1&timeout=0", map[string]string{"Last-Event-ID": "nope"}); r.status != 400 {
		t.Fatalf("bad header=%d", r.status)
	}
}

func TestPollUrgentEventCarriesBody(t *testing.T) {
	t.Parallel()
	s, base := eventServer(t)
	if w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "fire", "body": "now", "priority": "urgent"}, ""); w.Code != 201 {
		t.Fatal(w.Body)
	}
	r := poll(t, context.Background(), base, "secret-b", "b", "?timeout=0", nil)
	if len(r.resp.Events) != 1 || r.resp.Events[0].Priority != "urgent" || r.resp.Events[0].Body == nil || *r.resp.Events[0].Body != "now" {
		t.Fatalf("urgent: %s", r.body)
	}
}

func TestEventsCommand(t *testing.T) {
	t.Parallel()
	h := newCLIHarnessWith(t, func(s *Server) { s.mailCfg.PollMax = 2 * time.Second })
	h.configure(nil, "b")
	r := h.run(nil, "events", "--timeout", "0")
	if r.code != cli.ExitTimeout || r.stdout != "" || !strings.Contains(r.stderr, "cursor 0") {
		t.Fatalf("empty: code=%d stdout=%q stderr=%s", r.code, r.stdout, r.stderr)
	}
	if r := h.run(nil, "events", "--timeout", "0", "--json"); r.code != cli.ExitTimeout || r.stdout != `{"events":[],"cursor":0}` {
		t.Fatalf("empty json: code=%d stdout=%q", r.code, r.stdout)
	}
	call(h.s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "one\nline", "body": "x", "task_id": "t-1"}, "")
	call(h.s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "hot", "body": "x", "priority": "urgent"}, "")
	if out := h.mustRun(nil, "events"); out != "#1 a | one line | task=t-1\n#2 a | hot | urgent" {
		t.Fatalf("events output: %q", out)
	}
	if out := h.mustRun(nil, "events", "--after", "1", "--json"); !strings.Contains(out, `"cursor":2`) || !strings.Contains(out, `"id":2`) || strings.Contains(out, `"id":1,`) {
		t.Fatalf("events --json: %s", out)
	}
	// Waiting: woken by a send while the CLI waits.
	go func() {
		for subscribers(h.s, "b") == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		call(h.s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "late", "body": "x"}, "")
	}()
	if out := h.mustRun(nil, "events", "--after", "2", "--timeout", "30"); out != "#3 a | late" {
		t.Fatalf("woken events: %q", out)
	}
	// Server cap: a long --timeout ends at the cap with exit 3.
	start := time.Now()
	if r := h.run(nil, "events", "--after", "3", "--timeout", "600"); r.code != cli.ExitTimeout || time.Since(start) > 10*time.Second {
		t.Fatalf("capped: code=%d stderr=%s", r.code, r.stderr)
	}
	for _, args := range [][]string{{"events", "--after", "x"}, {"events", "--timeout", "-1"}, {"events", "--bogus"}} {
		if r := h.run(nil, args...); r.code != cli.ExitUsage {
			t.Fatalf("%v: code=%d", args, r.code)
		}
	}
	h.configure([]string{"--profile", "wrong"}, "b")
	if r := h.run([]string{"AGENT_MAIL_TOKEN=bad"}, "--profile", "wrong", "events", "--timeout", "0"); r.code != cli.ExitError {
		t.Fatalf("bad credential: code=%d stderr=%s", r.code, r.stderr)
	}
	if out := h.mustRun(nil, "events", "--help"); !strings.Contains(out, "--after") || !strings.Contains(out, "--timeout") {
		t.Fatalf("help: %s", out)
	}
}
