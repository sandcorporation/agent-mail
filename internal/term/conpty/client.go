package conpty

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
)

// Dialer connects to the keeper: the named pipe on Windows, any stream
// socket in tests.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is term.Backend over the keeper's pipe. It keeps one control
// connection (requests answered in order) and opens one connection per
// stream.
type Client struct {
	dial Dialer

	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	seq  uint64
}

var _ term.Backend = (*Client)(nil)

// NewClient returns a client that dials the keeper on first use.
func NewClient(dial Dialer) *Client { return &Client{dial: dial} }

// Close drops the control connection. The keeper and its sessions stay.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropLocked()
}

func (c *Client) dropLocked() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.r = nil, nil
	return err
}

// call sends one request and reads its answer. A request that cannot be
// written on a kept connection (the keeper was replaced since) is sent once
// more on a fresh one; nothing is retried once written.
// closedByPeer reports a connection the other end closed: a clean EOF, or on
// Windows a reset or abort of a socket closed with data still unread.
func closedByPeer(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	for _, e := range peerResets {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func (c *Client) call(ctx context.Context, req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	req.ID = c.seq
	for attempt := 0; ; attempt++ {
		fresh := c.conn == nil
		if fresh {
			conn, err := c.dial(ctx)
			if err != nil {
				return response{}, fmt.Errorf("keeper: dial: %w", err)
			}
			c.conn, c.r = conn, bufio.NewReader(conn)
		}
		conn := c.conn
		stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
		err := writeJSON(conn, req)
		if err != nil {
			stop()
			_ = c.dropLocked()
			if !fresh && attempt == 0 && ctx.Err() == nil {
				continue
			}
			return response{}, fmt.Errorf("keeper: %s: %w", req.Op, ctxErr(ctx, err))
		}
		var resp response
		err = readJSON(c.r, &resp)
		stop()
		if closedByPeer(err) && !fresh && attempt == 0 && ctx.Err() == nil {
			// The keeper answers every request it reads before reading the
			// next, so a kept connection closed without an answer was
			// closed by a keeper that went away (replaced or restarted)
			// without running the request.
			_ = c.dropLocked()
			continue
		}
		if err == nil && resp.ID != req.ID {
			err = fmt.Errorf("answer %d to request %d", resp.ID, req.ID)
		}
		if err != nil {
			_ = c.dropLocked()
			return response{}, fmt.Errorf("keeper: %s: %w", req.Op, ctxErr(ctx, err))
		}
		_ = conn.SetDeadline(time.Time{})
		return resp, resp.err()
	}
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// KeeperInfo is the keeper's answer to "version".
type KeeperInfo struct {
	Version  string
	Protocol int
	PID      int
}

// Version reports the keeper's switchboard version, protocol and process.
func (c *Client) Version(ctx context.Context) (KeeperInfo, error) {
	resp, err := c.call(ctx, request{Op: OpVersion})
	return KeeperInfo{Version: resp.Version, Protocol: resp.Protocol, PID: resp.PID}, err
}

// Shutdown asks the keeper to stop every session (grace each) and exit. It
// returns once the keeper has answered; the keeper exits after its
// children.
func (c *Client) Shutdown(ctx context.Context, grace time.Duration) error {
	_, err := c.call(ctx, request{Op: OpShutdown, GraceMS: grace.Milliseconds()})
	c.mu.Lock()
	_ = c.dropLocked()
	c.mu.Unlock()
	return err
}

// Launch implements term.Backend.
func (c *Client) Launch(ctx context.Context, s term.Spec) error {
	_, err := c.call(ctx, request{Op: OpLaunch, Spec: &s})
	return err
}

// List implements term.Backend.
func (c *Client) List(ctx context.Context) ([]term.Info, error) {
	resp, err := c.call(ctx, request{Op: OpList})
	return resp.Sessions, err
}

// Screen implements term.Backend.
func (c *Client) Screen(ctx context.Context, name string, history int) (term.Grid, error) {
	resp, err := c.call(ctx, request{Op: OpScreen, Name: name, History: history})
	if err != nil {
		return term.Grid{}, err
	}
	g, err := decodeGrid(resp.Grid)
	if err != nil {
		return term.Grid{}, fmt.Errorf("keeper: screen: %w", err)
	}
	return g, nil
}

// Input implements term.Backend.
func (c *Client) Input(ctx context.Context, name string, b []byte) error {
	_, err := c.call(ctx, request{Op: OpInput, Name: name, Data: b})
	return err
}

// Resize implements term.Backend.
func (c *Client) Resize(ctx context.Context, name string, cols, rows int) error {
	_, err := c.call(ctx, request{Op: OpResize, Name: name, Cols: cols, Rows: rows})
	return err
}

// Stop implements term.Backend.
func (c *Client) Stop(ctx context.Context, name string, grace time.Duration) error {
	_, err := c.call(ctx, request{Op: OpStop, Name: name, GraceMS: grace.Milliseconds()})
	return err
}

// Forget implements term.Backend.
func (c *Client) Forget(ctx context.Context, name string) error {
	_, err := c.call(ctx, request{Op: OpForget, Name: name})
	return err
}

// ErrDropped ends a stream whose reader fell behind the session's output;
// the viewer reconnects to a fresh snapshot.
var ErrDropped = errors.New("keeper: stream dropped: reader too slow")

// Stream implements term.Backend over a connection of its own.
func (c *Client) Stream(ctx context.Context, name string) (io.ReadCloser, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("keeper: dial: %w", err)
	}
	r := bufio.NewReader(conn)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	err = writeJSON(conn, request{ID: 1, Op: OpStream, Name: name})
	var resp response
	if err == nil {
		err = readJSON(r, &resp)
	}
	stop()
	if err == nil {
		err = resp.err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, ctxErr(ctx, err)
	}
	_ = conn.SetDeadline(time.Time{})
	s := &stream{conn: conn, r: r}
	s.stop = context.AfterFunc(ctx, s.shut)
	return s, nil
}

// stream reads a stream connection's output frames.
type stream struct {
	conn net.Conn
	r    *bufio.Reader
	stop func() bool

	closed atomic.Bool

	mu  sync.Mutex
	buf []byte
	err error
}

func (s *stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		kind, b, err := readFrame(s.r)
		switch {
		case s.closed.Load():
			s.err = io.ErrClosedPipe
		case err != nil:
			s.err = fmt.Errorf("keeper: stream: %w", err)
		case kind == frameOutput:
			s.buf = b
		case kind == frameEnd && string(b) == endDropped:
			s.err = ErrDropped
		case kind == frameEnd:
			s.err = io.EOF
		default:
			s.err = fmt.Errorf("keeper: stream: unexpected frame %q", kind)
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// Close ends the stream; a blocked Read returns io.ErrClosedPipe.
func (s *stream) Close() error {
	s.stop()
	s.shut()
	return nil
}

func (s *stream) shut() {
	s.closed.Store(true)
	_ = s.conn.Close()
}
