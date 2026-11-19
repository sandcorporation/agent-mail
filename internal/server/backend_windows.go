//go:build windows

package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

type nodeBackend = term.Backend

// newNodeBackend is the node's terminal backend on Windows: ConPTY sessions
// in the keeper process (switchboard __keeper) of this instance, started
// when none runs. A running keeper of this version is adopted with its
// sessions; one of another version is replaced, and the node relaunches its
// sessions with --resume. Windows older than 10 1809 is refused.
func newNodeBackend(o backendOptions) (nodeBackend, backendInfo, error) {
	if err := conpty.CheckWindows(); err != nil {
		return nil, backendInfo{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, backendInfo{}, fmt.Errorf("node: keeper: %w", err)
	}
	dir := o.dir
	if dir == "" {
		if dir, err = os.UserCacheDir(); err != nil {
			return nil, backendInfo{}, fmt.Errorf("node: keeper: %w", err)
		}
		dir = filepath.Join(dir, "switchboard")
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, backendInfo{}, fmt.Errorf("node: keeper: %w", err)
		}
	}
	run := conpty.RunOptions{Instance: o.instance, History: o.history, LogFile: filepath.Join(dir, "keeper-"+o.instance+".log")}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, how, err := conpty.Connect(ctx, conpty.ConnectOptions{
		Dial:    conpty.DialFor(o.instance),
		Version: o.version,
		Start:   func() error { return conpty.Start(exe, run) },
	})
	if err != nil {
		return nil, backendInfo{}, fmt.Errorf("node: keeper: %w", err)
	}
	log.Printf("node: keeper %s (%s)", conpty.PipeName(o.instance), how)
	return c, backendInfo{name: "conpty", socket: conpty.PipeName(o.instance), term: "xterm-256color"}, nil
}

// Keeper is the hidden `switchboard __keeper` command: the process holding
// this instance's pseudo-consoles until a node asks it to shut down.
func Keeper(v string, args []string) int {
	o, err := conpty.ParseRunArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if err := conpty.Run(o, v); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
