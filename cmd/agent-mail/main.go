package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sandcorporation/agent-mail/internal/cli"
	"github.com/sandcorporation/agent-mail/internal/config"
	"github.com/sandcorporation/agent-mail/internal/store"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

type Server struct {
	db               *sql.DB
	tokens           map[string]string
	adminProxyToken  string
	hub              mailHub
	eventKeepAlive   time.Duration
	pollMax          time.Duration       // cap on a long poll's wait; 0 means defaultPollMax
	done             chan struct{}       // closed on shutdown; ends event streams
	slotLease        time.Duration       // AGENT_MAIL_SLOT_LEASE; 0 means presence interval × missed
	presenceInterval time.Duration       // expected heartbeat interval; 0 means defaultPresenceInterval
	presenceMissed   int                 // heartbeats a session may miss; 0 means defaultPresenceMissed
	presenceRetain   *time.Duration      // how long offline sessions stay listed; nil means defaultPresenceRetain
	clock            func() time.Time    // nil means time.Now; tests move time forward
	sweepEvery       time.Duration       // AGENT_MAIL_SWEEP_INTERVAL; 0 means defaultSweepInterval
	labelStall       time.Duration       // AGENT_MAIL_LABEL_STALL; 0 means defaultLabelStall
	permissionTTL    time.Duration       // AGENT_MAIL_PERMISSION_TTL; 0 means defaultPermissionTTL
	bossRoles        map[string]bool     // AGENT_MAIL_BOSS_ROLES; may answer any permission request
	discord          *discordNotifier    // AGENT_MAIL_DISCORD_WEBHOOK; nil sends no question notices
	alerts           *alertThresholds    // SWITCHBOARD_ALERT_*; nil means defaultAlertThresholds
	oldHeaders       config.AliasCounter // requests that used an X-Agent-Mail-* header
	groups           groupCache          // collaborator groups (groups.go)
	adminMode        string              // SWITCHBOARD_ADMIN_AUTH; "" is default deny or the transition rule (adminauth.go)
	adminSessionTTL  time.Duration       // 0 means defaultSessionTTL
	insecureCookie   bool                // SWITCHBOARD_INSECURE_COOKIE=1: session cookie without Secure, for local http
	trustForwarded   bool                // SWITCHBOARD_TRUST_FORWARDED=1: client IP from X-Forwarded-For
	loginLimits      loginLimiter
	oidc             *oidcLogin   // SWITCHBOARD_ADMIN_AUTH=oidc (oidc.go)
	joinLimits       loginLimiter // failed POST /api/join per client IP (invites.go)
}

var version = "dev"

func main() {
	// Copy the arguments so parsing never rewrites os.Args.
	args := append([]string(nil), os.Args[1:]...)
	if len(args) == 0 {
		runServer()
		return
	}
	os.Exit(cli.Run(args, version, cliAdmin))
}

func runServer() {
	db, e := store.Open(config.Env("DB_PATH", "/data/mail.db"))
	if e != nil {
		log.Fatal(e)
	}
	s := &Server{db: db, tokens: map[string]string{}, adminProxyToken: config.EnvAlias("ADMIN_PROXY_TOKEN"), done: make(chan struct{})}
	if v, from := config.EnvAliasFrom(os.Getenv, "SSE_KEEPALIVE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			log.Fatal(from + " must be a positive duration such as 20s")
		}
		s.eventKeepAlive = d
	}
	if v, from := config.EnvAliasFrom(os.Getenv, "POLL_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatal(from + " must be a positive whole number of seconds such as 55")
		}
		s.pollMax = time.Duration(n) * time.Second
	}
	if v, from := config.EnvAliasFrom(os.Getenv, "SLOT_LEASE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			log.Fatal(from + " must be a duration of at least 1s such as 3m")
		}
		s.slotLease = d
	}
	if e = configurePresence(s, os.Getenv); e != nil {
		log.Fatal(e)
	}
	if e = configureStall(s, os.Getenv); e != nil {
		log.Fatal(e)
	}
	if e = configureAlerts(s, os.Getenv); e != nil {
		log.Fatal(e)
	}
	configureDiscord(s, os.Getenv)
	if e = configureAdminAuth(s, os.Getenv); e != nil {
		log.Fatal(e)
	}
	for _, pair := range strings.Split(config.EnvAlias("TOKENS"), ",") {
		p := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(p) == 2 && p[0] != "" && p[1] != "" {
			s.tokens[p[0]] = p[1]
		}
	}
	if e = migrateConfiguredRoles(s); e != nil {
		log.Fatal("cannot migrate configured roles")
	}
	if e = s.loadGroups(); e != nil {
		log.Fatal("cannot load groups")
	}
	if old := config.OldEnvNames(); len(old) > 0 {
		log.Printf("old environment names in use (rename AGENT_MAIL_* to SWITCHBOARD_*): %s", strings.Join(old, ", "))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/auth/", s.authAPI)
	mux.HandleFunc("/", s.root)
	addr := config.Env("LISTEN_ADDR", ":8080")
	// No WriteTimeout: /api/events responses stay open indefinitely and
	// /api/events/poll waits up to AGENT_MAIL_POLL_MAX.
	srv := &http.Server{Addr: addr, Handler: secureHeaders(mux), ReadHeaderTimeout: 10 * time.Second}
	srv.RegisterOnShutdown(func() { close(s.done) })
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	stopped := make(chan struct{})
	waitSweeper := s.startSweeper()
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(stopped)
	}()
	log.Printf("agent-mail listening on %s", addr)
	if e = srv.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Fatal(e)
	}
	<-stopped
	waitSweeper()
	_ = db.Close()
}

// normalizePriority returns the stored priority for a request value: empty
// means normal; anything other than normal or urgent is rejected.
func normalizePriority(p string) (string, bool) {
	switch p {
	case "":
		return "normal", true
	case "normal", "urgent":
		return p, true
	}
	return "", false
}

func migrateConfiguredRoles(s *Server) error {
	for role, token := range s.tokens {
		if reservedRole(role) {
			log.Printf("SWITCHBOARD_TOKENS: %s is reserved for system notices; ignored", role)
			continue
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO roles(role,token,created_at) VALUES(?,?,?)`, role, store.TokenHash(token), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// auth returns the mailbox name of an authenticated request (see authenticate).
func (s *Server) auth(r *http.Request) (string, bool) {
	p, ok := s.authenticate(r)
	return p.Role, ok
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
	if r.URL.Path == "/api/join" {
		s.join(w, r)
		return
	}
	who, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	role := who.Role
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if path == "permissions" || strings.HasPrefix(path, "permissions/") {
		s.permissionAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "permissions"), "/"))
		return
	}
	if path == "questions" || strings.HasPrefix(path, "questions/") {
		s.questionAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "questions"), "/"))
		return
	}
	if path == "decisions" {
		s.decisionsAPI(w, r, &who)
		return
	}
	if path == "presence" {
		s.presenceAPI(w, r, &who)
		return
	}
	if path == "whoami" {
		s.whoamiAPI(w, r, who)
		return
	}
	if path == "invites" {
		s.invitesAPI(w, r, who)
		return
	}
	if path == "metrics" {
		s.metricsAPI(w, r, s.isGlobalBoss(who), false)
		return
	}
	if path == "progress" {
		s.progressAPI(w, r, s.isGlobalBoss(who))
		return
	}
	if path == "slots" || strings.HasPrefix(path, "slots/") {
		s.slotAPI(w, r, who, strings.TrimPrefix(strings.TrimPrefix(path, "slots"), "/"))
		return
	}
	if path == "messages" {
		switch r.Method {
		case "POST":
			s.send(w, r, who)
		case "GET":
			s.list(w, r, role)
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}
	if path == "events/poll" && r.Method == "GET" {
		s.pollEvents(w, r, role)
		return
	}
	if path == "events" && r.Method == "GET" {
		s.events(w, r, role)
		return
	}
	if path == "watch" && r.Method == "GET" {
		s.watch(w, r, role)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "messages" {
		id, e := wire.ParseID(parts[1])
		if e != nil {
			http.Error(w, "invalid id", 400)
			return
		}
		if box, other := s.otherSlotMailbox(who, id); other {
			http.Error(w, "message belongs to "+box+"; send X-Switchboard-Role: "+box+" (CLI: switchboard --role "+box+" or SWITCHBOARD_ROLE)", 403)
			return
		}
		if len(parts) == 2 && r.Method == "GET" {
			s.read(w, r, role, id)
			return
		}
		if len(parts) == 3 && r.Method == "POST" {
			if parts[2] == "reply" {
				s.reply(w, r, who, id)
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

// adminAPI exposes mailbox reporting and settings to a signed-in admin (see
// adminIdentify; under the transition rule, a reverse proxy that injects the
// server-held secret). Caller-supplied identity headers are never trusted.
// Writes (PUT, POST, DELETE) also need the dashboard CSRF guard.
func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request) {
	who, ok := s.adminIdentify(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut, http.MethodPost, http.MethodDelete:
		if !adminWriteAllowed(r, s.header(r, "Dashboard")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/")
	if path == "question-settings" || strings.HasPrefix(path, "question-settings/") {
		target, hasTarget := strings.CutPrefix(path, "question-settings/")
		s.questionSettingsAPI(w, r, target, hasTarget)
		return
	}
	if rest, ok := strings.CutPrefix(path, "questions"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.adminQuestionsAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "roles"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.adminRolesAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "groups"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.adminGroupsAPI(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(path, "invites"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		s.adminInvitesAPI(w, r, who, strings.TrimPrefix(rest, "/"))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch path {
	case "me":
		writeJSON(w, who)
	case "collaborators":
		out, err := s.collaborators()
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
	case "slots":
		out, err := s.activeSlots("")
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
	case "presence":
		s.presenceAPI(w, r, nil)
	case "metrics":
		s.metricsAPI(w, r, true, true)
	case "progress":
		s.progressAPI(w, r, true)
	case "messages":
		adminList(w, r, s.db)
	case "decisions":
		s.decisionsAPI(w, r, nil)
	default:
		http.NotFound(w, r)
	}
}

func adminList(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	q, status, role, box := strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("status"), r.URL.Query().Get("role"), r.URL.Query().Get("box")
	thread := strings.TrimSpace(r.URL.Query().Get("thread"))
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if mb, ok := mailbox(db, role); ok {
		role = mb
	}
	if box != "inbox" && box != "outbox" && box != "held" {
		box = "all"
	}
	query := `SELECT ` + messageCols(`COALESCE(rs.status,m.status,'pending')`) + ` FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient WHERE 1=1`
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
	if box == "held" {
		query += ` AND m.status='held'`
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
	if task != "" {
		query += ` AND m.task_id=?`
		args = append(args, task)
	}
	if id != "" {
		query += ` AND m.id=?`
		args = append(args, id)
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
		if m, err := scanMessage(rows); err == nil {
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
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || !wire.ValidRole(req.Role) || req.Credential == "" {
		http.Error(w, "role and credential required", 400)
		return
	}
	if reservedRole(req.Role) {
		http.Error(w, "role name is reserved", 403)
		return
	}
	if g, ok := s.groupOf(req.Role); ok && g.RevokedAt != nil {
		http.Error(w, "group revoked", 403)
		return
	}
	var existing string
	var revokedAt sql.NullString
	err := s.db.QueryRow(`SELECT token,revoked_at FROM roles WHERE role=?`, req.Role).Scan(&existing, &revokedAt)
	if err == nil {
		if !tokenMatches(existing, req.Credential) {
			http.Error(w, "role already registered; valid credential required to resume", 409)
			return
		}
		if revokedAt.Valid {
			http.Error(w, "role revoked", 401)
			return
		}
		writeJSON(w, map[string]string{"role": req.Role, "status": "resumed"})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "registration failed", 500)
		return
	}
	enrollment := config.EnvAlias("ENROLLMENT_TOKEN")
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(req.Credential) < 24 || enrollment == "" || len(enrollment) != len(provided) || subtle.ConstantTimeCompare([]byte(enrollment), []byte(provided)) != 1 {
		http.Error(w, "enrollment authorization required", 401)
		return
	}
	if len(enrollment) == len(req.Credential) && subtle.ConstantTimeCompare([]byte(enrollment), []byte(req.Credential)) == 1 {
		http.Error(w, "role credential must differ from enrollment credential", 400)
		return
	}
	_, err = s.db.Exec(`INSERT INTO roles(role,token,created_at) VALUES(?,?,?)`, req.Role, store.TokenHash(req.Credential), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		http.Error(w, "role registration failed", 409)
		return
	}
	w.WriteHeader(201)
	writeJSON(w, map[string]string{"role": req.Role, "status": "registered"})
}
func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func (s *Server) send(w http.ResponseWriter, r *http.Request, p principal) {
	role := p.Role
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
	priority, ok := normalizePriority(m.Priority)
	if !ok {
		http.Error(w, `priority must be "normal" or "urgent"`, 400)
		return
	}
	m.Priority = priority
	m.AddressedTo, m.HeldID, m.HeldAt, m.DeliveredAt = "", 0, nil, nil
	var label labelAddr
	if isLabelAddress(m.Recipient) {
		var e error
		if label, e = parseLabelAddress(m.Recipient); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		if !roleExists(s.db, label.Base) {
			http.Error(w, "unknown recipient role", 403)
			return
		}
		if !s.maySend(p, label.Base) {
			http.Error(w, "outside your group", 403)
			return
		}
		m.Recipient = label.String()
	} else {
		box, ok := mailbox(s.db, m.Recipient)
		if !ok {
			http.Error(w, "unknown recipient role", 403)
			return
		}
		if !s.maySend(p, baseOf(s.db, box)) && !s.isReplyToSender(role, m.ReplyTo, box) {
			http.Error(w, "outside your group", 403)
			return
		}
		m.Recipient = box
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = m.IdempotencyKey
	}
	if key != "" {
		existing, e := scanMessage(s.db.QueryRow(`SELECT `+messageCols(`m.status`)+` FROM messages m WHERE m.sender=? AND m.idem=?`, role, key))
		if e == nil {
			w.WriteHeader(200)
			writeJSON(w, existing)
			return
		}
	}
	now := s.now().UTC()
	if m.ThreadID == 0 && m.ReplyTo > 0 {
		m.ThreadID = m.ReplyTo
	}
	m.Status = "pending"
	e := s.inTx(r.Context(), func(tx *sql.Tx) error {
		var heldAt any
		if label.Base != "" {
			// Look up the label's session in the same transaction as the insert, so
			// a registration either sees this message as held or is seen here.
			target, err := labelTarget(tx, label, s.now())
			if err != nil {
				return err
			}
			m.AddressedTo = label.String()
			if target != "" {
				m.Recipient = target
			} else {
				m.Status, m.HeldAt, heldAt = statusHeld, &now, now.Format(time.RFC3339Nano)
			}
		}
		res, err := tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,thread_id,reply_to,idem,priority,status,addressed_to,held_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, role, m.Recipient, m.Subject, m.Body, now.Format(time.RFC3339Nano), m.TaskID, nullInt(m.ThreadID), nullInt(m.ReplyTo), nullable(key), m.Priority, m.Status, nullable(m.AddressedTo), heldAt)
		if err != nil {
			return err
		}
		m.ID, err = res.LastInsertId()
		return err
	})
	if e != nil {
		http.Error(w, "could not store message", 500)
		return
	}
	m.CreatedAt = now
	if m.Status != statusHeld {
		s.hub.publish(m.Recipient)
	}
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
	query := `SELECT ` + messageCols(`COALESCE(CASE WHEN rs.status IS NOT NULL THEN rs.status ELSE m.status END,'pending')`) + ` FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=CASE WHEN m.sender=? THEN m.recipient ELSE ? END WHERE `
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
		if m, err := scanMessage(rows); err == nil {
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
	return scanMessage(s.db.QueryRow(`SELECT `+messageCols(`m.status`)+` FROM messages m WHERE m.id=?`, id))
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
func (s *Server) reply(w http.ResponseWriter, r *http.Request, p principal, id int64) {
	role := p.Role
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
	s.send(w, r, p)
}

// isReplyToSender: mail replyTo was received by role and box sent it. A group
// role may always answer whoever mailed it.
func (s *Server) isReplyToSender(role string, replyTo int64, box string) bool {
	if replyTo <= 0 {
		return false
	}
	orig, err := s.get(replyTo)
	return err == nil && orig.Recipient == role && orig.Sender == box
}
func (s *Server) watch(w http.ResponseWriter, r *http.Request, role string) {
	after, _ := wire.ParseID(r.URL.Query().Get("after"))
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

// health reports liveness and the build version. It is unauthenticated; the
// version (a short commit sha in CI images, "dev" locally) is not sensitive.
func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}{true, version})
}
