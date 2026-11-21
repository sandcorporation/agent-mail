// Package mail is the mailbox: sending, replying, listing and reading
// messages, their per-recipient state, idempotent sends, label addresses and
// held mail, the event hub with its stream (SSE), long poll and watch, and the
// rule for when one message answers another.
package mail

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	s.send(w, r, p, nil, nil)
}

// RecipientState is the receiving mailbox's state for m, just sent (#354):
// whether a session reads it, when its last one ended, whether m was held for
// that session, and for mail to a base whose slot 1 is empty the numbered
// mailboxes that are live. It is nil for label mail and unknown mailboxes.
func (s *Service) RecipientState(m *wire.Message) *wire.RecipientState {
	box := m.Recipient
	if strings.HasPrefix(box, handoffPrefix) {
		box = m.AddressedTo
	} else if m.AddressedTo != "" {
		return nil // label mail
	}
	base, number, ok := SlotOf(s.db, box)
	if !ok {
		return nil
	}
	now := ms(s.now())
	st := &wire.RecipientState{Mailbox: box, Held: m.Status == statusHeld}
	var until int64
	switch err := s.db.QueryRow(`SELECT lease_until FROM slots WHERE base=? AND number=?`, base, number).Scan(&until); {
	case err == nil && until > now:
		st.Live = true
		return st
	case err == nil:
		st.EndedS = (now - until) / 1000
	}
	if number != 1 {
		return st
	}
	rows, err := s.db.Query(`SELECT number FROM slots WHERE base=? AND number<>1 AND lease_until>? ORDER BY number`, base, now)
	if err != nil {
		return st
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		if rows.Scan(&n) == nil {
			st.LiveSlots = append(st.LiveSlots, identity.SlotRole(base, n))
		}
	}
	return st
}

// fillState sets the send response's recipient fields (#300, #354).
func (s *Service) fillState(m *wire.Message) {
	m.NoSession = s.noSession(m)
	m.RecipientState = s.RecipientState(m)
}

// noSession reports whether m waits for a role with no live session (#300).
// Mail held for a label is not counted: it waits for its label.
func (s *Service) noSession(m *wire.Message) bool {
	if m.Status == statusHeld {
		return false
	}
	var live int
	return s.db.QueryRow(`SELECT COUNT(*) FROM slots WHERE base=? AND lease_until>?`, identity.BaseOf(s.db, m.Recipient), s.now().UnixMilli()).Scan(&live) == nil && live == 0
}

// send stores the message in the request body. inTx, when set, runs in the
// insert's transaction (reply --complete marks the replied-to mail complete
// there; send --complete, the body's complete IDs, also run there).
// respond, when set, fills the response, also for an idempotent retry.
func (s *Service) send(w http.ResponseWriter, r *http.Request, p identity.Principal, inTx func(tx *sql.Tx, now time.Time) error, respond func(*wire.Message)) {
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
		if refuseRevoked(w, s.db, label.Base) {
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
		if refuseRevoked(w, s.db, identity.BaseOf(s.db, box)) {
			return
		}
		if !s.ids.MaySend(p, identity.BaseOf(s.db, box)) && !s.isReplyToSender(role, m.ReplyTo, box) {
			http.Error(w, "outside your group", 403)
			return
		}
		m.Recipient = box
	}
	// send --complete (#327): every ID must be mail the sender received;
	// otherwise nothing is stored.
	complete := m.Complete
	m.Complete = nil
	for _, id := range complete {
		got, err := s.Get(id)
		if err != nil {
			http.Error(w, fmt.Sprintf("complete: no message %d", id), 404)
			return
		}
		if got.Recipient != role {
			http.Error(w, fmt.Sprintf("complete: message %d was not sent to %s", id, role), 400)
			return
		}
	}
	if len(complete) > 0 {
		then := inTx
		inTx = func(tx *sql.Tx, now time.Time) error {
			if err := markComplete(tx, role, complete, now); err != nil {
				return err
			}
			if then == nil {
				return nil
			}
			return then(tx, now)
		}
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = m.IdempotencyKey
	}
	if key != "" {
		existing, e := scanMessage(s.db.QueryRow(`SELECT `+messageCols(`m.status`)+` FROM messages m WHERE m.sender=? AND m.idem=?`, role, key))
		if e == nil {
			s.fillState(&existing)
			if respond != nil {
				respond(&existing)
			}
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
	var told string
	e := store.InTx(r.Context(), s.db, func(tx *sql.Tx) error {
		told = ""
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
		res, err := tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,thread_id,reply_to,idem,priority,status,addressed_to,held_at,directive,no_reply) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, role, m.Recipient, m.Subject, m.Body, now.Format(time.RFC3339Nano), m.TaskID, nullInt(m.ThreadID), nullInt(m.ReplyTo), nullable(key), m.Priority, m.Status, nullable(m.AddressedTo), heldAt, m.Directive, m.NoReply)
		if err != nil {
			return err
		}
		if m.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if label.Base == "" {
			// A slot whose session ended: hold the mail for that session (#354).
			held, to, err := HoldIfEnded(tx, m.ID, m.Recipient, role, m.Subject, m.TaskID, now)
			if err != nil {
				return err
			}
			if held {
				m.AddressedTo, m.Recipient, m.Status, m.HeldAt, told = m.Recipient, handoffPrefix+m.TaskID, statusHeld, &now, to
			}
		}
		if inTx == nil {
			return nil
		}
		return inTx(tx, now)
	})
	if e != nil {
		http.Error(w, "could not store message", 500)
		return
	}
	m.CreatedAt = now
	if m.Status != statusHeld {
		s.hub.Publish(m.Recipient)
	}
	if told != "" {
		s.hub.Publish(told)
	}
	m.Done = false
	s.fillState(&m)
	if respond != nil {
		respond(&m)
	}
	w.WriteHeader(201)
	writeJSON(w, m)
}

// markComplete marks each of ids complete for role in tx.
func markComplete(tx *sql.Tx, role string, ids []int64, now time.Time) error {
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT INTO role_states(role,message_id,status,updated_at) VALUES(?,?,'complete',?) ON CONFLICT(role,message_id) DO UPDATE SET status=excluded.status,updated_at=excluded.updated_at`, role, id, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

// refuseRevoked answers 410 when base is a revoked role: no session can
// read mail to it. It runs before the idempotency lookup, so a retry of a
// refused send gets the same answer and nothing is stored.
func refuseRevoked(w http.ResponseWriter, q identity.Queryer, base string) bool {
	at, revoked := identity.RevokedAt(q, base)
	if revoked {
		http.Error(w, "recipient role revoked at "+at.UTC().Format(time.RFC3339), http.StatusGone)
	}
	return revoked
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
	s.list(w, r, role)
}

// ListOf serves GET /api/messages?of=MAILBOX for a boss role: the same list
// as List for another mailbox, read-only. The view is recorded (mailbox_views).
// Only a global boss may call it; the caller has checked that.
func (s *Service) ListOf(w http.ResponseWriter, r *http.Request, viewer identity.Principal, of string) {
	if err := s.recordView(viewer.Role, of, 0); err != nil {
		http.Error(w, "view not recorded", 500)
		return
	}
	s.list(w, r, of)
}

// ViewForeign serves GET /api/messages/{id} for a boss role when message id
// is neither sent nor received by viewer: it shows the message with its state
// untouched and records the view. It reports false for any other message.
func (s *Service) ViewForeign(w http.ResponseWriter, viewer identity.Principal, id int64) bool {
	m, err := s.View(id)
	if err != nil || m.Sender == viewer.Role || m.Recipient == viewer.Role {
		return false
	}
	if err := s.recordView(viewer.Role, m.Recipient, id); err != nil {
		http.Error(w, "view not recorded", 500)
		return true
	}
	m.AnswersDirective = s.AnswersDirective(m.Recipient, id)
	writeJSON(w, m)
	return true
}

// recordView appends a mailbox view: who looked at which mailbox (and message
// ID, 0 for a list) at what time. Body text is not stored.
func (s *Service) recordView(viewer, mailbox string, id int64) error {
	_, err := s.db.Exec(`INSERT INTO mailbox_views(viewer,mailbox,message_id,viewed_at) VALUES(?,?,?,?)`, viewer, mailbox, id, ms(s.now()))
	return err
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, role string) {
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
	rows.Close()
	if box == "inbox" {
		s.withAnswers(role, out)
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
	if m.Recipient == role {
		m.AnswersDirective = s.AnswersDirective(role, id)
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
	// LiveRecipient holds for mail m whose recipient's base role is not
	// revoked: the role itself, or <base>-N without a role row of its own
	// (identity.BaseOf). A recipient with no role behind it counts as live.
	LiveRecipient = `NOT EXISTS (SELECT 1 FROM roles rv WHERE rv.revoked_at IS NOT NULL AND (rv.role=m.recipient OR (` +
		`substr(m.recipient,1,length(rv.role)+1)=rv.role||'-' AND substr(m.recipient,length(rv.role)+2) GLOB '[1-9]*' ` +
		`AND substr(m.recipient,length(rv.role)+2) NOT GLOB '*[^0-9]*' AND NOT EXISTS (SELECT 1 FROM roles rx WHERE rx.role=m.recipient))))`
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
	if m.Subject == "" {
		m.Subject = ReplySubject(orig.Subject)
	}
	done := m.Done
	m.Done = false
	b, _ := json.Marshal(m)
	r.Body = io.NopCloser(bytes.NewReader(b))
	var inTx func(*sql.Tx, time.Time) error
	if done {
		// reply --complete (--done): the reply and the complete mark commit
		// together (#269), for any mail, directive or not (#327).
		inTx = func(tx *sql.Tx, now time.Time) error {
			return markComplete(tx, orig.Recipient, []int64{id}, now)
		}
	}
	var respond func(*wire.Message)
	if orig.ReplyTo == 0 || orig.Directive {
		respond = func(out *wire.Message) { out.DirectiveState = s.directiveState(id) }
	}
	s.send(w, r, p, inTx, respond)
}

// directiveState is a directive's state for its recipient: done when its
// recipient marked it complete or it was closed (no_reply), else open.
// Replies never close it (mail.AnsweredRule version 2).
func (s *Service) directiveState(id int64) string {
	st, ok := s.directiveStateOf(id)
	if !ok {
		return ""
	}
	st.Replied = true
	return stateName(st)
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
			resp := map[string]any{"messages": ids, "cursor": ids[len(ids)-1]}
			// answers_directive: message ID -> the directive it answers (#275).
			answers := map[string]*wire.DirectiveRef{}
			for _, id := range ids {
				if ref := s.AnswersDirective(role, id); ref != nil {
					answers[strconv.FormatInt(id, 10)] = ref
				}
			}
			if len(answers) > 0 {
				resp["answers_directive"] = answers
			}
			writeJSON(w, resp)
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

// ReplySubject is the subject of a reply sent without one: "Re: " and the
// replied-to subject, without stacking "Re: " (#269, so reply ID --done
// --body TEXT is the whole command).
func ReplySubject(orig string) string {
	if strings.HasPrefix(orig, "Re: ") {
		return orig
	}
	return "Re: " + orig
}
