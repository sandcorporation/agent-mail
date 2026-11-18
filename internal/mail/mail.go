// Package mail is the mailbox: sending, replying, listing and reading
// messages, their per-recipient state, idempotent sends, label addresses and
// held mail, the event hub with its stream (SSE), long poll and watch, and the
// rule for when one message answers another.
package mail

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Config is the mail settings read from the environment.
type Config struct {
	KeepAlive time.Duration // SSE keep-alive interval; 0 means defaultEventKeepAlive
	PollMax   time.Duration // cap on a long poll's wait; 0 means defaultPollMax
}

// Service is the mailbox over the messages and role_states tables. It holds
// only what the mail rules need, so callers build one per use.
type Service struct {
	db         *sql.DB
	clock      func() time.Time
	cfg        *Config
	hub        *Hub
	done       <-chan struct{}
	ids        *identity.Service
	authorized func(*http.Request) bool
}

// New returns the mailbox over db. clock is the current time, cfg the mail
// settings, hub the server's event hub, done is closed on shutdown (it ends
// event streams and long polls), ids the identity rules, and authorized
// re-checks a streaming request's credentials at each keep-alive.
func New(db *sql.DB, clock func() time.Time, cfg *Config, hub *Hub, done <-chan struct{}, ids *identity.Service, authorized func(*http.Request) bool) *Service {
	return &Service{db: db, clock: clock, cfg: cfg, hub: hub, done: done, ids: ids, authorized: authorized}
}

func (s *Service) now() time.Time { return s.clock() }

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func ms(t time.Time) int64 { return t.UnixMilli() }

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

// AdminList serves GET /api/admin/messages.
func (s *Service) AdminList(w http.ResponseWriter, r *http.Request) {
	db := s.db
	q, status, role, box := strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("status"), r.URL.Query().Get("role"), r.URL.Query().Get("box")
	thread := strings.TrimSpace(r.URL.Query().Get("thread"))
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if mb, ok := identity.Mailbox(db, role); ok {
		role = mb
	}
	if box != "inbox" && box != "outbox" && box != "held" {
		box = "all"
	}
	query := `SELECT ` + messageCols(StatusExpr) + ` FROM messages m ` + StatusJoin + ` WHERE 1=1`
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
		query += ` AND ` + StatusExpr + ` IN (?` + strings.Repeat(",?", len(values)-1) + `)`
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
	out := []wire.Message{}
	for rows.Next() {
		if m, err := scanMessage(rows); err == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, out)
}

// Send serves POST /api/messages for the caller p.
func (s *Service) Send(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	role := p.Role
	var m wire.Message
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
		label.Base = identity.Resolve(s.db, label.Base)
		if !identity.RoleExists(s.db, label.Base) {
			http.Error(w, "unknown recipient role", 403)
			return
		}
		if !s.ids.MaySend(p, label.Base) {
			http.Error(w, "outside your group", 403)
			return
		}
		m.Recipient = label.String()
	} else {
		box, ok := identity.Mailbox(s.db, m.Recipient)
		if !ok {
			http.Error(w, "unknown recipient role", 403)
			return
		}
		if !s.ids.MaySend(p, identity.BaseOf(s.db, box)) && !s.isReplyToSender(role, m.ReplyTo, box) {
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
	e := store.InTx(r.Context(), s.db, func(tx *sql.Tx) error {
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
		res, err := tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,thread_id,reply_to,idem,priority,status,addressed_to,held_at,directive) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, role, m.Recipient, m.Subject, m.Body, now.Format(time.RFC3339Nano), m.TaskID, nullInt(m.ThreadID), nullInt(m.ReplyTo), nullable(key), m.Priority, m.Status, nullable(m.AddressedTo), heldAt, m.Directive)
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
		s.hub.Publish(m.Recipient)
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

// List serves GET /api/messages (inbox or outbox of role).
func (s *Service) List(w http.ResponseWriter, r *http.Request, role string) {
	box := r.URL.Query().Get("box")
	if box != "outbox" {
		box = "inbox"
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	limit := 100
	query := `SELECT ` + messageCols(StatusExpr) + ` FROM messages m ` + StatusJoin + ` WHERE `
	args := []any{}
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
		query += `AND ` + StatusExpr + ` IN (?` + strings.Repeat(",?", len(values)-1) + `) `
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
	out := []wire.Message{}
	for rows.Next() {
		if m, err := scanMessage(rows); err == nil {
			out = append(out, m)
		}
	}
	writeJSON(w, out)
}

// Read serves GET /api/messages/{id}.
func (s *Service) Read(w http.ResponseWriter, r *http.Request, role string, id int64) {
	m, e := s.Get(id)
	if e != nil || (m.Sender != role && m.Recipient != role) {
		http.Error(w, "not found", 404)
		return
	}
	if m.Recipient == role {
		_, _ = s.db.Exec(`INSERT INTO role_states(role,message_id,status,updated_at) VALUES(?,?,?,?) ON CONFLICT(role,message_id) DO UPDATE SET status=CASE WHEN role_states.status='pending' THEN 'read' ELSE role_states.status END,updated_at=excluded.updated_at`, role, id, "read", time.Now().UTC().Format(time.RFC3339Nano))
	}
	if v, err := s.View(id); err == nil {
		m = v
	}
	writeJSON(w, m)
}

// StatusJoin attaches the recipient's state row to messages aliased m, and
// StatusExpr is the status that row gives. Every view of a message (read,
// inbox, outbox, admin list, events, progress, metrics) uses the pair, so the
// sender and the recipient see the same status. A message has one recipient
// (group sends store one row each), so the recipient's state is the message's.
const (
	StatusJoin = `LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient`
	StatusExpr = `COALESCE(rs.status,m.status,'pending')`
)

// View returns message id with the status every mailbox view shows.
func (s *Service) View(id int64) (wire.Message, error) {
	return scanMessage(s.db.QueryRow(`SELECT `+messageCols(StatusExpr)+` FROM messages m `+StatusJoin+` WHERE m.id=?`, id))
}

// Get returns message id as stored.
func (s *Service) Get(id int64) (wire.Message, error) {
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

// Status serves POST /api/messages/{id}/{ack,complete,read}.
func (s *Service) Status(w http.ResponseWriter, r *http.Request, role string, id int64, status string) {
	if status == "ack" {
		status = "acknowledged"
	}
	m, e := s.Get(id)
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

// Reply serves POST /api/messages/{id}/reply.
func (s *Service) Reply(w http.ResponseWriter, r *http.Request, p identity.Principal, id int64) {
	role := p.Role
	orig, e := s.Get(id)
	if e != nil || orig.Recipient != role {
		http.Error(w, "not found", 404)
		return
	}
	var m wire.Message
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
	s.Send(w, r, p)
}

// isReplyToSender: mail replyTo was received by role and box sent it. A group
// role may always answer whoever mailed it.
func (s *Service) isReplyToSender(role string, replyTo int64, box string) bool {
	if replyTo <= 0 {
		return false
	}
	orig, err := s.Get(replyTo)
	return err == nil && orig.Recipient == role && orig.Sender == box
}

// OtherSlotMailbox returns the mailbox message id belongs to when that is
// another mailbox of the caller's base (for example mail of <base>-2 while
// the header names <base>). Mailboxes of one base are kept apart: such
// requests are refused with 403 naming the role to act as, rather than
// showing the body while the state stays with the other mailbox.
func (s *Service) OtherSlotMailbox(p identity.Principal, id int64) (string, bool) {
	m, err := s.Get(id)
	if err != nil || m.Recipient == p.Role || m.Sender == p.Role {
		return "", false
	}
	for _, box := range []string{m.Recipient, m.Sender} {
		if identity.BaseOf(s.db, box) == p.Base {
			return box, true
		}
	}
	return "", false
}

// Watch serves GET /api/watch.
func (s *Service) Watch(w http.ResponseWriter, r *http.Request, role string) {
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
