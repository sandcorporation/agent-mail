package node

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Relay limits.
const (
	// RelayHistory is how many scrollback lines a snapshot carries.
	RelayHistory = 2000
	// relayRetry is how often a watched terminal that is not running is
	// tried again, so a relaunch is picked up.
	relayRetry = time.Second
	relayChunk = 32 << 10
)

// relay streams the terminals the host attached (a browser viewer is open)
// over one node channel connection: a snapshot first, then output. Nothing
// is read from the backend for a terminal nobody watches.
type relay struct {
	backend term.Backend
	owns    func(id string) bool
	send    func(kind byte, id string, b []byte) error
	logf    func(format string, a ...any)

	mu      sync.Mutex
	watches map[string]*watch
}

type watch struct {
	cancel context.CancelFunc
	snap   chan struct{} // send a fresh snapshot
}

func newRelay(backend term.Backend, owns func(string) bool, send func(byte, string, []byte) error, logf func(string, ...any)) *relay {
	return &relay{backend: backend, owns: owns, send: send, logf: logf, watches: map[string]*watch{}}
}

// attach starts streaming id, or sends a fresh snapshot when it already
// streams (another viewer joined).
func (r *relay) attach(ctx context.Context, id string) {
	if !r.owns(id) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.watches[id]; ok {
		signal(w.snap)
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &watch{cancel: cancel, snap: make(chan struct{}, 1)}
	r.watches[id] = w
	go r.run(wctx, id, w)
}

// detach stops streaming id.
func (r *relay) detach(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.watches[id]; ok {
		w.cancel()
		delete(r.watches, id)
	}
}

// stop ends every stream (the connection is gone).
func (r *relay) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.watches {
		w.cancel()
		delete(r.watches, id)
	}
}

// snapshot sends the terminal's current screen and history. A terminal that
// does not exist (yet) sends nothing.
func (r *relay) snapshot(ctx context.Context, id string) error {
	g, err := r.backend.Screen(ctx, TerminalName(id), RelayHistory)
	if errors.Is(err, term.ErrNoSession) {
		return nil
	}
	if err != nil {
		if ctx.Err() == nil {
			r.logf("node: screen %s: %v", id, err)
		}
		return nil
	}
	return r.send(wire.TermSnapshot, id, g.Snapshot())
}

// run streams one terminal until ctx is done: while the session runs, its
// output follows a snapshot taken after the stream opened; when it ends, a
// last snapshot carries what the stream missed, and the stream is opened
// again once the session runs again.
func (r *relay) run(ctx context.Context, id string, w *watch) {
	name := TerminalName(id)
	tick := time.NewTicker(relayRetry)
	defer tick.Stop()
	for first := true; ctx.Err() == nil; first = false {
		st, err := r.backend.Stream(ctx, name)
		if err == nil {
			if !r.follow(ctx, id, w, st) {
				return
			}
			continue
		}
		// Not running: a viewer still gets the last screen.
		if first && r.snapshot(ctx, id) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-w.snap:
			if r.snapshot(ctx, id) != nil {
				return
			}
		case <-tick.C:
		}
	}
}

// follow forwards one open stream; false means the channel is gone.
func (r *relay) follow(ctx context.Context, id string, w *watch, st io.ReadCloser) bool {
	defer st.Close()
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, relayChunk)
			n, err := st.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// The snapshot is taken after the stream opened, so nothing printed in
	// between is lost.
	if r.snapshot(ctx, id) != nil {
		return false
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-w.snap:
			// Output already read was printed before the screen is
			// captured; the snapshot covers it.
			for drained := false; !drained; {
				select {
				case _, ok := <-chunks:
					if !ok {
						drained = true
					}
				default:
					drained = true
				}
			}
			if r.snapshot(ctx, id) != nil {
				return false
			}
		case b, ok := <-chunks:
			if !ok {
				// The session ended. Its last output may not have reached
				// the stream (tmux), but the screen keeps it.
				return r.snapshot(ctx, id) == nil
			}
			if r.send(wire.TermOutput, id, b) != nil {
				return false
			}
		}
	}
}
