// Package node is node mode for terminal hosting (#173): it holds the
// records the host assigned to this machine and reconciles the terminal
// backend against them. The node is the source of truth for what is running;
// it keeps nothing durable, so a new Node over the same backend adopts what
// is already there.
//
// The package imports config, wire and term only; it never imports hosting.
package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
)

// Restart policies.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// Desired states.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
)

// Observed states.
const (
	ObservedStarting = "starting"
	ObservedRunning  = "running"
	ObservedStopped  = "stopped"
)

// NamePrefix starts every terminal name the node owns: "sb-<record id>".
const NamePrefix = "sb-"

// StopGrace is how long a stopped session gets before it is killed.
const StopGrace = 10 * time.Second

// Record is one hosted session the host assigned to this node: the fields
// of the host's hosted_sessions row (#184) the node needs. The host channel
// (#186) fills it from the wire shape.
type Record struct {
	ID        string
	Profile   string
	Label     string
	Parent    string
	Directory string
	// AgentSessionID is the agent conversation id passed as --session-id on
	// the first launch and --resume after it.
	AgentSessionID string
	// Launched is true once a launch of this record succeeded anywhere.
	Launched bool
	Args     []string
	// Env is extra non-secret environment; secret-like names are dropped.
	Env        map[string]string
	Restart    string
	Desired    string
	Cols, Rows int
}

// TerminalName is the backend session name for a record id.
func TerminalName(id string) string { return NamePrefix + id }

// State is what the node observes for one record, reported to the host.
type State struct {
	ID       string
	Observed string
	PID      int
	// Run counts launches by this node process; an adopted session counts
	// as run 1.
	Run       int
	StartedAt time.Time
	// LastExit is the exit code of the last finished run, nil before one.
	LastExit *int
	// Launched is true once this node has launched or adopted the record,
	// so the host can persist it.
	Launched bool
}

// Node reconciles a backend against the assigned records.
type Node struct {
	cfg     Config
	backend term.Backend

	mu      sync.Mutex
	records []Record
	states  map[string]*state
}

type state struct {
	State
	// seen is the start time of the run whose exit is already recorded.
	seen time.Time
}

// New returns a node over backend. cfg must come from Resolve (or be filled
// the same way in tests).
func New(cfg Config, backend term.Backend) (*Node, error) {
	if cfg.Agent == "" {
		return nil, errors.New("node: no agent CLI resolved")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Node{cfg: cfg, backend: backend, states: map[string]*state{}}, nil
}

// Assign replaces the node's records with the host's full list. The node
// keeps enforcing the last list it got.
func (n *Node) Assign(recs []Record) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.records = append([]Record(nil), recs...)
	ids := map[string]bool{}
	for _, r := range recs {
		ids[r.ID] = true
	}
	for id := range n.states {
		if !ids[id] {
			delete(n.states, id)
		}
	}
}

// States reports the observed state of every assigned record, in
// assignment order.
func (n *Node) States() []State {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]State, 0, len(n.records))
	for _, r := range n.records {
		s := n.stateOf(r)
		st := s.State
		if st.LastExit != nil {
			c := *st.LastExit
			st.LastExit = &c
		}
		out = append(out, st)
	}
	return out
}

func (n *Node) stateOf(r Record) *state {
	s, ok := n.states[r.ID]
	if !ok {
		s = &state{State: State{ID: r.ID, Observed: ObservedStopped, Launched: r.Launched}}
		n.states[r.ID] = s
	}
	if r.Launched {
		s.Launched = true
	}
	return s
}

// Run reconciles every interval until ctx is done.
func (n *Node) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		_ = n.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Reconcile makes one pass: it stops and forgets backend sessions with no
// record, adopts live ones, handles exits by restart policy, launches
// records that should run and stops records that should not. Errors from
// individual sessions are joined; the pass still visits every record.
func (n *Node) Reconcile(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	infos, err := n.backend.List(ctx)
	if err != nil {
		return fmt.Errorf("node: list sessions: %w", err)
	}
	byName := map[string]term.Info{}
	for _, in := range infos {
		byName[in.Name] = in
	}
	var errs []error
	assigned := map[string]bool{}
	for _, r := range n.records {
		assigned[TerminalName(r.ID)] = true
	}
	for _, in := range infos {
		if !strings.HasPrefix(in.Name, NamePrefix) || assigned[in.Name] {
			continue
		}
		errs = append(errs, n.drop(ctx, in))
	}
	for _, r := range n.records {
		in, ok := byName[TerminalName(r.ID)]
		errs = append(errs, n.reconcile(ctx, r, in, ok))
	}
	return errors.Join(errs...)
}

// drop stops and forgets a backend session no record owns.
func (n *Node) drop(ctx context.Context, in term.Info) error {
	if in.Alive {
		if err := n.backend.Stop(ctx, in.Name, StopGrace); err != nil {
			return fmt.Errorf("node: stop orphan %s: %w", in.Name, err)
		}
	}
	if err := n.backend.Forget(ctx, in.Name); err != nil && !errors.Is(err, term.ErrNoSession) {
		return fmt.Errorf("node: forget orphan %s: %w", in.Name, err)
	}
	return nil
}

func (n *Node) reconcile(ctx context.Context, r Record, in term.Info, exists bool) error {
	s := n.stateOf(r)
	name := TerminalName(r.ID)
	running := r.Desired != DesiredStopped
	if exists {
		// A session in the backend means the record was launched before,
		// by this node or an earlier one.
		s.Launched = true
	}
	switch {
	case exists && in.Alive && running:
		s.Observed, s.PID, s.StartedAt = ObservedRunning, in.PID, in.StartedAt
		if s.Run == 0 {
			s.Run = 1
		}
		return nil
	case exists && in.Alive:
		if err := n.backend.Stop(ctx, name, StopGrace); err != nil {
			return fmt.Errorf("node: stop %s: %w", name, err)
		}
		s.Observed, s.PID = ObservedStopped, 0
		return nil
	case exists:
		if !in.StartedAt.Equal(s.seen) {
			code := in.ExitCode
			s.LastExit, s.seen = &code, in.StartedAt
		}
		s.PID = 0
		if !running || !restarts(r.Restart, in.ExitCode) {
			s.Observed = ObservedStopped
			return nil
		}
		if err := n.backend.Forget(ctx, name); err != nil && !errors.Is(err, term.ErrNoSession) {
			return fmt.Errorf("node: forget %s: %w", name, err)
		}
		return n.launch(ctx, r, s)
	case running:
		return n.launch(ctx, r, s)
	default:
		s.Observed, s.PID = ObservedStopped, 0
		return nil
	}
}

// restarts applies a restart policy to an exit code.
func restarts(policy string, code int) bool {
	switch policy {
	case RestartNever:
		return false
	case RestartOnFailure:
		return code != 0
	default:
		return true
	}
}

func (n *Node) launch(ctx context.Context, r Record, s *state) error {
	spec := term.Spec{
		Name: TerminalName(r.ID),
		Argv: n.cfg.Argv(r, s.Launched),
		Dir:  r.Directory,
		Env:  n.cfg.ChildEnv(r),
		Cols: r.Cols,
		Rows: r.Rows,
	}
	if spec.Cols <= 0 {
		spec.Cols = DefaultCols
	}
	if spec.Rows <= 0 {
		spec.Rows = DefaultRows
	}
	if err := n.backend.Launch(ctx, spec); err != nil {
		s.Observed = ObservedStopped
		return fmt.Errorf("node: launch %s: %w", spec.Name, err)
	}
	s.Launched = true
	s.Run++
	s.Observed, s.PID, s.StartedAt = ObservedStarting, 0, n.cfg.Now()
	return nil
}

// Default terminal size when a record carries none.
const (
	DefaultCols = 120
	DefaultRows = 40
)
