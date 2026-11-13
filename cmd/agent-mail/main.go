package main

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Message struct {
	ID             int64     `json:"id"`
	Sender         string    `json:"sender"`
	Recipient      string    `json:"recipient"`
	Subject        string    `json:"subject"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"created_at"`
	TaskID         string    `json:"task_id,omitempty"`
	ThreadID       int64     `json:"thread_id,omitempty"`
	ReplyTo        int64     `json:"reply_to,omitempty"`
	Status         string    `json:"status"`
	IdempotencyKey string    `json:"-"`
}
type Server struct {
	db              *sql.DB
	tokens          map[string]string
	adminProxyToken string
}

var version = "dev"

func runServer() {
	path := env("DB_PATH", "/data/mail.db")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	db, e := sql.Open("sqlite", path)
	if e != nil {
		log.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,recipient TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,task_id TEXT,thread_id INTEGER,reply_to INTEGER,status TEXT NOT NULL DEFAULT 'pending',idem TEXT, UNIQUE(sender,idem)); CREATE INDEX IF NOT EXISTS idx_recipient ON messages(recipient,id); CREATE INDEX IF NOT EXISTS idx_sender ON messages(sender,id); CREATE TABLE IF NOT EXISTS role_states(role TEXT NOT NULL,message_id INTEGER NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(role,message_id)); CREATE TABLE IF NOT EXISTS roles(role TEXT PRIMARY KEY, token TEXT NOT NULL, created_at TEXT NOT NULL);`)
	if e != nil {
		log.Fatal(e)
	}
	s := &Server{db: db, tokens: map[string]string{}, adminProxyToken: os.Getenv("AGENT_MAIL_ADMIN_PROXY_TOKEN")}
	for _, pair := range strings.Split(os.Getenv("AGENT_MAIL_TOKENS"), ",") {
		p := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(p) == 2 && p[0] != "" && p[1] != "" {
			s.tokens[p[0]] = p[1]
		}
	}
	if e = migrateConfiguredRoles(s); e != nil {
		log.Fatal("cannot migrate configured roles")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/", dashboard)
	addr := env("LISTEN_ADDR", ":8080")
	log.Printf("agent-mail listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, secureHeaders(mux)))
}
func migrateConfiguredRoles(s *Server) error {
	for role, token := range s.tokens {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO roles(role,token,created_at) VALUES(?,?,?)`, role, token, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}
func env(k, d string) string {
	if x := os.Getenv(k); x != "" {
		return x
	}
	return d
}
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) auth(r *http.Request) (string, bool) {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	expected := r.Header.Get("X-Agent-Mail-Role")
	var role string
	if t == "" || expected == "" || !validRole(expected) {
		return "", false
	}
	if s.db.QueryRow(`SELECT role FROM roles WHERE token=?`, t).Scan(&role) == nil && role == expected {
		return role, true
	}
	return "", false
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/admin/") {
		s.adminAPI(w, r)
		return
	}
	if r.URL.Path == "/api/register" {
		s.register(w, r)
		return
	}
	role, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if path == "messages" {
		switch r.Method {
		case "POST":
			s.send(w, r, role)
		case "GET":
			s.list(w, r, role)
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}
	if path == "watch" && r.Method == "GET" {
		s.watch(w, r, role)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "messages" {
		id, e := parseID(parts[1])
		if e != nil {
			http.Error(w, "invalid id", 400)
			return
		}
		if len(parts) == 2 && r.Method == "GET" {
			s.read(w, r, role, id)
			return
		}
		if len(parts) == 3 && r.Method == "POST" {
			if parts[2] == "reply" {
				s.reply(w, r, role, id)
				return
			}
			if parts[2] == "ack" || parts[2] == "complete" || parts[2] == "read" {
				s.status(w, r, role, id, parts[2])
				return
			}
		}
	}
	http.NotFound(w, r)
}

// adminAPI exposes read-only mailbox reporting only to a reverse proxy that
// injects a server-held secret after authenticating its user. The backend
// must remain private; caller-supplied identity headers are never trusted.
func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request) {
	provided := r.Header.Get("X-Agent-Mail-Admin")
	if s.adminProxyToken == "" || len(provided) != len(s.adminProxyToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.adminProxyToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	switch path {
	case "roles":
		rows, err := s.db.Query(`SELECT role FROM roles ORDER BY role`)
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		defer rows.Close()
		roles := []string{}
		for rows.Next() {
			var role string
			if rows.Scan(&role) == nil {
				roles = append(roles, role)
			}
		}
		writeJSON(w, roles)
	case "messages":
		adminList(w, r, s.db)
	default:
		http.NotFound(w, r)
	}
}

func adminList(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	q, status, role, box := strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("status"), r.URL.Query().Get("role"), r.URL.Query().Get("box")
	thread := strings.TrimSpace(r.URL.Query().Get("thread"))
	if box != "inbox" && box != "outbox" {
		box = "all"
	}
	query := `SELECT m.id,m.sender,m.recipient,m.subject,m.body,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0),COALESCE(m.reply_to,0),COALESCE(rs.status,m.status,'pending') FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient WHERE 1=1`
	args := []any{}
	if role != "" {
		query += ` AND (m.sender=? OR m.recipient=?)`
		args = append(args, role, role)
	}
	if box == "inbox" && role != "" {
		query += ` AND m.recipient=?`
		args = append(args, role)
	}
	if box == "outbox" && role != "" {
		query += ` AND m.sender=?`
		args = append(args, role)
	}
	if q != "" {
		query += ` AND (m.subject LIKE ? OR m.body LIKE ? OR m.task_id LIKE ? OR m.sender LIKE ? OR m.recipient LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like)
	}
	if status != "" {
		values := statusValues(status)
		query += ` AND COALESCE(rs.status,m.status,'pending') IN (?` + strings.Repeat(",?", len(values)-1) + `)`
		args = append(args, values...)
	}
	if thread != "" {
		query += ` AND (m.thread_id=? OR m.id=?)`
		args = append(args, thread, thread)
	}
	query += ` ORDER BY m.id DESC LIMIT 500`
	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var created string
		if rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &created, &m.TaskID, &m.ThreadID, &m.ReplyTo, &m.Status) == nil {
			m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
			out = append(out, m)
		}
	}
	writeJSON(w, out)
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Role       string `json:"role"`
		Credential string `json:"credential"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || !validRole(req.Role) || req.Credential == "" {
		http.Error(w, "role and credential required", 400)
		return
	}
	var existing string
	err := s.db.QueryRow(`SELECT token FROM roles WHERE role=?`, req.Role).Scan(&existing)
	if err == nil {
		if len(existing) != len(req.Credential) || subtle.ConstantTimeCompare([]byte(existing), []byte(req.Credential)) != 1 {
			http.Error(w, "role already registered; valid credential required to resume", 409)
			return
		}
		writeJSON(w, map[string]string{"role": req.Role, "status": "resumed"})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "registration failed", 500)
		return
	}
	enrollment := os.Getenv("AGENT_MAIL_ENROLLMENT_TOKEN")
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(req.Credential) < 24 || enrollment == "" || len(enrollment) != len(provided) || subtle.ConstantTimeCompare([]byte(enrollment), []byte(provided)) != 1 {
		http.Error(w, "enrollment authorization required", 401)
		return
	}
	if len(enrollment) == len(req.Credential) && subtle.ConstantTimeCompare([]byte(enrollment), []byte(req.Credential)) == 1 {
		http.Error(w, "role credential must differ from enrollment credential", 400)
		return
	}
	_, err = s.db.Exec(`INSERT INTO roles(role,token,created_at) VALUES(?,?,?)`, req.Role, req.Credential, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		http.Error(w, "role registration failed", 409)
		return
	}
	w.WriteHeader(201)
	writeJSON(w, map[string]string{"role": req.Role, "status": "registered"})
}
func validRole(role string) bool {
	if len(role) < 1 || len(role) > 128 {
		return false
	}
	for _, c := range role {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-", c)) {
			return false
		}
	}
	return true
}
func parseID(x string) (int64, error)        { return strconv.ParseInt(x, 10, 64) }
func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func (s *Server) send(w http.ResponseWriter, r *http.Request, role string) {
	var m Message
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&m) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	m.Sender = role
	if m.Recipient == "" || m.Subject == "" {
		http.Error(w, "recipient and subject required", 400)
		return
	}
	var allowed int
	if s.db.QueryRow(`SELECT 1 FROM roles WHERE role=?`, m.Recipient).Scan(&allowed) != nil {
		http.Error(w, "unknown recipient role", 403)
		return
	}
	if m.Recipient == role { /* allowed */
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = m.IdempotencyKey
	}
	if key != "" {
		var existing Message
		var created string
		e := s.db.QueryRow(`SELECT id,sender,recipient,subject,body,created_at,COALESCE(task_id,''),COALESCE(thread_id,0),COALESCE(reply_to,0),status FROM messages WHERE sender=? AND idem=?`, role, key).Scan(&existing.ID, &existing.Sender, &existing.Recipient, &existing.Subject, &existing.Body, &created, &existing.TaskID, &existing.ThreadID, &existing.ReplyTo, &existing.Status)
		if e == nil {
			existing.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
			w.WriteHeader(200)
			writeJSON(w, existing)
			return
		}
	}
	now := time.Now().UTC()
	if m.ThreadID == 0 && m.ReplyTo > 0 {
		m.ThreadID = m.ReplyTo
	}
	res, e := s.db.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,thread_id,reply_to,idem) VALUES(?,?,?,?,?,?,?,?,?)`, role, m.Recipient, m.Subject, m.Body, now.Format(time.RFC3339Nano), m.TaskID, nullInt(m.ThreadID), nullInt(m.ReplyTo), nullable(key))
	if e != nil {
		http.Error(w, "could not store message", 500)
		return
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt = now
	m.Status = "pending"
	w.WriteHeader(201)
	writeJSON(w, m)
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullInt(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}
func (s *Server) list(w http.ResponseWriter, r *http.Request, role string) {
	box := r.URL.Query().Get("box")
	if box != "outbox" {
		box = "inbox"
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	limit := 100
	query := `SELECT m.id,m.sender,m.recipient,m.subject,m.body,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0),COALESCE(m.reply_to,0),COALESCE(CASE WHEN rs.status IS NOT NULL THEN rs.status ELSE m.status END,'pending') FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=CASE WHEN m.sender=? THEN m.recipient ELSE ? END WHERE `
	args := []any{role, role}
	if box == "outbox" {
		query += `m.sender=? `
	} else {
		query += `m.recipient=? `
	}
	args = append(args, role)
	if q != "" {
		query += `AND (m.subject LIKE ? OR m.body LIKE ? OR m.task_id LIKE ?) `
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	if status != "" {
		values := statusValues(status)
		query += `AND COALESCE(rs.status,m.status) IN (?` + strings.Repeat(",?", len(values)-1) + `) `
		args = append(args, values...)
	}
	query += `ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)
	rows, e := s.db.Query(query, args...)
	if e != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var created string
		if rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &created, &m.TaskID, &m.ThreadID, &m.ReplyTo, &m.Status) == nil {
			m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
			out = append(out, m)
		}
	}
	writeJSON(w, out)
}
func (s *Server) read(w http.ResponseWriter, r *http.Request, role string, id int64) {
	m, e := s.get(id)
	if e != nil || (m.Sender != role && m.Recipient != role) {
		http.Error(w, "not found", 404)
		return
	}
	if m.Recipient == role {
		_, _ = s.db.Exec(`INSERT INTO role_states(role,message_id,status,updated_at) VALUES(?,?,?,?) ON CONFLICT(role,message_id) DO UPDATE SET status=CASE WHEN role_states.status='pending' THEN 'read' ELSE role_states.status END,updated_at=excluded.updated_at`, role, id, "read", time.Now().UTC().Format(time.RFC3339Nano))
	}
	_ = s.db.QueryRow(`SELECT COALESCE(status,'pending') FROM role_states WHERE role=? AND message_id=?`, role, id).Scan(&m.Status)
	writeJSON(w, m)
}
func (s *Server) get(id int64) (Message, error) {
	var m Message
	var c string
	e := s.db.QueryRow(`SELECT id,sender,recipient,subject,body,created_at,COALESCE(task_id,''),COALESCE(thread_id,0),COALESCE(reply_to,0),status FROM messages WHERE id=?`, id).Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &c, &m.TaskID, &m.ThreadID, &m.ReplyTo, &m.Status)
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
	return m, e
}

// statusValues maps a status filter to stored values. /complete stores "complete";
// "completed" (the documented spelling before v0.4.1) is accepted as the same state.
func statusValues(status string) []any {
	if status == "complete" || status == "completed" {
		return []any{"complete", "completed"}
	}
	return []any{status}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, role string, id int64, status string) {
	if status == "ack" {
		status = "acknowledged"
	}
	m, e := s.get(id)
	if e != nil || m.Recipient != role {
		http.Error(w, "not found", 404)
		return
	}
	_, e = s.db.Exec(`INSERT INTO role_states(role,message_id,status,updated_at) VALUES(?,?,?,?) ON CONFLICT(role,message_id) DO UPDATE SET status=excluded.status,updated_at=excluded.updated_at`, role, id, status, time.Now().UTC().Format(time.RFC3339Nano))
	if e != nil {
		http.Error(w, "update failed", 500)
		return
	}
	m.Status = status
	writeJSON(w, m)
}
func (s *Server) reply(w http.ResponseWriter, r *http.Request, role string, id int64) {
	orig, e := s.get(id)
	if e != nil || orig.Recipient != role {
		http.Error(w, "not found", 404)
		return
	}
	var m Message
	if json.NewDecoder(r.Body).Decode(&m) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if m.ThreadID == 0 {
		m.ThreadID = orig.ID
		if orig.ThreadID != 0 {
			m.ThreadID = orig.ThreadID
		}
	}
	m.ReplyTo = id
	if m.Recipient == "" {
		m.Recipient = orig.Sender
	}
	b, _ := json.Marshal(m)
	r.Body = io.NopCloser(bytes.NewReader(b))
	s.send(w, r, role)
}
func (s *Server) watch(w http.ResponseWriter, r *http.Request, role string) {
	after, _ := parseID(r.URL.Query().Get("after"))
	deadline := time.Now().Add(25 * time.Second)
	for {
		rows, e := s.db.Query(`SELECT id FROM messages WHERE recipient=? AND id>? ORDER BY id LIMIT 100`, role, after)
		if e != nil {
			http.Error(w, "query failed", 500)
			return
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) > 0 {
			writeJSON(w, map[string]any{"messages": ids, "cursor": ids[len(ids)-1]})
			return
		}
		if time.Now().After(deadline) {
			writeJSON(w, map[string]any{"messages": ids, "cursor": after, "timeout": true})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}
func dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Agent Mail</title><style>body{font:16px system-ui;max-width:1100px;margin:2rem auto;padding:0 1rem;background:#f5f7fa;color:#172033}input,button,select{font:inherit;padding:.6rem;margin:.25rem}button{cursor:pointer}.row{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap}.card{background:white;padding:1rem;margin:.7rem 0;border-radius:8px;box-shadow:0 1px 5px #ccd}pre{white-space:pre-wrap;overflow-wrap:anywhere}.muted{color:#667}</style></head><body><h1>Agent Mail · Admin overview</h1><p>Read-only view of all registered roles and mailboxes.</p><section class="card row"><select id="box"><option value="all">All messages</option><option value="inbox">All inbox</option><option value="outbox">All outbox</option></select><select id="role"><option value="">All roles</option></select><select id="status"><option value="">All statuses</option><option>pending</option><option>read</option><option>acknowledged</option><option>complete</option></select><input id="search" placeholder="Search content, roles or task"><input id="thread" type="number" placeholder="Thread ID"><button onclick="load()">Search</button><span id="conn">Connecting…</span></section><div id="list"></div><div id="detail"></div><script>async function api(path){let r;try{r=await fetch('/api/admin/'+path,{credentials:'same-origin',headers:{'Accept':'application/json'}})}catch(e){throw Error('Network error. Check the private service connection and sign-in.')}if(r.redirected||!r.headers.get('content-type')?.includes('application/json'))throw Error('The request was redirected to a sign-in page. Sign in through the protected service URL and reload.');if(!r.ok)throw Error(r.status===401?'Admin access is not configured for this protected proxy.':'Request failed ('+r.status+').');return r.json()}function node(tag,text,cls){let n=document.createElement(tag);n.textContent=text;if(cls)n.className=cls;return n}async function init(){try{for(let role of await api('roles'))document.getElementById('role').append(new Option(role,role));await load()}catch(e){document.getElementById('conn').textContent=e.message}}async function load(){document.getElementById('conn').textContent='Loading…';document.getElementById('detail').replaceChildren();try{let q=new URLSearchParams({box:document.getElementById('box').value,role:document.getElementById('role').value,status:document.getElementById('status').value,q:document.getElementById('search').value,thread:document.getElementById('thread').value});let rows=await api('messages?'+q);let list=document.getElementById('list');list.replaceChildren();for(let m of rows){let card=node('article','', 'card');let title=node('h3','#'+m.id+' '+m.subject);let meta=node('p',m.sender+' → '+m.recipient+' · '+new Date(m.created_at).toLocaleString()+' · '+m.status,'muted');let open=node('button','Open');open.onclick=()=>show(m);card.append(title,meta,open);list.append(card)}document.getElementById('conn').textContent=rows.length+' messages (maximum 500)'}catch(e){document.getElementById('conn').textContent=e.message}}function show(m){let card=node('section','','card');card.append(node('h2',m.subject),node('p','#'+m.id+' · '+m.sender+' → '+m.recipient+' · '+m.status));let pre=node('pre',m.body);card.append(pre);if(m.thread_id)card.append(node('p','Thread #'+m.thread_id));document.getElementById('detail').replaceChildren(card)}document.getElementById('thread').addEventListener('input',()=>{});init()</script></body></html>`)
}
