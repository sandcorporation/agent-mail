package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

const (
	otherToken = "secret-other"
	adminToken = "admin-secret"
	label22    = "label:team/worker/22"
)

// labelServer is slotServer plus a second base team/other, the admin proxy
// secret and an HTTP listener for streams and polls.
func labelServer(t *testing.T) (*Server, *fakeClock, string) {
	t.Helper()
	s, clock := slotServer(t)
	s.admin.ProxyToken = adminToken
	s.mailCfg.KeepAlive = time.Hour
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/other',?,'now')`, otherToken); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(secureHeaders(http.HandlerFunc(s.api)))
	t.Cleanup(ts.Close)
	return s, clock, ts.URL
}

func labeled(machine, dir, label string) wire.SlotRequest {
	r := ident(machine, dir)
	r.Label = label
	return r
}

// sendFrom sends as role a and returns the stored message.
func sendFrom(t *testing.T, s *Server, to, subject, idem string, want int) wire.Message {
	t.Helper()
	w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": to, "subject": subject, "body": "b"}, idem)
	if w.Code != want {
		t.Fatalf("send to %q: %d %s", to, w.Code, w.Body)
	}
	var m wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m
}

func listAs(t *testing.T, s *Server, token, role, box string) []wire.Message {
	t.Helper()
	w := callAs(s, "GET", "/api/messages?box="+box, token, role, nil)
	if w.Code != 200 {
		t.Fatalf("%s %s: %d %s", role, box, w.Code, w.Body)
	}
	var out []wire.Message
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func adminMessages(t *testing.T, s *Server, query string) []wire.Message {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/admin/messages"+query, nil)
	r.Header.Set("X-Switchboard-Admin", adminToken)
	w := httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 200 {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
	var out []wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func heldCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE status='held'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLabelMailIsHeldUntilTheLabelConnects(t *testing.T) {
	t.Parallel()
	s, _, base := labelServer(t)
	slotDo(t, s, "register", ident("m", "/plain")) // team/worker-1, no label

	held := sendFrom(t, s, label22, "brief", "", 201)
	if held.Recipient != label22 || held.Status != "held" || held.AddressedTo != label22 || held.HeldAt == nil {
		t.Fatalf("held message: %+v", held)
	}
	for _, role := range []string{"team/worker", "team/worker-1", "team/worker-2"} {
		if got := listAs(t, s, workerToken, role, "inbox"); len(got) != 0 {
			t.Fatalf("%s inbox shows held mail: %+v", role, got)
		}
	}
	if out := listAs(t, s, "secret-a", "a", "outbox"); len(out) != 1 || out[0].Recipient != label22 || out[0].Status != "held" {
		t.Fatalf("outbox while held: %+v", out)
	}
	if got := adminMessages(t, s, "?box=held"); len(got) != 1 || got[0].ID != held.ID {
		t.Fatalf("admin held list: %+v", got)
	}

	// A later message to team/worker-2 moves that mailbox's cursor past the
	// held message's ID; the moved message must still reach the poll.
	direct := sendFrom(t, s, "team/worker-2", "direct", "", 201)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan pollResult, 1)
	go func() {
		got <- poll(t, ctx, base, workerToken, "team/worker-2", "?timeout=10&after="+itoa(direct.ID), nil)
	}()
	waitSubscribers(t, s, "team/worker-2", 1)

	g := slotDo(t, s, "register", labeled("m", "/w", "22"))
	if g.Role != "team/worker-2" || g.HeldDelivered != 1 {
		t.Fatalf("grant: %+v", g)
	}
	r := <-got
	if len(r.resp.Events) != 1 {
		t.Fatalf("poll after registration: %s", r.body)
	}
	ev := r.resp.Events[0]
	if ev.ID <= direct.ID || ev.Recipient != "team/worker-2" || ev.AddressedTo != label22 || ev.Subject != "brief" {
		t.Fatalf("moved event: %+v", ev)
	}
	if heldCount(t, s) != 0 {
		t.Fatal("mail still held after registration")
	}

	out := listAs(t, s, "secret-a", "a", "outbox")
	var moved wire.Message
	for _, m := range out {
		if m.Subject == "brief" {
			moved = m
		}
	}
	if moved.ID != ev.ID || moved.Recipient != "team/worker-2" || moved.AddressedTo != label22 || moved.HeldID != held.ID || moved.DeliveredAt == nil || moved.HeldAt == nil || moved.Status != "pending" {
		t.Fatalf("outbox after move: %+v", moved)
	}
	if in := inboxSubjects(t, s, "team/worker-2"); strings.Join(in, ",") != "brief,direct" {
		t.Fatalf("inbox after move: %v", in)
	}
	// The recipient can reply; the reply goes back to the sender.
	if w := callAs(s, "POST", "/api/messages/"+itoa(moved.ID)+"/reply", workerToken, "team/worker-2", map[string]any{"subject": "re"}); w.Code != 201 {
		t.Fatalf("reply: %d %s", w.Code, w.Body)
	}
	// A second registration with the label gets nothing.
	if g := slotDo(t, s, "register", labeled("m", "/w2", "22")); g.HeldDelivered != 0 {
		t.Fatalf("second session took mail: %+v", g)
	}
}

func TestLabelMailReachesStreamOfTheSharedMailbox(t *testing.T) {
	t.Parallel()
	s, _, base := labelServer(t)
	sendFrom(t, s, label22, "early", "", 201)
	st := openEvents(t, base, workerToken, "team/worker-1", nil, "")
	if ids, _ := st.untilReady(t); len(ids) != 0 {
		t.Fatalf("held mail streamed before registration: %v", ids)
	}
	if g := slotDo(t, s, "register", labeled("m", "/w", "22")); g.Role != "team/worker-1" || g.HeldDelivered != 1 {
		t.Fatalf("grant: %+v", g)
	}
	ev := st.nextMessage(t)
	if ev["subject"] != "early" || ev["recipient"] != "team/worker" || ev["addressed_to"] != label22 {
		t.Fatalf("event: %v", ev)
	}
}

func TestLabelMailGoesStraightToAnActiveSession(t *testing.T) {
	t.Parallel()
	s, clock, _ := labelServer(t)
	slotDo(t, s, "register", ident("m", "/a"))                // -1, no label
	slotDo(t, s, "register", labeled("m", "/b", "22"))        // -2
	slotDo(t, s, "register", labeled("m", "/c", "22"))        // -3
	slotDo(t, s, "register", labeled("m", "/d", "elsewhere")) // -4

	m := sendFrom(t, s, label22, "now", "", 201)
	if m.Recipient != "team/worker-2" || m.Status != "pending" || m.AddressedTo != label22 || m.HeldAt != nil || m.HeldID != 0 {
		t.Fatalf("direct label mail: %+v", m)
	}
	if got := inboxSubjects(t, s, "team/worker-2"); strings.Join(got, ",") != "now" {
		t.Fatalf("worker-2 inbox: %v", got)
	}

	// Leases end: only expired sessions carry the label, so mail is held, and
	// the next heartbeat of a labelled session collects it, with the unread
	// mail of the ended label session (held for its label, #354).
	clock.add(2 * time.Minute)
	if m := sendFrom(t, s, label22, "later", "", 201); m.Status != "held" {
		t.Fatalf("mail to expired label: %+v", m)
	}
	if g := slotDo(t, s, "heartbeat", labeled("m", "/c", "22")); g.Role != "team/worker-3" || g.HeldDelivered != 1 {
		t.Fatalf("heartbeat grant: %+v", g)
	}
	if got := inboxSubjects(t, s, "team/worker-3"); strings.Join(got, ",") != "now,later" {
		t.Fatalf("worker-3 inbox: %v", got)
	}
}

func TestLabelAddedByHeartbeatOrTakeoverCollectsHeldMail(t *testing.T) {
	t.Parallel()
	s, _, _ := labelServer(t)
	slotDo(t, s, "register", ident("m", "/a")) // -1
	slotDo(t, s, "register", ident("m", "/b")) // -2
	sendFrom(t, s, label22, "one", "", 201)
	if g := slotDo(t, s, "heartbeat", labeled("m", "/b", "22")); g.HeldDelivered != 1 || g.Role != "team/worker-2" {
		t.Fatalf("heartbeat: %+v", g)
	}
	sendFrom(t, s, "label:team/worker/23", "two", "", 201)
	if g := slotDo(t, s, "takeover", labeled("m", "/b", "23")); g.HeldDelivered != 1 || g.Role != "team/worker-1" {
		t.Fatalf("takeover: %+v", g)
	}
	if got := inboxSubjects(t, s, "team/worker"); strings.Join(got, ",") != "two" {
		t.Fatalf("-1 inbox after takeover: %v", got)
	}
	// Release never collects.
	sendFrom(t, s, "label:team/worker/24", "three", "", 201)
	w := callAs(s, "POST", "/api/slots/release", workerToken, "team/worker", labeled("m", "/b", "24"))
	if w.Code != 200 || heldCount(t, s) != 1 {
		t.Fatalf("release: %d %s held=%d", w.Code, w.Body, heldCount(t, s))
	}
}

func TestLabelConcurrentRegistrationsDeliverToOneSession(t *testing.T) {
	t.Parallel()
	s, _, _ := labelServer(t)
	for i := 0; i < 3; i++ {
		sendFrom(t, s, label22, "held-"+itoa(int64(i)), "", 201)
	}
	const n = 8
	grants := make([]wire.SlotGrant, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grants[i] = slotDo(t, s, "register", labeled("m", "/w"+itoa(int64(i)), "22"))
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, g := range grants {
		switch g.HeldDelivered {
		case 0:
		case 3:
			winners++
			box := g.Role
			if g.Number == 1 {
				box = g.Base
			}
			if got := inboxSubjects(t, s, box); len(got) != 3 {
				t.Fatalf("winner %s inbox: %v", box, got)
			}
		default:
			t.Fatalf("partial delivery: %+v", g)
		}
	}
	var total int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE addressed_to=?`, label22).Scan(&total)
	if winners != 1 || total != 3 || heldCount(t, s) != 0 {
		t.Fatalf("winners=%d total=%d held=%d", winners, total, heldCount(t, s))
	}
}

func TestLabelSendsRacingARegistrationAreNeverLeftHeld(t *testing.T) {
	t.Parallel()
	s, _, _ := labelServer(t)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sendFrom(t, s, label22, "race-"+itoa(int64(i)), "", 201)
		}(i)
	}
	wg.Add(1)
	go func() { defer wg.Done(); slotDo(t, s, "register", labeled("m", "/w", "22")) }()
	wg.Wait()
	if h := heldCount(t, s); h != 0 {
		t.Fatalf("%d messages left held with the label session active", h)
	}
	if got := inboxSubjects(t, s, "team/worker"); len(got) != n {
		t.Fatalf("session got %d of %d", len(got), n)
	}
}

func TestLabelAddressRules(t *testing.T) {
	t.Parallel()
	s, _, _ := labelServer(t)
	for _, to := range []string{
		"label:", "label:22", "label:/22", "label:team/worker/", "label:team/worker/a b",
		"label:team/worker/a:b", "label:team/worker/" + strings.Repeat("x", 129), "label:te am/22",
	} {
		sendFrom(t, s, to, "bad", "", 400)
	}
	sendFrom(t, s, "label:nobody/22", "unknown base", "", 403)
	sendFrom(t, s, "label:team/worker/a/b", "base team/worker/a is unknown", "", 403)
	if m := sendFrom(t, s, "label:team/worker/"+strings.Repeat("x", 128), "long", "", 201); m.Status != "held" {
		t.Fatalf("128-character label: %+v", m)
	}
	if m := sendFrom(t, s, "label:team/worker/Issue_22.v-2", "chars", "", 201); m.Status != "held" {
		t.Fatalf("label characters: %+v", m)
	}

	// Idempotent retries return the same message, held or moved.
	first := sendFrom(t, s, label22, "once", "k1", 201)
	if again := sendFrom(t, s, label22, "once", "k1", 200); again.ID != first.ID || again.Status != "held" {
		t.Fatalf("retry while held: %+v", again)
	}
	slotDo(t, s, "register", labeled("m", "/w", "22"))
	if again := sendFrom(t, s, label22, "once", "k1", 200); again.Recipient != "team/worker" || again.HeldID != first.ID {
		t.Fatalf("retry after move: %+v", again)
	}
	// The held ID is gone; nobody can address a held message as a recipient.
	if w := call(s, "GET", "/api/messages/"+itoa(first.ID), "secret-a", nil, ""); w.Code != 404 {
		t.Fatalf("old ID: %d", w.Code)
	}
	// A client cannot forge the label fields.
	w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "s", "addressed_to": label22, "held_id": 5}, "")
	var m wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 201 || m.AddressedTo != "" || m.HeldID != 0 {
		t.Fatalf("forged fields: %d %+v", w.Code, m)
	}
}

func TestLabelIsScopedToTheBase(t *testing.T) {
	t.Parallel()
	s, _, _ := labelServer(t)
	sendFrom(t, s, label22, "for worker", "", 201)
	// Another base registers the same label: it gets nothing.
	w := callAs(s, "POST", "/api/slots/register", otherToken, "team/other", labeled("m", "/o", "22"))
	var g wire.SlotGrant
	_ = json.Unmarshal(w.Body.Bytes(), &g)
	if w.Code != 200 || g.HeldDelivered != 0 {
		t.Fatalf("other base register: %d %s", w.Code, w.Body)
	}
	if got := listAs(t, s, otherToken, "team/other", "inbox"); len(got) != 0 {
		t.Fatalf("other base inbox: %+v", got)
	}
	if heldCount(t, s) != 1 {
		t.Fatal("held mail left its label")
	}
	// Mail to the other base's label goes to it, not to team/worker.
	if m := sendFrom(t, s, "label:team/other/22", "for other", "", 201); m.Recipient != "team/other" {
		t.Fatalf("label of team/other: %+v", m)
	}
	// A role cannot claim to be a label address.
	if w := callAs(s, "GET", "/api/messages", workerToken, label22, nil); w.Code != 401 {
		t.Fatalf("label as role: %d", w.Code)
	}
	slotDo(t, s, "register", labeled("m", "/w", "22"))
	if got := inboxSubjects(t, s, "team/worker"); strings.Join(got, ",") != "for worker" {
		t.Fatalf("worker inbox: %v", got)
	}
}

func TestLabelSchemaOnLegacyDatabase(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", testDSN(filepath.Join(t.TempDir(), "legacy.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err = db.Exec(legacySchema + `INSERT INTO roles VALUES('team/worker','` + workerToken + `','now');`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = store.Init(db); err != nil {
			t.Fatalf("schema run %d: %v", i, err)
		}
	}
	for _, c := range store.LabelColumns {
		if has, _ := store.HasColumn(db, "messages", c.Name); !has {
			t.Fatalf("missing column %s", c.Name)
		}
	}
	s := &Server{db: db, tokens: map[string]string{}}
	if got := listAs(t, s, "secret-b", "b", "inbox"); len(got) != 1 || got[0].Subject != "old mail" || got[0].AddressedTo != "" || got[0].HeldAt != nil {
		t.Fatalf("old mail: %+v", got)
	}
	sendFrom(t, s, label22, "after upgrade", "", 201)
	if g := slotDo(t, s, "register", labeled("m", "/w", "22")); g.HeldDelivered != 1 {
		t.Fatalf("register on migrated db: %+v", g)
	}
	// New IDs keep growing past the moved message.
	if m := sendFrom(t, s, "b", "next", "", 201); m.ID <= 3 {
		t.Fatalf("next ID %d", m.ID)
	}
}
