package wire

import (
	"fmt"
	"strconv"
	"time"
)

const (
	defaultMetricsDays = 7
	MaxMetricsDays     = 90
)

// ParseMetricsDays reads ?days=: empty means 7, otherwise 1-90.
func ParseMetricsDays(v string) (int, error) {
	if v == "" {
		return defaultMetricsDays, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxMetricsDays {
		return 0, fmt.Errorf("days must be a whole number from 1 to %d", MaxMetricsDays)
	}
	return n, nil
}

type MetricsReport struct {
	Window     MetricsWindow     `json:"window"`
	FirstReply FirstReplyMetrics `json:"first_reply"`
	Pending    []PendingAge      `json:"pending"`
	Threads    ThreadMetrics     `json:"threads"`
	Sessions   SessionMetrics    `json:"sessions"`
	Thresholds ThresholdSeconds  `json:"thresholds"`
	Aliases    *AliasUse         `json:"aliases,omitempty"` // admin view only
}

type MetricsWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days int       `json:"days"`
}

// LatencyStats summarises first-reply latencies in whole seconds. The
// percentiles are null when nothing was answered.
type LatencyStats struct {
	Count      int    `json:"count"`
	Answered   int    `json:"answered"`
	Unanswered int    `json:"unanswered"`
	P50        *int64 `json:"p50_s"`
	P90        *int64 `json:"p90_s"`
	Max        *int64 `json:"max_s"`
}

type FirstReplyMetrics struct {
	LatencyStats
	Buckets     []LatencyBucket    `json:"buckets"`
	ByRecipient []RecipientLatency `json:"by_recipient"`
}

type RecipientLatency struct {
	Base string `json:"base"`
	LatencyStats
}

// LatencyBucket counts answered latencies below UpToS (null: no upper bound)
// and at or above the previous bucket's bound.
type LatencyBucket struct {
	Label string `json:"label"`
	UpToS *int64 `json:"up_to_s"`
	Count int    `json:"count"`
}

type PendingAge struct {
	Role          string `json:"role"`
	Pending       int    `json:"pending"`
	OldestS       int64  `json:"oldest_s"`
	OverThreshold bool   `json:"over_threshold"`
}

type ThreadMetrics struct {
	MultiRound int            `json:"multi_round"`
	Top        []ThreadRounds `json:"top"`
}

type ThreadRounds struct {
	ThreadID   int64    `json:"thread_id"`
	Subject    string   `json:"subject"`
	Parties    []string `json:"parties"`
	RoundTrips int      `json:"round_trips"`
}

type SessionMetrics struct {
	ByRole  []RoleStateTime `json:"by_role"`
	Current []CurrentState  `json:"current"`
}

type RoleStateTime struct {
	Role     string     `json:"role"`
	ErrorS   int64      `json:"error_s"`
	BlockedS int64      `json:"blocked_s"`
	TopKinds []KindTime `json:"top_kinds"`
}

type KindTime struct {
	State   string `json:"state"`
	Seconds int64  `json:"seconds"`
}

// CurrentState is a session that is online and in error:* or blocked:* now.
type CurrentState struct {
	Role    string    `json:"role"`
	State   string    `json:"state"`
	Since   time.Time `json:"since"`
	Seconds int64     `json:"seconds"`
}

type ThresholdSeconds struct {
	PendingS int64 `json:"pending_s"`
	ErrorS   int64 `json:"error_s"`
	BlockedS int64 `json:"blocked_s"`
}
