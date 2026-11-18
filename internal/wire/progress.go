package wire

import "time"

type ProgressReport struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	PendingAlertS int64             `json:"pending_alert_s"` // AGENT_MAIL_ALERT_PENDING in seconds; 0: off
	Services      []ServiceProgress `json:"services"`
}

type ServiceProgress struct {
	Service        string              `json:"service"`
	Badge          string              `json:"badge"` // worst live state: error, blocked, busy, idle, or none
	Sessions       []ProgressSession   `json:"sessions"`
	LastReport     *ProgressMail       `json:"last_report"`
	LastActivityAt *time.Time          `json:"last_activity_at"`
	OpenDirectives []ProgressDirective `json:"open_directives"`
}

type ProgressSession struct {
	Role      string    `json:"role"`
	State     string    `json:"state"` // idle, busy, compacting, error:*, blocked:*, or unknown
	Since     time.Time `json:"since"`
	Directory string    `json:"directory"`
	LastSeen  time.Time `json:"last_seen"`
}

type ProgressMail struct {
	ID      int64     `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type ProgressDirective struct {
	ID            int64  `json:"id"`
	To            string `json:"to"`
	Subject       string `json:"subject"`
	Status        string `json:"status"`
	AgeS          int64  `json:"age_s"`
	OverThreshold bool   `json:"over_threshold"`
}
