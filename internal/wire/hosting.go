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
	Cols           int               `json:"cols"`
	Rows           int               `json:"rows"`
	CreatedAt      int64             `json:"created_at"`
	UpdatedAt      int64             `json:"updated_at"`
	Runtime        HostedRuntime     `json:"runtime"`
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
}

// HostedSessionCreate is the body of POST /api/admin/terminals and what
// `host add` sends. Empty Restart means "always", empty Desired "running".
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

// Node channel (#173): one WebSocket from a node to GET /api/node/socket,
// carrying NodeFrame JSON both ways.
const (
	// Host to node.
	NodeAssign  = "assign"  // Sessions: the node's full list of records
	NodeRestart = "restart" // ID: restart one session with resume
	// Node to host.
	NodeHelloType = "hello" // Hello: who the node is
	NodeStateType = "state" // States: what the node observes, every record
)

// NodeFrame is one message on the node channel; Type says which fields it
// carries.
type NodeFrame struct {
	Type     string            `json:"type"`
	Sessions []HostedSession   `json:"sessions,omitempty"`
	ID       string            `json:"id,omitempty"`
	Hello    *NodeHello        `json:"hello,omitempty"`
	States   []HostedNodeState `json:"states,omitempty"`
}

// NodeHello is what a node says about itself when it attaches.
type NodeHello struct {
	Name    string `json:"name"`
	Machine string `json:"machine"`
	OS      string `json:"os"`
	Version string `json:"version"`
	Backend string `json:"backend"`
}

// HostedNodeState is one record's observed state as its node reports it.
// Launched tells the host to resume (not start) the conversation next time.
type HostedNodeState struct {
	ID       string        `json:"id"`
	Launched bool          `json:"launched"`
	Runtime  HostedRuntime `json:"runtime"`
}
