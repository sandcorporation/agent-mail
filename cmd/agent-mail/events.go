package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
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

// mailEvent is the data of an SSE "message" event. Body is present only for
// urgent mail. Priority is always "normal" until messages store a priority.
type mailEvent struct {
	ID        int64     `json:"id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Priority  string    `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
	TaskID    string    `json:"task_id,omitempty"`
	ThreadID  int64     `json:"thread_id,omitempty"`
	Body      string    `json:"body,omitempty"`
}

const defaultEventKeepAlive = 20 * time.Second
const eventBatch = 200

func (s *Server) keepAliveInterval() time.Duration {
	if s.eventKeepAlive > 0 {
		return s.eventKeepAlive
	}
	return defaultEventKeepAlive
}

// events streams new pending mail for the authenticated role as server-sent
// events. Without a cursor it first sends every message currently pending for
// the role; with Last-Event-ID (or ?after=) it sends pending messages with a
// larger ID. It then sends each new pending message as it is stored.
func (s *Server) events(w http.ResponseWriter, r *http.Request, role string) {
	cursor := int64(0)
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("after"))
	}
	if raw != "" {
		id, e := parseID(raw)
		if e != nil || id < 0 {
			http.Error(w, "invalid Last-Event-ID", 400)
			return
		}
		cursor = id
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
	rows, e := s.db.Query(`SELECT m.id,m.sender,m.recipient,m.subject,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0) FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient WHERE m.recipient=? AND m.id>? AND COALESCE(rs.status,m.status,'pending')='pending' ORDER BY m.id LIMIT ?`, role, cursor, eventBatch)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []mailEvent{}
	for rows.Next() {
		var ev mailEvent
		var created string
		if e = rows.Scan(&ev.ID, &ev.Sender, &ev.Recipient, &ev.Subject, &created, &ev.TaskID, &ev.ThreadID); e != nil {
			return nil, e
		}
		ev.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		ev.Priority = "normal"
		out = append(out, ev)
	}
	return out, rows.Err()
}
