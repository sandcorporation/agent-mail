package wire

import (
	"errors"
	"strings"
	"time"
)

// Schedule kinds (#288, #292): cron fires on a five-field expression in the
// zone recorded at creation; once fires at one time.
const (
	ScheduleCron = "cron"
	ScheduleOnce = "once"
)

// Schedule states. active: it fires (or holds missed fires to deliver);
// ended: a once schedule delivered its fire; expired: it reached expires_at.
// A removed schedule is deleted.
const (
	ScheduleActive  = "active"
	ScheduleEnded   = "ended"
	ScheduleExpired = "expired"
)

// MaxSchedulePrompt caps the prompt a fire delivers.
const MaxSchedulePrompt = 16 << 10

// Schedule is a prompt delivered to its owner role at times
// (GET /api/schedules/ID). Missed counts the fires not yet delivered because
// no session of the role was attached (or the previous fire's mail was still
// unread); MissedLastAt is the last of them.
type Schedule struct {
	ID           int64      `json:"id"`
	Role         string     `json:"role"`
	Machine      string     `json:"machine"`
	Kind         string     `json:"kind"`
	Label        string     `json:"label"`
	Prompt       string     `json:"prompt"`
	Cron         string     `json:"cron,omitempty"`
	TZ           string     `json:"tz"`
	State        string     `json:"state"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	NextFireAt   *time.Time `json:"next_fire_at,omitempty"`
	LastFireAt   *time.Time `json:"last_fire_at,omitempty"`
	Missed       int        `json:"missed,omitempty"`
	MissedLastAt *time.Time `json:"missed_last_at,omitempty"`
}

// ScheduleCreate is the body of POST /api/schedules (switchboard schedule
// add). A cron schedule has Cron and TZ (the machine's zone); a once
// schedule has At.
type ScheduleCreate struct {
	Kind           string     `json:"kind"`
	Cron           string     `json:"cron,omitempty"`
	At             *time.Time `json:"at,omitempty"`
	TZ             string     `json:"tz"`
	Label          string     `json:"label,omitempty"`
	Prompt         string     `json:"prompt"`
	Machine        string     `json:"machine"`
	ExpiresIn      int64      `json:"expires_in,omitempty"` // seconds; 0 is DefaultWatchExpiry
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

// Check fills the label from the prompt and refuses a missing or over-long
// field and a kind without its time. The cron expression and the zone are
// checked by the server (internal/cron).
func (q *ScheduleCreate) Check() error {
	q.Label, q.Machine, q.Cron = oneLine(q.Label), strings.TrimSpace(q.Machine), strings.TrimSpace(q.Cron)
	switch {
	case q.Kind == ScheduleCron && q.Cron == "":
		return errors.New("cron schedule needs cron")
	case q.Kind == ScheduleOnce && (q.At == nil || q.At.IsZero()):
		return errors.New("once schedule needs at")
	case q.Kind != ScheduleCron && q.Kind != ScheduleOnce:
		return errors.New("kind must be cron or once")
	case strings.TrimSpace(q.Prompt) == "":
		return errors.New("prompt required")
	case len(q.Prompt) > MaxSchedulePrompt:
		return errors.New("prompt over 16 KB")
	case q.Machine == "":
		return errors.New("machine required")
	case q.ExpiresIn < 0:
		return errors.New("expires_in must not be negative")
	}
	if q.Label == "" {
		q.Label = oneLine(q.Prompt)
		if r := []rune(q.Label); len(r) > 60 {
			q.Label = string(r[:60])
		}
	}
	if len(q.Label) > MaxJobLabel {
		return errors.New("label over 200 bytes")
	}
	return nil
}
