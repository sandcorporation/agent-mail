//go:build !linux && !windows

package server

import (
	"runtime"
	"testing"
)

// e2eBackendFor: hosting has no backend here (no release build either).
func e2eBackendFor(t *testing.T) e2eBackend {
	t.Helper()
	e2eSkip(t, "no terminal backend on %s", runtime.GOOS)
	return e2eBackend{}
}
