package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// closeAs posts /api/messages/{id}/close as role.
func closeAs(s *Server, role string, id int64) (int, string) {
	w := callAs(s, "POST", fmt.Sprintf("/api/messages/%d/close", id), tokenFor(role), role, nil)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func closedJSON(id int64) string { return fmt.Sprintf(`{"id":%d,"closed":true}`, id) }

// The sender closes its own directive (#232): it leaves progress at once,
// a second close answers the same, and nobody else may close it.
func TestCloseDirectiveBySender(t *testing.T) {
	s, clock := slotsWithRoles(t)
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('partner-a/coordinator','secret-partner-a/coordinator','now')`); err != nil {
		t.Fatal(err)
	}
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "answered by a plain send"})
	kept := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "still open"})
	reply := apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", kept.ID), map[string]any{"subject": "re", "body": "on it"})
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID}) {
		t.Fatalf("open %v before close, want %d", got, d.ID)
	}

	for _, role := range []string{"team/worker", "partner-a/coordinator"} {
		if code, body := closeAs(s, role, d.ID); code != 403 {
			t.Fatalf("close by %s: %d %s, want 403", role, code, body)
		}
	}
	if got := openIDs(t, s); !sameIDs(got, []int64{d.ID}) {
		t.Fatalf("open %v after refused closes, want %d", got, d.ID)
	}
	for i := 0; i < 2; i++ {
		if code, body := closeAs(s, "hq/boss", d.ID); code != 200 || body != closedJSON(d.ID) {
			t.Fatalf("close #%d by sender: %d %s", i+1, code, body)
		}
	}
	if got := openIDs(t, s); len(got) != 0 {
		t.Fatalf("open %v after close, want none", got)
	}
	if m, err := s.mail().Get(d.ID); err != nil || !m.NoReply {
		t.Fatalf("stored %+v %v, want no_reply", m, err)
	}
	// A plain reply is not a directive, even for its own sender.
	if code, body := closeAs(s, "team/worker", reply.ID); code != 400 {
		t.Fatalf("close plain reply: %d %s, want 400", code, body)
	}
	if code, _ := closeAs(s, "hq/boss", 9999); code != 404 {
		t.Fatalf("close missing: %d, want 404", code)
	}
}

// POST /api/admin/messages/{id}/close closes any sender's directive, behind
// the dashboard write guard.
func TestAdminCloseDirective(t *testing.T) {
	s, clock := slotsWithRoles(t)
	s.admin.ProxyToken = "proxy-secret"
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "a"})
	reply := apiMail(t, s, clock, "team/worker", fmt.Sprintf("/api/messages/%d/reply", d.ID), map[string]any{"subject": "re", "body": "x"})
	other := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "b"})
	path := fmt.Sprintf("/api/admin/messages/%d/close", other.ID)

	if w := adminRequest(s, "POST", path, "", nil); w.Code != 403 {
		t.Fatalf("admin close without the write guard: %d %s", w.Code, w.Body)
	}
	for i := 0; i < 2; i++ {
		w := adminWrite(s, "POST", path, "")
		var got wire.Closed
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got != (wire.Closed{ID: other.ID, Closed: true}) {
			t.Fatalf("admin close #%d: %d %s", i+1, w.Code, w.Body)
		}
	}
	if got := openIDs(t, s); len(got) != 0 {
		t.Fatalf("open %v after admin close, want none", got)
	}
	if w := adminWrite(s, "POST", fmt.Sprintf("/api/admin/messages/%d/close", reply.ID), ""); w.Code != 400 {
		t.Fatalf("admin close plain reply: %d %s, want 400", w.Code, w.Body)
	}
	if w := adminWrite(s, "POST", "/api/admin/messages/9999/close", ""); w.Code != 404 {
		t.Fatalf("admin close missing: %d %s, want 404", w.Code, w.Body)
	}
	if w := adminWrite(s, "POST", fmt.Sprintf("/api/admin/messages/%d/other", d.ID), ""); w.Code != 404 {
		t.Fatalf("admin unknown message action: %d %s, want 404", w.Code, w.Body)
	}
}

// A closed directive gets no further reminder nor sender notice: the
// reminder sweep reads the same no_reply flag as progress.
func TestRemindStopsOnClose(t *testing.T) {
	s, clock := slotsWithRoles(t)
	remindOn(s)
	sweepOnce(t, s)
	closed := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "closed"})
	open := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "open"})
	readAs(t, s, "team/worker", closed.ID)
	readAs(t, s, "team/worker", open.ID)
	start := open.CreatedAt
	if n := sweepAt(t, s, clock, start.Add(remindGap)); n != 2 {
		t.Fatalf("first round sent %d, want 2", n)
	}
	if code, body := closeAs(s, "hq/boss", closed.ID); code != 200 {
		t.Fatalf("close: %d %s", code, body)
	}
	for _, h := range []time.Duration{3, 7, 15, 40} {
		sweepAt(t, s, clock, start.Add(h*remindGap))
	}
	want := []string{
		fmt.Sprintf("team/worker remind:%d:1", closed.ID),
		fmt.Sprintf("team/worker remind:%d:1", open.ID),
		fmt.Sprintf("team/worker remind:%d:2", open.ID),
		fmt.Sprintf("team/worker remind:%d:3", open.ID),
		fmt.Sprintf("hq/boss remind-sender:%d", open.ID),
	}
	if got := remindKeys(t, s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reminders %v, want %v", got, want)
	}
}

func TestCLIClose(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure(nil, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "work", "--body", "x")
	if out := h.mustRun(nil, "--profile", "a", "close", "1"); out != "#1 closed" {
		t.Fatalf("close: %q", out)
	}
	var got wire.Closed
	if out := h.mustRun(nil, "--profile", "a", "close", "1", "--json"); json.Unmarshal([]byte(out), &got) != nil || got != (wire.Closed{ID: 1, Closed: true}) {
		t.Fatalf("close --json: %q", out)
	}
	if r := h.run(nil, "close", "1"); r.code == 0 || !strings.Contains(r.stderr, "403") {
		t.Fatalf("close by the recipient: exit %d %q %q", r.code, r.stdout, r.stderr)
	}
	var sent []wire.Message
	if out := h.mustRun(nil, "--profile", "a", "outbox", "--json"); json.Unmarshal([]byte(out), &sent) != nil || len(sent) != 1 || !sent[0].NoReply {
		t.Fatalf("outbox after close: %s", out)
	}
	if help := h.mustRun(nil, "close", "--help"); !strings.Contains(help, "close ID") || !strings.Contains(help, "--json") {
		t.Fatalf("close --help: %s", help)
	}
	if !strings.Contains(h.mustRun(nil, "--help"), "close") {
		t.Fatal("--help lacks close")
	}
}
