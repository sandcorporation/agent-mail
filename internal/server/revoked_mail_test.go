package server

import (
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// revokedServer is alertServer (a is the boss) with the admin proxy secret
// and an alias team/old for team/worker.
func revokedServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := alertServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := s.db.Exec(`INSERT INTO role_aliases(alias,role,created_at) VALUES('team/old','team/worker','now')`); err != nil {
		t.Fatal(err)
	}
	return s, clock
}

func setRevoked(t *testing.T, s *Server, role string, revoke bool) {
	t.Helper()
	action := "/restore"
	if revoke {
		action = "/revoke"
	}
	if w := adminWrite(s, "POST", "/api/admin/roles/"+role+action, ""); w.Code != 200 {
		t.Fatalf("%s %s: %d %s", role, action, w.Code, w.Body)
	}
}

func messageCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSendToRevokedRoleIsGone(t *testing.T) {
	t.Parallel()
	s, _ := revokedServer(t)
	setRevoked(t, s, "team/worker", true)
	before := messageCount(t, s)
	for _, to := range []string{"team/worker", "team/worker-1", "team/worker-2", "label:team/worker/22", "team/old"} {
		for _, key := range []string{"", "retry-" + to, "retry-" + to} {
			w := call(s, "POST", "/api/messages", "secret-b", map[string]any{"recipient": to, "subject": "hi"}, key)
			if w.Code != 410 || strings.TrimSpace(w.Body.String()) != "recipient role revoked at 2026-11-17T09:00:00Z" {
				t.Fatalf("send to %s (key %q) = %d %q", to, key, w.Code, w.Body)
			}
		}
	}
	if got := messageCount(t, s); got != before {
		t.Fatalf("refused sends stored mail: %d -> %d", before, got)
	}
	setRevoked(t, s, "team/worker", false)
	sendMail(t, s, "secret-b", "team/old", "after restore")
}

func pendingOf(t *testing.T, s *Server, role string) int {
	t.Helper()
	m, err := s.insight().Metrics(7)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Pending {
		if p.Role == role {
			return p.Pending
		}
	}
	return 0
}

func directivesTo(t *testing.T, s *Server, prefix string) int {
	t.Helper()
	n := 0
	for _, sv := range adminProgress(t, s).Services {
		for _, d := range sv.OpenDirectives {
			if strings.HasPrefix(d.To, prefix) {
				n++
			}
		}
	}
	return n
}

func TestRevokedRolePendingMailLeftOutUntilRestored(t *testing.T) {
	t.Parallel()
	s, clock := revokedServer(t)
	sendMail(t, s, "secret-b", "team/worker", "one")
	sendMail(t, s, "secret-b", "team/worker-2", "two")
	sendMail(t, s, "secret-a", "team/worker", "directive")
	if pendingOf(t, s, "team/worker") != 2 || pendingOf(t, s, "team/worker-2") != 1 || directivesTo(t, s, "team/worker") != 1 {
		t.Fatalf("before revoke: %d %d %d", pendingOf(t, s, "team/worker"), pendingOf(t, s, "team/worker-2"), directivesTo(t, s, "team/worker"))
	}
	setRevoked(t, s, "team/worker", true)
	if pendingOf(t, s, "team/worker") != 0 || pendingOf(t, s, "team/worker-2") != 0 || directivesTo(t, s, "team/worker") != 0 {
		t.Fatalf("revoked role still counted: %d %d %d", pendingOf(t, s, "team/worker"), pendingOf(t, s, "team/worker-2"), directivesTo(t, s, "team/worker"))
	}
	clock.add(3 * time.Hour)
	if got := sweepOnce(t, s); got.Alerts != 0 || got.NoSession != 0 {
		t.Fatalf("sweep over a revoked role's mail: %+v", got)
	}
	setRevoked(t, s, "team/worker", false)
	if pendingOf(t, s, "team/worker") != 2 || pendingOf(t, s, "team/worker-2") != 1 || directivesTo(t, s, "team/worker") != 1 {
		t.Fatalf("after restore: %d %d %d", pendingOf(t, s, "team/worker"), pendingOf(t, s, "team/worker-2"), directivesTo(t, s, "team/worker"))
	}
	if got := sweepOnce(t, s); got.Alerts != 2 { // team/worker and team/worker-2
		t.Fatalf("pending alert after restore: %+v", got)
	}
}

func revokeNotices(t *testing.T, s *Server, token, role string) []wire.Message {
	t.Helper()
	var out []wire.Message
	for _, m := range systemMail(t, s, token, role) {
		if strings.HasPrefix(m.Subject, "recipient revoked: ") {
			out = append(out, m)
		}
	}
	return out
}

func TestRevokeTellsEachSenderOnce(t *testing.T) {
	t.Parallel()
	s, clock := revokedServer(t)
	a1 := sendMail(t, s, "secret-a", "team/worker", "first")
	clock.add(time.Minute)
	a2 := sendMail(t, s, "secret-a", "team/worker-2", "second")
	b1 := sendMail(t, s, "secret-b", "team/old", "third")
	own := callAs(s, "POST", "/api/messages", workerToken, "team/worker", map[string]any{"recipient": "team/worker-2", "subject": "own"})
	if own.Code != 201 {
		t.Fatalf("own send: %d %s", own.Code, own.Body)
	}
	setRevoked(t, s, "team/worker", true)
	setRevoked(t, s, "team/worker", true) // a second revoke keeps the first time: no new notice

	na := revokeNotices(t, s, "secret-a", "a")
	nb := revokeNotices(t, s, "secret-b", "b")
	if len(na) != 1 || len(nb) != 1 {
		t.Fatalf("notices: a %d, b %d", len(na), len(nb))
	}
	if na[0].Subject != "recipient revoked: team/worker" {
		t.Fatalf("subject %q", na[0].Subject)
	}
	for _, c := range []struct {
		m    wire.Message
		want []string
		not  []string
	}{
		{na[0], []string{"#" + itoa(a1) + " first", "#" + itoa(a2) + " second", "2026-11-17T09:00:00Z", "will not be read"}, []string{"third", "own"}},
		{nb[0], []string{"#" + itoa(b1) + " third"}, []string{"first", "second", "own"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(c.m.Body, w) {
				t.Fatalf("notice to %s lacks %q:\n%s", c.m.Recipient, w, c.m.Body)
			}
		}
		for _, w := range c.not {
			if strings.Contains(c.m.Body, w) {
				t.Fatalf("notice to %s lists %q:\n%s", c.m.Recipient, w, c.m.Body)
			}
		}
	}
	// The mail itself stays, pending.
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient LIKE 'team/worker%' AND status='pending'`).Scan(&pending); err != nil || pending != 4 {
		t.Fatalf("revoked role's mail: %d %v", pending, err)
	}

	// Nothing pending: no notice.
	before := messageCount(t, s)
	setRevoked(t, s, "b", true)
	if got := messageCount(t, s); got != before {
		t.Fatalf("revoke with nothing pending stored %d mail", got-before)
	}
}
