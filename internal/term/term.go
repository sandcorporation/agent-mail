// Package term is the seam between hosted sessions and the terminal that runs
// them. A Backend (tmux on Linux, ConPTY on Windows, termtest in tests) runs
// named sessions; both real backends report screens as a Grid built by the
// shared Emulator, and a viewer attaching gets Grid.Snapshot followed by the
// live stream.
//
// The package is a leaf: it imports no other switchboard package.
package term

import (
	"context"
	"errors"
	"io"
	"time"
)

// Backend runs terminal sessions that outlive the process driving them.
// Session names are "sb-<hosted session id>".
type Backend interface {
	// Launch starts a session. It fails with ErrSessionExists while a
	// session (alive or dead but not forgotten) holds the name.
	Launch(ctx context.Context, s Spec) error
	// List reports every session the backend holds, dead ones included.
	List(ctx context.Context) ([]Info, error)
	// Screen renders the session's screen with at most history lines of
	// scrollback (negative: all of it). A dead session keeps its last screen
	// until Forget.
	Screen(ctx context.Context, name string, history int) (Grid, error)
	// Stream returns the session's VT output from now on. It ends with
	// io.EOF when the session exits, and closes when ctx is done.
	Stream(ctx context.Context, name string) (io.ReadCloser, error)
	// Input writes bytes to the session as if typed.
	Input(ctx context.Context, name string, b []byte) error
	// Resize changes the session's terminal size.
	Resize(ctx context.Context, name string, cols, rows int) error
	// Stop ends the session, killing it after grace.
	Stop(ctx context.Context, name string, grace time.Duration) error
	// Forget drops a dead session's leftovers (screen, exit code).
	Forget(ctx context.Context, name string) error
}

// Spec describes a session to launch.
type Spec struct {
	Name string
	Argv []string
	Dir  string
	// Env holds "KEY=value" entries.
	Env  []string
	Cols int
	Rows int
}

// Info is one session as the backend sees it.
type Info struct {
	Name  string
	PID   int
	Alive bool
	// ExitCode is meaningful only when Alive is false.
	ExitCode  int
	StartedAt time.Time
}

// Errors every Backend reports the same way.
var (
	ErrNoSession     = errors.New("term: no such session")
	ErrSessionExists = errors.New("term: session already exists")
	ErrNotRunning    = errors.New("term: session is not running")
)
