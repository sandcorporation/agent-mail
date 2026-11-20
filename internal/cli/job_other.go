//go:build !unix

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// The detached runner is Linux and macOS only for now; Windows follows (#290).
var errNoRunner = errors.New("job run is not supported on this system yet")

var (
	stopSignals    = []os.Signal{os.Interrupt}
	ignoredSignals []os.Signal
)

func startDetached(*exec.Cmd) error      { return errNoRunner }
func childAttr() *syscall.SysProcAttr    { return nil }
func signalGroup(pid int, _ bool) error  { return killPID(pid) }
func stopRunner(pid int) error           { return killPID(pid) }
func exitCodeOf(ps *os.ProcessState) int { return ps.ExitCode() }

func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func pidAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
