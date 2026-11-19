//go:build !linux && !windows

package server

import "testing"

func boundPort(t *testing.T) string {
	t.Skip("no bound-port helper on this OS")
	return ""
}

func backendRunning([]string, string) bool { return false }
