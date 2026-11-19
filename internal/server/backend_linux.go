//go:build linux

package server

import (
	"context"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/tmux"
)

type nodeBackend = term.Backend

// newNodeBackend is the node's terminal backend on Linux: tmux (3.2+) with
// its own server "switchboard-<instance>". dir holds its config file (empty:
// the user cache directory).
func newNodeBackend(instance string, history int, dir string) (nodeBackend, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := tmux.New(ctx, tmux.Options{Instance: instance, History: history, Dir: dir})
	if err != nil {
		return nil, "", "", err
	}
	return b, "tmux", b.Socket(), nil
}
