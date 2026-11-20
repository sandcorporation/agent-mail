package wire

import (
	"fmt"
	"strings"
	"time"
)

var presenceStates = map[string]bool{"idle": true, "busy": true, "compacting": true}

// presenceKindPrefixes are the states that carry a kind: error:<kind> (the
// turn failed) and blocked:<kind> (the session waits on a person).
var presenceKindPrefixes = []string{"error:", "blocked:"}

const maxPresenceKind = 64

// MaxSubagents caps the reported number of running background sub-agents;
// larger reports are stored as this.
const MaxSubagents = 64

// SubagentCount is the number a report sets: 0 when absent, at most
// MaxSubagents. Call it after Presence has accepted the request.
func (q SlotRequest) SubagentCount() int64 {
	if q.Subagents == nil {
		return 0
	}
	return min(max(*q.Subagents, 0), MaxSubagents)
}

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

// presenceError reports a malformed presence field; it maps to 400.
type presenceError string

func (e presenceError) Error() string { return string(e) }

// Presence parses the presence fields of a slot request. last is the zero
// time when last_turn_at was not given.
func (q SlotRequest) Presence() (last time.Time, err error) {
	if q.State != "" && !validPresenceState(q.State) {
		return last, presenceError(fmt.Sprintf("state must be idle, busy, compacting, error:<kind> or blocked:<kind> (kind: 1-64 of A-Z a-z 0-9 . _ -), not %q", q.State))
	}
	if q.ContextTokens != nil && *q.ContextTokens < 0 {
		return last, presenceError("context_tokens must be a whole number of at least 0")
	}
	if q.Subagents != nil && *q.Subagents < 0 {
		return last, presenceError("subagents must be a whole number of at least 0")
	}
	if q.LastTurnAt != "" {
		if last, err = time.Parse(time.RFC3339, q.LastTurnAt); err != nil {
			return last, presenceError("last_turn_at must be an RFC 3339 time such as 2026-11-17T09:00:00Z")
		}
	}
	return last, nil
}

// PresenceView is one session in GET /api/presence. It never carries tokens.
type PresenceView struct {
	Role           string     `json:"role"`
	Base           string     `json:"base"`
	Number         int        `json:"number"`
	Online         bool       `json:"online"`
	Status         string     `json:"status"`              // offline when !online, else state or unknown
	State          string     `json:"state,omitempty"`     // last reported state, kept while offline
	ContextTokens  *int64     `json:"context_tokens"`      // null until reported
	Subagents      int64      `json:"subagents,omitempty"` // running background sub-agents; 0 (omitted) while offline
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
	Since          *time.Time `json:"since,omitempty"`         // when status began: offline_since when offline, else the start of the current state
	// CLIVersion is the version the session's CLI last sent (#308): a tag,
	// "dev", or CLIUnknown (no header); empty when not reported since #308.
	CLIVersion string `json:"cli_version"`
	// CLI is CLIVersion against the server minimum: current, old (옛 판) or
	// unknown (판 모름).
	CLI string `json:"cli"`
}

// PresenceList answers GET /api/presence and GET /api/admin/presence.
type PresenceList struct {
	Now             time.Time      `json:"now"`
	IntervalSeconds int            `json:"interval_seconds"`
	Missed          int            `json:"missed"`
	LeaseSeconds    int            `json:"lease_seconds"`
	RetainSeconds   int            `json:"retain_seconds"`
	Sessions        []PresenceView `json:"sessions"`
}
