//go:build !windows

package pty

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	cpty "github.com/creack/pty"
)

type sysSession struct {
	cmd *exec.Cmd
	f   *os.File
}

func startSys(s *Session, spec Spec) error {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	// A non-nil Env stops exec from inheriting the parent's environment.
	cmd.Env = append([]string{}, spec.Env...)
	f, err := cpty.StartWithAttrs(cmd,
		&cpty.Winsize{Cols: spec.Cols, Rows: spec.Rows},
		// Setsid makes the child a session and process group leader, so
		// pgid == pid and kill(-pid) reaches everything it starts.
		&syscall.SysProcAttr{Setsid: true, Setctty: true})
	if err != nil {
		return err
	}
	s.sys = sysSession{cmd: cmd, f: f}
	return nil
}

func (u *sysSession) read(p []byte) (int, error) {
	n, err := u.f.Read(p)
	// Linux reports EIO on the master once the child side is closed.
	if err != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		err = io.EOF
	}
	return n, err
}

func (u *sysSession) write(p []byte) (int, error) { return u.f.Write(p) }

func (u *sysSession) resize(cols, rows uint16) error {
	return cpty.Setsize(u.f, &cpty.Winsize{Cols: cols, Rows: rows})
}

func (u *sysSession) pid() int { return u.cmd.Process.Pid }

func (u *sysSession) wait() (int, error) {
	err := u.cmd.Wait()
	st := u.cmd.ProcessState
	if st == nil {
		return -1, err
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return st.ExitCode(), err
	}
	return st.ExitCode(), nil
}

func (u *sysSession) interrupt() { _ = syscall.Kill(-u.cmd.Process.Pid, syscall.SIGTERM) }

// kill sends SIGKILL to the child's process group. ESRCH (group already
// empty) is expected and ignored.
func (u *sysSession) kill() { _ = syscall.Kill(-u.cmd.Process.Pid, syscall.SIGKILL) }

func (u *sysSession) close() error { return u.f.Close() }
