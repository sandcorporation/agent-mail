package wire

import (
	"strconv"
	"strings"
	"time"
)

const MaxSlotNumber = 999

// SplitSlot parses "<base>-N" with N a positive number without leading zeros.
func SplitSlot(role string) (string, int, bool) {
	i := strings.LastIndexByte(role, '-')
	if i <= 0 || i == len(role)-1 {
		return "", 0, false
	}
	digits := role[i+1:]
	if digits[0] == '0' {
		return "", 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || n > MaxSlotNumber {
		return "", 0, false
	}
	return role[:i], n, true
}

// SlotRequest is the body of the slot endpoints. Machine and Directory form
// the identity; session, label and parent are stored for display, and the
// label also routes label mail (see labels.go). State, ContextTokens and LastTurnAt are the presence report
// (see presence.go); release ignores them.
type SlotRequest struct {
	Machine       string `json:"machine"`
	Directory     string `json:"directory"`
	SessionID     string `json:"session_id"`
	Label         string `json:"label"`
	Parent        string `json:"parent"`
	State         string `json:"state,omitempty"`          // idle | busy | compacting | error:<kind> | blocked:<kind>
	ContextTokens *int64 `json:"context_tokens,omitempty"` // >= 0
	LastTurnAt    string `json:"last_turn_at,omitempty"`   // RFC 3339
	Subagents     *int64 `json:"subagents,omitempty"`      // running background sub-agents, >= 0; absent means 0
	// CLIVersion is the client's X-Switchboard-Version header (#308), set by
	// the server, never read from the body; empty when absent.
	CLIVersion string `json:"-"`
}

// SlotView is one row of the slot table.
type SlotView struct {
	Role           string    `json:"role"`
	Base           string    `json:"base"`
	Number         int       `json:"number"`
	Machine        string    `json:"machine"`
	Directory      string    `json:"directory"`
	SessionID      string    `json:"session_id,omitempty"`
	Label          string    `json:"label,omitempty"`
	Parent         string    `json:"parent,omitempty"`
	RegisteredAt   time.Time `json:"registered_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// SlotGrant answers register, heartbeat and takeover.
type SlotGrant struct {
	SlotView
	LeaseSeconds int       `json:"lease_seconds"`
	OtherActive  int       `json:"other_active"`        // other leased slots under the same base
	Reclaimed    bool      `json:"reclaimed"`           // the identity already held this number
	Displaced    *SlotView `json:"displaced,omitempty"` // takeover: where the previous -1 holder went
	// HeldDelivered counts held label mail this call moved to the slot's
	// mailbox (see labels.go).
	HeldDelivered int `json:"held_delivered,omitempty"`
}
