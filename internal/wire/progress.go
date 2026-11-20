package wire

import "time"

type ProgressReport struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	PendingAlertS int64             `json:"pending_alert_s"` // SWITCHBOARD_ALERT_PENDING in seconds; 0: off
	IdleAfterS    int64             `json:"idle_after_s"`    // SWITCHBOARD_REMIND_IDLE in seconds; 0: off
	Services      []ServiceProgress `json:"services"`
	// CLIBehind is one row per machine with an old CLI (#308),
	// the 옛 판 rows of 진행 대기.
	CLIBehind []CLIBehind `json:"cli_behind"`
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

// ProgressStall is an open directive, the newest open one to its recipient,
// whose live sessions have all been idle since IdleSince.
type ProgressStall struct {
	ID         int64      `json:"id"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	Subject    string     `json:"subject"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"` // its newest reply; none when it got none
	IdleSince  time.Time  `json:"idle_since"`            // the latest of the directive, its newest reply and the start of the sessions' idle stretch
	IdleS      int64      `json:"idle_s"`
	Sessions   []string   `json:"sessions"`
}

// ProgressDirective is an open directive. Recipient is its recipient
// mailbox's live state: the worst state of the sessions reading it (idle,
// busy, ...), or offline when none is live.
type ProgressDirective struct {
	ID            int64      `json:"id"`
	To            string     `json:"to"`
	Subject       string     `json:"subject"`
	Status        string     `json:"status"`
	AgeS          int64      `json:"age_s"`
	OverThreshold bool       `json:"over_threshold"`
	RepliedAt     *time.Time `json:"replied_at,omitempty"` // its newest reply; open directives can have replies (#269)
	Recipient     string     `json:"recipient_state"`
}
