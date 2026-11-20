package node

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Reconnect backoff bounds for the node channel.
const (
	BackoffMin = time.Second
	BackoffMax = 30 * time.Second
)

// ReconcileInterval is how often an attached node reconciles by default.
const ReconcileInterval = time.Second

const (
	linkDialTimeout  = 10 * time.Second
	linkWriteTimeout = 10 * time.Second
	linkReadLimit    = 8 << 20
)

// Backoff is the wait before reconnect attempt n (0 for the first after a
// drop): 1 s, doubling, capped at 30 s.
func Backoff(n int) time.Duration {
	d := BackoffMin
	for i := 0; i < n && d < BackoffMax; i++ {
		d *= 2
	}
	return min(d, BackoffMax)
}

// Link attaches a Node to its host over the node channel (GET
// /api/node/socket with the node token): it takes the host's assign and
// restart frames and reports hello and state. While the channel is down it
// keeps enforcing the last assign and reconnects with Backoff. Until the
// first assign arrives it does not reconcile, so a starting node never stops
// sessions it has not been told about yet.
type Link struct {
	Node  *Node
	Host  string // the host's base URL, http://127.0.0.1:8090
	Token string
	Hello wire.NodeHello
	// Interval is the reconcile period (default ReconcileInterval).
	Interval time.Duration
	// Sleep waits between reconnects (nil: a timer); tests inject it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Logf logs connection changes (nil: log.Printf).
	Logf func(format string, a ...any)

	once     sync.Once
	assigned chan struct{} // closed at the first assign
	kick     chan struct{} // reconcile now
	changed  chan struct{} // states changed

	mu     sync.Mutex
	states []wire.HostedNodeState
	limit  wire.NodeLimit
}

// ErrRevoked is Run's error when the host answers the node channel with
// 401: the node's token is revoked (or was never the host's). Retrying
// cannot help, so the node stops; its hosted terminals are left as they are
// (#203).
var ErrRevoked = errors.New("the host refused this node's token (HTTP 401): the node was revoked")

// SocketURL is the node channel URL for a host base URL.
func SocketURL(host string) (string, error) {
	host = strings.TrimRight(host, "/")
	switch {
	case strings.HasPrefix(host, "http://"):
		return "ws://" + strings.TrimPrefix(host, "http://") + "/api/node/socket", nil
	case strings.HasPrefix(host, "https://"):
		return "wss://" + strings.TrimPrefix(host, "https://") + "/api/node/socket", nil
	}
	return "", errors.New("node: host URL must start with http:// or https://")
}

// Run keeps the node attached until ctx is done.
func (l *Link) Run(ctx context.Context) error {
	url, err := SocketURL(l.Host)
	if err != nil {
		return err
	}
	if l.Interval <= 0 {
		l.Interval = ReconcileInterval
	}
	if l.Sleep == nil {
		l.Sleep = sleep
	}
	if l.Logf == nil {
		l.Logf = log.Printf
	}
	// The reconcile loop ends with Run, so a node that stops (revoked) no
	// longer enforces the last assign; the sessions themselves stay.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.assigned = make(chan struct{})
	l.kick = make(chan struct{}, 1)
	l.changed = make(chan struct{}, 1)
	go l.reconcile(ctx)
	for attempt := 0; ; attempt++ {
		attached, err := l.session(ctx, url)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrRevoked) {
			l.Logf("node: %v; hosted terminals are left as they are; stopping", err)
			return err
		}
		if attached {
			attempt = 0
		}
		d := Backoff(attempt)
		l.Logf("node: host channel down (%v); retrying in %s", err, d)
		if err := l.Sleep(ctx, d); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// reconcile runs the node's loop once the first assign is in.
func (l *Link) reconcile(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-l.assigned:
	}
	t := time.NewTicker(l.Interval)
	defer t.Stop()
	for {
		if err := l.Node.Reconcile(ctx); err != nil && ctx.Err() == nil {
			l.Logf("node: %v", err)
		}
		l.report()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.kick:
		}
	}
}

// report stores the node's states for the host and flags a change.
func (l *Link) report() {
	sts := l.Node.States()
	out := make([]wire.HostedNodeState, 0, len(sts))
	for _, s := range sts {
		out = append(out, wire.HostedNodeState{ID: s.ID, Launched: s.Launched, AgentSessionID: s.AgentSessionID, Runtime: s.Wire()})
	}
	limit := l.Node.Limit()
	l.mu.Lock()
	l.states, l.limit = out, limit
	l.mu.Unlock()
	signal(l.changed)
}

// current is the state frame to send: every record's state and the usage
// limit.
func (l *Link) current() wire.NodeFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limit
	return wire.NodeFrame{Type: wire.NodeStateType, States: l.states, Limit: &limit}
}

// session runs one connection; attached reports whether it got as far as
// saying hello. Terminals the host attaches stream over it until it ends.
func (l *Link) session(ctx context.Context, url string) (attached bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, linkDialTimeout)
	ws, resp, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + l.Token}}})
	cancel()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return false, ErrRevoked
		}
		return false, err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(linkReadLimit)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	write := func(f wire.NodeFrame) error {
		wctx, done := context.WithTimeout(sctx, linkWriteTimeout)
		defer done()
		return wsjson.Write(wctx, ws, f)
	}
	rl := newRelay(l.Node.backend, l.Node.owns, func(kind byte, id string, b []byte) error {
		wctx, done := context.WithTimeout(sctx, linkWriteTimeout)
		defer done()
		return ws.Write(wctx, websocket.MessageBinary, wire.EncodeTermFrame(kind, id, b))
	}, l.Logf)
	defer rl.stop()
	hello := l.Hello
	if err := write(wire.NodeFrame{Type: wire.NodeHelloType, Hello: &hello}); err != nil {
		return false, err
	}
	l.Logf("node: attached to %s", l.Host)
	readErr := make(chan error, 1)
	go func() {
		for {
			var f wire.NodeFrame
			if err := wsjson.Read(sctx, ws, &f); err != nil {
				readErr <- err
				stop()
				return
			}
			l.receive(ctx, f, sctx, rl)
		}
	}()
	sent := "\x00"
	told := map[string]bool{} // notices sent on this connection
	for {
		cur := l.current()
		b, _ := json.Marshal(cur)
		if string(b) != sent {
			if err := write(cur); err != nil {
				return true, err
			}
			sent = string(b)
		}
		for _, nt := range l.Node.Notices() {
			key := nt.Kind + ":" + nt.ID + ":" + strconv.Itoa(nt.Run)
			if told[key] {
				continue
			}
			if err := write(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &nt}); err != nil {
				return true, err
			}
			told[key] = true
		}
		select {
		case <-sctx.Done():
			select {
			case err := <-readErr:
				return true, err
			default:
				return true, sctx.Err()
			}
		case <-l.changed:
		}
	}
}

// receive applies one host frame; attach and detach drive rl, the relay of
// this connection (sctx).
func (l *Link) receive(ctx context.Context, f wire.NodeFrame, sctx context.Context, rl *relay) {
	switch f.Type {
	case wire.NodeAttach:
		rl.attach(sctx, f.ID)
	case wire.NodeDetach:
		rl.detach(f.ID)
	case wire.NodeInput:
		// In the read loop, so input keeps its order.
		if err := l.Node.Input(sctx, f.ID, f.Data); err != nil && !errors.Is(err, term.ErrNoSession) {
			l.Logf("node: input %s: %v", f.ID, err)
		}
	case wire.NodeResize:
		if err := l.Node.Resize(sctx, f.ID, f.Cols, f.Rows); err != nil && !errors.Is(err, term.ErrNoSession) {
			l.Logf("node: resize %s: %v", f.ID, err)
		}
	case wire.NodeAssign:
		l.Node.Assign(f.Sessions)
		l.once.Do(func() { close(l.assigned) })
		signal(l.kick)
	case wire.NodeRestart:
		go func() {
			if err := l.Node.Restart(ctx, f.ID); err != nil {
				l.Logf("node: restart %s: %v", f.ID, err)
			}
			l.report()
			signal(l.kick)
		}()
	}
}
