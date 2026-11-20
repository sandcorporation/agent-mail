package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/presence"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// noSessionNotices lists the [미수신] mail stored so far as "<to> <subject>".
func noSessionNotices(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT recipient,subject FROM messages WHERE sender=? AND idem LIKE 'no-session:%' ORDER BY id`, identity.SystemSender)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var to, subject string
		if err = rows.Scan(&to, &subject); err != nil {
			t.Fatal(err)
		}
		out = append(out, to+" "+subject)
	}
	return out
}

func sweepNoSessionAt(t *testing.T, s *Server, clock *fakeClock, at time.Time) int {
	t.Helper()
	clock.add(at.Sub(clock.now()))
	return sweepOnce(t, s).NoSession
}

// Pending mail to a role with no session, past the grace period: one notice
// to the sender, and a second sweep sends nothing more (#300).
func TestNoSessionNoticeOncePerStretch(t *testing.T) {
	s, clock := slotsWithRoles(t)
	first := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do X"})
	apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do Y"})
	if n := sweepNoSessionAt(t, s, clock, first.CreatedAt.Add(presence.NoSessionGrace-time.Minute)); n != 0 {
		t.Fatalf("before the grace period: sent %d", n)
	}
	if n := sweepNoSessionAt(t, s, clock, first.CreatedAt.Add(presence.NoSessionGrace)); n != 1 {
		t.Fatalf("past the grace period: sent %d, want 1", n)
	}
	want := []string{"hq/boss [미수신] team/worker 붙은 세션 없음 — #" + itoa(first.ID) + " 외 1통"}
	if got := noSessionNotices(t, s); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("notices %q, want %q", got, want)
	}
	if n := sweepNoSessionAt(t, s, clock, first.CreatedAt.Add(time.Hour)); n != 0 {
		t.Fatalf("second sweep sent %d", n)
	}
}

// A session that attaches before the grace period is over: no notice.
func TestNoSessionNoNoticeWhenASessionAttaches(t *testing.T) {
	s, clock := slotsWithRoles(t)
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do X"})
	clock.add(time.Minute)
	slotDo(t, s, "register", ident("m", "/w"))
	if n := sweepNoSessionAt(t, s, clock, d.CreatedAt.Add(presence.NoSessionGrace+time.Minute)); n != 0 {
		t.Fatalf("a session is attached: sent %d", n)
	}
}

// A session that left within the grace period is a restart: no notice yet.
func TestNoSessionNoNoticeRightAfterARelease(t *testing.T) {
	s, clock := slotsWithRoles(t)
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do X"})
	slotDo(t, s, "register", ident("m", "/w"))
	clock.add(presence.NoSessionGrace)
	slotDo(t, s, "release", ident("m", "/w"))
	if n := sweepNoSessionAt(t, s, clock, clock.now().Add(time.Minute)); n != 0 {
		t.Fatalf("just released: sent %d", n)
	}
	if n := sweepNoSessionAt(t, s, clock, d.CreatedAt.Add(2*presence.NoSessionGrace+2*time.Minute)); n != 1 {
		t.Fatalf("released long ago: sent %d, want 1", n)
	}
}

// Read mail is not pending: no notice.
func TestNoSessionNoNoticeForReadMail(t *testing.T) {
	s, clock := slotsWithRoles(t)
	d := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "do X"})
	readAs(t, s, "team/worker", d.ID)
	if n := sweepNoSessionAt(t, s, clock, d.CreatedAt.Add(time.Hour)); n != 0 {
		t.Fatalf("read mail: sent %d", n)
	}
}

// The send response says whether the recipient role has a session (#300).
func TestSendReportsNoSession(t *testing.T) {
	s, clock := slotsWithRoles(t)
	send := func() wire.Message {
		t.Helper()
		clock.add(10 * time.Second)
		w := callAs(s, "POST", "/api/messages", tokenFor("hq/boss"), "hq/boss", map[string]any{"recipient": "team/worker", "subject": "hi"})
		var m wire.Message
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &m) != nil {
			t.Fatalf("send: %d %s", w.Code, w.Body)
		}
		return m
	}
	if !send().NoSession {
		t.Fatal("no session attached: no_session not set")
	}
	slotDo(t, s, "register", ident("m", "/w"))
	if send().NoSession {
		t.Fatal("a session is attached: no_session set")
	}
}

// switchboard send prints one stderr line when no session of the recipient is
// attached, and nothing extra when one is (#300).
func TestCLISendNotesNoSession(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "a")
	const note = "no session of b is attached; the mail waits in its inbox"
	r := h.run(nil, "send", "--to", "b", "--subject", "x", "--body", "y")
	if r.code != 0 || !strings.Contains(r.stderr, note) {
		t.Fatalf("no session: %+v", r)
	}
	if w := callAs(h.s, "POST", "/api/slots/register", "secret-b", "b", ident("m", "/w")); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	r = h.run(nil, "send", "--to", "b", "--subject", "x2", "--body", "y")
	if r.code != 0 || strings.Contains(r.stderr, "no session") {
		t.Fatalf("session attached: %+v", r)
	}
}
