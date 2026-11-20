//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/sandcorporation/switchboard/internal/winjob"
)

// The runner has no terminal to signal it: a stop reaches it through the
// server record (pollJob), so it listens for nothing.
var (
	stopSignals    = []os.Signal{os.Interrupt}
	ignoredSignals []os.Signal
)

// runnerCopies: Windows locks a running executable, so the runner runs from
// a copy (RunnerExecutable) and the installed binary can be replaced.
const runnerCopies = true

// checkRunner refuses before anything is registered when the terminal's
// job object would keep the runner (and end it with the terminal).
func checkRunner() error {
	if err := winjob.CheckBreakaway(); err != nil {
		if errors.Is(err, winjob.ErrNoBreakaway) {
			return errNoBreakaway
		}
		return fmt.Errorf("job object: %w", err)
	}
	return nil
}

// startDetached starts cmd outside the caller's job object, on a hidden
// console of its own (closing the caller's console does not reach it; the
// runner sends CTRL_BREAK on it to stop the command) in a process group of
// its own. A refused breakaway is errNoBreakaway: the runner is not started.
func startDetached(cmd *exec.Cmd) error {
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
		HideWindow:    true,
	}
	err := cmd.Start()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("%w: %v", errNoBreakaway, err)
	}
	return err
}

// childGroup is the job's command in a job object of its own (killed when
// the runner lets go of it) and a console process group of its own.
type childGroup struct {
	pid int
	job windows.Handle
}

// startChild starts cmd inside its job before it runs, so nothing it starts
// escapes the stop.
func startChild(cmd *exec.Cmd) (childGroup, error) {
	job, err := winjob.New(true, false)
	if err != nil {
		return childGroup{}, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err = winjob.Start(cmd, job); err != nil {
		_ = windows.CloseHandle(job)
		return childGroup{}, err
	}
	return childGroup{pid: cmd.Process.Pid, job: job}, nil
}

// stop sends CTRL_BREAK to the command's process group (a console program
// ends on it); kill, or a group no console event reaches, ends the whole
// job: the command and every process it started.
func (g childGroup) stop(kill bool) error {
	if !kill && windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(g.pid)) == nil {
		return nil
	}
	return windows.TerminateJobObject(g.job, 1)
}

// close ends what the command left running: its job is kill-on-close.
func (g childGroup) close() { _ = windows.CloseHandle(g.job) }

// stopRunner: a runner on Windows reads the stop `job stop` recorded on the
// server within jobPoll; there is no signal to send it.
func stopRunner(int) error { return nil }

func pidAlive(pid int) bool { return winjob.Alive(pid) }

func exitCodeOf(ps *os.ProcessState) int { return ps.ExitCode() }
