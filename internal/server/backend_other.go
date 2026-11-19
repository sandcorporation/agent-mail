//go:build !linux && !windows

package server

import (
	"fmt"
	"runtime"

	"github.com/sandcorporation/switchboard/internal/term"
)

type nodeBackend = term.Backend

// newNodeBackend refuses: terminals are hosted on Linux (tmux) and Windows
// (ConPTY) only.
func newNodeBackend(backendOptions) (nodeBackend, backendInfo, error) {
	return nil, backendInfo{}, fmt.Errorf("node: terminal hosting runs on Linux (tmux) and Windows (ConPTY); %s is not supported", runtime.GOOS)
}
