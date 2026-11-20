//go:build unix && !linux

package cli

import "os/exec"

// scopeCheck: no systemd scope holds the runner here.
func scopeCheck() error { return nil }

// startUnit: no user manager here; the runner is detached twice.
func startUnit(*exec.Cmd) (bool, error) { return false, nil }
