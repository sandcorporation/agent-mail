// Package termtest is an in-memory term.Backend for tests of everything above
// the terminal backend. Tests script what a session prints (Output, or a
// Responder for input), read what was typed (Input), end sessions with chosen
// exit codes (Exit) and inspect launches, resizes and stops. Sessions live in
// the Fake, not in its users: two node instances sharing one Fake see the
// same sessions, which is how a node restart is simulated.
package termtest

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
)

// StopExitCode is the exit code a session stopped through Stop reports
// (128 + SIGTERM, as a shell reports it).
const StopExitCode = 143

// Size is a terminal size.
type Size struct{ Cols, Rows int }

// Stop records one Stop call.
type Stop struct {
	Name  string
	Grace time.Duration
}

// Responder answers input the way a scripted program would: it gets the
// typed bytes and returns what the program prints back (nil for nothing).
type Responder func(name string, input []byte) []byte

// Fake is an in-memory term.Backend. The zero value is not usable; use New.
type Fake struct {
	mu        sync.Mutex
	now       func() time.Time
	nextPID   int
	sessions  map[string]*session
	launches  []term.Spec
	stops     []Stop
	responder Responder
}

type session struct {
	spec     term.Spec
	pid      int
	started  time.Time
	alive    bool
	exitCode int
	emu      *term.Emulator
	input    []byte
	resizes  []Size
	streams  map[*stream]struct{}
}

var _ term.Backend = (*Fake)(nil)

// New returns an empty fake using time.Now for start times.
func New() *Fake {
	return &Fake{now: time.Now, nextPID: 1000, sessions: map[string]*session{}}
}

// SetClock replaces the clock used for start times.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// SetResponder installs r for every later Input; nil removes it.
func (f *Fake) SetResponder(r Responder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responder = r
}

// Launch implements term.Backend.
func (f *Fake) Launch(_ context.Context, s term.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[s.Name]; ok {
		return fmt.Errorf("launch %s: %w", s.Name, term.ErrSessionExists)
	}
	f.nextPID++
	s.Argv = slices.Clone(s.Argv)
	s.Env = slices.Clone(s.Env)
	f.sessions[s.Name] = &session{
		spec:    s,
		pid:     f.nextPID,
		started: f.now(),
		alive:   true,
		emu:     term.NewEmulator(s.Cols, s.Rows, 0),
		streams: map[*stream]struct{}{},
	}
	f.launches = append(f.launches, s)
	return nil
}

// List implements term.Backend; sessions come sorted by name.
func (f *Fake) List(context.Context) ([]term.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]term.Info, 0, len(f.sessions))
	for name, s := range f.sessions {
		out = append(out, term.Info{Name: name, PID: s.pid, Alive: s.alive, ExitCode: s.exitCode, StartedAt: s.started})
	}
	slices.SortFunc(out, func(a, b term.Info) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return out, nil
}

// Screen implements term.Backend.
func (f *Fake) Screen(_ context.Context, name string, history int) (term.Grid, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.get(name)
	if err != nil {
		return term.Grid{}, err
	}
	return s.emu.Grid(history), nil
}

// Stream implements term.Backend.
func (f *Fake) Stream(ctx context.Context, name string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.running(name)
	if err != nil {
		return nil, err
	}
	st := newStream()
	st.onClose = func() {
		f.mu.Lock()
		delete(s.streams, st)
		f.mu.Unlock()
	}
	s.streams[st] = struct{}{}
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	st.mu.Lock()
	st.stopWatch = stop
	st.mu.Unlock()
	return st, nil
}

// Input implements term.Backend: the bytes are recorded, and a Responder's
// answer is printed by the session.
func (f *Fake) Input(_ context.Context, name string, b []byte) error {
	f.mu.Lock()
	s, err := f.running(name)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	s.input = append(s.input, b...)
	r := f.responder
	f.mu.Unlock()
	if r == nil {
		return nil
	}
	if out := r(name, slices.Clone(b)); out != nil {
		return f.Output(name, out)
	}
	return nil
}

// Resize implements term.Backend.
func (f *Fake) Resize(_ context.Context, name string, cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.running(name)
	if err != nil {
		return err
	}
	s.emu.Resize(cols, rows)
	s.resizes = append(s.resizes, Size{cols, rows})
	return nil
}

// Stop implements term.Backend: the session exits with StopExitCode at once.
func (f *Fake) Stop(_ context.Context, name string, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.get(name)
	if err != nil {
		return err
	}
	f.stops = append(f.stops, Stop{Name: name, Grace: grace})
	if s.alive {
		s.exit(StopExitCode)
	}
	return nil
}

// Forget implements term.Backend. A live session cannot be forgotten.
func (f *Fake) Forget(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.get(name)
	if err != nil {
		return err
	}
	if s.alive {
		return fmt.Errorf("forget %s: session is alive", name)
	}
	_ = s.emu.Close()
	delete(f.sessions, name)
	return nil
}

// Output makes the session print b: it reaches the screen and every open
// stream.
func (f *Fake) Output(name string, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.running(name)
	if err != nil {
		return err
	}
	if _, err := s.emu.Write(b); err != nil {
		return err
	}
	for st := range s.streams {
		st.write(b)
	}
	return nil
}

// Exit ends the session's program with code; its screen stays until Forget
// and its streams end with io.EOF.
func (f *Fake) Exit(name string, code int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.running(name)
	if err != nil {
		return err
	}
	s.exit(code)
	return nil
}

// Typed returns everything Input delivered to the session so far.
func (f *Fake) Typed(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[name]; ok {
		return slices.Clone(s.input)
	}
	return nil
}

// Resizes returns the sizes the session was resized to, in order.
func (f *Fake) Resizes(name string) []Size {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[name]; ok {
		return slices.Clone(s.resizes)
	}
	return nil
}

// Launches returns every launched Spec, in order, forgotten sessions included.
func (f *Fake) Launches() []term.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.launches)
}

// Stops returns every Stop call, in order.
func (f *Fake) Stops() []Stop {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stops)
}

// Streams reports how many streams of the session are open.
func (f *Fake) Streams(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[name]; ok {
		return len(s.streams)
	}
	return 0
}

func (f *Fake) get(name string) (*session, error) {
	s, ok := f.sessions[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, term.ErrNoSession)
	}
	return s, nil
}

func (f *Fake) running(name string) (*session, error) {
	s, err := f.get(name)
	if err != nil {
		return nil, err
	}
	if !s.alive {
		return nil, fmt.Errorf("%s: %w", name, term.ErrNotRunning)
	}
	return s, nil
}

// exit runs with f.mu held. Streams end without removing themselves here:
// their onClose takes f.mu, so they are dropped from the map directly.
func (s *session) exit(code int) {
	s.alive = false
	s.exitCode = code
	for st := range s.streams {
		st.end()
		delete(s.streams, st)
	}
}

// stream is an unbounded byte queue: writers never block, Read blocks until
// data, end (io.EOF after the data) or Close.
type stream struct {
	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	ended   bool
	closed  bool
	once    sync.Once
	onClose func()
	// stopWatch detaches the context watch; set under mu after creation.
	stopWatch func() bool
}

func newStream() *stream {
	st := &stream{}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *stream) write(b []byte) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ended || st.closed {
		return
	}
	st.buf = append(st.buf, b...)
	st.cond.Broadcast()
}

func (st *stream) end() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ended = true
	st.cond.Broadcast()
}

func (st *stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for len(st.buf) == 0 && !st.ended && !st.closed {
		st.cond.Wait()
	}
	if st.closed {
		return 0, io.ErrClosedPipe
	}
	if len(st.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, st.buf)
	st.buf = st.buf[n:]
	return n, nil
}

// Close stops the stream; a blocked Read returns io.ErrClosedPipe.
func (st *stream) Close() error {
	st.once.Do(func() {
		st.mu.Lock()
		stop := st.stopWatch
		st.mu.Unlock()
		if stop != nil {
			stop()
		}
		// Leave the session first, so a reader woken below already sees the
		// stream gone.
		st.onClose()
		st.mu.Lock()
		st.closed = true
		st.cond.Broadcast()
		st.mu.Unlock()
	})
	return nil
}
