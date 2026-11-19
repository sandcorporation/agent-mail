//go:build unix

package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
)

// requireEnv set to "require" (CI) turns a missing tmux into a failure.
const requireEnv = "SWITCHBOARD_TEST_TMUX"

var instanceSeq int

// newBackend starts a backend on a private TMUX_TMPDIR, so neither the
// operator's tmux nor other tests share its socket directory, and kills its
// server at the end.
func newBackend(t *testing.T, history int) *Backend {
	t.Helper()
	if _, err := Check(context.Background(), ""); err != nil {
		if os.Getenv(requireEnv) == "require" {
			t.Fatalf("%s=require: %v", requireEnv, err)
		}
		t.Skipf("tmux unavailable: %v", err)
	}
	t.Setenv("TMUX_TMPDIR", shortTempDir(t))
	instanceSeq++
	b, err := New(context.Background(), Options{
		Instance: fmt.Sprintf("t%d-%d", os.Getpid(), instanceSeq),
		History:  history,
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.command(nil, "kill-server").Run() })
	return b
}

// shortTempDir keeps socket paths under the unix socket length limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sbtmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// eventually polls f until it reports done or 5 s pass.
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
		time.Sleep(20 * time.Millisecond)
	}
}

func info(t *testing.T, b *Backend, name string) (term.Info, bool) {
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

func waitExit(t *testing.T, b *Backend, name string) term.Info {
	t.Helper()
	var last term.Info
	eventually(t, name+" exits", func() (bool, string) {
		i, ok := info(t, b, name)
		last = i
		return ok && !i.Alive, fmt.Sprintf("%+v", i)
	})
	return last
}

// rawCat launches a program that copies its input to its output (or to
// file) byte for byte, and waits until the terminal is in raw mode.
func rawCat(t *testing.T, b *Backend, name string, cols, rows int, file string) {
	t.Helper()
	cmd := "stty raw -echo; exec cat"
	if file != "" {
		cmd += " > '" + file + "'"
	}
	launch(t, b, term.Spec{Name: name, Argv: []string{"/bin/sh", "-c", cmd}, Cols: cols, Rows: rows})
	eventually(t, name+" runs cat", func() (bool, string) {
		out, err := b.run(ctx(t), name, "display-message", "-p", "-t", target(name), "#{pane_current_command}")
		return err == nil && strings.TrimSpace(out) == "cat", out
	})
}

func launch(t *testing.T, b *Backend, s term.Spec) {
	t.Helper()
	if err := b.Launch(ctx(t), s); err != nil {
		t.Fatal(err)
	}
}

func screenText(t *testing.T, b *Backend, name string) string {
	t.Helper()
	g, err := b.Screen(ctx(t), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	return g.Dump()
}

func TestCheckRefusesOldTmux(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'tmux 3.1c'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := Check(context.Background(), bin)
	if !errors.Is(err, ErrUnavailable) || v.Raw != "3.1c" {
		t.Fatalf("Check = %+v, %v", v, err)
	}
	for _, want := range []string{"3.1c", "apt-get install tmux"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, err := New(context.Background(), Options{Instance: "old", Binary: bin, Dir: t.TempDir()}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("New with old tmux: %v", err)
	}
}

func TestLaunchListExitForget(t *testing.T) {
	b := newBackend(t, 0)
	dir := t.TempDir()
	launch(t, b, term.Spec{
		Name: "sb-1",
		Argv: []string{"/bin/sh", "-c", `printf '%s|%s|%s' "$SB_A" "$SB_B" "$(pwd)"; sleep 0.3; exit 7`},
		Dir:  dir,
		Env:  []string{"SB_A=one", "SB_B=two words"},
		Cols: 60, Rows: 4,
	})
	i, ok := info(t, b, "sb-1")
	if !ok || i.PID <= 0 || time.Since(i.StartedAt) > time.Minute {
		t.Fatalf("listed %+v (%v)", i, ok)
	}
	if err := b.Launch(ctx(t), term.Spec{Name: "sb-1", Argv: []string{"true"}, Cols: 10, Rows: 2}); !errors.Is(err, term.ErrSessionExists) {
		t.Errorf("second launch: %v, want ErrSessionExists", err)
	}
	if i = waitExit(t, b, "sb-1"); i.ExitCode != 7 {
		t.Errorf("exit code %d, want 7", i.ExitCode)
	}
	// The dead pane keeps its screen and status until Forget.
	if got := screenText(t, b, "sb-1"); !strings.Contains(got, "one|two words|"+dir) {
		t.Errorf("screen:\n%s", got)
	}
	if err := b.Input(ctx(t), "sb-1", []byte("x")); !errors.Is(err, term.ErrNotRunning) {
		t.Errorf("input to dead: %v", err)
	}
	if _, err := b.Stream(ctx(t), "sb-1"); !errors.Is(err, term.ErrNotRunning) {
		t.Errorf("stream of dead: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if i, _ = info(t, b, "sb-1"); i.Alive || i.ExitCode != 7 {
		t.Errorf("later: %+v", i)
	}
	if err := b.Forget(ctx(t), "sb-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := info(t, b, "sb-1"); ok {
		t.Error("listed after Forget")
	}
	if _, err := b.Screen(ctx(t), "sb-1", 0); !errors.Is(err, term.ErrNoSession) {
		t.Errorf("screen after Forget: %v", err)
	}
}

func TestNamesMatchExactly(t *testing.T) {
	b := newBackend(t, 0)
	launch(t, b, term.Spec{Name: "sb-12", Argv: []string{"sleep", "30"}, Cols: 10, Rows: 2})
	for _, err := range []error{
		b.Input(ctx(t), "sb-1", []byte("x")),
		b.Resize(ctx(t), "sb-1", 20, 5),
		b.Stop(ctx(t), "sb-1", time.Second),
		b.Forget(ctx(t), "sb-1"),
		func() error { _, err := b.Screen(ctx(t), "sb-1", 0); return err }(),
	} {
		if !errors.Is(err, term.ErrNoSession) {
			t.Errorf("sb-1 reached: %v", err)
		}
	}
	if i, ok := info(t, b, "sb-12"); !ok || !i.Alive {
		t.Errorf("sb-12: %+v", i)
	}
}

func TestListWithoutServer(t *testing.T) {
	b := newBackend(t, 0)
	infos, err := b.List(ctx(t))
	if err != nil || len(infos) != 0 {
		t.Fatalf("List = %v, %v", infos, err)
	}
}

func TestInputBytesArriveUnchanged(t *testing.T) {
	b := newBackend(t, 0)
	out := filepath.Join(t.TempDir(), "typed")
	rawCat(t, b, "sb-in", 40, 5, out)
	want := []byte("\x03\x1b\x1b[A\x1b[B\x1bOP\x04\x1a\x11\x13\x7f\r\n\t\\'\"; héllo 한")
	for c := range 256 {
		want = append(want, byte(c))
	}
	if err := b.Input(ctx(t), "sb-in", want); err != nil {
		t.Fatal(err)
	}
	eventually(t, "typed bytes", func() (bool, string) {
		got, _ := os.ReadFile(out)
		return bytes.Equal(got, want), fmt.Sprintf("%q", got)
	})
	if i, _ := info(t, b, "sb-in"); !i.Alive {
		t.Errorf("Ctrl-C ended the raw-mode program: %+v", i)
	}
}

func TestStream(t *testing.T) {
	b := newBackend(t, 0)
	// The program exits a moment after its last output: tmux 3.5 drops
	// control-mode output a pane prints right before it dies (capture-pane
	// still has it, so a viewer's next snapshot does).
	launch(t, b, term.Spec{Name: "sb-s", Argv: []string{"/bin/sh", "-c", "stty raw -echo; printf ready; head -c 13; sleep 0.3"}, Cols: 30, Rows: 4})
	eventually(t, "raw mode", func() (bool, string) {
		s := screenText(t, b, "sb-s")
		return strings.Contains(s, "ready"), s
	})
	r, err := b.Stream(ctx(t), "sb-s")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Give the control client time to attach before anything is printed.
	time.Sleep(300 * time.Millisecond)
	sent := []byte("a\\b\x1b[1m한\r\n\x01")
	if len(sent) != 13 {
		t.Fatalf("sent %d bytes", len(sent))
	}
	if err := b.Input(ctx(t), "sb-s", sent); err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		got, _ := io.ReadAll(r) // ends with io.EOF when head exits
		done <- got
	}()
	select {
	case got := <-done:
		if !bytes.Equal(got, sent) {
			t.Errorf("stream = %q, want %q", got, sent)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end when the program exited")
	}
}

func TestStreamCloseAndContext(t *testing.T) {
	b := newBackend(t, 0)
	launch(t, b, term.Spec{Name: "sb-c", Argv: []string{"sleep", "30"}, Cols: 30, Rows: 4})
	clients := func() int {
		out, err := b.run(ctx(t), "", "list-clients", "-F", "#{client_name}")
		if err != nil {
			t.Fatal(err)
		}
		return len(strings.Fields(out))
	}
	r, err := b.Stream(ctx(t), "sb-c")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "client attaches", func() (bool, string) { n := clients(); return n == 1, strconv.Itoa(n) })
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "client detaches on Close", func() (bool, string) { n := clients(); return n == 0, strconv.Itoa(n) })

	c, cancel := context.WithCancel(context.Background())
	r, err = b.Stream(c, "sb-c")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "client attaches", func() (bool, string) { n := clients(); return n == 1, strconv.Itoa(n) })
	cancel()
	if _, err := io.ReadAll(r); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("read after cancel: %v", err)
	}
	eventually(t, "client detaches on cancel", func() (bool, string) { n := clients(); return n == 0, strconv.Itoa(n) })
	if i, _ := info(t, b, "sb-c"); !i.Alive {
		t.Error("closing a stream ended the session")
	}
}

func TestResize(t *testing.T) {
	b := newBackend(t, 0)
	launch(t, b, term.Spec{Name: "sb-r", Argv: []string{"/bin/sh", "-c", "trap 'stty size' WINCH; stty size; while :; do sleep 0.05; done"}, Cols: 30, Rows: 5})
	eventually(t, "initial size", func() (bool, string) {
		s := screenText(t, b, "sb-r")
		return strings.Contains(s, "5 30"), s
	})
	// A stream client attached must not change the size.
	r, err := b.Stream(ctx(t), "sb-r")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := b.Resize(ctx(t), "sb-r", 44, 9); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resized", func() (bool, string) {
		g, err := b.Screen(ctx(t), "sb-r", 0)
		if err != nil {
			t.Fatal(err)
		}
		return g.Cols == 44 && g.Rows == 9 && strings.Contains(g.Dump(), "9 44"), g.Dump()
	})
}

func TestStopGrace(t *testing.T) {
	b := newBackend(t, 0)
	pidFile := filepath.Join(t.TempDir(), "child")
	// The program obeys SIGTERM; its background child in the same group
	// goes too.
	launch(t, b, term.Spec{Name: "sb-ok", Argv: []string{"/bin/sh", "-c", "sleep 60 & echo $! > '" + pidFile + "'; wait"}, Cols: 20, Rows: 3})
	// This one ignores it.
	launch(t, b, term.Spec{Name: "sb-stubborn", Argv: []string{"/bin/sh", "-c", "trap '' TERM; while :; do sleep 0.05; done"}, Cols: 20, Rows: 3})
	eventually(t, "child pid", func() (bool, string) { b, _ := os.ReadFile(pidFile); return len(b) > 0, string(b) })
	time.Sleep(100 * time.Millisecond) // the trap is set

	start := time.Now()
	if err := b.Stop(ctx(t), "sb-ok", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("obeying program took %v to stop", d)
	}
	if i, ok := info(t, b, "sb-ok"); !ok || i.Alive || i.ExitCode != 128+int(syscall.SIGTERM) {
		t.Errorf("sb-ok after Stop: %+v (%v)", i, ok)
	}
	raw, _ := os.ReadFile(pidFile)
	child, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	eventually(t, "group child gone", func() (bool, string) {
		return child > 0 && syscall.Kill(child, 0) != nil, strconv.Itoa(child)
	})

	start = time.Now()
	grace := 400 * time.Millisecond
	if err := b.Stop(ctx(t), "sb-stubborn", grace); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < grace {
		t.Errorf("stubborn program stopped after %v, before the grace", d)
	}
	if i, ok := info(t, b, "sb-stubborn"); !ok || i.Alive || i.ExitCode != 128+int(syscall.SIGKILL) {
		t.Errorf("sb-stubborn after Stop: %+v (%v)", i, ok)
	}
	// Stopping a dead session is a no-op; Forget then drops it.
	if err := b.Stop(ctx(t), "sb-stubborn", grace); err != nil {
		t.Error(err)
	}
	if err := b.Forget(ctx(t), "sb-stubborn"); err != nil {
		t.Error(err)
	}
}

func TestForgetRefusesLiveSession(t *testing.T) {
	b := newBackend(t, 0)
	launch(t, b, term.Spec{Name: "sb-live", Argv: []string{"sleep", "30"}, Cols: 20, Rows: 3})
	if err := b.Forget(ctx(t), "sb-live"); err == nil {
		t.Fatal("forgot a live session")
	}
	if _, ok := info(t, b, "sb-live"); !ok {
		t.Fatal("live session gone")
	}
}

func TestOwnSocketLeavesUserTmuxAlone(t *testing.T) {
	b := newBackend(t, 0) // TMUX_TMPDIR is private, so its "default" socket is a stand-in for the operator's
	user := func(args ...string) (string, error) {
		cmd := exec.Command(b.bin, append([]string{"-f", "/dev/null"}, args...)...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := user("new-session", "-d", "-s", "sb-u", "sleep", "60"); err != nil {
		t.Fatalf("user tmux: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = user("kill-server") })
	// As if the node ran inside the operator's tmux.
	sock, err := user("display-message", "-p", "-t", "=sb-u:", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", strings.TrimSpace(sock)+",1,0")

	if _, ok := info(t, b, "sb-u"); ok {
		t.Fatal("user session listed")
	}
	launch(t, b, term.Spec{Name: "sb-u", Argv: []string{"sleep", "60"}, Cols: 20, Rows: 3})
	if err := b.Stop(ctx(t), "sb-u", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := b.Forget(ctx(t), "sb-u"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	out, err := user("list-panes", "-a", "-F", "#{session_name} #{pane_dead}")
	if err != nil || strings.TrimSpace(out) != "sb-u 0" {
		t.Fatalf("user tmux changed: %v %q", err, out)
	}
}

func TestConfigInEffect(t *testing.T) {
	// An operator config that would change everything must not be read.
	home := t.TempDir()
	conf := "set -g history-limit 7\nset -g remain-on-exit off\nset -g status on\n"
	for _, p := range []string{filepath.Join(home, ".tmux.conf"), filepath.Join(home, ".config", "tmux", "tmux.conf")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	b := newBackend(t, 1234)
	launch(t, b, term.Spec{Name: "sb-cfg", Argv: []string{"sleep", "30"}, Cols: 20, Rows: 3})
	for opt, want := range map[string]string{
		"history-limit":    "1234",
		"remain-on-exit":   "on",
		"status":           "off",
		"default-terminal": "tmux-256color",
	} {
		out, err := b.run(ctx(t), "", "show-options", "-gv", opt)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("%s = %q, want %q", opt, got, want)
		}
	}
	out, err := b.run(ctx(t), "", "show-options", "-wv", "-t", target("sb-cfg"), "window-size")
	if err != nil || strings.TrimSpace(out) != "manual" {
		t.Errorf("window-size = %q, %v", out, err)
	}
}

func TestScreenHistory(t *testing.T) {
	b := newBackend(t, 0)
	launch(t, b, term.Spec{Name: "sb-h", Argv: []string{"/bin/sh", "-c", "for i in 1 2 3 4 5 6 7 8; do echo line$i; done; printf '\\033[1mend'; sleep 30"}, Cols: 12, Rows: 3})
	eventually(t, "printed", func() (bool, string) {
		s := screenText(t, b, "sb-h")
		return strings.Contains(s, "end"), s
	})
	for _, tc := range []struct {
		history int
		want    []string
	}{
		{0, nil},
		{2, []string{"line5", "line6"}},
		{-1, []string{"line1", "line2", "line3", "line4", "line5", "line6"}},
	} {
		g, err := b.Screen(ctx(t), "sb-h", tc.history)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, l := range g.History {
			var s strings.Builder
			for _, c := range l {
				s.WriteString(c.Text)
			}
			got = append(got, s.String())
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("history(%d) = %q, want %q", tc.history, got, tc.want)
		}
		if g.Cursor.X != 3 || g.Cursor.Y != 2 || !g.Screen[2][0].Attr.Bold {
			t.Errorf("history(%d): cursor %+v, bold %v\n%s", tc.history, g.Cursor, g.Screen[2][0].Attr.Bold, g.Dump())
		}
	}
}

// TestGoldenScenes plays every golden scene through a real program in tmux
// and checks the backend's Grid against the same golden file the emulator
// meets.
func TestGoldenScenes(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "scenes", "*.vt"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenes: %v", err)
	}
	b := newBackend(t, 0)
	for i, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := term.ParseScene(raw)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		t.Run(strings.TrimSuffix(filepath.Base(path), ".vt"), func(t *testing.T) {
			name := fmt.Sprintf("sb-g%d", i)
			rawCat(t, b, name, sc.Cols, sc.Rows, "")
			// Each step must land before the next, so output is checked
			// against the emulator replaying the scene up to it. A resize is
			// only waited for: tmux rewraps lines on resize and the emulator
			// does not, which the program's redraw then covers.
			for n, step := range sc.Steps {
				if step.Data == nil {
					if err := b.Resize(ctx(t), name, step.Cols, step.Rows); err != nil {
						t.Fatal(err)
					}
					eventually(t, fmt.Sprintf("step %d resize", n+1), func() (bool, string) {
						g, err := b.Screen(ctx(t), name, 0)
						if err != nil {
							t.Fatal(err)
						}
						return g.Cols == step.Cols && g.Rows == step.Rows, g.Dump()
					})
					continue
				}
				if err := b.Input(ctx(t), name, step.Data); err != nil {
					t.Fatal(err)
				}
				want := play(t, term.Scene{Cols: sc.Cols, Rows: sc.Rows, Steps: sc.Steps[:n+1]})
				eventually(t, fmt.Sprintf("step %d", n+1), func() (bool, string) {
					g, err := b.Screen(ctx(t), name, -1)
					if err != nil {
						t.Fatal(err)
					}
					return g.Dump() == want.Dump(), "--- tmux\n" + g.Dump() + "--- emulator\n" + want.Dump()
				})
			}
			golden, err := os.ReadFile(strings.TrimSuffix(path, ".vt") + ".golden")
			if err != nil {
				t.Fatal(err)
			}
			g, err := b.Screen(ctx(t), name, -1)
			if err != nil {
				t.Fatal(err)
			}
			if g.Dump() != string(golden) {
				t.Errorf("grid differs from golden\n--- got\n%s--- want\n%s", g.Dump(), golden)
			}
		})
	}
}

// play replays a scene through a fresh emulator: what every backend must
// show.
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
