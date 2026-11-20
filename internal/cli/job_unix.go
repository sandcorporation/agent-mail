//go:build unix

package cli

import (
	"fmt"
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

// checkRunner refuses where the runner would stay in the caller's systemd
// scope (Linux, no user manager to start it outside); elsewhere a runner can
// always leave the caller.
func checkRunner() error { return scopeCheck() }

// startDetached starts cmd so that neither closing the terminal nor ending
// the caller's process tree reaches it: on Linux with a systemd user manager
// as a transient unit of its own (outside the terminal's scope); otherwise
// detached twice through sh — a session of its own, and not a child of the
// caller — with stdin from /dev/null. The runner writes its own PID file.
func startDetached(cmd *exec.Cmd) error {
	if done, err := startUnit(cmd); done || err != nil {
		return err
	}
	return detachTwice(cmd)
}

// detachTwice runs sh in a session of its own, which starts cmd in the
// background and exits at once: the runner's parent is then init (or the
// nearest subreaper), never the caller.
func detachTwice(cmd *exec.Cmd) error {
	sh := exec.Command("/bin/sh", append([]string{"-c", `"$@" </dev/null &`, "sh", cmd.Path}, cmd.Args[1:]...)...)
	sh.Dir, sh.Env, sh.Stdout, sh.Stderr = cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sh.Run(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	return nil
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
