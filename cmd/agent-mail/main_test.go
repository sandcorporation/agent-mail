package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,recipient TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,task_id TEXT,thread_id INTEGER,reply_to INTEGER,status TEXT NOT NULL DEFAULT 'pending',idem TEXT, UNIQUE(sender,idem)); CREATE TABLE role_states(role TEXT NOT NULL,message_id INTEGER NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(role,message_id));`)
	if e != nil {
		t.Fatal(e)
	}
	return &Server{db: db, tokens: map[string]string{"a": "secret-a", "b": "secret-b"}}
}
func call(s *Server, method, path, token string, body any, idem string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.Header.Set("Authorization", "Bearer "+token)
	if idem != "" {
		r.Header.Set("Idempotency-Key", idem)
	}
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}
func TestPersistenceRetryAndStatus(t *testing.T) {
	s := testServer(t)
	payload := map[string]any{"recipient": "b", "subject": "hello", "body": "durable"}
	w := call(s, "POST", "/api/messages", "secret-a", payload, "key-1")
	if w.Code != 201 {
		t.Fatalf("send %d %s", w.Code, w.Body)
	}
	first := w.Body.String()
	w = call(s, "POST", "/api/messages", "secret-a", payload, "key-1")
	if w.Code != 200 || w.Body.String() == "" {
		t.Fatalf("retry %d %s", w.Code, w.Body)
	}
	var a, b Message
	_ = json.Unmarshal([]byte(first), &a)
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if a.ID != b.ID {
		t.Fatalf("duplicate IDs %d %d", a.ID, b.ID)
	}
	// Re-open the same SQLite file to ensure restart durability.
	var dbPath string
	_ = s.db.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &dbPath)
	s.db.Close()
	db, e := sql.Open("sqlite", dbPath)
	if e != nil {
		t.Fatal(e)
	}
	s.db = db
	w = call(s, "GET", "/api/messages/"+itoa(a.ID), "secret-b", nil, "")
	if w.Code != 200 {
		t.Fatalf("read after restart %d", w.Code)
	}
	w = call(s, "POST", "/api/messages/"+itoa(a.ID)+"/ack", "secret-b", nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = call(s, "GET", "/api/messages?box=inbox&status=acknowledged", "secret-b", nil, "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("acknowledged")) {
		t.Fatalf("status filter: %s", w.Body)
	}
	w = call(s, "GET", "/api/messages?box=outbox&status=acknowledged", "secret-a", nil, "")
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("acknowledged")) {
		t.Fatalf("outbox recipient status: %d %s", w.Code, w.Body)
	}
}
func TestAuthorizationAndWatchCursor(t *testing.T) {
	s := testServer(t)
	if w := call(s, "GET", "/api/messages", "wrong", nil, ""); w.Code != 401 {
		t.Fatalf("auth=%d", w.Code)
	}
	call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "s", "body": "b"}, "")
	r := httptest.NewRequest("GET", "/api/watch?after=0", nil)
	r.Header.Set("Authorization", "Bearer secret-b")
	w := httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"cursor":1`)) {
		t.Fatalf("watch: %d %s", w.Code, w.Body)
	}
	// Independent watchers replay from the same durable cursor; neither claims mail.
	w2 := httptest.NewRecorder()
	s.api(w2, r.Clone(r.Context()))
	if w2.Code != 200 || !bytes.Contains(w2.Body.Bytes(), []byte(`"messages":[1]`)) {
		t.Fatalf("replay: %d %s", w2.Code, w2.Body)
	}
	w = call(s, "GET", "/api/messages?box=inbox", "secret-b", nil, "")
	if !bytes.Contains(w.Body.Bytes(), []byte(`"status":"pending"`)) {
		t.Fatalf("watch consumed message: %s", w.Body)
	}
}

func TestParseIDRejectsTrailingJunk(t *testing.T) {
	if _, err := parseID("12junk"); err == nil {
		t.Fatal("expected malformed ID to be rejected")
	}
	if id, err := parseID("12"); err != nil || id != 12 {
		t.Fatalf("parseID(12) = %d, %v", id, err)
	}
}
