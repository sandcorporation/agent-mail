//go:build unix

package tmux

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
)

// Options configures a Backend.
type Options struct {
	// Instance names the tmux server: socket "switchboard-<Instance>".
	Instance string
	// Binary is the tmux binary, a name looked up in PATH or a path
	// (default "tmux").
	Binary string
	// History is the scrollback kept per session (default
	// term.DefaultHistory).
	History int
	// Dir holds the server's config file (default the user cache directory's
	// "switchboard", else the temporary directory).
	Dir string
}

// Backend is term.Backend over a dedicated tmux server.
type Backend struct {
	bin     string
	socket  string
	conf    string
	history int
	version Version
}

var _ term.Backend = (*Backend)(nil)

// killWait bounds the wait for a pane to die after SIGKILL.
const killWait = 2 * time.Second

// New checks tmux (see Check) and writes the server config. The server
// itself starts with the first Launch.
func New(ctx context.Context, o Options) (*Backend, error) {
	if o.Instance == "" || strings.ContainsAny(o.Instance, "/:. \t") {
		return nil, fmt.Errorf("tmux: bad instance %q", o.Instance)
	}
	v, err := Check(ctx, o.Binary)
	if err != nil {
		return nil, err
	}
	bin := o.Binary
	if bin == "" {
		bin = "tmux"
	}
	if bin, err = exec.LookPath(bin); err != nil {
		return nil, err
	}
	if o.History <= 0 {
		o.History = term.DefaultHistory
	}
	if o.Dir == "" {
		if d, err := os.UserCacheDir(); err == nil {
			o.Dir = filepath.Join(d, "switchboard")
		} else {
			o.Dir = os.TempDir()
		}
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("tmux: config dir: %w", err)
	}
	socket := "switchboard-" + o.Instance
	conf := filepath.Join(o.Dir, socket+".tmux.conf")
	if err := writeFile(conf, Config(o.History)); err != nil {
		return nil, fmt.Errorf("tmux: config: %w", err)
	}
	return &Backend{bin: bin, socket: socket, conf: conf, history: o.History, version: v}, nil
}

// Config is the server config file. window-size manual is set per window
// after new-session instead: tmux 3.5 and 3.6 crash creating a detached
// session while it is the global value.
func Config(history int) string {
	return strings.Join([]string{
		"set -g remain-on-exit on",
		// tmux 3.3+ prints "Pane is dead" on the last screen; 3.2 lacks the
		// option, hence -q.
		"set -gq remain-on-exit-format ''",
		"set -g history-limit " + strconv.Itoa(history),
		"set -g status off",
		"set -g default-terminal tmux-256color",
		"set -g default-shell /bin/sh",
		"set -g default-command ''",
		"set -g allow-rename off",
		"set -g automatic-rename off",
		"",
	}, "\n")
}

func writeFile(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Version is the tmux release the backend runs.
func (b *Backend) Version() Version { return b.version }

// Socket is the tmux server's socket name (tmux -L).
func (b *Backend) Socket() string { return b.socket }

func (b *Backend) command(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"-u", "-L", b.socket, "-f", b.conf}, args...)
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command(b.bin, full...)
	} else {
		cmd = exec.CommandContext(ctx, b.bin, full...)
	}
	// An enclosing tmux must not leak into ours.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=")
	})
	return cmd
}

// run runs one tmux command line and returns its stdout.
func (b *Backend) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := b.command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", classify(name, args[0], strings.TrimSpace(errb.String()), err)
	}
	return out.String(), nil
}

func classify(name, verb, stderr string, err error) error {
	switch {
	case strings.Contains(stderr, "duplicate session"):
		return fmt.Errorf("%s: %w", name, term.ErrSessionExists)
	case strings.Contains(stderr, "can't find"),
		strings.Contains(stderr, "no server running"),
		strings.Contains(stderr, "error connecting to"),
		strings.Contains(stderr, "no current"):
		return fmt.Errorf("%s: %w", name, term.ErrNoSession)
	}
	if stderr == "" {
		stderr = err.Error()
	}
	return fmt.Errorf("tmux %s %s: %s", verb, name, stderr)
}

// target is an exact-match tmux target: "sb-1" alone would also match
// "sb-12" by prefix.
func target(name string) string { return "=" + name + ":" }

// Launch implements term.Backend. The program runs directly (exec from
// /bin/sh, so the pane's pid is the program's), in a process group of its
// own.
func (b *Backend) Launch(ctx context.Context, s term.Spec) error {
	if s.Name == "" || strings.ContainsAny(s.Name, ":.") {
		return fmt.Errorf("tmux: bad session name %q", s.Name)
	}
	if len(s.Argv) == 0 {
		return fmt.Errorf("tmux: launch %s: empty argv", s.Name)
	}
	args := []string{"new-session", "-d", "-s", s.Name,
		"-x", strconv.Itoa(max(s.Cols, 1)), "-y", strconv.Itoa(max(s.Rows, 1))}
	if s.Dir != "" {
		args = append(args, "-c", s.Dir)
	}
	for _, kv := range s.Env {
		args = append(args, "-e", kv)
	}
	args = append(args, "--", "exec "+shellQuote(s.Argv),
		";", "set-option", "-w", "-t", target(s.Name), "window-size", "manual")
	if _, err := b.run(ctx, s.Name, args...); err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	return nil
}

func shellQuote(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

const listFormat = "#{session_name}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_dead_signal}\t#{session_created}"

// List implements term.Backend; sessions come sorted by name. No server
// running means no sessions.
func (b *Backend) List(ctx context.Context) ([]term.Info, error) {
	out, err := b.run(ctx, "", "list-panes", "-a", "-F", listFormat)
	if errors.Is(err, term.ErrNoSession) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var infos []term.Info
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		info, err := parsePane(line)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	slices.SortFunc(infos, func(a, b term.Info) int { return strings.Compare(a.Name, b.Name) })
	return infos, nil
}

func parsePane(line string) (term.Info, error) {
	f := strings.Split(line, "\t")
	if len(f) != 6 {
		return term.Info{}, fmt.Errorf("tmux: list-panes line %q", line)
	}
	pid, _ := strconv.Atoi(f[1])
	created, _ := strconv.ParseInt(f[5], 10, 64)
	info := term.Info{Name: f[0], PID: pid, Alive: f[2] != "1", StartedAt: time.Unix(created, 0)}
	if !info.Alive {
		info.ExitCode = exitCode(f[3], f[4])
	}
	return info, nil
}

// exitCode is the pane's exit status, or 128+signal for a program killed by
// a signal, as a shell reports it.
func exitCode(status, signal string) int {
	if n, err := strconv.Atoi(status); err == nil {
		return n
	}
	if n, err := strconv.Atoi(signal); err == nil {
		return 128 + n
	}
	return -1
}

type pane struct {
	pid  int
	dead bool
}

func (b *Backend) pane(ctx context.Context, name string) (pane, error) {
	// list-panes, not display-message: display-message falls back to the
	// current pane when the target is missing instead of failing.
	out, err := b.run(ctx, name, "list-panes", "-t", target(name), "-F", "#{pane_pid} #{pane_dead}")
	if err != nil {
		return pane{}, err
	}
	f := strings.Fields(out)
	if len(f) < 1 {
		return pane{}, fmt.Errorf("%s: %w", name, term.ErrNoSession)
	}
	pid, _ := strconv.Atoi(f[0])
	return pane{pid: pid, dead: len(f) > 1 && f[1] == "1"}, nil
}

func (b *Backend) running(ctx context.Context, name string) (pane, error) {
	p, err := b.pane(ctx, name)
	if err != nil {
		return p, err
	}
	if p.dead {
		return p, fmt.Errorf("%s: %w", name, term.ErrNotRunning)
	}
	return p, nil
}

const screenFormat = "#{pane_width} #{pane_height} #{cursor_x} #{cursor_y} #{cursor_flag} #{history_size}"

// Screen implements term.Backend: capture-pane's text with attributes is
// replayed into a fresh term.Emulator.
func (b *Backend) Screen(ctx context.Context, name string, history int) (term.Grid, error) {
	start := "-"
	switch {
	case history == 0:
		start = "0"
	case history > 0:
		start = "-" + strconv.Itoa(history)
	}
	// One invocation, so the size, cursor and text belong together.
	out, err := b.run(ctx, name,
		"display-message", "-p", "-t", target(name), screenFormat, ";",
		"capture-pane", "-p", "-e", "-N", "-S", start, "-t", target(name))
	if err != nil {
		return term.Grid{}, fmt.Errorf("screen: %w", err)
	}
	head, text, _ := strings.Cut(out, "\n")
	var cols, rows, cx, cy, visible, hsize int
	if _, err := fmt.Sscan(head, &cols, &rows, &cx, &cy, &visible, &hsize); err != nil {
		return term.Grid{}, fmt.Errorf("tmux: screen %s: %q: %w", name, head, err)
	}
	return render(cols, rows, cx, cy, visible == 1, history, text)
}

// render replays captured lines (history first, then the screen) into an
// emulator of the pane's size and places the cursor.
func render(cols, rows, cx, cy int, visible bool, history int, text string) (term.Grid, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	e := term.NewEmulator(cols, rows, max(len(lines)-rows, 1))
	defer e.Close()
	// tmux carries attributes from one line to the next, and -N pads every
	// line to the full width, so plain line breaks rebuild every cell.
	var buf bytes.Buffer
	for i, l := range lines {
		if i > 0 {
			buf.WriteString("\r\n")
		}
		buf.WriteString(l)
	}
	fmt.Fprintf(&buf, "\x1b[0m\x1b[%d;%dH", cy+1, min(cx, cols-1)+1)
	if !visible {
		buf.WriteString("\x1b[?25l")
	}
	if _, err := e.Write(buf.Bytes()); err != nil {
		return term.Grid{}, err
	}
	return e.Grid(history), nil
}

// Input implements term.Backend with send-keys -H, so every byte arrives
// unchanged.
func (b *Backend) Input(ctx context.Context, name string, data []byte) error {
	if _, err := b.running(ctx, name); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	const chunk = 512
	for len(data) > 0 {
		n := min(len(data), chunk)
		args := []string{"send-keys", "-t", target(name), "-H"}
		for _, c := range data[:n] {
			args = append(args, hex.EncodeToString([]byte{c}))
		}
		if _, err := b.run(ctx, name, args...); err != nil {
			return fmt.Errorf("input: %w", err)
		}
		data = data[n:]
	}
	return nil
}

// Resize implements term.Backend.
func (b *Backend) Resize(ctx context.Context, name string, cols, rows int) error {
	if _, err := b.running(ctx, name); err != nil {
		return fmt.Errorf("resize: %w", err)
	}
	if _, err := b.run(ctx, name, "resize-window", "-t", target(name),
		"-x", strconv.Itoa(max(cols, 1)), "-y", strconv.Itoa(max(rows, 1))); err != nil {
		return fmt.Errorf("resize: %w", err)
	}
	return nil
}

// Stop implements term.Backend: SIGTERM to the program's process group,
// then after grace SIGKILL. The dead pane stays, with its exit status,
// until Forget; only a pane that will not die is killed with its session.
func (b *Backend) Stop(ctx context.Context, name string, grace time.Duration) error {
	p, err := b.pane(ctx, name)
	if err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if p.dead {
		return nil
	}
	if p.pid > 0 {
		_ = syscall.Kill(-p.pid, syscall.SIGTERM)
		dead, err := b.waitDead(ctx, name, grace)
		if err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		// Children left in the group go too, whether or not the program
		// itself obeyed.
		if syscall.Kill(-p.pid, 0) == nil {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
		if !dead {
			if dead, err = b.waitDead(ctx, name, killWait); err != nil {
				return fmt.Errorf("stop: %w", err)
			}
		}
		if dead {
			return nil
		}
	}
	if _, err := b.run(ctx, name, "kill-session", "-t", target(name)); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// waitDead polls until the pane is dead or d passes. A session gone
// altogether counts as dead.
func (b *Backend) waitDead(ctx context.Context, name string, d time.Duration) (bool, error) {
	deadline := time.Now().Add(d)
	for {
		p, err := b.pane(ctx, name)
		if errors.Is(err, term.ErrNoSession) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if p.dead {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(min(50*time.Millisecond, time.Until(deadline)+time.Millisecond)):
		}
	}
}

// Forget implements term.Backend. A live session cannot be forgotten.
func (b *Backend) Forget(ctx context.Context, name string) error {
	p, err := b.pane(ctx, name)
	if err != nil {
		return fmt.Errorf("forget: %w", err)
	}
	if !p.dead {
		return fmt.Errorf("forget %s: session is alive", name)
	}
	if _, err := b.run(ctx, name, "kill-session", "-t", target(name)); err != nil {
		return fmt.Errorf("forget: %w", err)
	}
	return nil
}
