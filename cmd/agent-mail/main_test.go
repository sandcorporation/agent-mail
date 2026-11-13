package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,recipient TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,task_id TEXT,thread_id INTEGER,reply_to INTEGER,status TEXT NOT NULL DEFAULT 'pending',idem TEXT, UNIQUE(sender,idem)); CREATE TABLE role_states(role TEXT NOT NULL,message_id INTEGER NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(role,message_id)); CREATE TABLE roles(role TEXT PRIMARY KEY,token TEXT NOT NULL,created_at TEXT NOT NULL); INSERT INTO roles VALUES('a','secret-a','now'),('b','secret-b','now');`)
	if e != nil {
		t.Fatal(e)
	}
	return &Server{db: db, tokens: map[string]string{"a": "secret-a", "b": "secret-b"}}
}

func TestDynamicRegistrationResumeAndMigration(t *testing.T) {
	s := testServer(t)
	t.Setenv("AGENT_MAIL_ENROLLMENT_TOKEN", "enrollment-secret")
	payload := map[string]string{"role": "team/worker", "credential": "worker-credential-long-enough"}
	r := httptest.NewRequest("POST", "/api/register", bytes.NewBufferString(`{"role":"team/worker","credential":"worker-credential-long-enough"}`))
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthorized registration=%d", w.Code)
	}
	r = httptest.NewRequest("POST", "/api/register", bytes.NewBufferString(`{"role":"team/worker","credential":"worker-credential-long-enough"}`))
	r.Header.Set("Authorization", "Bearer enrollment-secret")
	w = httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 201 {
		t.Fatalf("register=%d %s", w.Code, w.Body)
	}
	if role, ok := s.auth(httptest.NewRequest("GET", "/", nil)); ok || role != "" {
		t.Fatal("empty token authenticated")
	}
	if w = call(s, "POST", "/api/messages", "worker-credential-long-enough", map[string]string{"recipient": "b", "subject": "spoof", "body": "x", "sender": "a"}, ""); w.Code != 201 {
		t.Fatalf("dynamic role send=%d %s", w.Code, w.Body)
	}
	var msg Message
	_ = json.Unmarshal(w.Body.Bytes(), &msg)
	if msg.Sender != "team/worker" {
		t.Fatalf("sender spoofed: %q", msg.Sender)
	}
	if w = call(s, "POST", "/api/messages", "worker-credential-long-enough", map[string]string{"recipient": "missing", "subject": "x", "body": "x"}, ""); w.Code != 403 {
		t.Fatalf("unknown recipient=%d", w.Code)
	}
	r = httptest.NewRequest("POST", "/api/register", bytes.NewBufferString(`{"role":"team/worker","credential":"worker-credential-long-enough"}`))
	r.Header.Set("Authorization", "Bearer arbitrary")
	w = httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("resumed")) {
		t.Fatalf("resume=%d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest("POST", "/api/register", bytes.NewBufferString(`{"role":"team/worker","credential":"another-credential-long-enough"}`))
	r.Header.Set("Authorization", "Bearer enrollment-secret")
	w = httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 409 {
		t.Fatalf("takeover=%d", w.Code)
	}
	if err := migrateConfiguredRoles(s); err != nil {
		t.Fatal(err)
	}
	var dbPath string
	_ = s.db.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &dbPath)
	s.db.Close()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var token string
	if err = db.QueryRow(`SELECT token FROM roles WHERE role='team/worker'`).Scan(&token); err != nil || token != payload["credential"] {
		t.Fatalf("dynamic role lost after restart: %v", err)
	}
	if err = migrateConfiguredRoles(&Server{db: db, tokens: map[string]string{"legacy": "legacy-token"}}); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT token FROM roles WHERE role='legacy'`).Scan(&token); err != nil || token != "legacy-token" {
		t.Fatalf("static migration failed: %v", err)
	}
}

func TestHelpDoesNotReadConfigOrConnect(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"agent-mail", "send", "--help"}
	if !hasHelp(os.Args[2:]) {
		t.Fatal("subcommand help was not recognized")
	}
	os.Args = []string{"agent-mail", "--help"}
	if profileName() != "default" {
		t.Fatal("help profile setup unexpectedly required config")
	}
}

func TestActualCLILifecycle(t *testing.T) {
	s := testServer(t)
	t.Setenv("AGENT_MAIL_ENROLLMENT_TOKEN", "enrollment-secret")
	httpServer := httptest.NewServer(http.HandlerFunc(s.api))
	defer httpServer.Close()
	dir := t.TempDir()
	roleFile := filepath.Join(dir, "role.token")
	enrollFile := filepath.Join(dir, "enrollment.token")
	if err := os.WriteFile(roleFile, []byte("cli-role-credential-long-enough\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollFile, []byte("enrollment-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "AGENT_MAIL_TEST_CLI=1", "AGENT_MAIL_CONFIG_DIR="+dir)
	run := func(args ...string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
		argJSON, _ := json.Marshal(args)
		cmd.Env = append(env, "AGENT_MAIL_TEST_ARGS="+string(argJSON))
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %v failed: %v: %s", args, err, output)
		}
		return string(output)
	}
	if out := run("--help"); !strings.Contains(out, "role addressed durable mailbox") {
		t.Fatalf("root help: %s", out)
	}
	if out := run("send", "--help"); !strings.Contains(out, "Usage: agent-mail send") {
		t.Fatalf("command help: %s", out)
	}
	if out := run("config", "--help"); !strings.Contains(out, "Usage: agent-mail config set") {
		t.Fatalf("config help: %s", out)
	}
	if out := run("register", "--endpoint", httpServer.URL, "--role", "team/cli", "--token-file", roleFile, "--enrollment-token-file", enrollFile); !strings.Contains(out, "registered") {
		t.Fatalf("register output: %s", out)
	}
	run("--profile", "cli-worker", "config", "set", "--endpoint", httpServer.URL, "--role", "team/cli", "--token-file", roleFile)
	if out := run("--profile", "cli-worker", "send", "--to", "b", "--subject", "cli lifecycle", "--body", "durable", "--idempotency-key", "cli-key", "--json"); !strings.Contains(out, "cli lifecycle") {
		t.Fatalf("send output: %s", out)
	}
	if w := call(s, "POST", "/api/messages", "secret-a", map[string]string{"recipient": "team/cli", "subject": "incoming lifecycle", "body": "inbox durable"}, "incoming-key"); w.Code != 201 {
		t.Fatalf("incoming message=%d %s", w.Code, w.Body)
	}
	if out := run("--profile", "cli-worker", "inbox", "--json"); !strings.Contains(out, "incoming lifecycle") {
		t.Fatalf("inbox output: %s", out)
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_MAIL_TEST_CLI") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("AGENT_MAIL_TEST_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"agent-mail"}, args...)
	main()
}
func call(s *Server, method, path, token string, body any, idem string) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.Header.Set("Authorization", "Bearer "+token)
	var role string
	_ = s.db.QueryRow(`SELECT role FROM roles WHERE token=?`, token).Scan(&role)
	r.Header.Set("X-Agent-Mail-Role", role)
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
	rMismatch := httptest.NewRequest("GET", "/api/messages", nil)
	rMismatch.Header.Set("Authorization", "Bearer secret-a")
	rMismatch.Header.Set("X-Agent-Mail-Role", "b")
	wMismatch := httptest.NewRecorder()
	s.api(wMismatch, rMismatch)
	if wMismatch.Code != 401 {
		t.Fatalf("mismatched selected role auth=%d", wMismatch.Code)
	}
	call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "s", "body": "b"}, "")
	r := httptest.NewRequest("GET", "/api/watch?after=0", nil)
	r.Header.Set("Authorization", "Bearer secret-b")
	r.Header.Set("X-Agent-Mail-Role", "b")
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
