//go:build unix

package tmux

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// deadSub names the control-mode subscription that reports the pane's death.
const deadSub = "sbdead"

// Stream implements term.Backend with a read-only control-mode client
// (tmux -C attach -r) that lives only as long as the returned reader: its
// %output lines are decoded to the program's bytes. The reader ends with
// io.EOF when the program exits or the session goes. tmux 3.5 drops output
// printed right before the program exits from control mode; Screen has it.
func (b *Backend) Stream(ctx context.Context, name string) (io.ReadCloser, error) {
	if _, err := b.running(ctx, name); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	cmd := b.command(nil, "-C", "attach-session", "-r", "-t", "="+name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("stream %s: %w", name, err)
	}
	// Pane death does not end a control client (the session stays with
	// remain-on-exit); a subscription reports it.
	if _, err := io.WriteString(stdin, "refresh-client -B '"+deadSub+"::#{pane_dead}'\n"); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("stream %s: %w", name, err)
	}
	pr, pw := io.Pipe()
	s := &stream{pr: pr, stdin: stdin, cmd: cmd, done: make(chan struct{})}
	go s.pump(stdout, pw)
	s.stopWatch = context.AfterFunc(ctx, func() { _ = s.Close() })
	return s, nil
}

type stream struct {
	pr        *io.PipeReader
	stdin     io.WriteCloser
	cmd       *exec.Cmd
	done      chan struct{}
	stopWatch func() bool
	once      sync.Once
}

func (s *stream) Read(p []byte) (int, error) { return s.pr.Read(p) }

// Close ends the control client and the reader.
func (s *stream) Close() error {
	s.once.Do(func() {
		if s.stopWatch != nil {
			s.stopWatch()
		}
		_ = s.pr.Close()
		// Closing stdin detaches the control client; kill it if it lingers.
		_ = s.stdin.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
	return nil
}

// deadSettle is how long the stream keeps reading after the pane dies: tmux
// can report the death before the program's last output.
const deadSettle = 200 * time.Millisecond

type event int

const (
	evNone event = iota
	evOutput
	evDead
	evEnd
)

// pump decodes the control client's output into pw until the program exits,
// the client ends, or the reader is closed.
func (s *stream) pump(stdout io.Reader, pw *io.PipeWriter) {
	defer close(s.done)
	lines := make(chan []byte)
	stop := make(chan struct{})
	go func() {
		defer close(lines)
		r := bufio.NewReaderSize(stdout, 64*1024)
		discard := false
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 && !discard {
				select {
				case lines <- bytes.TrimSuffix(line, []byte("\n")):
				case <-stop:
					discard = true
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var settle <-chan time.Time
loop:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			switch s.handle(line, pw) {
			case evEnd:
				break loop
			case evDead:
				if settle == nil {
					settle = time.After(deadSettle)
				}
			case evOutput:
				if settle != nil {
					settle = time.After(deadSettle)
				}
			}
		case <-settle:
			break loop
		}
	}
	_ = pw.Close()
	close(stop)
	// Detach, and drain so the client never blocks on a full pipe while
	// exiting.
	_ = s.stdin.Close()
	for range lines {
	}
	_ = s.cmd.Wait()
}

// handle processes one control-mode line.
func (s *stream) handle(line []byte, pw *io.PipeWriter) event {
	switch {
	case bytes.HasPrefix(line, []byte("%output ")):
		// %output %<pane> <data>
		rest := line[len("%output "):]
		i := bytes.IndexByte(rest, ' ')
		if i < 0 {
			return evNone
		}
		if _, err := pw.Write(unescape(rest[i+1:])); err != nil {
			return evEnd // the reader is closed
		}
		return evOutput
	case bytes.HasPrefix(line, []byte("%subscription-changed "+deadSub+" ")):
		// %subscription-changed <name> $s @w i %p : <value>
		if _, v, ok := strings.Cut(string(line), " : "); ok && strings.TrimSpace(v) == "1" {
			return evDead
		}
	case bytes.Equal(line, []byte("%exit")) || bytes.HasPrefix(line, []byte("%exit ")):
		return evEnd
	}
	return evNone
}

// unescape decodes control mode's escaping: bytes below space and the
// backslash come as \ooo (three octal digits); everything else is raw.
func unescape(b []byte) []byte {
	if bytes.IndexByte(b, '\\') < 0 {
		return bytes.Clone(b)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+3 < len(b) && isOctal(b[i+1]) && isOctal(b[i+2]) && isOctal(b[i+3]) {
			out = append(out, (b[i+1]-'0')<<6|(b[i+2]-'0')<<3|(b[i+3]-'0'))
			i += 3
			continue
		}
		out = append(out, b[i])
	}
	return out
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
