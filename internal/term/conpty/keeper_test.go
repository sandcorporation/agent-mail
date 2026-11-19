package conpty_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

// fakeProc is a child in an in-memory pseudo-console. By default it is a
// line shell: "exit N" exits with N, anything else is echoed as
// "echo <line>".
type fakeProc struct {
	pid      int
	outR     *io.PipeReader
	outW     *io.PipeWriter
	exited   chan struct{}
	once     sync.Once
	code     int
	stubborn bool // ignores Terminate's request; killed after grace
	onInput  func(p *fakeProc, b []byte)

	mu    sync.Mutex
	line  []byte
	sizes [][2]int
}

func (p *fakeProc) Read(b []byte) (int, error) { return p.outR.Read(b) }

func (p *fakeProc) Write(b []byte) (int, error) {
	if p.onInput != nil {
		p.onInput(p, b)
		return len(b), nil
	}
	for _, c := range b {
		if c != '\r' {
			p.mu.Lock()
			p.line = append(p.line, c)
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		line := string(p.line)
		p.line = nil
		p.mu.Unlock()
		var n int
		if _, err := fmt.Sscanf(line, "exit %d", &n); err == nil {
			p.exit(n)
			continue
		}
		p.print("echo " + line + "\r\n")
	}
	return len(b), nil
}

func (p *fakeProc) print(s string) { _, _ = p.outW.Write([]byte(s)) }

func (p *fakeProc) exit(code int) {
	p.once.Do(func() {
		p.code = code
		_ = p.outW.Close()
		close(p.exited)
	})
}

func (p *fakeProc) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes = append(p.sizes, [2]int{cols, rows})
	return nil
}

func (p *fakeProc) PID() int { return p.pid }

func (p *fakeProc) Wait() (int, error) {
	<-p.exited
	return p.code, nil
}

func (p *fakeProc) Terminate(grace time.Duration) {
	if !p.stubborn {
		p.exit(143)
		return
	}
	time.AfterFunc(grace, func() { p.exit(137) })
}

func (p *fakeProc) Close() error { return nil }

// spawner hands out fakeProcs and remembers them by session name.
type spawner struct {
	mu      sync.Mutex
	next    int
	procs   map[string]*fakeProc
	specs   []term.Spec
	prepare func(*fakeProc, term.Spec)
}

func newSpawner() *spawner { return &spawner{next: 1000, procs: map[string]*fakeProc{}} }

func (s *spawner) spawn(spec term.Spec) (conpty.Proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if spec.Argv[0] == "missing" {
		return nil, errors.New("program not found")
	}
	s.next++
	r, w := io.Pipe()
	p := &fakeProc{pid: s.next, outR: r, outW: w, exited: make(chan struct{})}
	if s.prepare != nil {
		s.prepare(p, spec)
	}
	s.procs[spec.Name] = p
	s.specs = append(s.specs, spec)
	return p, nil
}

func (s *spawner) proc(name string) *fakeProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[name]
}

// keeperRig is a keeper serving on a loopback socket, as the Windows keeper
// serves its pipe.
type keeperRig struct {
	t       *testing.T
	sp      *spawner
	keeper  *conpty.Keeper
	addr    string
	version string
}

func newKeeper(t *testing.T, version string) *keeperRig {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &keeperRig{t: t, sp: newSpawner(), addr: l.Addr().String(), version: version}
	r.keeper = conpty.NewKeeper(conpty.KeeperOptions{Spawn: r.sp.spawn, Version: version, Logf: t.Logf})
	go func() { _ = r.keeper.Serve(l) }()
	t.Cleanup(func() { r.keeper.Shutdown(time.Second) })
	return r
}

func (r *keeperRig) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", r.addr)
}

func (r *keeperRig) client() *conpty.Client {
	c := conpty.NewClient(r.dial)
	r.t.Cleanup(func() { _ = c.Close() })
	return c
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func eventually(t *testing.T, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, state := f()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out; last state:\n%s", what, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func screenText(t *testing.T, b term.Backend, name string) string {
	t.Helper()
	g, err := b.Screen(ctx(t), name, -1)
	if err != nil {
		t.Fatal(err)
	}
	return g.Dump()
}

func info(t *testing.T, b term.Backend, name string) (term.Info, bool) {
	t.Helper()
	infos, err := b.List(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range infos {
		if i.Name == name {
			return i, true
		}
	}
	return term.Info{}, false
}

func shell(name string) term.Spec {
	return term.Spec{Name: name, Argv: []string{"agent", "--session-id", "x"}, Dir: "/work", Env: []string{"A=1"}, Cols: 20, Rows: 4}
}

func TestClientDrivesKeeperSessions(t *testing.T) {
	r := newKeeper(t, "v1")
	c := r.client()
	if err := c.Launch(ctx(t), shell("sb-a")); err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(ctx(t), shell("sb-a")); !errors.Is(err, term.ErrSessionExists) {
		t.Fatalf("second launch: %v", err)
	}
	if err := c.Launch(ctx(t), term.Spec{Name: "sb-m", Argv: []string{"missing"}}); err == nil || !strings.Contains(err.Error(), "program not found") {
		t.Fatalf("launch of a missing program: %v", err)
	}
	if got := r.sp.specs[0]; got.Dir != "/work" || got.Env[0] != "A=1" || got.Cols != 20 || got.Rows != 4 {
		t.Fatalf("spawned spec %+v", got)
	}
	in, ok := info(t, c, "sb-a")
	if !ok || !in.Alive || in.PID != r.sp.proc("sb-a").pid || in.StartedAt.IsZero() {
		t.Fatalf("list %+v %v", in, ok)
	}

	st, err := c.Stream(ctx(t), "sb-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Input(ctx(t), "sb-a", []byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "echo on screen", func() (bool, string) {
		s := screenText(t, c, "sb-a")
		return strings.Contains(s, "echo hello"), s
	})
	if err := c.Resize(ctx(t), "sb-a", 30, 5); err != nil {
		t.Fatal(err)
	}
	if g, _ := c.Screen(ctx(t), "sb-a", 0); g.Cols != 30 || g.Rows != 5 {
		t.Fatalf("screen after resize %dx%d", g.Cols, g.Rows)
	}
	if got := r.sp.proc("sb-a").sizes; len(got) != 1 || got[0] != [2]int{30, 5} {
		t.Fatalf("console resizes %v", got)
	}
	if err := c.Forget(ctx(t), "sb-a"); err == nil {
		t.Fatal("forgot a live session")
	}

	if err := c.Input(ctx(t), "sb-a", []byte("exit 3\r")); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("stream ends with %v", err)
	}
	if string(out) != "echo hello\r\n" {
		t.Fatalf("stream %q", out)
	}
	eventually(t, "exit", func() (bool, string) {
		in, _ := info(t, c, "sb-a")
		return !in.Alive, fmt.Sprintf("%+v", in)
	})
	if in, _ := info(t, c, "sb-a"); in.ExitCode != 3 {
		t.Fatalf("exit code %d", in.ExitCode)
	}
	if s := screenText(t, c, "sb-a"); !strings.Contains(s, "echo hello") {
		t.Fatalf("dead session lost its screen:\n%s", s)
	}
	if err := c.Input(ctx(t), "sb-a", []byte("x")); !errors.Is(err, term.ErrNotRunning) {
		t.Fatalf("input to a dead session: %v", err)
	}
	if _, err := c.Stream(ctx(t), "sb-a"); !errors.Is(err, term.ErrNotRunning) {
		t.Fatalf("stream of a dead session: %v", err)
	}
	if err := c.Stop(ctx(t), "sb-a", time.Second); err != nil {
		t.Fatalf("stop of a dead session: %v", err)
	}
	if err := c.Forget(ctx(t), "sb-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Screen(ctx(t), "sb-a", 0); !errors.Is(err, term.ErrNoSession) {
		t.Fatalf("screen after forget: %v", err)
	}
	if err := c.Launch(ctx(t), shell("sb-a")); err != nil {
		t.Fatalf("relaunch after forget: %v", err)
	}
}

func TestStopKillsAfterGrace(t *testing.T) {
	r := newKeeper(t, "v1")
	c := r.client()
	r.sp.prepare = func(p *fakeProc, s term.Spec) { p.stubborn = s.Name == "sb-stubborn" }
	for _, n := range []string{"sb-polite", "sb-stubborn"} {
		if err := c.Launch(ctx(t), shell(n)); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := c.Stop(ctx(t), "sb-polite", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("polite stop took %s", d)
	}
	start = time.Now()
	if err := c.Stop(ctx(t), "sb-stubborn", 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("stubborn stop returned after %s, before grace", d)
	}
	for n, want := range map[string]int{"sb-polite": 143, "sb-stubborn": 137} {
		if in, _ := info(t, c, n); in.Alive || in.ExitCode != want {
			t.Fatalf("%s after stop: %+v", n, in)
		}
	}
}

// TestSessionsOutliveTheClient is the node restart: a second client (a new
// node process) finds the same live session with its screen.
func TestSessionsOutliveTheClient(t *testing.T) {
	r := newKeeper(t, "v1")
	c1 := conpty.NewClient(r.dial)
	if err := c1.Launch(ctx(t), shell("sb-a")); err != nil {
		t.Fatal(err)
	}
	if err := c1.Input(ctx(t), "sb-a", []byte("before\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "echo", func() (bool, string) {
		s := screenText(t, c1, "sb-a")
		return strings.Contains(s, "echo before"), s
	})
	// Output while no client is connected still reaches the screen.
	_ = c1.Close()
	r.sp.proc("sb-a").print("while away\r\n")

	c2 := r.client()
	in, ok := info(t, c2, "sb-a")
	if !ok || !in.Alive || in.PID != r.sp.proc("sb-a").pid {
		t.Fatalf("after reconnect: %+v %v", in, ok)
	}
	eventually(t, "output while away", func() (bool, string) {
		s := screenText(t, c2, "sb-a")
		return strings.Contains(s, "echo before") && strings.Contains(s, "while away"), s
	})
	if n := len(r.sp.specs); n != 1 {
		t.Fatalf("%d launches", n)
	}
}

func TestClientRedialsAReplacedKeeper(t *testing.T) {
	r := newKeeper(t, "v1")
	c := r.client()
	if _, err := c.Version(ctx(t)); err != nil {
		t.Fatal(err)
	}
	// The keeper drops the connection (it was replaced, or restarted).
	r.keeper.Shutdown(time.Second)
	r2 := newKeeper(t, "v1")
	r.addr = r2.addr
	if _, err := c.List(ctx(t)); err != nil {
		t.Fatalf("list after the keeper went: %v", err)
	}
}

// pipeListener serves synchronous in-memory connections: nothing buffers
// between the keeper and a client that does not read.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "pipe"} }

func (l *pipeListener) dial(context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.conns <- a:
		return b, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func TestSlowStreamIsDroppedNotTheChild(t *testing.T) {
	l := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	r := &keeperRig{t: t, sp: newSpawner()}
	r.keeper = conpty.NewKeeper(conpty.KeeperOptions{Spawn: r.sp.spawn, Logf: t.Logf, StreamBuffer: 4})
	go func() { _ = r.keeper.Serve(l) }()
	t.Cleanup(func() { r.keeper.Shutdown(time.Second) })
	c := conpty.NewClient(l.dial)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Launch(ctx(t), shell("sb-a")); err != nil {
		t.Fatal(err)
	}
	st, err := c.Stream(ctx(t), "sb-a")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Nobody reads the stream while the child prints far more than the
	// socket and the subscriber buffer hold; the child never blocks.
	p := r.sp.proc("sb-a")
	line := strings.Repeat("x", 100) + "\r\n"
	done := make(chan struct{})
	go func() {
		for range 200 {
			p.print(line)
		}
		p.print("last\r\n")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the child blocked on a slow stream")
	}
	eventually(t, "screen keeps up", func() (bool, string) {
		s := screenText(t, c, "sb-a")
		return strings.Contains(s, "last"), s
	})
	_, err = io.Copy(io.Discard, st)
	if !errors.Is(err, conpty.ErrDropped) {
		t.Fatalf("slow stream ends with %v", err)
	}
}

func TestStreamCloseAndContext(t *testing.T) {
	r := newKeeper(t, "v1")
	c := r.client()
	if err := c.Launch(ctx(t), shell("sb-a")); err != nil {
		t.Fatal(err)
	}
	st, err := c.Stream(ctx(t), "sb-a")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := st.Read(make([]byte, 10)); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	_ = st.Close()
	if err := <-errc; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("read after close: %v", err)
	}

	sctx, cancel := context.WithCancel(context.Background())
	st, err = c.Stream(sctx, "sb-a")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, err := st.Read(make([]byte, 10)); errc <- err }()
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("read after cancel returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the stream")
	}
	// Closed streams unsubscribe: output still flows to the screen.
	if err := c.Input(ctx(t), "sb-a", []byte("after\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "echo", func() (bool, string) {
		s := screenText(t, c, "sb-a")
		return strings.Contains(s, "echo after"), s
	})
}

func TestShutdownStopsEveryChild(t *testing.T) {
	r := newKeeper(t, "v1")
	c := r.client()
	for _, n := range []string{"sb-a", "sb-b"} {
		if err := c.Launch(ctx(t), shell(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Shutdown(ctx(t), time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.keeper.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("keeper did not finish shutting down")
	}
	for _, n := range []string{"sb-a", "sb-b"} {
		select {
		case <-r.sp.proc(n).exited:
		default:
			t.Fatalf("%s still running after shutdown", n)
		}
	}
	if _, err := r.dial(ctx(t)); err == nil {
		t.Fatal("keeper still accepts connections after shutdown")
	}
	if err := r.keeper.Launch(shell("sb-c")); err == nil {
		t.Fatal("launch accepted while shut down")
	}
}

// slowCloseListener takes a while to close, so a keeper that closes Done
// before its listener is caught by the dial right after Done.
type slowCloseListener struct {
	net.Listener
	mu     sync.Mutex
	closed bool
}

func (l *slowCloseListener) Close() error {
	time.Sleep(50 * time.Millisecond)
	err := l.Listener.Close()
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return err
}

func (l *slowCloseListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// TestDoneMeansListenerClosed: Done is closed only after the listener is,
// so a caller that waits on Done cannot connect any more (#227).
func TestDoneMeansListenerClosed(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &slowCloseListener{Listener: inner}
	k := conpty.NewKeeper(conpty.KeeperOptions{Spawn: newSpawner().spawn, Logf: t.Logf})
	served := make(chan error, 1)
	go func() { served <- k.Serve(l) }()
	var d net.Dialer
	open, err := d.DialContext(ctx(t), "tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	go k.Shutdown(time.Second)
	select {
	case <-k.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("keeper did not finish shutting down")
	}
	if !l.isClosed() {
		t.Fatal("Done closed before the listener was closed")
	}
	if c, err := d.DialContext(ctx(t), "tcp", inner.Addr().String()); err == nil {
		c.Close()
		t.Fatal("keeper still accepts connections after Done")
	}
	_ = open.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := open.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("open connection not closed by Done: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

// TestConnect covers the node's start: no keeper (start one), a keeper of
// this version (adopt its sessions), a keeper of another version (shut it
// down, stopping its children, and start a new one).
func TestConnect(t *testing.T) {
	var (
		mu      sync.Mutex
		current *keeperRig
		starts  int
	)
	dial := func(ctx context.Context) (net.Conn, error) {
		mu.Lock()
		r := current
		mu.Unlock()
		if r == nil {
			return nil, errors.New("no keeper")
		}
		select {
		case <-r.keeper.Done():
			return nil, errors.New("no keeper")
		default:
		}
		return r.dial(ctx)
	}
	startAs := func(version string) func() error {
		return func() error {
			mu.Lock()
			defer mu.Unlock()
			starts++
			current = newKeeper(t, version)
			return nil
		}
	}
	connect := func(version string) (*conpty.Client, conpty.Adoption) {
		t.Helper()
		c, how, err := conpty.Connect(ctx(t), conpty.ConnectOptions{Dial: dial, Version: version, Start: startAs(version), Grace: time.Second, Wait: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c, how
	}

	c, how := connect("v1")
	if how != conpty.Started || starts != 1 {
		t.Fatalf("first connect: %s, %d starts", how, starts)
	}
	if err := c.Launch(ctx(t), shell("sb-a")); err != nil {
		t.Fatal(err)
	}
	first := current

	c, how = connect("v1")
	if how != conpty.Adopted || starts != 1 {
		t.Fatalf("same version: %s, %d starts", how, starts)
	}
	if in, ok := info(t, c, "sb-a"); !ok || !in.Alive {
		t.Fatalf("adopted keeper lost sb-a: %+v", in)
	}

	c, how = connect("v2")
	if how != conpty.Replaced || starts != 2 {
		t.Fatalf("other version: %s, %d starts", how, starts)
	}
	select {
	case <-first.sp.proc("sb-a").exited:
	default:
		t.Fatal("the replaced keeper's child still runs")
	}
	if infos, err := c.List(ctx(t)); err != nil || len(infos) != 0 {
		t.Fatalf("new keeper sessions %v %v", infos, err)
	}

	_, _, err := conpty.Connect(ctx(t), conpty.ConnectOptions{Dial: func(context.Context) (net.Conn, error) { return nil, errors.New("no keeper") },
		Version: "v1", Start: func() error { return nil }, Wait: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("connect to a keeper that never answers: %v", err)
	}
}

// TestGoldenScenesThroughKeeper plays every golden scene as a child's output
// in the keeper (one step per keypress, resizes through the client) and
// checks the screen the client reads against the golden file.
func TestGoldenScenesThroughKeeper(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "scenes", "*.vt"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenes: %v", err)
	}
	r := newKeeper(t, "v1")
	c := r.client()
	for i, path := range paths {
		sc := readScene(t, path)
		t.Run(strings.TrimSuffix(filepath.Base(path), ".vt"), func(t *testing.T) {
			var data [][]byte
			for _, s := range sc.Steps {
				if s.Data != nil {
					data = append(data, s.Data)
				}
			}
			r.sp.prepare = func(p *fakeProc, _ term.Spec) {
				p.onInput = func(p *fakeProc, b []byte) {
					for range b {
						p.mu.Lock()
						next := data[0]
						data = data[1:]
						p.mu.Unlock()
						_, _ = p.outW.Write(next)
					}
				}
			}
			name := fmt.Sprintf("sb-g%d", i)
			if err := c.Launch(ctx(t), term.Spec{Name: name, Argv: []string{"scene"}, Cols: sc.Cols, Rows: sc.Rows}); err != nil {
				t.Fatal(err)
			}
			for n, step := range sc.Steps {
				if step.Data == nil {
					if err := c.Resize(ctx(t), name, step.Cols, step.Rows); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := c.Input(ctx(t), name, []byte("n")); err != nil {
					t.Fatal(err)
				}
				want := play(t, term.Scene{Cols: sc.Cols, Rows: sc.Rows, Steps: sc.Steps[:n+1]})
				eventually(t, fmt.Sprintf("step %d", n+1), func() (bool, string) {
					s := screenText(t, c, name)
					return s == want.Dump(), "--- keeper\n" + s + "--- emulator\n" + want.Dump()
				})
			}
			golden, err := os.ReadFile(strings.TrimSuffix(path, ".vt") + ".golden")
			if err != nil {
				t.Fatal(err)
			}
			if got := screenText(t, c, name); got != string(golden) {
				t.Errorf("grid differs from golden\n--- got\n%s--- want\n%s", got, golden)
			}
		})
	}
}

func readScene(t *testing.T, path string) term.Scene {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := term.ParseScene(raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return sc
}

// play replays a scene through a fresh emulator.
func play(t *testing.T, sc term.Scene) term.Grid {
	t.Helper()
	e := term.NewEmulator(sc.Cols, sc.Rows, 0)
	defer e.Close()
	for _, s := range sc.Steps {
		if s.Data == nil {
			e.Resize(s.Cols, s.Rows)
			continue
		}
		if _, err := e.Write(s.Data); err != nil {
			t.Fatal(err)
		}
	}
	return e.Grid(-1)
}

func TestResolveCommand(t *testing.T) {
	look := func(p string) (string, error) {
		switch p {
		case "claude":
			return `C:\npm\claude.CMD`, nil
		case "node":
			return `C:\node\node.exe`, nil
		}
		return "", errors.New("not found")
	}
	quote := func(s string) string {
		if strings.ContainsAny(s, " \"") {
			return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		return s
	}
	cl, err := conpty.ResolveCommand([]string{"claude", "--session-id", "a b"}, look, `C:\Windows\system32\cmd.exe`, quote)
	if err != nil {
		t.Fatal(err)
	}
	want := conpty.CommandLine{Program: `C:\Windows\system32\cmd.exe`, Line: `C:\Windows\system32\cmd.exe /d /s /c "C:\npm\claude.CMD --session-id "a b""`}
	if cl != want {
		t.Fatalf("shim: %+v\nwant %+v", cl, want)
	}
	cl, err = conpty.ResolveCommand([]string{"node", "x"}, look, "", quote)
	if err != nil || cl != (conpty.CommandLine{Program: `C:\node\node.exe`, Line: `C:\node\node.exe x`}) {
		t.Fatalf("exe: %+v %v", cl, err)
	}
	if _, err := conpty.ResolveCommand([]string{"nope"}, look, "", quote); err == nil {
		t.Fatal("resolved a missing program")
	}
}

func TestRunArgsRoundTrip(t *testing.T) {
	o := conpty.RunOptions{Instance: "n0123", History: 500, LogFile: `C:\data\keeper.log`}
	args := o.Args()
	if args[0] != conpty.KeeperCommand {
		t.Fatalf("args %q", args)
	}
	got, err := conpty.ParseRunArgs(args[1:])
	if err != nil || got != o {
		t.Fatalf("parsed %+v %v", got, err)
	}
	if _, err := conpty.ParseRunArgs(nil); err == nil {
		t.Fatal("parsed args without an instance")
	}
}

func TestBadFramesEndTheConnection(t *testing.T) {
	r := newKeeper(t, "v1")
	conn, err := r.dial(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A length beyond the limit.
	if _, err := conn.Write([]byte{0xff, 0xff, 0xff, 0xff, 'J'}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("keeper answered %d bytes to a bad frame", n)
	}
	// The keeper still serves others.
	if _, err := r.client().Version(ctx(t)); err != nil {
		t.Fatal(err)
	}
}

func TestVersion(t *testing.T) {
	r := newKeeper(t, "v9")
	ki, err := r.client().Version(ctx(t))
	if err != nil || ki.Version != "v9" || ki.Protocol != conpty.Protocol || ki.PID != os.Getpid() {
		t.Fatalf("version %+v %v", ki, err)
	}
}
