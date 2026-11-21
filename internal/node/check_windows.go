//go:build windows

package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/sandcorporation/switchboard/internal/winjob"
)

// checkCommand is command under cmd /c, the line passed as written.
func checkCommand(command string) *exec.Cmd {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	cmd := exec.Command(shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /d /s /c "` + command + `"`, HideWindow: true}
	return cmd
}

// startCheck starts cmd inside a job object that kills everything in it
// when its handle closes; kill terminates the job, release closes it.
func startCheck(cmd *exec.Cmd) (kill, release func(), err error) {
	job, err := winjob.New(true, false)
	if err != nil {
		return nil, nil, err
	}
	if err = winjob.Start(cmd, job); err != nil {
		_ = windows.CloseHandle(job)
		return nil, nil, err
	}
	kill = func() { _ = windows.TerminateJobObject(job, 1) }
	release = func() { _ = windows.CloseHandle(job) }
	return kill, release, nil
}
