package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Session presence.
//
// A session holding a numbered slot reports presence through the slot
// heartbeat: state, context tokens and last turn time ride on the same body,
// and each report renews the lease. There is one rule for offline: a session
// is offline exactly when its slot lease has ended. The lease length is
// AGENT_MAIL_PRESENCE_INTERVAL × AGENT_MAIL_PRESENCE_MISSED (60s × 3 = 3m by
// default), so missing that many heartbeats both marks the session offline
// and frees its number. AGENT_MAIL_SLOT_LEASE, when set, overrides the
// product directly. Offline rows stay listed for AGENT_MAIL_PRESENCE_RETAIN
// (default 24h) unless another identity takes the number first.

const (
	defaultPresenceInterval = 60 * time.Second
	defaultPresenceMissed   = 3
	defaultPresenceRetain   = 24 * time.Hour
)

var presenceStates = map[string]bool{"idle": true, "busy": true, "compacting": true}

// presenceKindPrefixes are the states that carry a kind: error:<kind> (the
// turn failed) and blocked:<kind> (the session waits on a person).
var presenceKindPrefixes = []string{"error:", "blocked:"}

const maxPresenceKind = 64

// validPresenceState reports whether state is idle, busy, compacting, or
// error:<kind> / blocked:<kind> with a kind of 1-64 characters from
// A-Z a-z 0-9 . _ -.
func validPresenceState(state string) bool {
	if presenceStates[state] {
		return true
	}
	for _, prefix := range presenceKindPrefixes {
		if kind, ok := strings.CutPrefix(state, prefix); ok {
			return validPresenceKind(kind)
		}
	}
	return false
}

func validPresenceKind(kind string) bool {
	if kind == "" || len(kind) > maxPresenceKind {
		return false
	}
	for _, c := range kind {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// presenceColumns are added to slots in place; see migratePresence.
var presenceColumns = []struct{ name, def string }{
	{"state", `TEXT NOT NULL DEFAULT ''`},
	{"context_tokens", `INTEGER NOT NULL DEFAULT -1`}, // -1: never reported
	{"last_turn_at", `INTEGER NOT NULL DEFAULT 0`},    // Unix ms, 0: never reported
	{"reported_at", `INTEGER NOT NULL DEFAULT 0`},     // Unix ms of the last register/heartbeat/takeover
	// offline_noticed_at is the place to remember that an offline transition
	// has been handled (for example, a parent notified). Every report resets
	// it to 0. See offlineUnnoticed and markOfflineNoticed.
	{"offline_noticed_at", `INTEGER NOT NULL DEFAULT 0`},
}

func migratePresence(db *sql.DB) error {
	for _, c := range presenceColumns {
		has, err := hasColumn(db, "slots", c.name)
		if err != nil {
			return err
		}
		if !has {
			if _, err = db.Exec(`ALTER TABLE slots ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

// configurePresence reads the presence settings from the environment.
func configurePresence(s *Server, getenv func(string) string) error {
	if v, from := envAliasFrom(getenv, "PRESENCE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return errors.New(from + " must be a duration of at least 1s such as 60s")
		}
		s.presenceInterval = d
	}
	if v, from := envAliasFrom(getenv, "PRESENCE_MISSED"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New(from + " must be a whole number of at least 1 such as 3")
		}
		s.presenceMissed = n
	}
	if v, from := envAliasFrom(getenv, "PRESENCE_RETAIN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return errors.New(from + " must be a duration such as 24h (0 hides offline sessions)")
		}
		s.presenceRetain = &d
	}
	return nil
}

func (s *Server) heartbeatInterval() time.Duration {
	if s.presenceInterval > 0 {
		return s.presenceInterval
	}
	return defaultPresenceInterval
}

func (s *Server) missedHeartbeats() int {
	if s.presenceMissed > 0 {
		return s.presenceMissed
	}
	return defaultPresenceMissed
}

func (s *Server) offlineRetention() time.Duration {
	if s.presenceRetain != nil {
		return *s.presenceRetain
	}
	return defaultPresenceRetain
}

// presenceError reports a malformed presence field; it maps to 400.
type presenceError string

func (e presenceError) Error() string { return string(e) }

// presence parses the presence fields of a slot request. last is the zero
// time when last_turn_at was not given.
func (q slotRequest) presence() (last time.Time, err error) {
	if q.State != "" && !validPresenceState(q.State) {
		return last, presenceError(fmt.Sprintf("state must be idle, busy, compacting, error:<kind> or blocked:<kind> (kind: 1-64 of A-Z a-z 0-9 . _ -), not %q", q.State))
	}
	if q.ContextTokens != nil && *q.ContextTokens < 0 {
		return last, presenceError("context_tokens must be a whole number of at least 0")
	}
	if q.LastTurnAt != "" {
		if last, err = time.Parse(time.RFC3339, q.LastTurnAt); err != nil {
			return last, presenceError("last_turn_at must be an RFC 3339 time such as 2026-11-17T09:00:00Z")
		}
	}
	return last, nil
}

// applyPresence records a report on a slot row. A fresh claim (register or
// takeover) starts from the given fields only; a heartbeat keeps fields it
// does not send. A change of state is written to presence_log.
func applyPresence(tx *sql.Tx, base string, number int, req slotRequest, now time.Time, fresh bool) error {
	last, err := req.presence()
	if err != nil {
		return err
	}
	set, args := `reported_at=?,offline_noticed_at=0`, []any{ms(now)}
	if fresh {
		set += `,state='',context_tokens=-1,last_turn_at=0`
	}
	if req.State != "" {
		set += `,state=?`
		args = append(args, req.State)
	}
	if req.ContextTokens != nil {
		set += `,context_tokens=?`
		args = append(args, *req.ContextTokens)
	}
	if !last.IsZero() {
		set += `,last_turn_at=?`
		args = append(args, ms(last))
	}
	if _, err = tx.Exec(`UPDATE slots SET `+set+` WHERE base=? AND number=?`, append(args, base, number)...); err != nil {
		return err
	}
	return logPresenceReport(tx, base, number, now)
}

// presenceView is one session in GET /api/presence. It never carries tokens.
type presenceView struct {
	Role           string     `json:"role"`
	Base           string     `json:"base"`
	Number         int        `json:"number"`
	Online         bool       `json:"online"`
	Status         string     `json:"status"`          // offline when !online, else state or unknown
	State          string     `json:"state,omitempty"` // last reported state, kept while offline
	ContextTokens  *int64     `json:"context_tokens"`  // null until reported
	LastTurnAt     *time.Time `json:"last_turn_at,omitempty"`
	Machine        string     `json:"machine"`
	Directory      string     `json:"directory"`
	SessionID      string     `json:"session_id,omitempty"`
	Label          string     `json:"label,omitempty"`
	Parent         string     `json:"parent,omitempty"`
	RegisteredAt   time.Time  `json:"registered_at"`
	ReportedAt     *time.Time `json:"reported_at,omitempty"` // absent for rows from before presence
	LeaseExpiresAt time.Time  `json:"lease_expires_at"`
	OfflineSince   *time.Time `json:"offline_since,omitempty"` // = lease_expires_at when offline
}

// presenceList answers GET /api/presence and GET /api/admin/presence.
type presenceList struct {
	Now             time.Time      `json:"now"`
	IntervalSeconds int            `json:"interval_seconds"`
	Missed          int            `json:"missed"`
	LeaseSeconds    int            `json:"lease_seconds"`
	RetainSeconds   int            `json:"retain_seconds"`
	Sessions        []presenceView `json:"sessions"`
}

func presenceCols() string {
	return slotCols() + `,state,context_tokens,last_turn_at,reported_at`
}

func scanPresence(sc interface{ Scan(...any) error }, now time.Time) (presenceView, error) {
	var v presenceView
	var reg, until, tokens, last, reported int64
	err := sc.Scan(&v.Base, &v.Number, &v.Machine, &v.Directory, &v.SessionID, &v.Label, &v.Parent, &reg, &until, &v.State, &tokens, &last, &reported)
	v.Role = slotRole(v.Base, v.Number)
	v.RegisteredAt, v.LeaseExpiresAt = fromMS(reg), fromMS(until)
	v.Online = until > ms(now)
	switch {
	case !v.Online:
		v.Status = "offline"
		t := v.LeaseExpiresAt
		v.OfflineSince = &t
	case v.State != "":
		v.Status = v.State
	default:
		v.Status = "unknown"
	}
	if tokens >= 0 {
		v.ContextTokens = &tokens
	}
	if last > 0 {
		t := fromMS(last)
		v.LastTurnAt = &t
	}
	if reported > 0 {
		t := fromMS(reported)
		v.ReportedAt = &t
	}
	return v, err
}

// matchRole selects sessions for ?role=: empty matches everything, "x*"
// matches roles starting with x, otherwise the value matches a base (all its
// numbers) or one numbered role ("team/worker-1").
func matchRole(filter string, v presenceView) bool {
	switch {
	case filter == "":
		return true
	case strings.HasSuffix(filter, "*"):
		return strings.HasPrefix(v.Role, strings.TrimSuffix(filter, "*"))
	default:
		return filter == v.Base || filter == v.Role
	}
}

func (s *Server) presence(filter string) (presenceList, error) {
	now := s.now()
	out := presenceList{
		Now:             now.UTC(),
		IntervalSeconds: int(s.heartbeatInterval() / time.Second),
		Missed:          s.missedHeartbeats(),
		LeaseSeconds:    int(s.leaseLength() / time.Second),
		RetainSeconds:   int(s.offlineRetention() / time.Second),
		Sessions:        []presenceView{},
	}
	rows, err := s.db.Query(`SELECT `+presenceCols()+` FROM slots WHERE lease_until>? ORDER BY base,number`, ms(now.Add(-s.offlineRetention())))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanPresence(rows, now)
		if err != nil {
			return out, err
		}
		if matchRole(filter, v) {
			out.Sessions = append(out.Sessions, v)
		}
	}
	return out, rows.Err()
}

// presenceAPI serves GET /api/presence. Any authenticated role may read every
// session: the table holds no secrets, and a lead needs to see all sessions.
func (s *Server) presenceAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	out, err := s.presence(r.URL.Query().Get("role"))
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	writeJSON(w, out)
}

// offlineUnnoticed lists sessions whose lease has ended (offline) and whose
// offline transition nobody has marked handled since their last report. It
// is the hook for acting once per offline transition (notifying a parent):
// call it periodically and mark each handled row with markOfflineNoticed.
// Rows from before presence (never reported) are left out.
func (s *Server) offlineUnnoticed() ([]presenceView, error) {
	now := s.now()
	rows, err := s.db.Query(`SELECT `+presenceCols()+` FROM slots WHERE lease_until<=? AND lease_until>? AND reported_at>0 AND offline_noticed_at=0 ORDER BY base,number`, ms(now), ms(now.Add(-s.offlineRetention())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []presenceView{}
	for rows.Next() {
		v, err := scanPresence(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// markOfflineNoticed records that v's offline transition was handled. It does
// nothing (and returns false) when the session reported again since v was
// read, so a renewal racing with the handler is not hidden.
func (s *Server) markOfflineNoticed(v presenceView) (bool, error) {
	res, err := s.db.Exec(`UPDATE slots SET offline_noticed_at=? WHERE base=? AND number=? AND lease_until=? AND lease_until<=? AND offline_noticed_at=0`,
		ms(s.now()), v.Base, v.Number, ms(v.LeaseExpiresAt), ms(s.now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
