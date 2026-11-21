package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// A boss role lists and opens another role's mail without changing its
// state, every view is recorded, and other roles get 403 (#355).
func TestBossViewsOtherMailboxReadOnly(t *testing.T) {
	s, clock := slotsWithRoles(t)
	m := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "secret subject", "body": "do the thing"})
	status := func() string {
		got, err := s.mail().View(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	if status() != "pending" {
		t.Fatalf("start status %q", status())
	}

	// A third boss (not sender or recipient) views.
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('hq/boss2','secret-hq/boss2','now')`); err != nil {
		t.Fatal(err)
	}
	s.bossRoles["hq/boss2"] = true
	w := callAs(s, "GET", "/api/messages?box=inbox&of=team/worker", tokenFor("hq/boss2"), "hq/boss2", nil)
	var list []wire.Message
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("list of: %d %s", w.Code, w.Body)
	}
	w = callAs(s, "GET", fmt.Sprintf("/api/messages/%d", m.ID), tokenFor("hq/boss2"), "hq/boss2", nil)
	var got wire.Message
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Body != "do the thing" || got.Status != "pending" {
		t.Fatalf("read foreign: %d %s", w.Code, w.Body)
	}
	if status() != "pending" {
		t.Fatalf("status after boss views: %q, want pending", status())
	}

	rows, err := s.db.Query(`SELECT viewer,mailbox,message_id FROM mailbox_views ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var v, b string
		var id int64
		_ = rows.Scan(&v, &b, &id)
		seen = append(seen, fmt.Sprintf("%s>%s#%d", v, b, id))
	}
	want := []string{"hq/boss2>team/worker#0", fmt.Sprintf("hq/boss2>team/worker#%d", m.ID)}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("views %v, want %v", seen, want)
	}

	// Writes are not widened.
	for _, path := range []string{"complete", "ack"} {
		if w := callAs(s, "POST", fmt.Sprintf("/api/messages/%d/%s", m.ID, path), tokenFor("hq/boss2"), "hq/boss2", nil); w.Code < 400 {
			t.Fatalf("%s by viewer: %d, want refusal", path, w.Code)
		}
	}
	if status() != "pending" {
		t.Fatalf("status after refused writes: %q", status())
	}
}

func TestNonBossCannotViewOtherMailbox(t *testing.T) {
	s, clock := slotsWithRoles(t)
	m := apiMail(t, s, clock, "hq/boss", "/api/messages", map[string]any{"recipient": "team/worker", "subject": "s", "body": "b"})
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('other/worker','secret-other/worker','now')`); err != nil {
		t.Fatal(err)
	}
	w := callAs(s, "GET", "/api/messages?of=team/worker", tokenFor("other/worker"), "other/worker", nil)
	if w.Code != 403 {
		t.Fatalf("--of by non-boss: %d, want 403", w.Code)
	}
	w = callAs(s, "GET", fmt.Sprintf("/api/messages/%d", m.ID), tokenFor("other/worker"), "other/worker", nil)
	if w.Code != 404 {
		t.Fatalf("read by non-boss: %d, want 404", w.Code)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM mailbox_views`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d views recorded for refused calls", n)
	}
}
