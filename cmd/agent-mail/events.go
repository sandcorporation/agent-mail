package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// mailHub wakes event subscribers when mail is stored for their role. A wake-up
// carries no message data: subscribers re-read SQLite, which stays the only
// source of truth. Each subscriber channel holds at most one pending wake-up,
// so publishing never blocks a sender and a slow subscriber loses nothing (the
// next query covers every message stored since its cursor). The zero value is
// ready to use.
type mailHub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func (h *mailHub) subscribe(role string) (<-chan struct{}, func()) {
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

func (h *mailHub) publish(role string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[role] {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already queued; it will see this message too
		}
	}
}

const defaultEventKeepAlive = 20 * time.Second
const eventBatch = 200

func (s *Server) keepAliveInterval() time.Duration {
	if s.eventKeepAlive > 0 {
		return s.eventKeepAlive
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

// events streams new pending mail for the authenticated role as server-sent
// events. Without a cursor it first sends every message currently pending for
// the role; with Last-Event-ID (or ?after=) it sends pending messages with a
// larger ID. It then sends each new pending message as it is stored.
func (s *Server) events(w http.ResponseWriter, r *http.Request, role string) {
	cursor, ok := eventCursor(r)
	if !ok {
		http.Error(w, "invalid Last-Event-ID", 400)
		return
	}
	rc := http.NewResponseController(w)
	// Subscribe before the first query so mail stored between the query and the
	// wait still wakes this stream.
	wake, unsubscribe := s.hub.subscribe(role)
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
			if _, ok := s.authenticate(r); !ok {
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
func (s *Server) pendingAfter(role string, cursor int64) ([]mailEvent, error) {
	rows, e := s.db.Query(`SELECT m.id,m.sender,m.recipient,m.subject,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0),COALESCE(m.priority,'normal'),COALESCE(m.addressed_to,''),m.body FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient WHERE m.recipient=? AND m.id>? AND COALESCE(rs.status,m.status,'pending')='pending' ORDER BY m.id LIMIT ?`, role, cursor, eventBatch)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []mailEvent{}
	for rows.Next() {
		var ev mailEvent
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
	return out, rows.Err()
}

const defaultPollWait = 25 * time.Second
const defaultPollMax = 55 * time.Second

// pollResponse is the body of GET /api/events/poll. Cursor is the ID of the
// last event returned, or the request cursor when there are none.
type pollResponse struct {
	Events []mailEvent `json:"events"`
	Cursor int64       `json:"cursor"`
}

func (s *Server) pollMaxWait() time.Duration {
	if s.pollMax > 0 {
		return s.pollMax
	}
	return defaultPollMax
}

// pollWait reads ?timeout= seconds for a long-poll: default 25, capped by
// the server maximum. ok is false for a malformed or negative value.
func (s *Server) pollWait(r *http.Request) (time.Duration, bool) {
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

// pollEvents is the long-poll form of events for clients that can only read a
// complete response. It answers at once with the pending messages after the
// cursor (same cursor rules and event shape as the stream). When there are
// none it waits up to ?timeout= seconds (default 25, capped by the server) for
// new mail and answers with whatever arrived, or an empty list.
func (s *Server) pollEvents(w http.ResponseWriter, r *http.Request, role string) {
	cursor, ok := eventCursor(r)
	if !ok {
		http.Error(w, "invalid cursor", 400)
		return
	}
	wait, ok := s.pollWait(r)
	if !ok {
		http.Error(w, "invalid timeout", 400)
		return
	}
	// Subscribe before the first query so mail stored between the query and the
	// wait still wakes this request.
	wake, unsubscribe := s.hub.subscribe(role)
	defer unsubscribe()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	evs := []mailEvent{}
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
	writeJSON(w, pollResponse{Events: evs, Cursor: cursor})
}
