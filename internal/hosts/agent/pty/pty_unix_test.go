//go:build !windows

package pty

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var shEnv = []string{"PATH=/usr/bin:/bin"}

// output collects everything the session prints, on a background reader.
type output struct {
	mu   sync.Mutex
	buf  strings.Builder
	more chan struct{}
}

func collect(t *testing.T, s *Session) *output {
	t.Helper()
	o := &output{more: make(chan struct{}, 1)}
	go func() {
		p := make([]byte, 4096)
		for {
			n, err := s.Read(p)
			if n > 0 {
				o.mu.Lock()
				o.buf.Write(p[:n])
				o.mu.Unlock()
				select {
				case o.more <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// waitFor waits until the output after offset `from` contains want and
// returns the output length just past it.
func (o *output) waitFor(t *testing.T, from int, want string) int {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s := o.String()
		if from <= len(s) {
			if i := strings.Index(s[from:], want); i >= 0 {
				return from + i + len(want)
			}
		}
		select {
		case <-o.more:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %q; output so far: %q", want, s)
		}
	}
}

func start(t *testing.T, spec Spec) (*Session, *output) {
	t.Helper()
	s, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Stop(0)
		_ = s.Close()
	})
	return s, collect(t, s)
}

func waitCode(t *testing.T, s *Session) int {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit")
	}
	code, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return code
}

func TestEchoRoundTrip(t *testing.T) {
	s, out := start(t, Spec{Argv: []string{"/bin/cat"}})
	if _, err := s.Write([]byte("hello pty\n")); err != nil {
		t.Fatal(err)
	}
	// The terminal echoes the line, then cat prints it back.
	n := out.waitFor(t, 0, "hello pty")
	out.waitFor(t, n, "hello pty")
}

func TestResizeVisibleToChild(t *testing.T) {
	s, out := start(t, Spec{
		Argv: []string{"/bin/sh", "-c", "stty size; read x; stty size; read y"},
		Env:  shEnv, Cols: 80, Rows: 24,
	})
	n := out.waitFor(t, 0, "24 80")
	if err := s.Resize(100, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, err := s.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, n, "40 100")
}

func TestExitCode(t *testing.T) {
	s, _ := start(t, Spec{Argv: []string{"/bin/sh", "-c", "exit 7"}})
	if code := waitCode(t, s); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
}

func TestStopGraceful(t *testing.T) {
	s, out := start(t, Spec{
		Argv: []string{"/bin/sh", "-c", `trap 'echo bye; exit 3' TERM; echo ready; while :; do sleep 0.05; done`},
		Env:  shEnv,
	})
	out.waitFor(t, 0, "ready")
	began := time.Now()
	if err := s.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("graceful Stop took %v; the trap should have ended it", took)
	}
	if code := waitCode(t, s); code != 3 {
		t.Fatalf("exit code = %d, want 3 from the TERM trap", code)
	}
	out.waitFor(t, 0, "bye")
}

func TestStopForced(t *testing.T) {
	s, out := start(t, Spec{
		Argv: []string{"/bin/sh", "-c", `trap '' TERM; echo ready; while :; do sleep 0.05; done`},
		Env:  shEnv,
	})
	out.waitFor(t, 0, "ready")
	began := time.Now()
	const grace = 200 * time.Millisecond
	if err := s.Stop(grace); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took < grace {
		t.Fatalf("Stop returned after %v, before the %v grace", took, grace)
	}
	if code := waitCode(t, s); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit code = %d, want %d (SIGKILL)", code, 128+int(syscall.SIGKILL))
	}
}

func TestEnvIsNotInherited(t *testing.T) {
	t.Setenv("PTY_PARENT_ONLY", "leak")
	s, out := start(t, Spec{Argv: []string{"/usr/bin/env"}, Env: []string{"ONLY=this"}})
	if code := waitCode(t, s); code != 0 {
		t.Fatalf("env exit code = %d", code)
	}
	out.waitFor(t, 0, "ONLY=this")
	// Give the reader a moment to drain anything else env printed.
	time.Sleep(50 * time.Millisecond)
	got := strings.Fields(out.String())
	if len(got) != 1 || got[0] != "ONLY=this" {
		t.Fatalf("child env = %q, want exactly [ONLY=this]", got)
	}
}

func TestNoOrphanAfterStop(t *testing.T) {
	// The leader leaves politely on TERM, but its background child ignores
	// TERM. Stop's final group kill must still take it down.
	s, out := start(t, Spec{
		Argv: []string{"/bin/sh", "-c", `(trap '' TERM; sleep 300) & echo "kid=$!."; trap 'exit 0' TERM; wait`},
		Env:  shEnv,
	})
	n := out.waitFor(t, 0, "kid=")
	end := out.waitFor(t, n, ".")
	kid, err := strconv.Atoi(strings.TrimSpace(out.String()[n : end-1]))
	if err != nil {
		t.Fatalf("parse child pid: %v (output %q)", err, out.String())
	}
	if err := s.Stop(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(kid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d still alive after Stop (kill 0: %v)", kid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, Spec{Argv: []string{"/bin/sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cancel()
	if code := waitCode(t, s); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit code = %d, want SIGKILL", code)
	}
}

func TestStartRejectsEmptyArgv(t *testing.T) {
	if _, err := Start(context.Background(), Spec{}); !errors.Is(err, ErrNoArgv) {
		t.Fatalf("err = %v, want ErrNoArgv", err)
	}
}
