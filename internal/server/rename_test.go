package server

import (
	"strings"
	"testing"
)

// TestRenameRoleKeepsMailTokensAndOldName renames old/coord to new/coord:
// earlier mail moves with it, the same token works under both names, mail to
// the old name (and its numbered forms) reaches the new role, and the old name
// cannot be registered again.
func TestRenameRoleKeepsMailTokensAndOldName(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('old/coord','secret-c','now')`); err != nil {
		t.Fatal(err)
	}
	send := func(token, as, to, subject string) string {
		t.Helper()
		w := callAs(s, "POST", "/api/messages", token, as, map[string]string{"recipient": to, "subject": subject, "body": "x"})
		if w.Code != 201 {
			t.Fatalf("send %s -> %s: %d %s", as, to, w.Code, w.Body)
		}
		return w.Body.String()
	}
	send("secret-a", "a", "old/coord", "before-in")
	send("secret-c", "old/coord", "a", "before-out")

	rename := func(old, to string) (int, string) {
		w := adminRequest(s, "POST", "/api/admin/roles/"+old+"/rename", `{"to":"`+to+`"}`, dashboardHeaders)
		return w.Code, w.Body.String()
	}
	if code, body := rename("old/coord", "new/coord"); code != 200 || !strings.Contains(body, `"role":"new/coord"`) || !strings.Contains(body, `"aliases":["old/coord"]`) {
		t.Fatalf("rename: %d %s", code, body)
	}

	for _, as := range []string{"old/coord", "new/coord"} {
		w := callAs(s, "GET", "/api/messages?box=inbox", "secret-c", as, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "before-in") || !strings.Contains(w.Body.String(), `"recipient":"new/coord"`) {
			t.Fatalf("inbox as %s: %d %s", as, w.Code, w.Body)
		}
		w = callAs(s, "GET", "/api/messages?box=outbox", "secret-c", as, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "before-out") || !strings.Contains(w.Body.String(), `"sender":"new/coord"`) {
			t.Fatalf("outbox as %s: %d %s", as, w.Code, w.Body)
		}
	}
	if w := callAs(s, "GET", "/api/messages?box=inbox", "secret-a", "old/coord", nil); w.Code != 401 {
		t.Fatalf("old name with another role's token: %d", w.Code)
	}

	// New mail to the old name, and to its numbered forms, reaches the new role.
	if body := send("secret-a", "a", "old/coord", "after"); !strings.Contains(body, `"recipient":"new/coord"`) {
		t.Fatalf("send to old name: %s", body)
	}
	if body := send("secret-a", "a", "old/coord-2", "after-2"); !strings.Contains(body, `"recipient":"new/coord-2"`) {
		t.Fatalf("send to old numbered name: %s", body)
	}
	if body := send("secret-c", "old/coord-2", "a", "from-slot"); !strings.Contains(body, `"sender":"new/coord-2"`) {
		t.Fatalf("send as old numbered name: %s", body)
	}

	// Conflicts and unknown names.
	for _, c := range []struct {
		old, to string
		code    int
	}{
		{"old/coord", "other/coord", 404}, // old name is an alias now, not a role
		{"a", "new/coord", 409},           // taken by a role
		{"a", "old/coord", 409},           // taken by an alias
		{"a", "new/coord-3", 409},         // numbered form of a role
		{"a", "a", 400},
		{"a", "switchboard/system", 403},
	} {
		if code, body := rename(c.old, c.to); code != c.code {
			t.Errorf("rename %s -> %s: %d %s, want %d", c.old, c.to, code, body, c.code)
		}
	}
	if w := call(s, "POST", "/api/register", "", map[string]string{"role": "old/coord", "credential": "another-credential-long"}, ""); w.Code != 409 {
		t.Fatalf("register old name: %d %s", w.Code, w.Body)
	}

	// A second rename keeps one level of aliases.
	if code, body := rename("new/coord", "third/coord"); code != 200 || !strings.Contains(body, `"aliases":["new/coord","old/coord"]`) {
		t.Fatalf("second rename: %d %s", code, body)
	}
	if body := send("secret-a", "a", "old/coord", "after-second"); !strings.Contains(body, `"recipient":"third/coord"`) {
		t.Fatalf("send to first name after second rename: %s", body)
	}
}

// TestRenameRoleRewritesLabelsAndBases checks stored label addresses and
// base columns follow the rename.
func TestRenameRoleRewritesLabelsAndBases(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('old/coord','secret-c','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,addressed_to) VALUES('a','label:old/coord/review','s','b','now','label:old/coord/review'),('a','old/coordinator','s','b','now','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO slots(base,number,machine,directory,registered_at,lease_until) VALUES('old/coord',2,'m','d',0,0)`); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(s, "POST", "/api/admin/roles/old/coord/rename", `{"to":"new/coord"}`, dashboardHeaders); w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE recipient='label:new/coord/review' AND addressed_to='label:new/coord/review'`).Scan(&n)
	if n != 1 {
		t.Fatal("label address not rewritten")
	}
	_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE recipient='old/coordinator'`).Scan(&n)
	if n != 1 {
		t.Fatal("a name that only starts with the old name was rewritten")
	}
	_ = s.db.QueryRow(`SELECT count(*) FROM slots WHERE base='new/coord' AND number=2`).Scan(&n)
	if n != 1 {
		t.Fatal("slot base not rewritten")
	}
}
