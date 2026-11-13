package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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
	db     *sql.DB
	tokens map[string]string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("agent-mail dev")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "config" {
		cliConfig(os.Args[2:])
		return
	}
	if len(os.Args) > 1 {
		cli(os.Args[1:])
		return
	}
	runServer()
}
func runServer() {
	path := env("DB_PATH", "/data/mail.db")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	db, e := sql.Open("sqlite", path)
	if e != nil {
		log.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,recipient TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,task_id TEXT,thread_id INTEGER,reply_to INTEGER,status TEXT NOT NULL DEFAULT 'pending',idem TEXT, UNIQUE(sender,idem)); CREATE INDEX IF NOT EXISTS idx_recipient ON messages(recipient,id); CREATE INDEX IF NOT EXISTS idx_sender ON messages(sender,id); CREATE TABLE IF NOT EXISTS role_states(role TEXT NOT NULL,message_id INTEGER NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(role,message_id));`)
	if e != nil {
		log.Fatal(e)
	}
	s := &Server{db: db, tokens: map[string]string{}}
	for _, pair := range strings.Split(os.Getenv("AGENT_MAIL_TOKENS"), ",") {
		p := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(p) == 2 && p[0] != "" && p[1] != "" {
			s.tokens[p[0]] = p[1]
		}
	}
	if len(s.tokens) == 0 {
		log.Fatal("AGENT_MAIL_TOKENS required (role=token, comma separated)")
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
	for role, v := range s.tokens {
		if len(v) == len(t) && subtle.ConstantTimeCompare([]byte(v), []byte(t)) == 1 {
			return role, true
		}
	}
	return "", false
}
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
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
	if _, allowed := s.tokens[m.Recipient]; !allowed {
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
		query += `AND COALESCE(rs.status,m.status)=? `
		args = append(args, status)
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Agent Mail</title><style>body{font:16px system-ui;max-width:1100px;margin:2rem auto;padding:0 1rem;background:#f5f7fa;color:#172033}input,textarea,button,select{font:inherit;padding:.6rem;margin:.25rem}input,textarea{border:1px solid #ccd3df;border-radius:6px}button{cursor:pointer}header,.row{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap}.card{background:white;padding:1rem;margin:.7rem 0;border-radius:8px;box-shadow:0 1px 5px #ccd}pre{white-space:pre-wrap;overflow-wrap:anywhere}.muted{color:#667}</style></head><body><h1>Agent Mail</h1><p>Role mailbox · content is displayed as text</p><section class="card"><input id="endpoint" placeholder="Service URL" value=""><input id="token" type="password" placeholder="Bearer token"><button onclick="save()">Connect</button><span id="conn">Disconnected</span></section><div class="row"><button onclick="loadBox('inbox')">Inbox</button><button onclick="loadBox('outbox')">Outbox</button><input id="search" placeholder="Search"><button onclick="loadBox(box)">Search</button><select id="status"><option value="">All statuses</option><option>pending</option><option>read</option><option>acknowledged</option><option>completed</option></select></div><div id="list"></div><section class="card"><h2>Send message</h2><input id="to" placeholder="Recipient role"><input id="subject" placeholder="Subject"><input id="task" placeholder="Task ID"><textarea id="body" rows="5" style="width:95%" placeholder="Markdown or plain text"></textarea><button onclick="send()">Send</button><span id="sendresult"></span></section><div id="detail"></div><script>let box='inbox';function config(){return JSON.parse(localStorage.getItem('amcfg')||'{"endpoint":""}')}function save(){let c={endpoint:document.getElementById('endpoint').value.replace(/\/$/,''),token:document.getElementById('token').value};localStorage.setItem('amcfg',JSON.stringify(c));loadBox('inbox')}async function api(p,opt={}){let c=config();let r=await fetch(c.endpoint+'/api/'+p,{...opt,headers:{'Content-Type':'application/json','Authorization':'Bearer '+c.token,...(opt.headers||{})}});if(!r.ok)throw Error(r.status+' '+await r.text());return r.json()}function init(){let c=config();document.getElementById('endpoint').value=c.endpoint||location.origin;document.getElementById('token').value=c.token||''}async function loadBox(b){box=b;document.getElementById('conn').textContent='Connecting…';try{let q=new URLSearchParams({box:b,q:document.getElementById('search').value,status:document.getElementById('status').value});let a=await api('messages?'+q);document.getElementById('conn').textContent='Connected';let el=document.getElementById('list');el.replaceChildren();for(let m of a){let d=document.createElement('article');d.className='card';let h=document.createElement('h3');h.textContent='#'+m.id+' '+m.subject;let p=document.createElement('p');p.className='muted';p.textContent=m.sender+' → '+m.recipient+' · '+new Date(m.created_at).toLocaleString()+' · '+m.status;let btn=document.createElement('button');btn.textContent='Open';btn.onclick=()=>show(m.id);d.append(h,p,btn);el.append(d)}}catch(e){document.getElementById('conn').textContent='Error: '+e.message}}async function show(id){let m=await api('messages/'+id);let d=document.getElementById('detail');d.replaceChildren();let card=document.createElement('section');card.className='card';let h=document.createElement('h2');h.textContent=m.subject;let meta=document.createElement('p');meta.textContent='#'+m.id+' '+m.sender+' → '+m.recipient+' · '+m.status;let body=document.createElement('pre');body.textContent=m.body;card.append(h,meta,body);for(let s of ['ack','complete']){let b=document.createElement('button');b.textContent=s;b.onclick=async()=>{await api('messages/'+id+'/'+s,{method:'POST'});show(id);loadBox(box)};card.append(b)}let ta=document.createElement('textarea');ta.placeholder='Reply';let rb=document.createElement('button');rb.textContent='Reply';rb.onclick=async()=>{await api('messages/'+id+'/reply',{method:'POST',body:JSON.stringify({subject:'Re: '+m.subject,body:ta.value})});ta.value='';loadBox(box)};card.append(ta,rb);d.append(card)}async function send(){try{let r=await api('messages',{method:'POST',headers:{'Idempotency-Key':crypto.randomUUID()},body:JSON.stringify({recipient:document.getElementById('to').value,subject:document.getElementById('subject').value,task_id:document.getElementById('task').value,body:document.getElementById('body').value})});document.getElementById('sendresult').textContent='Sent #'+r.id;loadBox(box)}catch(e){document.getElementById('sendresult').textContent=e.message}}init();</script></body></html>`)
}

type Config struct {
	Endpoint string `json:"endpoint"`
	Role     string `json:"role"`
	Token    string `json:"token"`
}

func configPath() string {
	d, e := os.UserConfigDir()
	if e != nil {
		d = "."
	}
	return filepath.Join(d, "agent-mail", "config.json")
}
func readConfig() Config {
	b, e := os.ReadFile(configPath())
	if e != nil {
		return Config{}
	}
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}
func cliConfig(args []string) {
	if len(args) < 2 || args[0] != "set" {
		fmt.Fprintln(os.Stderr, "usage: agent-mail config set --endpoint URL --role ROLE --token-file PATH")
		os.Exit(2)
	}
	c := readConfig()
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--endpoint":
			i++
			if i < len(args) {
				c.Endpoint = args[i]
			}
		case "--role":
			i++
			if i < len(args) {
				c.Role = args[i]
			}
		case "--token-file":
			i++
			if i < len(args) {
				b, e := os.ReadFile(args[i])
				if e != nil {
					fatal("cannot read token file")
				}
				c.Token = strings.TrimSpace(string(b))
			}
		}
	}
	if c.Endpoint == "" || c.Role == "" || c.Token == "" {
		fatal("endpoint, role and token required")
	}
	p := configPath()
	_ = os.MkdirAll(filepath.Dir(p), 0700)
	b, _ := json.MarshalIndent(c, "", "  ")
	if os.WriteFile(p, b, 0600) != nil {
		fatal("cannot write config")
	}
	fmt.Println("Saved", p)
}
func cli(args []string) {
	c := readConfig()
	if c.Endpoint == "" || c.Token == "" || c.Role == "" {
		fatal("configure with: agent-mail config set --endpoint URL --role ROLE --token-file PATH")
	}
	cmd := args[0]
	flags := map[string]string{}
	jsonMode := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--json" {
			jsonMode = true
			continue
		}
		if strings.HasPrefix(args[i], "--") && i+1 < len(args) {
			flags[args[i]] = args[i+1]
			i++
		}
	}
	if cmd == "send" {
		body, e := inputBody(flags)
		if e != nil {
			fatal(e.Error())
		}
		v := map[string]any{"recipient": flags["--to"], "subject": flags["--subject"], "body": body, "task_id": flags["--task"]}
		b, _ := json.Marshal(v)
		key := flags["--idempotency-key"]
		req, _ := http.NewRequest("POST", strings.TrimRight(c.Endpoint, "/")+"/api/messages", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Token)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		doRequest(req, jsonMode)
		return
	}
	if cmd == "inbox" || cmd == "outbox" {
		u := strings.TrimRight(c.Endpoint, "/") + "/api/messages?box=" + cmd
		if flags["--q"] != "" {
			u += "&q=" + template.URLQueryEscaper(flags["--q"])
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		doRequest(req, jsonMode)
		return
	}
	if cmd == "watch" {
		watchCLI(c, flags, jsonMode)
		return
	}
	if cmd == "read" || cmd == "ack" || cmd == "complete" {
		if len(args) < 2 {
			fatal(cmd + " requires message ID")
		}
		id := args[1]
		method := "GET"
		url := strings.TrimRight(c.Endpoint, "/") + "/api/messages/" + id
		if cmd != "read" {
			method = "POST"
			url += "/" + cmd
		}
		req, _ := http.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		doRequest(req, jsonMode)
		return
	}
	if cmd == "reply" {
		if len(args) < 2 {
			fatal("reply requires message ID")
		}
		body, e := inputBody(flags)
		if e != nil {
			fatal(e.Error())
		}
		b, _ := json.Marshal(map[string]any{"subject": flags["--subject"], "body": body, "recipient": flags["--to"]})
		req, _ := http.NewRequest("POST", strings.TrimRight(c.Endpoint, "/")+"/api/messages/"+args[1]+"/reply", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		doRequest(req, jsonMode)
		return
	}
	fatal("commands: send inbox outbox read reply ack complete watch config")
}
func inputBody(flags map[string]string) (string, error) {
	if f := flags["--body-file"]; f != "" {
		b, e := os.ReadFile(f)
		return string(b), e
	}
	if flags["--body"] != "" {
		return flags["--body"], nil
	}
	st, e := os.Stdin.Stat()
	if e == nil && st.Mode()&os.ModeCharDevice == 0 {
		b, e := io.ReadAll(os.Stdin)
		return string(b), e
	}
	return "", errors.New("provide --body, --body-file or stdin")
}
func doRequest(req *http.Request, jsonMode bool) {
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		fatal(e.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		fatal(fmt.Sprintf("server returned %s: %s", resp.Status, string(b)))
	}
	if jsonMode {
		var out bytes.Buffer
		if json.Indent(&out, b, "", "  ") == nil {
			b = out.Bytes()
		}
		fmt.Println(string(b))
		return
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		pretty(v)
	} else {
		fmt.Println(string(b))
	}
}
func pretty(v any) {
	switch x := v.(type) {
	case []any:
		for _, y := range x {
			pretty(y)
			fmt.Println()
		}
	case map[string]any:
		fmt.Printf("#%v %v\n%s → %s · %s\n%s\n", x["id"], x["subject"], x["sender"], x["recipient"], x["status"], x["body"])
	default:
		fmt.Println(v)
	}
}
func watchCLI(c Config, flags map[string]string, jsonMode bool) {
	after, _ := parseID(flags["--after"])
	for {
		u := fmt.Sprintf("%s/api/watch?after=%d", strings.TrimRight(c.Endpoint, "/"), after)
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var v struct {
			Messages []int64 `json:"messages"`
			Cursor   int64   `json:"cursor"`
		}
		e = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if e != nil {
			time.Sleep(time.Second)
			continue
		}
		if v.Cursor > after {
			after = v.Cursor
		}
		for _, id := range v.Messages {
			if jsonMode {
				fmt.Printf("{\"message_id\":%d,\"cursor\":%d}\n", id, after)
			} else {
				fmt.Printf("New message #%d (cursor %d)\n", id, after)
			}
		}
	}
}
func fatal(s string) { fmt.Fprintln(os.Stderr, s); os.Exit(1) }

var _ = bufio.NewReader
var _ = context.Background
