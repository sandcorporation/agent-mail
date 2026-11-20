//go:build unix

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// stopSignals make the runner stop its command; ignoredSignals it ignores
// (it has no terminal, but a hangup must never end it).
var (
	stopSignals    = []os.Signal{syscall.SIGTERM, syscall.SIGINT}
	ignoredSignals = []os.Signal{syscall.SIGHUP}
)

// runnerCopies: Unix does not lock a running executable, so the runner runs
// from the installed binary itself.
const runnerCopies = false

// checkRunner: a runner can always leave the terminal (setsid).
func checkRunner() error { return nil }

// startDetached starts cmd in a session of its own with stdin from
// /dev/null, so closing the terminal or ending the caller does not reach it.
func startDetached(cmd *exec.Cmd) error {
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// childGroup is the job's command in a process group of its own, so a stop
// reaches everything it started.
type childGroup struct{ pid int }

func startChild(cmd *exec.Cmd) (childGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return childGroup{}, err
	}
	return childGroup{pid: cmd.Process.Pid}, nil
}

// stop terminates (or kills) the group.
func (g childGroup) stop(kill bool) error {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-g.pid, sig)
}

// close leaves what the command started in the background alone.
func (g childGroup) close() {}

// stopRunner asks a runner to stop its job.
func stopRunner(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// exitCodeOf is the command's exit code, 128+N when signal N ended it.
func exitCodeOf(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
