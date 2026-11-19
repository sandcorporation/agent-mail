package conpty

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
)

// Proc is one child running in a pseudo-console, as the keeper drives it.
// On Windows it is a ConPTY with the child spawned into it; tests use an
// in-memory program.
type Proc interface {
	// Read returns the pseudo-console's VT output. It returns io.EOF once
	// the child has exited and the last output is read.
	Read(p []byte) (int, error)
	// Write sends input as if typed.
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	PID() int
	// Wait blocks until the child exits and returns its exit code.
	Wait() (int, error)
	// Terminate asks the child to end, and kills it (and every process it
	// started) when it has not ended after grace. It does not wait.
	Terminate(grace time.Duration)
	// Close releases the pseudo-console after the child has exited.
	Close() error
}

// Spawner starts a child for a Spec in a pseudo-console of Spec.Cols x
// Spec.Rows.
type Spawner func(term.Spec) (Proc, error)

// KeeperOptions configures a Keeper.
type KeeperOptions struct {
	Spawn Spawner
	// History is the scrollback kept per session (<= 0: term.DefaultHistory).
	History int
	// Version is the switchboard version the keeper reports.
	Version string
	// Now is the clock for StartedAt (nil: time.Now).
	Now func() time.Time
	// Logf logs keeper events (nil: log.Printf).
	Logf func(format string, args ...any)
	// StreamBuffer is how many output chunks a stream may fall behind
	// before it is dropped (<= 0: 256).
	StreamBuffer int
}

// Keeper holds sessions and serves the pipe protocol. Every session's output
// is drained continuously into its Emulator and to its stream subscribers,
// whether or not a client is connected.
type Keeper struct {
	o        KeeperOptions
	mu       sync.Mutex
	sessions map[string]*session
	conns    map[net.Conn]struct{}
	closing  bool                      // Shutdown has begun: no new launches
	stopped  bool                      // listeners and connections closed
	lns      map[net.Listener]struct{} // listeners Serve is answering on
	done     chan struct{}             // closed by Shutdown, after stopped
	once     sync.Once
}

// DefaultGrace is how long shutdown gives children before killing them.
const DefaultGrace = 10 * time.Second

// drainWait bounds the wait for a dead child's last output.
const drainWait = 5 * time.Second

type session struct {
	name    string
	proc    Proc
	pid     int
	started time.Time
	drained chan struct{}
	dead    chan struct{}

	mu    sync.Mutex
	emu   *term.Emulator
	alive bool
	code  int
	subs  map[*subscriber]struct{}
}

type subscriber struct {
	ch  chan []byte
	why string // set when ch is closed
}

// NewKeeper returns a keeper with no sessions.
func NewKeeper(o KeeperOptions) *Keeper {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.StreamBuffer <= 0 {
		o.StreamBuffer = 256
	}
	return &Keeper{o: o, sessions: map[string]*session{}, conns: map[net.Conn]struct{}{}, lns: map[net.Listener]struct{}{}, done: make(chan struct{})}
}

// Done is closed when a client has asked the keeper to shut down, every
// child has ended and every listener Serve had taken before Shutdown, and
// every connection, has been closed, so a caller that has seen Done cannot
// connect through those listeners any more. A listener passed to Serve after
// Shutdown is not covered: that Serve closes it itself (see Serve).
func (k *Keeper) Done() <-chan struct{} { return k.done }

// Serve answers connections on l until Shutdown, which closes l. It returns
// nil once Shutdown has closed l. Serve called after Shutdown has begun
// closing listeners does not serve: it closes l and returns nil, and l is
// closed by the time it returns; Done may already be closed by then.
func (k *Keeper) Serve(l net.Listener) error {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		_ = l.Close()
		return nil
	}
	k.lns[l] = struct{}{}
	k.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			var ne net.Error
			k.mu.Lock()
			switch {
			case k.stopped:
				k.mu.Unlock()
				return nil
			case errors.As(err, &ne) && ne.Timeout():
				k.mu.Unlock()
				continue
			}
			k.mu.Unlock()
			return err // l stays registered: Shutdown closes it
		}
		go k.serveConn(c)
	}
}

// serveConn answers one connection's requests in order. A stream request
// turns the connection into that stream's output.
func (k *Keeper) serveConn(c net.Conn) {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		_ = c.Close()
		return
	}
	k.conns[c] = struct{}{}
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		delete(k.conns, c)
		k.mu.Unlock()
		_ = c.Close()
	}()
	r := bufio.NewReader(c)
	for {
		var req request
		if err := readJSON(r, &req); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				k.o.Logf("keeper: connection: %v", err)
			}
			return
		}
		if req.Op == OpStream {
			k.serveStream(c, r, req)
			return
		}
		resp := k.handle(req)
		resp.ID = req.ID
		if err := writeJSON(c, resp); err != nil {
			return
		}
		if req.Op == OpShutdown {
			k.Shutdown(time.Duration(req.GraceMS) * time.Millisecond)
			return
		}
	}
}

func (k *Keeper) handle(req request) response {
	var err error
	resp := response{}
	switch req.Op {
	case OpVersion:
		resp.Version, resp.Protocol, resp.PID = k.o.Version, Protocol, os.Getpid()
	case OpLaunch:
		if req.Spec == nil {
			err = errors.New("launch: no spec")
			break
		}
		err = k.Launch(*req.Spec)
	case OpList:
		resp.Sessions = k.List()
	case OpScreen:
		var g term.Grid
		if g, err = k.Screen(req.Name, req.History); err == nil {
			resp.Grid, err = encodeGrid(g)
		}
	case OpInput:
		err = k.Input(req.Name, req.Data)
	case OpResize:
		err = k.Resize(req.Name, req.Cols, req.Rows)
	case OpStop:
		err = k.Stop(req.Name, time.Duration(req.GraceMS)*time.Millisecond)
	case OpForget:
		err = k.Forget(req.Name)
	case OpShutdown:
		// Answered first; serveConn then shuts down.
	default:
		err = fmt.Errorf("unknown op %q", req.Op)
	}
	if err != nil {
		return errorResponse(req.ID, err)
	}
	return resp
}

func (k *Keeper) get(name string) (*session, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	s, ok := k.sessions[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, term.ErrNoSession)
	}
	return s, nil
}

func (k *Keeper) running(name string) (*session, error) {
	s, err := k.get(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	alive := s.alive
	s.mu.Unlock()
	if !alive {
		return nil, fmt.Errorf("%s: %w", name, term.ErrNotRunning)
	}
	return s, nil
}

// Launch starts a session.
func (k *Keeper) Launch(spec term.Spec) error {
	if spec.Name == "" || len(spec.Argv) == 0 {
		return errors.New("launch: name and argv are required")
	}
	if spec.Cols <= 0 || spec.Rows <= 0 {
		spec.Cols, spec.Rows = 80, 24
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closing {
		return errors.New("launch: keeper is shutting down")
	}
	if _, ok := k.sessions[spec.Name]; ok {
		return fmt.Errorf("launch %s: %w", spec.Name, term.ErrSessionExists)
	}
	p, err := k.o.Spawn(spec)
	if err != nil {
		return fmt.Errorf("launch %s: %w", spec.Name, err)
	}
	s := &session{
		name: spec.Name, proc: p, pid: p.PID(), started: k.o.Now(),
		drained: make(chan struct{}), dead: make(chan struct{}),
		emu: term.NewEmulator(spec.Cols, spec.Rows, k.o.History), alive: true,
		subs: map[*subscriber]struct{}{},
	}
	k.sessions[spec.Name] = s
	go s.drain()
	go s.wait(k.o.Logf)
	return nil
}

// drain copies the child's output into the emulator and the subscribers
// until the pseudo-console closes. A subscriber that cannot keep up is
// dropped; the child is never slowed.
func (s *session) drain() {
	defer close(s.drained)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.proc.Read(buf)
		if n > 0 {
			b := slices.Clone(buf[:n])
			s.mu.Lock()
			_, _ = s.emu.Write(b)
			for sub := range s.subs {
				select {
				case sub.ch <- b:
				default:
					s.endSub(sub, endDropped)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// wait records the exit once the last output is in, and ends the streams.
func (s *session) wait(logf func(string, ...any)) {
	code, err := s.proc.Wait()
	if err != nil {
		logf("keeper: %s: wait: %v", s.name, err)
	}
	select {
	case <-s.drained:
	case <-time.After(drainWait):
		logf("keeper: %s: output still open %s after exit", s.name, drainWait)
	}
	if err := s.proc.Close(); err != nil {
		logf("keeper: %s: close: %v", s.name, err)
	}
	s.mu.Lock()
	s.alive, s.code = false, code
	for sub := range s.subs {
		s.endSub(sub, endExited)
	}
	s.mu.Unlock()
	close(s.dead)
}

// endSub closes a subscriber; s.mu is held.
func (s *session) endSub(sub *subscriber, why string) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	sub.why = why
	close(sub.ch)
}

// List reports every session, dead ones included, by name.
func (k *Keeper) List() []term.Info {
	k.mu.Lock()
	all := make([]*session, 0, len(k.sessions))
	for _, s := range k.sessions {
		all = append(all, s)
	}
	k.mu.Unlock()
	out := make([]term.Info, 0, len(all))
	for _, s := range all {
		s.mu.Lock()
		out = append(out, term.Info{Name: s.name, PID: s.pid, Alive: s.alive, ExitCode: s.code, StartedAt: s.started})
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Screen renders a session's screen; a dead session keeps its last one.
func (k *Keeper) Screen(name string, history int) (term.Grid, error) {
	s, err := k.get(name)
	if err != nil {
		return term.Grid{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu.Grid(history), nil
}

// Input writes to a running session.
func (k *Keeper) Input(name string, b []byte) error {
	s, err := k.running(name)
	if err != nil {
		return err
	}
	if _, err := s.proc.Write(b); err != nil {
		return fmt.Errorf("input %s: %w", name, err)
	}
	return nil
}

// Resize changes a running session's size. The emulator follows at once;
// the pseudo-console's repaint arrives through the output.
func (k *Keeper) Resize(name string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("resize %s: bad size %dx%d", name, cols, rows)
	}
	s, err := k.running(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.proc.Resize(cols, rows); err != nil {
		return fmt.Errorf("resize %s: %w", name, err)
	}
	s.emu.Resize(cols, rows)
	return nil
}

// stopWait bounds Stop's wait for the child to die after it was killed.
const stopWait = 10 * time.Second

// Stop ends a session, killing its process tree after grace, and returns
// once it is dead. Stopping a dead session does nothing.
func (k *Keeper) Stop(name string, grace time.Duration) error {
	s, err := k.get(name)
	if err != nil {
		return err
	}
	select {
	case <-s.dead:
		return nil
	default:
	}
	s.proc.Terminate(grace)
	select {
	case <-s.dead:
		return nil
	case <-time.After(grace + stopWait):
		return fmt.Errorf("stop %s: still alive %s after grace", name, stopWait)
	}
}

// Forget drops a dead session. A live one cannot be forgotten.
func (k *Keeper) Forget(name string) error {
	s, err := k.get(name)
	if err != nil {
		return err
	}
	select {
	case <-s.dead:
	default:
		return fmt.Errorf("forget %s: session is alive", name)
	}
	k.mu.Lock()
	if k.sessions[name] == s {
		delete(k.sessions, name)
	}
	k.mu.Unlock()
	return s.emu.Close()
}

// Shutdown stops every session (grace each, concurrently; <= 0:
// DefaultGrace), refuses new launches, closes every listener and connection
// and then closes Done. It is idempotent.
func (k *Keeper) Shutdown(grace time.Duration) {
	if grace <= 0 {
		grace = DefaultGrace
	}
	k.once.Do(func() {
		k.mu.Lock()
		k.closing = true
		names := make([]string, 0, len(k.sessions))
		for n := range k.sessions {
			names = append(names, n)
		}
		k.mu.Unlock()
		var wg sync.WaitGroup
		for _, n := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := k.Stop(n, grace); err != nil {
					k.o.Logf("keeper: shutdown: %v", err)
				}
			}()
		}
		wg.Wait()
		k.mu.Lock()
		k.stopped = true
		lns, conns := k.lns, k.conns
		k.lns, k.conns = map[net.Listener]struct{}{}, map[net.Conn]struct{}{}
		k.mu.Unlock()
		for l := range lns {
			_ = l.Close()
		}
		for c := range conns {
			_ = c.Close()
		}
		close(k.done)
	})
}

// serveStream subscribes the connection to a session's output: a response,
// then output frames until the session exits, the subscriber falls behind
// or the client closes the connection.
func (k *Keeper) serveStream(c net.Conn, r *bufio.Reader, req request) {
	s, err := k.running(req.Name)
	if err != nil {
		_ = writeJSON(c, errorResponse(req.ID, err))
		return
	}
	sub := &subscriber{ch: make(chan []byte, k.o.StreamBuffer)}
	s.mu.Lock()
	if !s.alive {
		s.mu.Unlock()
		_ = writeJSON(c, errorResponse(req.ID, fmt.Errorf("%s: %w", req.Name, term.ErrNotRunning)))
		return
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	unsubscribe := func() {
		s.mu.Lock()
		s.endSub(sub, endClosed)
		s.mu.Unlock()
	}
	defer unsubscribe()
	if err := writeJSON(c, response{ID: req.ID}); err != nil {
		return
	}
	// The client sends nothing more; a read returning means it closed.
	go func() {
		_, _ = io.Copy(io.Discard, r)
		unsubscribe()
	}()
	for b := range sub.ch {
		if err := writeFrame(c, frameOutput, b); err != nil {
			return
		}
	}
	s.mu.Lock()
	why := sub.why
	s.mu.Unlock()
	if why != endClosed {
		_ = writeFrame(c, frameEnd, []byte(why))
	}
}
