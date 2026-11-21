//go:build !windows

package node

import (
	"os/exec"
	"syscall"
)

// checkCommand is command under sh -c, in a process group of its own.
func checkCommand(command string) *exec.Cmd {
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// startCheck starts cmd; kill ends its process group, and release does the
// same for anything it left behind after it exited.
func startCheck(cmd *exec.Cmd) (kill, release func(), err error) {
	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}
	pgid := cmd.Process.Pid
	kill = func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
	return kill, kill, nil
}
