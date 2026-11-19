package conpty_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

// TestNodeOverKeeper runs the node's reconcile loop over the keeper client:
// a node restart adopts the live session without relaunching it, and a
// keeper replaced for its version is the relaunch path (--resume of the
// same conversation).
func TestNodeOverKeeper(t *testing.T) {
	var (
		mu      sync.Mutex
		current *keeperRig
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
	connect := func(version string) *conpty.Client {
		t.Helper()
		c, _, err := conpty.Connect(ctx(t), conpty.ConnectOptions{Dial: dial, Version: version, Grace: time.Second, Wait: 2 * time.Second,
			Start: func() error {
				mu.Lock()
				defer mu.Unlock()
				current = newKeeper(t, version)
				return nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	rec := node.Record{
		ID: "a", Profile: "team-worker", Directory: "/work/a", AgentSessionID: "conv-a",
		Restart: node.RestartAlways, Desired: node.DesiredRunning,
	}
	cfg := node.Config{Agent: "/opt/agent/claude", Machine: "pc-1", Endpoint: "http://127.0.0.1:8090"}
	run := func(c *conpty.Client, r node.Record) *node.Node {
		t.Helper()
		n, err := node.New(cfg, c)
		if err != nil {
			t.Fatal(err)
		}
		n.Assign([]node.Record{r})
		for range 2 {
			if err := n.Reconcile(ctx(t)); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}

	n1 := run(connect("v1"), rec)
	first := current
	if len(first.sp.specs) != 1 || first.sp.specs[0].Argv[1] != "--session-id" {
		t.Fatalf("first launch %+v", first.sp.specs)
	}
	pid := n1.States()[0].PID

	// Node restart: same keeper, session adopted.
	n2 := run(connect("v1"), rec)
	if current != first || len(first.sp.specs) != 1 {
		t.Fatalf("relaunched on node restart: %+v", first.sp.specs)
	}
	if st := n2.States()[0]; st.Observed != node.ObservedRunning || st.PID != pid {
		t.Fatalf("adopted state %+v", st)
	}

	// New switchboard version: the keeper is replaced and the session
	// resumes the same conversation in the new one.
	rec.Launched = true
	run(connect("v2"), rec)
	if current == first {
		t.Fatal("keeper not replaced")
	}
	select {
	case <-first.sp.proc("sb-a").exited:
	default:
		t.Fatal("old keeper's child still runs")
	}
	if got := current.sp.specs; len(got) != 1 || got[0].Argv[1] != "--resume" || got[0].Argv[2] != "conv-a" {
		t.Fatalf("relaunch %+v", got)
	}
}
