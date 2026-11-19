package mail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Hub wakes event subscribers when mail is stored for their role. A wake-up
// carries no message data: subscribers re-read SQLite, which stays the only
// source of truth. Each subscriber channel holds at most one pending wake-up,
// so publishing never blocks a sender and a slow subscriber loses nothing (the
// next query covers every message stored since its cursor). The zero value is
// ready to use.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// Subscribe registers a wake-up channel for role; the returned function
// removes it.
func (h *Hub) Subscribe(role string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan struct{}]struct{}{}
	}
	if h.subs[role] == nil {
		h.subs[role] = map[chan struct{}]struct{}{}
	}
	h.subs[role][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[role], ch)
		if len(h.subs[role]) == 0 {
			delete(h.subs, role)
		}
		h.mu.Unlock()
	}
}

// Publish wakes every subscriber of role.
func (h *Hub) Publish(role string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[role] {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already queued; it will see this message too
		}
	}
}

// Subscribers is how many streams or long polls wait for role.
func (h *Hub) Subscribers(role string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[role])
}

// Waiting is how many roles have at least one subscriber.
func (h *Hub) Waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

const defaultEventKeepAlive = 20 * time.Second
const eventBatch = 200

func (s *Service) keepAliveInterval() time.Duration {
	if s.cfg.KeepAlive > 0 {
		return s.cfg.KeepAlive
	}
	return defaultEventKeepAlive
}

// eventCursor reads the resume cursor shared by the event stream and the long
// poll: the Last-Event-ID header, else ?after=. A missing cursor is 0; a
// non-integer or negative cursor is invalid.
func eventCursor(r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("after"))
	}
	if raw == "" {
		return 0, true
	}
	id, e := wire.ParseID(raw)
	return id, e == nil && id >= 0
}

// Events streams new pending mail for the authenticated role as server-sent
// events. Without a cursor it first sends every message currently pending for
// the role; with Last-Event-ID (or ?after=) it sends pending messages with a
// larger ID. It then sends each new pending message as it is stored.
func (s *Service) Events(w http.ResponseWriter, r *http.Request, role string) {
	cursor, ok := eventCursor(r)
	if !ok {
		http.Error(w, "invalid Last-Event-ID", 400)
		return
	}
	rc := http.NewResponseController(w)
	// Subscribe before the first query so mail stored between the query and the
	// wait still wakes this stream.
	wake, unsubscribe := s.hub.Subscribe(role)
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	// The stream is long lived; lift any server write deadline for it.
	_ = rc.SetWriteDeadline(time.Time{})

	send := func() bool {
		for {
			evs, e := s.pendingAfter(role, cursor)
			if e != nil {
				return false
			}
			for _, ev := range evs {
				b, _ := json.Marshal(ev)
				if _, e = fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", ev.ID, b); e != nil {
					return false
				}
				cursor = ev.ID
			}
			if len(evs) < eventBatch {
				return rc.Flush() == nil
			}
		}
	}
	if _, e := fmt.Fprint(w, "retry: 3000\n\n"); e != nil || !send() {
		return
	}
	if _, e := fmt.Fprintf(w, "event: ready\ndata: {\"cursor\":%d}\n\n", cursor); e != nil || rc.Flush() != nil {
		return
	}
	ticker := time.NewTicker(s.keepAliveInterval())
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-wake:
			if !send() {
				return
			}
		case <-ticker.C:
			// A role revoked while streaming loses the stream at the next ping.
			if !s.authorized(r) {
				return
			}
			if _, e := fmt.Fprint(w, ": ping\n\n"); e != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// pendingAfter returns messages addressed to role with ID greater than cursor
// whose recipient state is still pending, oldest first. SQLite IDs are
// assigned and committed in order on the single connection, so no message with
// a smaller ID can appear after a larger one has been seen.
func (s *Service) pendingAfter(role string, cursor int64) ([]wire.MailEvent, error) {
	rows, e := s.db.Query(`SELECT m.id,m.sender,m.recipient,m.subject,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0),COALESCE(m.priority,'normal'),COALESCE(m.addressed_to,''),m.body FROM messages m `+StatusJoin+` WHERE m.recipient=? AND m.id>? AND `+StatusExpr+`='pending' ORDER BY m.id LIMIT ?`, role, cursor, eventBatch)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []wire.MailEvent{}
	for rows.Next() {
		var ev wire.MailEvent
		var created, body string
		if e = rows.Scan(&ev.ID, &ev.Sender, &ev.Recipient, &ev.Subject, &created, &ev.TaskID, &ev.ThreadID, &ev.Priority, &ev.AddressedTo, &body); e != nil {
			return nil, e
		}
		ev.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if ev.Priority == "urgent" {
			ev.Body = &body
		}
		out = append(out, ev)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	rows.Close() // the store has one connection: free it before the lookups
	for i := range out {
		out[i].AnswersDirective = s.AnswersDirective(role, out[i].ID)
	}
	return out, nil
}

const defaultPollWait = 25 * time.Second
const defaultPollMax = 55 * time.Second

// PollResponse is the body of GET /api/events/poll. Cursor is the ID of the
// last event returned, or the request cursor when there are none.
type PollResponse struct {
	Events []wire.MailEvent `json:"events"`
	Cursor int64            `json:"cursor"`
}

func (s *Service) pollMaxWait() time.Duration {
	if s.cfg.PollMax > 0 {
		return s.cfg.PollMax
	}
	return defaultPollMax
}

// PollWait reads ?timeout= seconds for a long-poll: default 25, capped by
// the server maximum. ok is false for a malformed or negative value.
func (s *Service) PollWait(r *http.Request) (time.Duration, bool) {
	wait := defaultPollWait
	if raw := strings.TrimSpace(r.URL.Query().Get("timeout")); raw != "" {
		n, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || n < 0 {
			return 0, false
		}
		if n > int64(s.pollMaxWait()/time.Second) { // also avoids Duration overflow
			n = int64(s.pollMaxWait() / time.Second)
		}
		wait = time.Duration(n) * time.Second
	}
	if wait > s.pollMaxWait() {
		wait = s.pollMaxWait()
	}
	return wait, true
}

// PollEvents is the long-poll form of events for clients that can only read a
// complete response. It answers at once with the pending messages after the
// cursor (same cursor rules and event shape as the stream). When there are
// none it waits up to ?timeout= seconds (default 25, capped by the server) for
// new mail and answers with whatever arrived, or an empty list.
func (s *Service) PollEvents(w http.ResponseWriter, r *http.Request, role string) {
	cursor, ok := eventCursor(r)
	if !ok {
		http.Error(w, "invalid cursor", 400)
		return
	}
	wait, ok := s.PollWait(r)
	if !ok {
		http.Error(w, "invalid timeout", 400)
		return
	}
	// Subscribe before the first query so mail stored between the query and the
	// wait still wakes this request.
	wake, unsubscribe := s.hub.Subscribe(role)
	defer unsubscribe()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	evs := []wire.MailEvent{}
	for {
		var e error
		if evs, e = s.pendingAfter(role, cursor); e != nil {
			http.Error(w, "database error", 500)
			return
		}
		if len(evs) > 0 || wait == 0 {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
		case <-timer.C:
		case <-wake:
			continue
		}
		break
	}
	if len(evs) > 0 {
		cursor = evs[len(evs)-1].ID
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	writeJSON(w, PollResponse{Events: evs, Cursor: cursor})
}
