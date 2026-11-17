// Package pty runs one child process attached to a pseudo-terminal.
//
// The same API covers every OS: a Unix PTY on Linux and macOS, ConPTY on
// Windows. The child runs in its own process group (Unix) or job object
// (Windows) so Stop can take down everything it started.
package pty

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Default terminal size used when Spec leaves Cols or Rows at zero.
const (
	DefaultCols = 80
	DefaultRows = 24
)

// ErrNoArgv is returned by Start when Spec.Argv is empty.
var ErrNoArgv = errors.New("pty: empty Argv")

// Spec describes the child to start.
type Spec struct {
	// Argv is the program and its arguments. Argv[0] is resolved against the
	// parent's PATH when it has no path separator.
	Argv []string
	// Dir is the child's working directory; empty means the parent's.
	Dir string
	// Env is the child's whole environment, as KEY=VALUE pairs. Nothing is
	// inherited from the parent: a nil or empty Env gives an empty environment.
	Env []string
	// Cols and Rows set the initial terminal size; zero means the default.
	Cols, Rows uint16
}

// Session is a running child attached to a pseudo-terminal. Read and Write
// carry the terminal's output and input. Close releases the terminal once
// the caller is done reading.
type Session struct {
	sys sysSession

	done     chan struct{}
	exitCode int
	waitErr  error

	closeOnce sync.Once
	closeErr  error
}

// Start launches spec.Argv on a new pseudo-terminal. When ctx is done before
// the child exits, the child and everything it started are killed.
func Start(ctx context.Context, spec Spec) (*Session, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, ErrNoArgv
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Cols == 0 {
		spec.Cols = DefaultCols
	}
	if spec.Rows == 0 {
		spec.Rows = DefaultRows
	}
	s := &Session{done: make(chan struct{})}
	if err := startSys(s, spec); err != nil {
		return nil, err
	}
	go func() {
		code, err := s.sys.wait()
		s.exitCode, s.waitErr = code, err
		close(s.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			s.sys.kill()
		case <-s.done:
		}
	}()
	return s, nil
}

// Read reads terminal output. It returns io.EOF once the child side is gone
// and the buffered output is drained, or after Close.
func (s *Session) Read(p []byte) (int, error) { return s.sys.read(p) }

// Write sends input to the terminal.
func (s *Session) Write(p []byte) (int, error) { return s.sys.write(p) }

// Resize changes the terminal size seen by the child.
func (s *Session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errors.New("pty: Resize needs non-zero cols and rows")
	}
	return s.sys.resize(cols, rows)
}

// Pid returns the child's process id.
func (s *Session) Pid() int { return s.sys.pid() }

// Done is closed when the child has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Wait blocks until the child exits and returns its exit code. On Unix a
// child killed by a signal reports 128+signal, as shells do. The error is
// non-nil only when the exit status could not be collected.
func (s *Session) Wait() (int, error) {
	<-s.done
	return s.exitCode, s.waitErr
}

// Stop asks the child to stop, then kills the whole process group (Unix) or
// job object (Windows) once grace passes. The polite stop is SIGTERM to the
// group on Unix and Ctrl-C on the terminal on Windows; grace <= 0 skips it.
// The final kill runs even when the child left on its own, so nothing the
// child started outlives Stop. Stop returns after the child has exited.
func (s *Session) Stop(grace time.Duration) error {
	if grace > 0 {
		select {
		case <-s.done:
		default:
			s.sys.interrupt()
			t := time.NewTimer(grace)
			select {
			case <-s.done:
			case <-t.C:
			}
			t.Stop()
		}
	}
	s.sys.kill()
	<-s.done
	return nil
}

// Close releases the terminal and its handles. It does not wait for the
// child; call Stop first to end it. Pending reads return io.EOF.
func (s *Session) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.sys.close() })
	return s.closeErr
}
