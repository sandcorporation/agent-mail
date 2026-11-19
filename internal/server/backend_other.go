//go:build !linux

package server

import (
	"fmt"
	"runtime"

	"github.com/sandcorporation/switchboard/internal/term"
)

type nodeBackend = term.Backend

// newNodeBackend refuses: this release hosts terminals on Linux (tmux) only;
// Windows waits for the ConPTY backend.
func newNodeBackend(string, int, string) (nodeBackend, string, string, error) {
	return nil, "", "", fmt.Errorf("node: terminal hosting runs on Linux (tmux) in this release; %s is not supported yet", runtime.GOOS)
}
