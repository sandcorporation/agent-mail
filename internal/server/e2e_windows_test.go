//go:build windows

package server

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

// e2eBackendFor is the ConPTY keeper up starts; the cleanup asks it to shut
// down, which ends every child it holds.
func e2eBackendFor(t *testing.T) e2eBackend {
	t.Helper()
	if err := conpty.CheckWindows(); err != nil {
		e2eSkip(t, "ConPTY unavailable: %v", err)
	}
	return e2eBackend{
		// No console signal reaches a process without one; terminate it.
		stop: func(c *exec.Cmd) { _ = c.Process.Kill() },
		end: func(instance string) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := conpty.NewClient(conpty.DialFor(instance))
			defer c.Close()
			if err := c.Shutdown(ctx, time.Second); err != nil {
				t.Logf("keeper shutdown: %v", err)
			}
		},
	}
}
