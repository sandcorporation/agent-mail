package node_test

import (
	"testing"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/wire"
)

func eventKinds(n *node.Node, id string) []string {
	var out []string
	for _, e := range n.Events() {
		if e.ID == id {
			out = append(out, e.Kind+"/"+e.Actor)
		}
	}
	return out
}

// start → crash → automatic restart → stop are four events in order, with
// the exit code on the crash and the restart count after each (#252).
func TestEventsStartCrashRestartStop(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	if err := r.fake.Exit("sb-a", 3); err != nil {
		t.Fatal(err)
	}
	r.relaunch(n)
	a.Desired = node.DesiredStopped
	n.Assign([]node.Record{a})
	r.reconcile(n)
	r.reconcile(n) // the stopped session's exit is not a crash
	ev := n.Events()
	want := []string{"start/node", "crash/", "restart/policy", "stop/admin"}
	if got := eventKinds(n, "a"); !equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}
	if ev[1].ExitCode == nil || *ev[1].ExitCode != 3 {
		t.Errorf("crash exit %v", ev[1].ExitCode)
	}
	if ev[0].Restarts != 0 || ev[2].Restarts != 1 || ev[3].Restarts != 1 {
		t.Errorf("restarts %d %d %d", ev[0].Restarts, ev[2].Restarts, ev[3].Restarts)
	}
	for i := 1; i < len(ev); i++ {
		if ev[i].At < ev[i-1].At {
			t.Errorf("event %d at %d before %d", i, ev[i].At, ev[i-1].At)
		}
	}
}

// Start and Restart asked for by the host, a clean exit, and a session a
// new node process finds already running (keeper adoption after `up`
// restarts) are their own kinds.
func TestEventsAdminRequestsExitAndAdopt(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	a.Restart = "on-failure"
	n := r.node(a)
	r.reconcile(n)
	a.RestartGen++
	n.Assign([]node.Record{a})
	r.reconcile(n)
	if err := r.fake.Exit("sb-a", 0); err != nil {
		t.Fatal(err)
	}
	r.reconcile(n)
	a.StartGen++
	n.Assign([]node.Record{a})
	r.reconcile(n)
	want := []string{"start/node", "restart/admin", "exit/", "start/admin"}
	if got := eventKinds(n, "a"); !equal(got, want) {
		t.Fatalf("events %q, want %q", got, want)
	}

	// A new node process over the same backend adopts the live session.
	a.Launched = true
	n2 := r.node(a)
	r.reconcile(n2)
	if got := eventKinds(n2, "a"); !equal(got, []string{wire.HostedEventAdopt + "/node"}) {
		t.Fatalf("after a node restart: %q", got)
	}
}

// A node keeps only the newest events per session.
func TestEventsAreBoundedOnTheNode(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	for range node.EventKeep + 5 {
		a.RestartGen++
		n.Assign([]node.Record{a})
		r.reconcile(n)
	}
	if got := len(n.Events()); got != node.EventKeep {
		t.Fatalf("%d events kept, want %d", got, node.EventKeep)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
