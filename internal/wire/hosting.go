package wire

// Hosted sessions (#173): the host's records of what should run on a node.
// Timestamps are Unix milliseconds.

// HostedSession is one row of GET /api/admin/terminals and the reply to its
// writes. Runtime is what the node last reported; until a node reports it
// says "unknown".
type HostedSession struct {
	ID             string            `json:"id"`
	Node           string            `json:"node"`
	Base           string            `json:"base"`
	Label          string            `json:"label"`
	Parent         string            `json:"parent"`
	Profile        string            `json:"profile"`
	Directory      string            `json:"directory"`
	AgentSessionID string            `json:"agent_session_id"`
	Launched       bool              `json:"launched"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	Restart        string            `json:"restart"`
	Desired        string            `json:"desired"`
	// Class is what the session does: "lead", "coordinator", "execute"
	// (default) or "review" (#190).
	Class string `json:"class"`
	// AcceptingWork is false for an execute or review session while the
	// subscription limit holds (HostingLimit): it takes no new work items.
	AcceptingWork bool `json:"accepting_work"`
	// SkipPermissions launches the agent with its permission-skip
	// arguments (#220): on for new records, off for records made before.
	SkipPermissions bool `json:"skip_permissions"`
	Cols            int  `json:"cols"`
	Rows            int  `json:"rows"`
	// RestartGen and StartGen count Restart and Start requests; a node
	// acts when one grows (Restart: stop and resume at once; Start: clear
	// backoff and crash loop).
	RestartGen int64         `json:"restart_gen"`
	StartGen   int64         `json:"start_gen"`
	CreatedAt  int64         `json:"created_at"`
	UpdatedAt  int64         `json:"updated_at"`
	Runtime    HostedRuntime `json:"runtime"`
	// LastRun is the run the host last saw reported for the record, sent
	// to its node only (#348): a node that starts over continues counting
	// from it. Zero when the host has not seen one.
	LastRun int `json:"last_run,omitempty"`
	// Slot is the numbered slot the session's plugin holds (the host's
	// list only; nodes never get it), and Presence what it says:
	// PresenceOnline while its lease runs, PresenceProcessAlive when the
	// lease ended but the process runs, PresenceOffline otherwise.
	Slot     *HostedSlot `json:"slot,omitempty"`
	Presence string      `json:"presence,omitempty"`
}

// Presence values of a HostedSession.
const (
	PresenceOnline       = "online"
	PresenceOffline      = "offline"
	PresenceProcessAlive = "offline (process alive)"
)

// HostedSlot is the slot a hosted session's plugin registered: the role
// with its number (team/worker-1), the conversation it reported and when
// its lease ends (Unix ms).
type HostedSlot struct {
	Role       string `json:"role"`
	Directory  string `json:"directory"`
	SessionID  string `json:"session_id,omitempty"`
	LeaseUntil int64  `json:"lease_until"`
	Online     bool   `json:"online"`
}

// HostedRuntime is the observed state of a hosted session, held in host
// memory and reported by its node.
type HostedRuntime struct {
	State         string `json:"state"` // "unknown" until the node reports, "node offline" while its node is away
	Since         *int64 `json:"since"` // when the host saw State change
	PID           int    `json:"pid"`
	Run           int    `json:"run"`
	StartedAt     *int64 `json:"started_at"`
	LastExit      *int   `json:"last_exit"`
	Restarts      int    `json:"restarts"`
	NextAttemptAt *int64 `json:"next_attempt_at"`
	// ResumeFailed marks a conversation that replaced one whose --resume
	// failed.
	ResumeFailed bool `json:"resume_failed,omitempty"`
}

// HostedSessionCreate is the body of POST /api/admin/terminals and what
// `host add` sends. Empty Restart means "always", empty Desired "running",
// empty Class "execute", nil SkipPermissions true.
type HostedSessionCreate struct {
	Node      string            `json:"node"`
	Base      string            `json:"base"`
	Profile   string            `json:"profile"`
	Directory string            `json:"directory"`
	Label     string            `json:"label"`
	Parent    string            `json:"parent"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Restart   string            `json:"restart"`
	Desired   string            `json:"desired"`
	Class     string            `json:"class"`
	// SkipPermissions is nil for the default (skip); `host add
	// --ask-permissions` sends false. `host set --skip-permissions` sends it
	// with the set action.
	SkipPermissions *bool `json:"skip_permissions,omitempty"`
}

// HostedSessionChange is the body of PUT /api/admin/terminals/{id}. A nil
// field is left as it is. Changes other than Desired apply at the next launch.
type HostedSessionChange struct {
	Label   *string            `json:"label,omitempty"`
	Parent  *string            `json:"parent,omitempty"`
	Args    *[]string          `json:"args,omitempty"`
	Env     *map[string]string `json:"env,omitempty"`
	Restart *string            `json:"restart,omitempty"`
	Desired *string            `json:"desired,omitempty"`
	Class   *string            `json:"class,omitempty"`
	// SkipPermissions applies at the next launch or restart.
	SkipPermissions *bool `json:"skip_permissions,omitempty"`
}

// Session classes (#190). While the subscription limit holds, lead and
// coordinator sessions keep taking work; execute and review sessions do not.
const (
	ClassLead        = "lead"
	ClassCoordinator = "coordinator"
	ClassExecute     = "execute"
	ClassReview      = "review"
)

// HostingLimit is GET /api/admin/terminals/limit and `host limit`: whether
// the agent subscription's usage limit holds (#190). Source is "operator"
// (set by hand) or "screen" (a session showed the limit message). ResetAt
// is the reset time the message gave, when it gave one. ClearedAt is when
// the limit last cleared: work items queued before it go first (see
// hosting.ResumeOrder).
type HostingLimit struct {
	Limited   bool   `json:"limited"`
	Source    string `json:"source,omitempty"`
	Since     *int64 `json:"since"`
	ResetAt   *int64 `json:"reset_at"`
	ClearedAt *int64 `json:"cleared_at"`
}

// HostingLimitChange is the body of PUT /api/admin/terminals/limit: the
// operator sets or clears the limit.
type HostingLimitChange struct {
	Limited bool `json:"limited"`
}

// NodeLimit is the usage limit as a node sees it on its sessions' screens,
// sent with every state frame. Since is when the newest showing began;
// ResetAt is the latest reset time the messages gave (nil when none did).
type NodeLimit struct {
	Limited bool   `json:"limited"`
	Since   int64  `json:"since,omitempty"`
	ResetAt *int64 `json:"reset_at,omitempty"`
}

// NodeView is one row of GET /api/admin/nodes. It never carries the token or
// its hash.
type NodeView struct {
	Name       string `json:"name"`
	Machine    string `json:"machine"`
	OS         string `json:"os"`
	Version    string `json:"version"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt *int64 `json:"last_seen_at"`
	RevokedAt  *int64 `json:"revoked_at"`
}

// NodeJoin is the body of POST /api/node/join: a one-time code from
// `switchboard node invite`.
type NodeJoin struct {
	Code string `json:"code"`
}

// NodeJoined answers a valid NodeJoin once: the invited node's name and its
// token, which the host keeps only as a hash.
type NodeJoined struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// Node channel (#173): one WebSocket from a node to GET /api/node/socket,
// carrying NodeFrame JSON both ways and, from node to host, binary terminal
// frames (EncodeTermFrame) for terminals a browser viewer is attached to.
const (
	// Host to node.
	NodeAssign  = "assign"  // Sessions: the node's full list of records
	NodeRestart = "restart" // ID: restart one session with resume (older hosts; records carry RestartGen)
	NodeAttach  = "attach"  // ID: a browser viewer wants this terminal: stream it, starting with a fresh snapshot
	NodeDetach  = "detach"  // ID: no viewer is left: stop streaming it
	NodeInput   = "input"   // ID, Data: type these bytes into the terminal
	NodeResize  = "resize"  // ID, Cols, Rows: resize the terminal
	// NodeCredentialType carries Credential: the role credential of record ID
	// (#343), sent before the assign that needs it, only to a node that
	// asked for credentials (config.NodeCredentialsHeaderName).
	NodeCredentialType = "credential"
	// Node to host.
	NodeHelloType  = "hello"  // Hello: who the node is
	NodeStateType  = "state"  // States: what the node observes, every record
	NodeNoticeType = "notice" // Notice: a crash loop or a resume failure
	NodeEventType  = "event"  // Event: a start, restart, stop, crash, exit or adoption (#252)
)

// Hosted session event kinds (#252).
const (
	HostedEventStart   = "start"   // launched: first time, asked for, or relaunched by the node itself
	HostedEventRestart = "restart" // stopped and launched again: asked for, or by the restart policy after an exit
	HostedEventStop    = "stop"    // stopped because the record was set to stopped
	HostedEventCrash   = "crash"   // ended with a non-zero exit code
	HostedEventExit    = "exit"    // ended with exit code 0
	HostedEventAdopt   = "adopt"   // a node process found the session already running (its keeper or tmux kept it)
)

// Who asked for a hosted session event, where known.
const (
	HostedActorAdmin  = "admin"  // a start, restart, stop or new conversation on the record
	HostedActorPolicy = "policy" // the record's restart policy after an exit
	HostedActorNode   = "node"   // the node on its own: a first launch, a relaunch after its holder went away, an adoption
)

// HostedEvent is one entry of a hosted session's history: what happened
// (Kind), when (At, Unix ms), the exit code of a crash or exit, the restart
// count after it and who asked (Actor, empty when nobody did). In node
// frames ID is the record; the host keeps the newest per session.
type HostedEvent struct {
	ID       string `json:"id,omitempty"`
	At       int64  `json:"at"`
	Kind     string `json:"kind"`
	ExitCode *int   `json:"exit_code"`
	Reason   string `json:"reason,omitempty"`
	Restarts int    `json:"restarts"`
	Actor    string `json:"actor,omitempty"`
}

// Notice kinds.
const (
	NoticeCrashLoop    = "crashloop"
	NoticeResumeFailed = "resumefail"
)

// NodeNotice is something the host tells a person about once. A node sends
// it again on every new connection while it holds; the host acts on it
// idempotently (keyed by Kind, ID and Run).
type NodeNotice struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Run  int    `json:"run"`
	// AgentSessionID is the conversation the notice is about; for a resume
	// failure the host replaces it only while the record still holds it.
	AgentSessionID string `json:"agent_session_id"`
	LastExit       int    `json:"last_exit"`
	// Runs is how many short runs led to it.
	Runs int `json:"runs"`
}

// Terminal frames are the binary messages of the node channel, node to host:
// one byte kind, one byte id length, the record id, then the VT bytes.
const (
	TermSnapshot byte = 's' // the screen and history, serialised (term.Grid.Snapshot)
	TermOutput   byte = 'o' // the terminal's output since the last frame
)

// EncodeTermFrame builds a terminal frame. id must be at most 255 bytes.
func EncodeTermFrame(kind byte, id string, payload []byte) []byte {
	b := make([]byte, 0, 2+len(id)+len(payload))
	b = append(b, kind, byte(len(id)))
	b = append(b, id...)
	return append(b, payload...)
}

// DecodeTermFrame splits a terminal frame; ok is false for a malformed one.
func DecodeTermFrame(b []byte) (kind byte, id string, payload []byte, ok bool) {
	if len(b) < 2 || len(b) < 2+int(b[1]) {
		return 0, "", nil, false
	}
	n := 2 + int(b[1])
	return b[0], string(b[2:n]), b[n:], true
}

// NodeFrame is one message on the node channel; Type says which fields it
// carries.
type NodeFrame struct {
	Type     string            `json:"type"`
	Sessions []HostedSession   `json:"sessions,omitempty"`
	ID       string            `json:"id,omitempty"`
	Hello    *NodeHello        `json:"hello,omitempty"`
	States   []HostedNodeState `json:"states,omitempty"`
	Data     []byte            `json:"data,omitempty"`
	Cols     int               `json:"cols,omitempty"`
	Rows     int               `json:"rows,omitempty"`
	Notice   *NodeNotice       `json:"notice,omitempty"`
	Event    *HostedEvent      `json:"event,omitempty"`
	// Credential rides on credential frames (#343).
	Credential *NodeCredential `json:"credential,omitempty"`
	// Limit rides on state frames (#190).
	Limit *NodeLimit `json:"limit,omitempty"`
}

// NodeCredential is a role credential for one hosted record (#343): the
// node writes it as the profile HostedProfileName(record) and the record's
// session runs with that profile. Token crosses only the node channel.
type NodeCredential struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Token string `json:"token"`
}

// HostedProfileName is the profile a node writes for the credential of
// record id (#343).
func HostedProfileName(id string) string { return "hosted-" + id }

// NodeHello is what a node says about itself when it attaches.
type NodeHello struct {
	Name    string `json:"name"`
	Machine string `json:"machine"`
	OS      string `json:"os"`
	Version string `json:"version"`
	Backend string `json:"backend"`
	// Max is the node's running-session cap (SWITCHBOARD_HOSTING_MAX).
	Max int `json:"max,omitempty"`
}

// HostedNodeState is one record's observed state as its node reports it.
// Launched tells the host to resume (not start) the conversation next time.
type HostedNodeState struct {
	ID       string `json:"id"`
	Launched bool   `json:"launched"`
	// AgentSessionID is the conversation Launched is about; the host
	// ignores Launched for a conversation the record no longer holds.
	AgentSessionID string        `json:"agent_session_id,omitempty"`
	Runtime        HostedRuntime `json:"runtime"`
}

// TerminalTicketRequest is the body of POST /api/admin/terminals/{id}/ticket.
type TerminalTicketRequest struct {
	Mode string `json:"mode"` // "view" or "input"
}

// TerminalTicket is a one-time ticket for the browser terminal socket.
type TerminalTicket struct {
	Ticket    string `json:"ticket"`
	Mode      string `json:"mode"`
	ExpiresAt int64  `json:"expires_at"` // unix milliseconds
}

// TerminalMessage is a text frame of the browser terminal socket, host to
// browser: "hello" first (State, Cols, Rows), then "state" on changes,
// "size" (Cols, Rows) when a viewer set the terminal's size, and "refused"
// (Error) for a client frame the host did not take.
// Binary frames carry VT bytes: a snapshot, then output.
type TerminalMessage struct {
	Type  string `json:"type"`
	State string `json:"state,omitempty"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Error string `json:"error,omitempty"`
}

// TerminalInput is a text frame of the browser terminal socket, browser to
// host: {"type":"input","data":…} or {"type":"resize","cols":…,"rows":…}.
// Only a socket opened with an "input" ticket is heard.
type TerminalInput struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// TerminalLogEntry is one row of a terminal's attach/input log: who, when
// (unix ms), what ("attach", "detach", "input") and, for input, how many
// bytes in that minute. Input content is never kept.
type TerminalLogEntry struct {
	AdminUser string `json:"admin_user"`
	Kind      string `json:"kind"`
	At        int64  `json:"at"`
	Bytes     int    `json:"bytes"`
}
