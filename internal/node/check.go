package node

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// ErrChecksOff is a precheck's error on a node started without
// --allow-checks (#334).
const ErrChecksOff = "this node does not run prechecks (start it with --allow-checks or SWITCHBOARD_HOSTING_ALLOW_CHECKS=on)"

// checkWaitDelay bounds the wait for a precheck's output pipe after it
// exits or is killed.
const checkWaitDelay = 5 * time.Second

// Check runs a scheduled run's precheck for record id (#334): chk.Command
// through the shell, in the record's directory, with the session's
// environment minus the role profile, killing its whole process tree at
// chk.TimeoutMS or when ctx ends.
func (n *Node) Check(ctx context.Context, id string, chk wire.NodeCheck) wire.NodeCheckResult {
	res := wire.NodeCheckResult{Ref: chk.Ref}
	if !n.cfg.AllowChecks {
		res.Error = ErrChecksOff
		return res
	}
	var rec Record
	found := false
	n.mu.Lock()
	for _, r := range n.records {
		if r.ID == id {
			rec, found = r, true
			break
		}
	}
	n.mu.Unlock()
	if !found {
		res.Error = "record " + id + " is not on this node"
		return res
	}
	timeout := time.Duration(chk.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = wire.DefaultCheckTimeoutMS * time.Millisecond
	}
	out := RunCheck(ctx, rec.Directory, n.cfg.CheckEnv(rec), chk.Command, timeout)
	out.Ref = chk.Ref
	return out
}

// CheckEnv is a precheck's environment: the record's session environment
// without its role profile (the role token never reaches a child).
func (c Config) CheckEnv(r Record) []string {
	r.Profile = ""
	return c.ChildEnv(r)
}

// RunCheck runs command with the platform shell (sh -c; cmd /c on Windows)
// in dir with env. Past timeout, or when ctx ends, it kills the command and
// everything it started (a process group; a job object on Windows). Output
// is the end of stdout, at most wire.CheckOutputMax bytes.
func RunCheck(ctx context.Context, dir string, env []string, command string, timeout time.Duration) wire.NodeCheckResult {
	var res wire.NodeCheckResult
	tail := &tailBuffer{max: wire.CheckOutputMax}
	cmd := checkCommand(command)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.WaitDelay = dir, env, tail, checkWaitDelay
	start := time.Now()
	kill, release, err := startCheck(cmd)
	if err != nil {
		res.Error = "start: " + err.Error()
		return res
	}
	defer release()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err = <-done:
	case <-t.C:
		res.TimedOut = true
		kill()
		err = <-done
	case <-ctx.Done():
		kill()
		err = <-done
		res.Error = "stopped: " + ctx.Err().Error()
	}
	res.DurationMS = time.Since(start).Milliseconds()
	res.Output = tail.String()
	if res.TimedOut || res.Error != "" {
		return res
	}
	var exit *exec.ExitError
	switch {
	case cmd.ProcessState != nil:
		code := cmd.ProcessState.ExitCode()
		res.ExitCode = &code
	case err != nil && !errors.As(err, &exit):
		res.Error = err.Error()
	}
	return res
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

// String is the kept bytes as text, starting at a whole character.
func (t *tailBuffer) String() string {
	b := t.b
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
		b = b[1:]
	}
	return strings.ToValidUTF8(string(b), "�")
}
