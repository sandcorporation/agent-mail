package node_test

import (
	"context"
	"errors"
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

// fakeHost is a host side of the node channel: it checks the token (401
// when wrong), answers 503 while refuse is set, sends assign on attach and
// records what the node says.
type fakeHost struct {
	mu      sync.Mutex
	token   string
	assign  []wire.HostedSession
	refuse  bool
	hellos  []wire.NodeHello
	states  [][]wire.HostedNodeState
	notices []wire.NodeNotice
	conns   []*websocket.Conn
	attachs int
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	refuse := h.refuse
	h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+h.token || r.URL.Path != "/api/node/socket" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if refuse {
		// A host not taking nodes yet; a 401 would mean the token is revoked.
		http.Error(w, "starting", http.StatusServiceUnavailable)
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
		case wire.NodeNoticeType:
			h.notices = append(h.notices, *f.Notice)
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

// A notice goes to the host once per connection while the node holds it.
func TestLinkSendsNoticesOncePerConnection(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	for range node.CrashLoopRuns {
		r.exitAfter(n, time.Minute, 1)
		if r.state(n, "a").Observed == node.ObservedBackingOff {
			r.waitOut(n)
		}
	}
	if r.state(n, "a").Observed != node.ObservedCrashLoop {
		t.Fatalf("no crash loop: %+v", r.state(n, "a"))
	}
	host := &fakeHost{token: "sbn_t", assign: []wire.HostedSession{a}}
	srv := httptest.NewServer(host)
	defer srv.Close()
	runLink(t, &node.Link{Node: n, Host: srv.URL, Token: "sbn_t", Interval: 10 * time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }, Logf: t.Logf})
	count := func() int { host.mu.Lock(); defer host.mu.Unlock(); return len(host.notices) }
	waitFor(t, "the notice", func() bool { return count() == 1 })
	waitFor(t, "a few state reports", func() bool { host.mu.Lock(); defer host.mu.Unlock(); return len(host.states) > 0 })
	host.drop()
	waitFor(t, "the notice again after reconnecting", func() bool { return count() == 2 })
	host.mu.Lock()
	got := host.notices[0]
	host.mu.Unlock()
	if got.Kind != wire.NoticeCrashLoop || got.ID != "a" || got.Run != 5 || got.Runs != 5 {
		t.Fatalf("notice %+v", got)
	}
}

// A host that is not there (connection refused, as while it restarts)
// keeps the node retrying with backoff; only a 401 stops it (#203).
func TestLinkRetriesWhileTheHostIsDown(t *testing.T) {
	r := newRig(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	sl := &sleeps{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&node.Link{Node: r.node(), Host: url, Token: "sbn_t", Sleep: sl.sleep, Logf: t.Logf}).Run(ctx)
	}()
	waitFor(t, "four retries", func() bool { return len(sl.waits()) >= 4 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

// A 401 on the first connect (a revoked or replaced token) stops the node
// without a retry.
func TestLinkStopsOnUnauthorized(t *testing.T) {
	r := newRig(t)
	srv := httptest.NewServer(&fakeHost{token: "sbn_other"})
	defer srv.Close()
	sl := &sleeps{}
	done := make(chan error, 1)
	go func() {
		done <- (&node.Link{Node: r.node(), Host: srv.URL, Token: "sbn_t", Sleep: sl.sleep, Logf: t.Logf}).Run(context.Background())
	}()
	select {
	case err := <-done:
		if !errors.Is(err, node.ErrRevoked) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kept retrying on 401")
	}
	if w := sl.waits(); len(w) != 0 {
		t.Fatalf("waited %v", w)
	}
}
