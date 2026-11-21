package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// bossSend sends from hq/boss to to without moving the clock.
func bossSend(t *testing.T, s *Server, to, subject string) wire.Message {
	t.Helper()
	w := callAs(s, "POST", "/api/messages", tokenFor("hq/boss"), "hq/boss", map[string]any{"recipient": to, "subject": subject})
	var m wire.Message
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &m) != nil {
		t.Fatalf("send to %s: %d %s", to, w.Code, w.Body)
	}
	return m
}

// heldNotices lists the subjects of the [미수신] held-mail notices to box.
func heldNotices(t *testing.T, s *Server, box, kind string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT subject FROM messages WHERE sender=? AND recipient=? AND idem LIKE ? ORDER BY id`, identity.SystemSender, box, kind+":%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var subject string
		if err = rows.Scan(&subject); err != nil {
			t.Fatal(err)
		}
		out = append(out, subject)
	}
	return out
}

// A session's unread mail is held for it when its lease ends: the next
// session that gets the number (another folder) does not see it, the sender
// is told once, and the session gets it when it comes back (#354).
func TestHandoffHoldsEndedSlotMail(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, role, box string
		// keep registers a live slot 1 first, so the ended session holds number 2.
		keep bool
	}{
		{"slot 1", "team/worker-1", "team/worker", false},
		{"slot 2", "team/worker-2", "team/worker-2", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, clock := slotsWithRoles(t)
			if c.keep {
				slotDo(t, s, "register", ident("m", "/keep"))
			}
			if g := slotDo(t, s, "register", ident("m", "/x")); g.Role != c.role {
				t.Fatalf("A got %s, want %s", g.Role, c.role)
			}
			d := bossSend(t, s, c.box, "do X")
			clock.add(2 * time.Minute) // A's lease (1m) ends
			if c.keep {
				slotDo(t, s, "heartbeat", ident("m", "/keep"))
			}
			if g := slotDo(t, s, "register", ident("m", "/y")); g.Role != c.role {
				t.Fatalf("B got %s, want %s", g.Role, c.role)
			}
			if got := inboxSubjects(t, s, c.box); len(got) != 0 {
				t.Fatalf("B's inbox has A's mail: %v", got)
			}
			want := []string{"[미수신] " + c.box + " 받는 세션 없음 — #" + itoa(d.ID) + " do X"}
			if got := heldNotices(t, s, "hq/boss", "handoff-held"); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("sender notices %q, want %q", got, want)
			}
			// A comes back (its number went to B) and gets the mail.
			g := slotDo(t, s, "register", ident("m", "/x"))
			if got := inboxSubjects(t, s, g.Role); strings.Join(got, ",") != "do X" {
				t.Fatalf("A (%s) inbox: %v", g.Role, got)
			}
			if got := inboxSubjects(t, s, c.box); len(got) != 0 {
				t.Fatalf("B's inbox: %v", got)
			}
			if got := heldNotices(t, s, "hq/boss", "handoff-held"); len(got) != 1 {
				t.Fatalf("sender told again: %q", got)
			}
		})
	}
}

// Mail sent to a mailbox whose session ended is held at once; the send
// response names the mailbox state: live, ended and held, or (base with an
// empty slot 1) the live numbered slots (#354).
func TestSendReportsRecipientState(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)

	never := bossSend(t, s, "team/worker", "first")
	if st := never.RecipientState; st == nil || st.Live || st.Held || st.Mailbox != "team/worker" || never.Status != "pending" {
		t.Fatalf("never attached: %+v %+v", never, st)
	}

	slotDo(t, s, "register", ident("m", "/x")) // team/worker, takes "first"
	live := bossSend(t, s, "team/worker", "live")
	if st := live.RecipientState; st == nil || !st.Live || st.Held || len(st.LiveSlots) != 0 {
		t.Fatalf("live: %+v", st)
	}

	slotDo(t, s, "register", ident("m", "/y")) // team/worker-2
	clock.add(50 * time.Second)
	slotDo(t, s, "heartbeat", ident("m", "/y"))
	clock.add(5*time.Minute - 50*time.Second)   // /x's lease ended 4m ago
	slotDo(t, s, "heartbeat", ident("m", "/y")) // holds /x's unread "first" and "live"
	ended := bossSend(t, s, "team/worker", "ended")
	st := ended.RecipientState
	if st == nil || st.Live || !st.Held || st.EndedS != 4*60 || ended.Status != "held" || ended.AddressedTo != "team/worker" {
		t.Fatalf("ended: %+v %+v", ended, st)
	}
	if strings.Join(st.LiveSlots, ",") != "team/worker-2" {
		t.Fatalf("live slots: %v", st.LiveSlots)
	}
	if got := heldNotices(t, s, "hq/boss", "handoff-held"); len(got) != 3 || !strings.Contains(got[2], "#"+itoa(ended.ID)+" ended") {
		t.Fatalf("sender notices: %q", got)
	}
	if got := inboxSubjects(t, s, "team/worker"); len(got) != 0 {
		t.Fatalf("team/worker inbox: %v", got)
	}
	g := slotDo(t, s, "register", ident("m", "/x"))
	if got := inboxSubjects(t, s, g.Role); strings.Join(got, ",") != "ended,live,first" { // newest first
		t.Fatalf("A (%s) inbox: %v", g.Role, got)
	}
}

// Held mail that no session takes within SWITCHBOARD_HANDOFF_EXPIRE is
// ended: complete where it is, kept, and its sender gets one line (#354).
func TestHandoffExpires(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)
	s.handoffTTL = time.Hour
	slotDo(t, s, "register", ident("m", "/x"))
	clock.add(2 * time.Minute)
	m := bossSend(t, s, "team/worker", "late")
	if m.Status != "held" {
		t.Fatalf("not held: %+v", m)
	}
	clock.add(30 * time.Minute)
	if n := sweepOnce(t, s).Handoffs; n != 0 {
		t.Fatalf("before the limit: ended %d", n)
	}
	clock.add(31 * time.Minute)
	if n := sweepOnce(t, s).Handoffs; n != 1 {
		t.Fatalf("past the limit: ended %d, want 1", n)
	}
	if n := sweepOnce(t, s).Handoffs; n != 0 {
		t.Fatalf("second sweep ended %d", n)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM messages WHERE id=?`, m.ID).Scan(&status); err != nil || status != "complete" {
		t.Fatalf("expired mail: %q %v", status, err)
	}
	if got := heldNotices(t, s, "hq/boss", "handoff-expired"); len(got) != 1 || !strings.HasPrefix(got[0], "[미수신] #"+itoa(m.ID)+" 끝냄") {
		t.Fatalf("expiry notices: %q", got)
	}
	g := slotDo(t, s, "register", ident("m", "/x"))
	if got := inboxSubjects(t, s, g.Role); len(got) != 0 {
		t.Fatalf("expired mail delivered: %v", got)
	}
}

// The [미수신] sweep looks at the receiving mailbox: mail to a slot with no
// session is noticed even while another slot of the base is live (#354).
func TestNoSessionNoticePerMailbox(t *testing.T) {
	t.Parallel()
	s, clock := slotsWithRoles(t)
	s.presenceCfg.SlotLease = time.Hour
	slotDo(t, s, "register", ident("m", "/x")) // team/worker
	slotDo(t, s, "register", ident("m", "/y")) // team/worker-2
	slotDo(t, s, "release", ident("m", "/x"))
	clock.add(10 * time.Minute)
	d := bossSend(t, s, "team/worker-3", "nobody") // never had a session: stays
	if d.Status != "pending" {
		t.Fatalf("mail to a never-attached slot: %+v", d)
	}
	if n := sweepNoSessionAt(t, s, clock, clock.now().Add(10*time.Minute)); n != 1 {
		t.Fatalf("notices: %d, want 1", n)
	}
	if got := noSessionNotices(t, s); len(got) != 1 || !strings.Contains(got[0], "team/worker-3 붙은 세션 없음") {
		t.Fatalf("notices %q", got)
	}
}
