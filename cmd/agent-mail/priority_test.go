package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/agent-mail/internal/store"
)

func decodeMessage(t *testing.T, raw []byte) Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

func TestSendPriorityDefaultsUrgentAndValidation(t *testing.T) {
	s := testServer(t)
	w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "plain", "body": "x"}, "")
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"priority":"normal"`) {
		t.Fatalf("omitted priority: %d %s", w.Code, w.Body)
	}
	w = call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "fire", "body": "now", "priority": "urgent"}, "k1")
	urgent := decodeMessage(t, w.Body.Bytes())
	if w.Code != 201 || urgent.Priority != "urgent" {
		t.Fatalf("urgent send: %d %s", w.Code, w.Body)
	}
	// Idempotent retry returns the stored message, priority included.
	w = call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "fire", "body": "now", "priority": "urgent"}, "k1")
	if again := decodeMessage(t, w.Body.Bytes()); w.Code != 200 || again.ID != urgent.ID || again.Priority != "urgent" {
		t.Fatalf("idempotent retry: %d %s", w.Code, w.Body)
	}
	for _, bad := range []any{"URGENT", "high", 1} {
		if w = call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "bad", "body": "x", "priority": bad}, ""); w.Code != 400 {
			t.Fatalf("priority %v: %d %s", bad, w.Code, w.Body)
		}
	}
	w = call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "explicit", "body": "x", "priority": "normal"}, "")
	if decodeMessage(t, w.Body.Bytes()).Priority != "normal" {
		t.Fatalf("explicit normal: %s", w.Body)
	}

	// Every Message view carries priority.
	var inbox []Message
	_ = json.Unmarshal(call(s, "GET", "/api/messages?box=inbox", "secret-b", nil, "").Body.Bytes(), &inbox)
	var outbox []Message
	_ = json.Unmarshal(call(s, "GET", "/api/messages?box=outbox", "secret-a", nil, "").Body.Bytes(), &outbox)
	for name, list := range map[string][]Message{"inbox": inbox, "outbox": outbox} {
		if len(list) != 3 {
			t.Fatalf("%s: %+v", name, list)
		}
		for _, m := range list {
			want := "normal"
			if m.ID == urgent.ID {
				want = "urgent"
			}
			if m.Priority != want {
				t.Fatalf("%s #%d priority=%q", name, m.ID, m.Priority)
			}
		}
	}
	if m := decodeMessage(t, call(s, "GET", "/api/messages/"+itoa(urgent.ID), "secret-b", nil, "").Body.Bytes()); m.Priority != "urgent" {
		t.Fatalf("read: %+v", m)
	}
	s.adminProxyToken = "proxy-secret"
	r := httptest.NewRequest("GET", "/api/admin/messages?role=b", nil)
	r.Header.Set("X-Agent-Mail-Admin", "proxy-secret")
	aw := httptest.NewRecorder()
	s.api(aw, r)
	var admin []Message
	if err := json.Unmarshal(aw.Body.Bytes(), &admin); err != nil || len(admin) != 3 || admin[1].Priority != "urgent" || admin[0].Priority != "normal" {
		t.Fatalf("admin list: %d %s", aw.Code, aw.Body)
	}
}

// A new CLI must keep working against a server that predates priority: normal
// sends carry no priority field, and messages without one read as normal.
func TestCLIAgainstServerWithoutPriority(t *testing.T) {
	var sendBodies []map[string]any
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			sendBodies = append(sendBodies, b)
			w.WriteHeader(201)
			io.WriteString(w, `{"id":7,"sender":"a","recipient":"b","subject":"s","body":"x","created_at":"2026-01-01T00:00:00Z","status":"pending"}`)
			return
		}
		io.WriteString(w, `[{"id":7,"sender":"a","recipient":"b","subject":"s","body":"x","created_at":"2026-01-01T00:00:00Z","status":"pending"}]`)
	}))
	t.Cleanup(old.Close)
	h := newCLIHarness(t)
	h.url = old.URL
	h.configure(nil, "a")
	if out := h.mustRun(nil, "send", "--to", "b", "--subject", "s", "--body", "x"); !strings.HasPrefix(out, "#7 s\n") {
		t.Fatalf("send output: %q", out)
	}
	h.mustRun(nil, "reply", "7", "--subject", "re", "--body", "x")
	for _, b := range sendBodies {
		if _, has := b["priority"]; has {
			t.Fatalf("normal request sent priority: %v", b)
		}
	}
	if out := h.mustRun(nil, "wait", "--timeout", "5"); out != "#7 a | s" {
		t.Fatalf("wait against old server: %q", out)
	}
	if out := h.mustRun(nil, "wait", "--json"); !strings.Contains(out, `"priority": "normal"`) {
		t.Fatalf("wait --json against old server: %s", out)
	}
}

func TestReplyPriority(t *testing.T) {
	s := testServer(t)
	orig := decodeMessage(t, call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "q", "body": "x"}, "").Body.Bytes())
	w := call(s, "POST", "/api/messages/"+itoa(orig.ID)+"/reply", "secret-b", map[string]any{"subject": "re", "body": "plain reply"}, "")
	if m := decodeMessage(t, w.Body.Bytes()); w.Code != 201 || m.Priority != "normal" || m.ReplyTo != orig.ID {
		t.Fatalf("reply default: %d %s", w.Code, w.Body)
	}
	w = call(s, "POST", "/api/messages/"+itoa(orig.ID)+"/reply", "secret-b", map[string]any{"subject": "re", "body": "hurry", "priority": "urgent"}, "")
	if m := decodeMessage(t, w.Body.Bytes()); w.Code != 201 || m.Priority != "urgent" || m.Recipient != "a" {
		t.Fatalf("urgent reply: %d %s", w.Code, w.Body)
	}
	if w = call(s, "POST", "/api/messages/"+itoa(orig.ID)+"/reply", "secret-b", map[string]any{"subject": "re", "body": "x", "priority": "low"}, ""); w.Code != 400 {
		t.Fatalf("bad reply priority: %d", w.Code)
	}
}

func TestEventsCarryPriorityAndUrgentBody(t *testing.T) {
	s, base := eventServer(t)
	st := openEvents(t, base, "secret-b", "b", nil, "")
	st.untilReady(t)
	sendMail(t, s, "secret-a", "b", "plain")
	long := strings.Repeat("긴급 본문 ", 2000) // the server never truncates
	if w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "fire", "body": long, "priority": "urgent"}, ""); w.Code != 201 {
		t.Fatal(w.Body)
	}
	if w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "empty", "body": "", "priority": "urgent"}, ""); w.Code != 201 {
		t.Fatal(w.Body)
	}
	plain := st.nextMessage(t)
	if _, has := plain["body"]; has || plain["priority"] != "normal" {
		t.Fatalf("normal event: %v", plain)
	}
	urgent := st.nextMessage(t)
	if urgent["priority"] != "urgent" || urgent["body"] != long || urgent["subject"] != "fire" {
		t.Fatalf("urgent event priority=%v subject=%v body len=%d", urgent["priority"], urgent["subject"], len(urgent["body"].(string)))
	}
	empty := st.nextMessage(t)
	if b, has := empty["body"]; !has || b != "" || empty["priority"] != "urgent" {
		t.Fatalf("urgent empty-body event: %v", empty)
	}
}

// legacySchema is the messages table as created before message priority.
const legacySchema = `CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,recipient TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,task_id TEXT,thread_id INTEGER,reply_to INTEGER,status TEXT NOT NULL DEFAULT 'pending',idem TEXT, UNIQUE(sender,idem)); CREATE TABLE role_states(role TEXT NOT NULL,message_id INTEGER NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(role,message_id)); CREATE TABLE roles(role TEXT PRIMARY KEY, token TEXT NOT NULL, created_at TEXT NOT NULL); INSERT INTO roles VALUES('a','secret-a','now'),('b','secret-b','now'); INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,idem) VALUES('a','b','old mail','kept body','2026-01-01T00:00:00Z','T-9','old-key'); INSERT INTO role_states VALUES('b',1,'acknowledged','2026-01-01T00:00:00Z');`

func TestMigrationAddsPriorityAndKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if has, _ := store.HasColumn(db, "messages", "priority"); has {
		t.Fatal("legacy schema already has priority")
	}
	// Twice: the migration runs on every start and must be idempotent.
	for i := 0; i < 2; i++ {
		if err = store.Init(db); err != nil {
			t.Fatalf("migration run %d: %v", i, err)
		}
	}
	s := &Server{db: db, tokens: map[string]string{}}
	m := decodeMessage(t, call(s, "GET", "/api/messages/1", "secret-b", nil, "").Body.Bytes())
	if m.Subject != "old mail" || m.Body != "kept body" || m.TaskID != "T-9" || m.Status != "acknowledged" || m.Priority != "normal" {
		t.Fatalf("migrated message: %+v", m)
	}
	// The old idempotency key still dedupes.
	w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "old mail", "body": "kept body"}, "old-key")
	if again := decodeMessage(t, w.Body.Bytes()); w.Code != 200 || again.ID != 1 || again.Priority != "normal" {
		t.Fatalf("legacy idempotent retry: %d %s", w.Code, w.Body)
	}
	w = call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "new", "body": "x", "priority": "urgent"}, "")
	if m := decodeMessage(t, w.Body.Bytes()); w.Code != 201 || m.ID != 2 || m.Priority != "urgent" {
		t.Fatalf("send after migration: %d %s", w.Code, w.Body)
	}
	db.Close()
}

func TestCLIUrgentSendReplyAndMarkers(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure(nil, "b")
	h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "plain", "--body", "x", "--task", "t-1")
	out := h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", "fire", "--body", "now", "--urgent", "--json")
	if !strings.Contains(out, `"priority": "urgent"`) {
		t.Fatalf("send --urgent --json: %s", out)
	}
	// Normal wait lines are unchanged; urgent lines gain a trailing marker.
	if out := h.mustRun(nil, "wait", "--timeout", "5"); out != "#2 a | fire | urgent\n#1 a | plain | task=t-1" {
		t.Fatalf("wait output: %q", out)
	}
	if out := h.mustRun(nil, "wait", "--json"); !strings.Contains(out, `"priority": "normal"`) || !strings.Contains(out, `"priority": "urgent"`) {
		t.Fatalf("wait --json: %s", out)
	}
	inbox := h.mustRun(nil, "inbox")
	if !strings.Contains(inbox, "#2 [urgent] fire\n") || !strings.Contains(inbox, "#1 plain\n") {
		t.Fatalf("inbox markers: %q", inbox)
	}
	if out := h.mustRun(nil, "read", "2"); !strings.HasPrefix(out, "#2 [urgent] fire\n") {
		t.Fatalf("read urgent: %q", out)
	}
	if out := h.mustRun(nil, "read", "1"); !strings.HasPrefix(out, "#1 plain\na → b") {
		t.Fatalf("read normal: %q", out)
	}
	h.mustRun(nil, "reply", "2", "--subject", "re: fire", "--body", "on it", "--urgent")
	h.mustRun(nil, "reply", "1", "--subject", "re: plain", "--body", "ok")
	outbox := h.mustRun(nil, "outbox", "--json")
	var sent []Message
	if err := json.Unmarshal([]byte(outbox), &sent); err != nil || len(sent) != 2 || sent[0].Priority != "normal" || sent[1].Priority != "urgent" {
		t.Fatalf("reply priorities: %s", outbox)
	}
	if out := h.mustRun(nil, "--profile", "a", "outbox"); !strings.Contains(out, "#2 [urgent] fire") {
		t.Fatalf("outbox marker: %q", out)
	}
	help := h.mustRun(nil, "send", "--help")
	if !strings.Contains(help, "--urgent") || !strings.Contains(h.mustRun(nil, "reply", "--help"), "--urgent") {
		t.Fatalf("help lacks --urgent: %s", help)
	}
}
