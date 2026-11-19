//go:build windows

package conpty_test

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

var recordDir = flag.String("conpty.record", "", "write each golden scene's ConPTY output to DIR/<scene>.vt")

// TestMain lets the test binary act as the programs these tests run: the
// keeper (as the node starts it), a node that starts a keeper and exits, an
// echo shell and a scene player inside a pseudo-console.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case conpty.KeeperCommand:
			os.Exit(runKeeper(os.Args[2:]))
		case "__node":
			os.Exit(runNode(os.Args[2:]))
		case "__echo":
			os.Exit(runEcho())
		case "__scene":
			os.Exit(runScene(os.Args[2]))
		}
	}
	os.Exit(m.Run())
}

// runKeeper is the keeper; its version is read from <log file>.version so a
// test can make the next keeper report another version.
func runKeeper(args []string) int {
	o, err := conpty.ParseRunArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	v, _ := os.ReadFile(o.LogFile + ".version")
	if err := conpty.Run(o, strings.TrimSpace(string(v))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// runNode is a node process: it connects to (starting) the keeper, launches
// one echo session and exits.
func runNode(args []string) int {
	o, err := conpty.ParseRunArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	c, err := connectKeeper(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Launch(ctx, echoSpec("sb-node")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func connectKeeper(o conpty.RunOptions) (*conpty.Client, error) {
	v, _ := os.ReadFile(o.LogFile + ".version")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, _, err := conpty.Connect(ctx, conpty.ConnectOptions{
		Dial: conpty.DialFor(o.Instance), Version: strings.TrimSpace(string(v)), Grace: 2 * time.Second,
		Start: func() error { return conpty.Start(os.Args[0], o) },
	})
	return c, err
}

// console opens the pseudo-console in raw VT mode: input byte by byte with
// no echo, output with VT processing and no implicit carriage return.
func console() (in, out *os.File) {
	open := func(name string) *os.File {
		p, _ := windows.UTF16PtrFromString(name)
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, name, err)
			os.Exit(3)
		}
		return os.NewFile(uintptr(h), name)
	}
	in, out = open("CONIN$"), open("CONOUT$")
	_ = windows.SetConsoleMode(windows.Handle(in.Fd()), windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	_ = windows.SetConsoleMode(windows.Handle(out.Fd()), windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	return in, out
}

// runEcho is a line shell: "exit N" exits with N, anything else is echoed.
func runEcho() int {
	in, out := console()
	fmt.Fprint(out, "ready\r\n")
	r := bufio.NewReader(in)
	var line []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return 0
		}
		if c != '\r' {
			line = append(line, c)
			continue
		}
		var n int
		if _, err := fmt.Sscanf(string(line), "exit %d", &n); err == nil {
			return n
		}
		fmt.Fprintf(out, "echo %s\r\n", line)
		line = nil
	}
}

// runScene paints the scene's data steps, one per key pressed; the test
// makes the resizes.
func runScene(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 3
	}
	sc, err := term.ParseScene(raw)
	if err != nil {
		return 3
	}
	in, out := console()
	key := make([]byte, 1)
	for _, s := range sc.Steps {
		if s.Data == nil {
			continue
		}
		if _, err := in.Read(key); err != nil {
			return 0
		}
		_, _ = out.Write(s.Data)
	}
	_, _ = io.Copy(io.Discard, in)
	return 0
}

func echoSpec(name string) term.Spec {
	return term.Spec{Name: name, Argv: []string{os.Args[0], "__echo"}, Env: os.Environ(), Cols: 40, Rows: 10}
}

var instanceSeq int

// keeperOpts names a keeper of its own for a test, reporting version.
func keeperOpts(t *testing.T, version string) conpty.RunOptions {
	t.Helper()
	instanceSeq++
	o := conpty.RunOptions{Instance: fmt.Sprintf("t%d-%d", os.Getpid(), instanceSeq), History: 1000,
		LogFile: filepath.Join(t.TempDir(), "keeper.log")}
	setVersion(t, o, version)
	t.Cleanup(func() {
		c := conpty.NewClient(conpty.DialFor(o.Instance))
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = c.Shutdown(cctx, time.Second)
		if log, err := os.ReadFile(o.LogFile); err == nil {
			t.Logf("keeper log:\n%s", log)
		}
	})
	return o
}

func setVersion(t *testing.T, o conpty.RunOptions, version string) {
	t.Helper()
	if err := os.WriteFile(o.LogFile+".version", []byte(version), 0o600); err != nil {
		t.Fatal(err)
	}
}

func connect(t *testing.T, o conpty.RunOptions, version string) (*conpty.Client, conpty.Adoption) {
	t.Helper()
	c, how, err := conpty.Connect(ctx(t), conpty.ConnectOptions{
		Dial: conpty.DialFor(o.Instance), Version: version, Grace: 2 * time.Second,
		Start: func() error { return conpty.Start(os.Args[0], o) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, how
}

func processGone(t *testing.T, pid int, within time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // no such process
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(within.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

func TestWindowsVersionIsSupported(t *testing.T) {
	if err := conpty.CheckWindows(); err != nil {
		t.Fatal(err)
	}
}

// TestKeeperOutlivesTheNode: a node process starts the keeper, launches a
// session and exits; the next node adopts the session without relaunching.
func TestKeeperOutlivesTheNode(t *testing.T) {
	o := keeperOpts(t, "v1")
	cmd := exec.Command(os.Args[0], append([]string{"__node"}, o.Args()[1:]...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	c, how := connect(t, o, "v1")
	if how != conpty.Adopted {
		t.Fatalf("second node: keeper %s", how)
	}
	in, ok := info(t, c, "sb-node")
	if !ok || !in.Alive {
		t.Fatalf("session after the node exited: %+v %v", in, ok)
	}
	eventually(t, "ready", func() (bool, string) {
		s := screenText(t, c, "sb-node")
		return strings.Contains(s, "ready"), s
	})
}

// TestKeeperBackend drives a real ConPTY session through the keeper process
// and its pipe.
func TestKeeperBackend(t *testing.T) {
	o := keeperOpts(t, "v1")
	c, how := connect(t, o, "v1")
	if how != conpty.Started {
		t.Fatalf("keeper %s", how)
	}
	ki, err := c.Version(ctx(t))
	if err != nil || ki.Version != "v1" || ki.Protocol != conpty.Protocol {
		t.Fatalf("version %+v %v", ki, err)
	}
	if err := c.Launch(ctx(t), echoSpec("sb-a")); err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(ctx(t), echoSpec("sb-a")); !errors.Is(err, term.ErrSessionExists) {
		t.Fatalf("second launch: %v", err)
	}
	eventually(t, "ready", func() (bool, string) {
		s := screenText(t, c, "sb-a")
		return strings.Contains(s, "ready"), s
	})
	st, err := c.Stream(ctx(t), "sb-a")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var (
		mu  sync.Mutex
		got []byte
	)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := st.Read(buf)
			mu.Lock()
			got = append(got, buf[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	if err := c.Input(ctx(t), "sb-a", []byte("hello 한글\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "echo on screen", func() (bool, string) {
		s := screenText(t, c, "sb-a")
		return strings.Contains(s, "echo hello 한글"), s
	})
	eventually(t, "echo on stream", func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(string(got), "echo hello"), fmt.Sprintf("%q", got)
	})
	if err := c.Resize(ctx(t), "sb-a", 60, 12); err != nil {
		t.Fatal(err)
	}
	if g, err := c.Screen(ctx(t), "sb-a", 0); err != nil || g.Cols != 60 || g.Rows != 12 {
		t.Fatalf("screen after resize %dx%d %v", g.Cols, g.Rows, err)
	}
	if err := c.Forget(ctx(t), "sb-a"); err == nil {
		t.Fatal("forgot a live session")
	}
	if err := c.Input(ctx(t), "sb-a", []byte("exit 7\r")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "exit", func() (bool, string) {
		in, _ := info(t, c, "sb-a")
		return !in.Alive, fmt.Sprintf("%+v", in)
	})
	if in, _ := info(t, c, "sb-a"); in.ExitCode != 7 {
		t.Fatalf("exit code %d", in.ExitCode)
	}
	if s := screenText(t, c, "sb-a"); !strings.Contains(s, "echo hello") {
		t.Fatalf("dead session lost its screen:\n%s", s)
	}
	if err := c.Forget(ctx(t), "sb-a"); err != nil {
		t.Fatal(err)
	}

	// Stop of a live session ends it within the grace.
	if err := c.Launch(ctx(t), echoSpec("sb-b")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ready", func() (bool, string) {
		s := screenText(t, c, "sb-b")
		return strings.Contains(s, "ready"), s
	})
	in, _ := info(t, c, "sb-b")
	if err := c.Stop(ctx(t), "sb-b", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if in2, _ := info(t, c, "sb-b"); in2.Alive {
		t.Fatalf("alive after stop: %+v", in2)
	}
	if !processGone(t, in.PID, 5*time.Second) {
		t.Fatalf("child %d still runs after stop", in.PID)
	}
}

// TestPipeGrantsOnlyTheCurrentUser asserts the pipe's security descriptor:
// a protected DACL with one entry, for this user's SID. (A second account to
// try the pipe from is not available on the runner.)
func TestPipeGrantsOnlyTheCurrentUser(t *testing.T) {
	o := keeperOpts(t, "v1")
	connect(t, o, "v1")
	p, _ := windows.UTF16PtrFromString(conpty.PipeName(o.Instance))
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	dacl := sddl[strings.Index(sddl, "D:"):]
	if !strings.HasPrefix(dacl, "D:P") || strings.Count(dacl, "(") != 1 || !strings.HasSuffix(dacl, ";;;"+u.User.Sid.String()+")") || !strings.Contains(dacl, "(A;") {
		t.Fatalf("pipe DACL %q, want one allow entry for %s", dacl, u.User.Sid)
	}
}

// TestKilledKeeperTakesItsChildren: the keeper's job kills every child when
// the keeper dies, and the next node starts a fresh keeper.
func TestKilledKeeperTakesItsChildren(t *testing.T) {
	o := keeperOpts(t, "v1")
	c, _ := connect(t, o, "v1")
	for _, n := range []string{"sb-a", "sb-b"} {
		if err := c.Launch(ctx(t), echoSpec(n)); err != nil {
			t.Fatal(err)
		}
	}
	ki, err := c.Version(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	infos, err := c.List(ctx(t))
	if err != nil || len(infos) != 2 {
		t.Fatalf("sessions %+v %v", infos, err)
	}
	kh, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(ki.PID))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.TerminateProcess(kh, 1); err != nil {
		t.Fatal(err)
	}
	_, _ = windows.WaitForSingleObject(kh, 10000)
	_ = windows.CloseHandle(kh)
	for _, in := range infos {
		if !processGone(t, in.PID, 10*time.Second) {
			t.Fatalf("child %s (%d) outlived the keeper", in.Name, in.PID)
		}
	}
	c2, how := connect(t, o, "v1")
	if how != conpty.Started {
		t.Fatalf("after the keeper died: %s", how)
	}
	if infos, err := c2.List(ctx(t)); err != nil || len(infos) != 0 {
		t.Fatalf("new keeper sessions %+v %v", infos, err)
	}
}

// TestKeeperOfAnotherVersionIsReplaced: the node shuts an old keeper down
// (its children end) and starts its own.
func TestKeeperOfAnotherVersionIsReplaced(t *testing.T) {
	o := keeperOpts(t, "v1")
	c, _ := connect(t, o, "v1")
	if err := c.Launch(ctx(t), echoSpec("sb-a")); err != nil {
		t.Fatal(err)
	}
	in, _ := info(t, c, "sb-a")
	setVersion(t, o, "v2")
	c2, how := connect(t, o, "v2")
	if how != conpty.Replaced {
		t.Fatalf("keeper %s", how)
	}
	if !processGone(t, in.PID, 10*time.Second) {
		t.Fatalf("old keeper's child %d still runs", in.PID)
	}
	if ki, err := c2.Version(ctx(t)); err != nil || ki.Version != "v2" {
		t.Fatalf("version %+v %v", ki, err)
	}
}

// teeProc records a session's ConPTY output.
type teeProc struct {
	conpty.Proc
	mu  *sync.Mutex
	out *[]byte
}

func (p teeProc) Read(b []byte) (int, error) {
	n, err := p.Proc.Read(b)
	p.mu.Lock()
	*p.out = append(*p.out, b[:n]...)
	p.mu.Unlock()
	return n, err
}

// TestGoldenScenesConPTY plays every golden scene through a real program in
// a ConPTY and checks the keeper's Grid against the same golden files the
// emulator and tmux meet. With -conpty.record DIR it also writes each
// scene's raw ConPTY output, the testdata/conpty recordings.
func TestGoldenScenesConPTY(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "scenes", "*.vt"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenes: %v", err)
	}
	var (
		mu  sync.Mutex
		out []byte
	)
	k := conpty.NewKeeper(conpty.KeeperOptions{Logf: t.Logf, Spawn: func(s term.Spec) (conpty.Proc, error) {
		p, err := conpty.Spawn(s)
		if err != nil {
			return nil, err
		}
		return teeProc{Proc: p, mu: &mu, out: &out}, nil
	}})
	t.Cleanup(func() { k.Shutdown(time.Second) })
	mark := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		b := out
		out = nil
		return b
	}
	// quiet waits until the console has written nothing for a while.
	quiet := func() {
		last, still := -1, 0
		for still < 5 {
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			n := len(out)
			mu.Unlock()
			if n == last {
				still++
			} else {
				last, still = n, 0
			}
		}
	}
	screen := func(name string) term.Grid {
		g, err := k.Screen(name, -1)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	v := windows.RtlGetVersion()
	for i, path := range paths {
		sc := readScene(t, path)
		scene := strings.TrimSuffix(filepath.Base(path), ".vt")
		t.Run(scene, func(t *testing.T) {
			abs, _ := filepath.Abs(path)
			name := "sb-g" + strconv.Itoa(i)
			mark()
			if err := k.Launch(term.Spec{Name: name, Argv: []string{os.Args[0], "__scene", abs}, Env: os.Environ(), Cols: sc.Cols, Rows: sc.Rows}); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = k.Stop(name, time.Second)
				_ = k.Forget(name)
			}()
			quiet()
			rec := recording{cols: sc.Cols, rows: sc.Rows}
			for n, step := range sc.Steps {
				if step.Data == nil {
					rec.parts = append(rec.parts, recordPart{data: mark()})
					if err := k.Resize(name, step.Cols, step.Rows); err != nil {
						t.Fatal(err)
					}
					rec.parts = append(rec.parts, recordPart{cols: step.Cols, rows: step.Rows})
					quiet()
					continue
				}
				if err := k.Input(name, []byte("n")); err != nil {
					t.Fatal(err)
				}
				want := play(t, term.Scene{Cols: sc.Cols, Rows: sc.Rows, Steps: sc.Steps[:n+1]}).Dump()
				deadline := time.Now().Add(5 * time.Second)
				for screen(name).Dump() != want && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				quiet()
				if got := screen(name).Dump(); got != want {
					t.Errorf("step %d\n--- conpty\n%s--- emulator\n%s", n+1, got, want)
				}
			}
			rec.parts = append(rec.parts, recordPart{data: mark()})
			if *recordDir != "" {
				header := fmt.Sprintf("ConPTY output of scenes/%s.vt on Windows %d.%d build %d,\nrecorded by TestGoldenScenesConPTY -conpty.record; replayed by TestRecordedConPTYOutputMatchesGolden.",
					scene, v.MajorVersion, v.MinorVersion, v.BuildNumber)
				if err := os.MkdirAll(*recordDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(*recordDir, scene+".vt"), rec.encode(header), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(strings.TrimSuffix(path, ".vt") + ".golden")
			if err != nil {
				t.Fatal(err)
			}
			if got := screen(name).Dump(); got != string(golden) {
				t.Errorf("grid differs from golden\n--- got\n%s--- want\n%s", got, golden)
			}
		})
	}
}
