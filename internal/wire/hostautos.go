package wire

// Scheduled runs of a hosted session (#332, #333): at its cron times the
// host launches the session when it is stopped and mails the prompt to its
// base role.

// HostAuto states.
const (
	HostAutoActive = "active"
	HostAutoPaused = "paused"
)

// HostAutoRun results. T1 (#333) records fired, waiting, node-offline and
// error; the precheck (#334) adds skipped and timeout, the stop rules (#335)
// busy and overrun.
const (
	AutoFired       = "fired"
	AutoSkipped     = "skipped"
	AutoError       = "error"
	AutoTimeout     = "timeout"
	AutoBusy        = "busy"
	AutoNodeOffline = "node-offline"
	AutoOverrun     = "overrun"
	AutoWaiting     = "waiting"
)

// HostAuto is one scheduled run of a hosted session. Times are Unix ms.
type HostAuto struct {
	ID      int64  `json:"id"`
	Session string `json:"session"` // the hosted session record (hs_…)
	Cron    string `json:"cron"`
	TZ      string `json:"tz"` // the host machine's zone when it was added
	Prompt  string `json:"prompt"`
	Label   string `json:"label"`
	// Resume continues the session's last conversation when it launches;
	// false starts a new conversation each run.
	Resume bool `json:"resume"`
	// Notify are the roles told about runs that need attention (#336).
	Notify     []string `json:"notify"`
	State      string   `json:"state"`
	NextFireAt *int64   `json:"next_fire_at"`
	// RunRequestedAt is a `host auto run` the host has not handled yet.
	RunRequestedAt *int64       `json:"run_requested_at,omitempty"`
	CreatedAt      int64        `json:"created_at"`
	UpdatedAt      int64        `json:"updated_at"`
	LastRun        *HostAutoRun `json:"last_run,omitempty"`
}

// HostAutoCreate is the body of POST /api/admin/terminals/{id}/autos and what
// `host auto add` sends.
type HostAutoCreate struct {
	Cron   string   `json:"cron"`
	Prompt string   `json:"prompt"`
	Label  string   `json:"label,omitempty"`
	Resume bool     `json:"resume,omitempty"`
	Notify []string `json:"notify,omitempty"`
}

// HostAutoRun is one line of a scheduled run's history.
type HostAutoRun struct {
	ID     int64  `json:"id"`
	Auto   int64  `json:"auto"`
	At     int64  `json:"at"`
	Result string `json:"result"`
	// Manual marks a `host auto run` (the schedule's time was ignored).
	Manual bool `json:"manual,omitempty"`
	// ExitCode, DurationMS and Output are the precheck's (#334).
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
	Output     string `json:"output,omitempty"`
	MailID     *int64 `json:"mail_id,omitempty"`
	// Launched is true when this run started the session; AgentSessionID is
	// then the conversation it started (or resumed).
	Launched       bool   `json:"launched"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// HostAutoView is `host auto show` and GET …/autos/{id}: the record and its
// newest runs, newest first.
type HostAutoView struct {
	HostAuto
	Runs []HostAutoRun `json:"runs"`
}
