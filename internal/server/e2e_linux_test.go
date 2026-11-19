//go:build linux

package server

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/sandcorporation/switchboard/internal/term/tmux"
)

// e2eBackendFor is tmux: its server gets a short socket directory of its
// own (TMUX_TMPDIR), and the cleanup kills that server.
func e2eBackendFor(t *testing.T) e2eBackend {
	t.Helper()
	if _, err := tmux.Check(context.Background(), ""); err != nil {
		if os.Getenv("SWITCHBOARD_TEST_TMUX") == "require" {
			t.Fatalf("SWITCHBOARD_TEST_TMUX=require: %v", err)
		}
		e2eSkip(t, "tmux unavailable: %v", err)
	}
	d, err := os.MkdirTemp("", "sbtmux")
	if err != nil {
		t.Fatal(err)
	}
	return e2eBackend{
		env:  []string{"TMUX_TMPDIR=" + d},
		stop: func(c *exec.Cmd) { _ = c.Process.Signal(syscall.SIGTERM) },
		end: func(instance string) {
			kill := exec.Command("tmux", "-L", "switchboard-"+instance, "kill-server")
			kill.Env = append(os.Environ(), "TMUX_TMPDIR="+d)
			_ = kill.Run()
			_ = os.RemoveAll(d)
		},
	}
}
