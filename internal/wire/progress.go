package wire

import "time"

type ProgressReport struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	PendingAlertS int64             `json:"pending_alert_s"` // SWITCHBOARD_ALERT_PENDING in seconds; 0: off
	IdleAfterS    int64             `json:"idle_after_s"`    // SWITCHBOARD_REMIND_IDLE in seconds; 0: off
	Services      []ServiceProgress `json:"services"`
}

type ServiceProgress struct {
	Service        string              `json:"service"`
	Badge          string              `json:"badge"` // worst live state: error, blocked, busy, idle, or none
	Sessions       []ProgressSession   `json:"sessions"`
	LastReport     *ProgressMail       `json:"last_report"`
	LastActivityAt *time.Time          `json:"last_activity_at"`
	OpenDirectives []ProgressDirective `json:"open_directives"`
	// Stalled lists the directives answered but not completed whose
	// recipient's sessions have been idle since for SWITCHBOARD_REMIND_IDLE
	// (#268), the 대기 멈춤 rows.
	Stalled []ProgressStall `json:"stalled"`
}

type ProgressSession struct {
	Role      string    `json:"role"`
	State     string    `json:"state"` // idle, busy, compacting, error:*, blocked:*, or unknown
	Since     time.Time `json:"since"`
	Directory string    `json:"directory"`
	LastSeen  time.Time `json:"last_seen"`
	Subagents int64     `json:"subagents,omitempty"` // running background sub-agents
}

type ProgressMail struct {
	ID      int64     `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

// ProgressStall is a directive answered (a plain reply) but not completed,
// the newest directive to its recipient, whose live sessions have all been
// idle since IdleSince.
type ProgressStall struct {
	ID         int64     `json:"id"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Subject    string    `json:"subject"`
	AnsweredAt time.Time `json:"answered_at"`
	IdleSince  time.Time `json:"idle_since"` // the later of the answer and the start of the sessions' idle stretch
	IdleS      int64     `json:"idle_s"`
	Sessions   []string  `json:"sessions"`
}

type ProgressDirective struct {
	ID            int64  `json:"id"`
	To            string `json:"to"`
	Subject       string `json:"subject"`
	Status        string `json:"status"`
	AgeS          int64  `json:"age_s"`
	OverThreshold bool   `json:"over_threshold"`
}
