//go:build linux

package server

import (
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

// boundPort holds a loopback port bound but not listening, the way the
// program that held 8090 on the first Windows run did (#217): nothing lists
// it as a listener, and every bind of it fails until the test ends.
func boundPort(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(sa.(*syscall.SockaddrInet4).Port)
}

// backendRunning reports whether the tmux server of instance runs (env
// carries the backend's TMUX_TMPDIR).
func backendRunning(env []string, instance string) bool {
	c := exec.Command("tmux", "-L", "switchboard-"+instance, "list-sessions")
	c.Env = env
	return c.Run() == nil
}
