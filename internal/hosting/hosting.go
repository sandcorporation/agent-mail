// Package hosting is the host side of terminal hosting (#173): the
// hosted_sessions records (what should run on which node) and the nodes
// registry. It imports only config, store and wire; anything it needs from
// another domain (does a role exist?) is passed in as a function.
package hosting

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Restart policies and desired states a record may hold.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"

	DesiredRunning = "running"
	DesiredStopped = "stopped"

	// StateUnknown is the runtime state of a record no node has reported.
	StateUnknown = "unknown"

	defaultCols = 120
	defaultRows = 40
)

var (
	// ErrNotFound means no record has the given id.
	ErrNotFound = errors.New("no such hosted session")
	// ErrDuplicate means another record has the same (node, base, directory).
	ErrDuplicate = errors.New("a hosted session for this node, base and directory already exists")
)

// InvalidError is a record that breaks a validation rule; its message names
// the field and the rule.
type InvalidError struct{ msg string }

func (e InvalidError) Error() string { return e.msg }

func invalidf(format string, a ...any) error { return InvalidError{fmt.Sprintf(format, a...)} }

// Service is the hosted-session store over one database.
type Service struct {
	db         *sql.DB
	clock      func() time.Time
	roleExists func(base string) bool
	creds      *RoleCredentials
}

// New builds the service. roleExists answers whether base is a registered
// role (identity.RoleExists, passed in by the server).
func New(db *sql.DB, clock func() time.Time, roleExists func(base string) bool) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{db: db, clock: clock, roleExists: roleExists}
}

var (
	nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	profilePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	winAbsPattern   = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	idPattern       = regexp.MustCompile(`^hs_[0-9a-f]{32}$`)
)

// refusedArgs are agent flags the node sets itself (session id, resume) or
// that make the agent non-interactive.
var refusedArgs = map[string]string{
	"--session-id": "the node sets the session id",
	"--resume":     "the node resumes the conversation itself",
	"-r":           "the node resumes the conversation itself",
	"--continue":   "the node resumes the conversation itself",
	"-c":           "the node resumes the conversation itself",
	"--print":      "a hosted session is interactive",
	"-p":           "a hosted session is interactive",
}

// refusedEnvWords mark environment names that look like secrets. Secrets
// belong in the profile file on the node, which neither host nor node reads.
var refusedEnvWords = []string{"TOKEN", "SECRET", "KEY", "PASSWORD"}

func checkArgs(args []string) error {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if why, ok := refusedArgs[name]; ok {
			return invalidf("args: %s is not allowed (%s)", name, why)
		}
	}
	return nil
}

func checkEnv(env map[string]string) error {
	for name := range env {
		if !envNamePattern.MatchString(name) {
			return invalidf("env: invalid name %q (letters, digits and _, not starting with a digit)", name)
		}
		upper := strings.ToUpper(name)
		for _, w := range refusedEnvWords {
			if strings.Contains(upper, w) {
				return invalidf("env: %s is not allowed (names containing %s look like secrets; keep secrets in the profile)", name, w)
			}
		}
	}
	return nil
}

func checkRestart(v string) error {
	switch v {
	case RestartAlways, RestartOnFailure, RestartNever:
		return nil
	}
	return invalidf("restart: %q is not one of always, on-failure, never", v)
}

func checkDesired(v string) error {
	switch v {
	case DesiredRunning, DesiredStopped:
		return nil
	}
	return invalidf("desired: %q is not one of running, stopped", v)
}

func checkParent(v string) error {
	if v != "" && !wire.ValidRole(v) {
		return invalidf("parent: invalid role %q", v)
	}
	return nil
}

func checkLabel(v string) error {
	if len(v) > 200 || strings.ContainsAny(v, "\r\n\x00") {
		return invalidf("label: at most 200 bytes on one line")
	}
	return nil
}

// cleanDirectory accepts an absolute path for either kind of node: /… on
// Linux, C:\… or \\server\share\… on Windows. Trailing separators are
// dropped (except a root) so one directory has one spelling.
func cleanDirectory(dir string) (string, error) {
	switch {
	case strings.HasPrefix(dir, "/"):
		if t := strings.TrimRight(dir, "/"); t != "" {
			dir = t
		} else {
			dir = "/"
		}
	case winAbsPattern.MatchString(dir):
		if t := strings.TrimRight(dir, `\/`); len(t) > 2 {
			dir = t
		} else {
			dir = dir[:3]
		}
	case strings.HasPrefix(dir, `\\`) && len(strings.Trim(dir, `\`)) > 0:
		dir = strings.TrimRight(dir, `\/`)
	default:
		return "", invalidf("directory: %q is not an absolute path", dir)
	}
	return dir, nil
}

// validate checks a new record and fills its defaults.
func (s *Service) validate(c *wire.HostedSessionCreate) error {
	if err := checkNodeName(c.Node); err != nil {
		return err
	}
	if err := s.checkNodeUsable(c.Node); err != nil {
		return err
	}
	if !wire.ValidRole(c.Base) {
		return invalidf("base: invalid role %q", c.Base)
	}
	if s.roleExists == nil || !s.roleExists(c.Base) {
		return invalidf("base: role %s does not exist", c.Base)
	}
	if !profilePattern.MatchString(c.Profile) {
		return invalidf("profile: invalid name %q (1-64 of A-Z a-z 0-9 _ -)", c.Profile)
	}
	dir, err := cleanDirectory(c.Directory)
	if err != nil {
		return err
	}
	c.Directory = dir
	if c.Restart == "" {
		c.Restart = RestartAlways
	}
	if c.Desired == "" {
		c.Desired = DesiredRunning
	}
	if c.Class == "" {
		c.Class = wire.ClassExecute
	}
	if c.Args == nil {
		c.Args = []string{}
	}
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	for _, err := range []error{checkLabel(c.Label), checkParent(c.Parent), checkArgs(c.Args), checkEnv(c.Env), checkRestart(c.Restart), checkDesired(c.Desired), checkClass(c.Class)} {
		if err != nil {
			return err
		}
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newSessionID is a random (version 4) UUID for the agent conversation.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// ValidID reports whether id has the shape of a record id (hs_ + 32 hex).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Create validates and stores a new record with a fresh id and agent
// session id.
func (s *Service) Create(c wire.HostedSessionCreate) (wire.HostedSession, error) {
	if err := s.validate(&c); err != nil {
		return wire.HostedSession{}, err
	}
	suffix, err := randomHex(16)
	if err != nil {
		return wire.HostedSession{}, err
	}
	sid, err := newSessionID()
	if err != nil {
		return wire.HostedSession{}, err
	}
	now := s.clock().UnixMilli()
	h := wire.HostedSession{
		ID: "hs_" + suffix, Node: c.Node, Base: c.Base, Label: c.Label, Parent: c.Parent, Profile: c.Profile,
		Directory: c.Directory, AgentSessionID: sid, Args: c.Args, Env: c.Env, Restart: c.Restart, Desired: c.Desired,
		Class: c.Class, AcceptingWork: true, SkipPermissions: c.SkipPermissions == nil || *c.SkipPermissions, Cols: defaultCols, Rows: defaultRows, CreatedAt: now, UpdatedAt: now, Runtime: unknownRuntime(),
	}
	args, _ := json.Marshal(h.Args)
	env, _ := json.Marshal(h.Env)
	var dup int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM hosted_sessions WHERE node=? AND base=? AND directory=?`, h.Node, h.Base, h.Directory).Scan(&dup); err != nil {
		return wire.HostedSession{}, err
	}
	if dup > 0 {
		return wire.HostedSession{}, ErrDuplicate
	}
	cond, gargs, max, err := s.capGuard(h.Node)
	if err != nil {
		return wire.HostedSession{}, err
	}
	res, err := s.db.Exec(`INSERT INTO hosted_sessions(id,node,base,label,parent,profile,directory,agent_session_id,launched,args,env,restart,desired,class,skip_permissions,cols,rows,created_at,updated_at) SELECT ?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?,? WHERE ?<>? OR `+cond,
		append([]any{h.ID, h.Node, h.Base, h.Label, h.Parent, h.Profile, h.Directory, h.AgentSessionID, string(args), string(env), h.Restart, h.Desired, h.Class, boolInt(h.SkipPermissions), h.Cols, h.Rows, now, now, h.Desired, DesiredRunning}, gargs...)...)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return wire.HostedSession{}, ErrDuplicate
	}
	if err != nil {
		return wire.HostedSession{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return wire.HostedSession{}, CapacityError{Node: h.Node, Max: max}
	}
	h.AcceptingWork = s.acceptingWork(h.Class)
	return h, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unknownRuntime() wire.HostedRuntime { return wire.HostedRuntime{State: StateUnknown} }

const sessionColumns = `id,node,base,label,parent,profile,directory,agent_session_id,launched,args,env,restart,desired,class,skip_permissions,cols,rows,restart_gen,start_gen,created_at,updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanSession(r rowScanner) (wire.HostedSession, error) {
	var h wire.HostedSession
	var launched, skip int
	var args, env string
	if err := r.Scan(&h.ID, &h.Node, &h.Base, &h.Label, &h.Parent, &h.Profile, &h.Directory, &h.AgentSessionID, &launched, &args, &env, &h.Restart, &h.Desired, &h.Class, &skip, &h.Cols, &h.Rows, &h.RestartGen, &h.StartGen, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return h, err
	}
	h.Launched = launched != 0
	h.SkipPermissions = skip != 0
	if json.Unmarshal([]byte(args), &h.Args) != nil || h.Args == nil {
		h.Args = []string{}
	}
	if json.Unmarshal([]byte(env), &h.Env) != nil || h.Env == nil {
		h.Env = map[string]string{}
	}
	h.Runtime = unknownRuntime()
	h.AcceptingWork = true
	return h, nil
}

// List returns every record, oldest first.
func (s *Service) List() ([]wire.HostedSession, error) {
	rows, err := s.db.Query(`SELECT ` + sessionColumns + ` FROM hosted_sessions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.HostedSession{}
	for rows.Next() {
		h, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	limit, err := s.Limit()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].AcceptingWork = AcceptsWork(out[i].Class, limit.Limited)
	}
	return out, nil
}

// Get returns one record.
func (s *Service) Get(id string) (wire.HostedSession, error) {
	h, err := scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM hosted_sessions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// SetSize stores the terminal size of the record id, used on its next
// launch; changed is false when it already had that size or is gone.
func (s *Service) SetSize(id string, cols, rows int) (changed bool, err error) {
	res, err := s.db.Exec(`UPDATE hosted_sessions SET cols=?, rows=? WHERE id=? AND (cols<>? OR rows<>?)`, cols, rows, id, cols, rows)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Update applies the non-nil fields of c to the record id.
func (s *Service) Update(id string, c wire.HostedSessionChange) (wire.HostedSession, error) {
	h, err := s.Get(id)
	if err != nil {
		return h, err
	}
	if c.Label != nil {
		h.Label = *c.Label
	}
	if c.Parent != nil {
		h.Parent = *c.Parent
	}
	if c.Args != nil {
		h.Args = *c.Args
		if h.Args == nil {
			h.Args = []string{}
		}
	}
	if c.Env != nil {
		h.Env = *c.Env
		if h.Env == nil {
			h.Env = map[string]string{}
		}
	}
	if c.Restart != nil {
		h.Restart = *c.Restart
	}
	if c.Desired != nil {
		h.Desired = *c.Desired
	}
	if c.Class != nil {
		h.Class = *c.Class
	}
	if c.SkipPermissions != nil {
		h.SkipPermissions = *c.SkipPermissions
	}
	for _, err := range []error{checkLabel(h.Label), checkParent(h.Parent), checkArgs(h.Args), checkEnv(h.Env), checkRestart(h.Restart), checkDesired(h.Desired), checkClass(h.Class)} {
		if err != nil {
			return h, err
		}
	}
	h.AcceptingWork = s.acceptingWork(h.Class)
	h.UpdatedAt = s.clock().UnixMilli()
	if c.Desired != nil && *c.Desired == DesiredRunning {
		// Start, even of a record that should already run: its node clears
		// backoff and a crash loop and relaunches an ended session.
		h.StartGen++
	}
	args, _ := json.Marshal(h.Args)
	env, _ := json.Marshal(h.Env)
	return h, s.guardedUpdate(id, h.Node, h.Desired == DesiredRunning, `UPDATE hosted_sessions SET label=?,parent=?,args=?,env=?,restart=?,desired=?,class=?,skip_permissions=?,start_gen=start_gen+?,updated_at=?`,
		h.Label, h.Parent, string(args), string(env), h.Restart, h.Desired, h.Class, boolInt(h.SkipPermissions), startBump(c), h.UpdatedAt)
}

func startBump(c wire.HostedSessionChange) int {
	if c.Desired != nil && *c.Desired == DesiredRunning {
		return 1
	}
	return 0
}

// Restart asks the record's node to stop the session and resume it at once,
// clearing backoff and a crash loop. A stopped record is set to run instead.
func (s *Service) Restart(id string) (wire.HostedSession, error) {
	h, err := s.Get(id)
	if err != nil {
		return h, err
	}
	if h.Desired == DesiredStopped {
		return s.SetDesired(id, DesiredRunning)
	}
	h.UpdatedAt = s.clock().UnixMilli()
	h.RestartGen++
	return h, s.exec1(`UPDATE hosted_sessions SET restart_gen=restart_gen+1,updated_at=? WHERE id=?`, h.UpdatedAt, id)
}

// NewConversation gives the record a new agent session id, so its node
// stops the session and starts the new conversation with --session-id, and
// sets it to run.
func (s *Service) NewConversation(id string) (wire.HostedSession, error) {
	h, err := s.Get(id)
	if err != nil {
		return h, err
	}
	sid, err := newSessionID()
	if err != nil {
		return h, err
	}
	h.UpdatedAt = s.clock().UnixMilli()
	bump := 0
	if h.Desired != DesiredRunning {
		h.Desired, h.StartGen, bump = DesiredRunning, h.StartGen+1, 1
	}
	h.AgentSessionID, h.Launched = sid, false
	return h, s.guardedUpdate(id, h.Node, true, `UPDATE hosted_sessions SET agent_session_id=?,launched=0,desired=?,start_gen=start_gen+?,updated_at=?`,
		sid, h.Desired, bump, h.UpdatedAt)
}

// replaceConversation gives the record a new agent session id after its
// node could not resume old; it does nothing (changed false) when the
// record no longer holds old.
func (s *Service) replaceConversation(id, old string) (h wire.HostedSession, changed bool, err error) {
	sid, err := newSessionID()
	if err != nil {
		return h, false, err
	}
	res, err := s.db.Exec(`UPDATE hosted_sessions SET agent_session_id=?,launched=0,updated_at=? WHERE id=? AND agent_session_id=?`,
		sid, s.clock().UnixMilli(), id, old)
	if err != nil {
		return h, false, err
	}
	n, _ := res.RowsAffected()
	h, err = s.Get(id)
	return h, n > 0, err
}

func (s *Service) exec1(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDesired records whether the session should run.
func (s *Service) SetDesired(id, desired string) (wire.HostedSession, error) {
	return s.Update(id, wire.HostedSessionChange{Desired: &desired})
}

// Delete removes the record id and returns it as it was, and revokes its
// role credential (#343). The node stops the session when it next
// reconciles; the conversation files stay on disk.
func (s *Service) Delete(id string) (wire.HostedSession, error) {
	h, err := s.Get(id)
	if err != nil {
		return h, err
	}
	res, err := s.db.Exec(`DELETE FROM hosted_sessions WHERE id=?`, id)
	if err != nil {
		return h, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return h, ErrNotFound
	}
	if _, err = s.db.Exec(`DELETE FROM hosted_events WHERE session=?`, id); err != nil {
		return h, err
	}
	return h, s.revokeCredentials(func(c LiveCredential) bool { return c.Record == id })
}

// Nodes lists the node registry, by name.
func (s *Service) Nodes() ([]wire.NodeView, error) {
	rows, err := s.db.Query(`SELECT name,machine,os,version,created_at,last_seen_at,revoked_at FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.NodeView{}
	for rows.Next() {
		var n wire.NodeView
		var seen, revoked sql.NullInt64
		if err := rows.Scan(&n.Name, &n.Machine, &n.OS, &n.Version, &n.CreatedAt, &seen, &revoked); err != nil {
			return nil, err
		}
		if seen.Valid {
			n.LastSeenAt = &seen.Int64
		}
		if revoked.Valid {
			n.RevokedAt = &revoked.Int64
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
