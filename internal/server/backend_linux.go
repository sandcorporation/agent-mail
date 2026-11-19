//go:build linux

package server

import (
	"context"
	"log"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/tmux"
)

type nodeBackend = term.Backend

// newNodeBackend is the node's terminal backend on Linux: tmux (3.2+) with
// its own server "switchboard-<instance>". o.dir holds its config file.
func newNodeBackend(o backendOptions) (nodeBackend, backendInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := tmux.New(ctx, tmux.Options{Instance: o.instance, History: o.history, Dir: o.dir})
	if err != nil {
		return nil, backendInfo{}, err
	}
	// Like the keeper's line on Windows: a server still holding sessions
	// (up ended, tmux kept running) is adopted with them.
	how := "started on first launch"
	if infos, err := b.List(ctx); err == nil && len(infos) > 0 {
		how = "adopted"
	}
	log.Printf("node: tmux server %s (%s)", b.Socket(), how)
	return b, backendInfo{name: "tmux", socket: b.Socket()}, nil
}
