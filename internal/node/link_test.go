package node_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestBackoff(t *testing.T) {
	var got []time.Duration
	for n := 0; n < 8; n++ {
		got = append(got, node.Backoff(n))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i := range want {
		want[i] *= time.Second
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backoff %v, want %v", got, want)
	}
}

// fakeHost is a host side of the node channel: it checks the token, sends
// assign on attach and records what the node says.
type fakeHost struct {
	mu      sync.Mutex
	token   string
	assign  []wire.HostedSession
	refuse  bool
	hellos  []wire.NodeHello
	states  [][]wire.HostedNodeState
	conns   []*websocket.Conn
	attachs int
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	refuse := h.refuse || r.Header.Get("Authorization") != "Bearer "+h.token
	h.mu.Unlock()
	if refuse || r.URL.Path != "/api/node/socket" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	h.mu.Lock()
	h.conns = append(h.conns, ws)
	h.attachs++
	recs := h.assign
	h.mu.Unlock()
	if err := wsjson.Write(r.Context(), ws, wire.NodeFrame{Type: wire.NodeAssign, Sessions: recs}); err != nil {
		return
	}
	for {
		var f wire.NodeFrame
		if err := wsjson.Read(r.Context(), ws, &f); err != nil {
			return
		}
		h.mu.Lock()
		switch f.Type {
		case wire.NodeHelloType:
			h.hellos = append(h.hellos, *f.Hello)
		case wire.NodeStateType:
			h.states = append(h.states, f.States)
		}
		h.mu.Unlock()
	}
}

// drop closes every open node socket.
func (h *fakeHost) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		c.CloseNow()
	}
	h.conns = nil
}

func (h *fakeHost) lastState(id string) (wire.HostedNodeState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.states) == 0 {
		return wire.HostedNodeState{}, false
	}
	for _, s := range h.states[len(h.states)-1] {
		if s.ID == id {
			return s, true
		}
	}
	return wire.HostedNodeState{}, false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sleeps records reconnect waits instead of sleeping (a short real pause
// keeps the loop from spinning).
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Millisecond):
		return nil
	}
}

func (s *sleeps) waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func runLink(t *testing.T, l *node.Link) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestLinkBacksOffUntilTheHostTakesIt(t *testing.T) {
	r := newRig(t)
	host := &fakeHost{token: "sbn_t", refuse: true}
	srv := httptest.NewServer(host)
	defer srv.Close()
	// A session from an earlier node process the host has not assigned
	// yet: it must survive until the first assign says otherwise.
	if err := r.fake.Launch(context.Background(), term.Spec{Name: "sb-old", Argv: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	n := r.node()
	sl := &sleeps{}
	runLink(t, &node.Link{Node: n, Host: srv.URL, Token: "sbn_t", Interval: 10 * time.Millisecond, Sleep: sl.sleep, Logf: t.Logf,
		Hello: wire.NodeHello{Name: "pc", Machine: "pc-1", OS: "linux", Backend: "fake"}})
	waitFor(t, "seven refused attempts", func() bool { return len(sl.waits()) >= 7 })
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if got := sl.waits()[:7]; !slices.Equal(got, want) {
		t.Fatalf("waits %v, want %v", got, want)
	}
	if len(r.fake.Stops()) != 0 {
		t.Fatalf("a node with no assign stopped %v", r.fake.Stops())
	}

	host.mu.Lock()
	host.refuse = false
	host.assign = []wire.HostedSession{rec("a")}
	host.mu.Unlock()
	waitFor(t, "a running", func() bool {
		s, ok := host.lastState("a")
		return ok && s.Runtime.State == node.ObservedRunning && s.Launched
	})
	host.mu.Lock()
	if len(host.hellos) == 0 || host.hellos[0].Name != "pc" || host.hellos[0].Backend != "fake" {
		t.Errorf("hello %+v", host.hellos)
	}
	host.mu.Unlock()
	if st := r.fake.Stops(); len(st) != 1 || st[0].Name != "sb-old" {
		t.Errorf("stops after the first assign %v", st)
	}

	// The host goes away: the node keeps the session, retries after 1 s
	// (it had attached), and reports again on re-attach.
	n0 := len(sl.waits())
	host.mu.Lock()
	host.refuse = true
	host.mu.Unlock()
	host.drop()
	waitFor(t, "a retry", func() bool { return len(sl.waits()) > n0 })
	if got := sl.waits()[n0]; got != time.Second {
		t.Errorf("first wait after a drop %v", got)
	}
	launches := len(r.fake.Launches())
	time.Sleep(50 * time.Millisecond)
	if infos, _ := r.fake.List(context.Background()); !slices.ContainsFunc(infos, func(in term.Info) bool { return in.Name == "sb-a" && in.Alive }) {
		t.Fatal("session died while the host was away")
	}
	host.mu.Lock()
	host.refuse = false
	host.states = nil
	host.mu.Unlock()
	waitFor(t, "state after re-attach", func() bool { _, ok := host.lastState("a"); return ok })
	if len(r.fake.Launches()) != launches {
		t.Error("re-attach relaunched")
	}
}

func TestLinkRestart(t *testing.T) {
	r := newRig(t)
	host := &fakeHost{token: "sbn_t", assign: []wire.HostedSession{rec("a")}}
	srv := httptest.NewServer(host)
	defer srv.Close()
	runLink(t, &node.Link{Node: r.node(), Host: srv.URL, Token: "sbn_t", Interval: 10 * time.Millisecond, Logf: t.Logf})
	waitFor(t, "a running", func() bool { s, ok := host.lastState("a"); return ok && s.Runtime.State == node.ObservedRunning })
	host.mu.Lock()
	c := host.conns[0]
	host.mu.Unlock()
	if err := wsjson.Write(context.Background(), c, wire.NodeFrame{Type: wire.NodeRestart, ID: "a"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a relaunched", func() bool { return len(r.fake.Launches()) == 2 })
	if l := r.launches(); l[1][1] != "--resume" {
		t.Errorf("restart launched %v", l[1])
	}
	waitFor(t, "two runs reported", func() bool { s, ok := host.lastState("a"); return ok && s.Runtime.Run == 2 && s.Runtime.Restarts == 1 })
}
