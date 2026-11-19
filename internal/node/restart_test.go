package node_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// exitAfter lets the session run for d, then ends it with code and passes
// the exit.
func (r *rig) exitAfter(n *node.Node, d time.Duration, code int) {
	r.t.Helper()
	r.now = r.now.Add(d)
	if err := r.fake.Exit("sb-a", code); err != nil {
		r.t.Fatal(err)
	}
	r.reconcile(n)
}

// waitOut moves the clock to the record's next attempt and reconciles.
func (r *rig) waitOut(n *node.Node) {
	r.t.Helper()
	st := r.state(n, "a")
	if st.Observed != node.ObservedBackingOff || st.NextAttemptAt.IsZero() {
		r.t.Fatalf("not backing off: %+v", st)
	}
	r.now = st.NextAttemptAt.Add(-time.Second)
	r.reconcile(n)
}

func noContinue(t *testing.T, launches [][]string) {
	t.Helper()
	for _, a := range launches {
		if slices.Contains(a, "--continue") || slices.Contains(a, "-c") {
			t.Fatalf("launch with --continue: %q", a)
		}
	}
}

func TestRestartBackoff(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second,
		64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := node.RestartBackoff(i + 1); got != w {
			t.Errorf("RestartBackoff(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestBackoffSequenceAndReset(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"))
	r.reconcile(n)
	for i, wait := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		r.exitAfter(n, 30*time.Second, 1)
		exited := r.now
		st := r.state(n, "a")
		if st.Observed != node.ObservedBackingOff || !st.NextAttemptAt.Equal(exited.Add(wait)) {
			t.Fatalf("exit %d: %+v, want next attempt in %s", i+1, st, wait)
		}
		if rt := st.Wire(); rt.NextAttemptAt == nil || *rt.NextAttemptAt != exited.Add(wait).UnixMilli() || rt.State != "backing off" {
			t.Fatalf("exit %d wire: %+v", i+1, rt)
		}
		launches := len(r.launches())
		r.now = exited.Add(wait - 2*time.Second)
		r.reconcile(n) // one second early
		if len(r.launches()) != launches {
			t.Fatalf("exit %d: relaunched before the backoff ended", i+1)
		}
		r.reconcile(n)
		if len(r.launches()) != launches+1 {
			t.Fatalf("exit %d: not relaunched after %s", i+1, wait)
		}
		if st := r.state(n, "a"); !st.NextAttemptAt.IsZero() {
			t.Fatalf("exit %d: next attempt kept after launch: %+v", i+1, st)
		}
	}

	// A run of ten minutes resets the backoff (and the crash-loop count).
	r.exitAfter(n, node.LongRun, 1)
	if st := r.state(n, "a"); !st.NextAttemptAt.Equal(r.now.Add(node.BackoffBase)) {
		t.Fatalf("after a long run: %+v", st)
	}
	r.waitOut(n)
	for range 4 {
		r.exitAfter(n, 30*time.Second, 1)
		r.waitOut(n)
	}
	if st := r.state(n, "a"); st.Observed == node.ObservedCrashLoop {
		t.Fatalf("crash loop counted runs before the long one: %+v", st)
	}
}

func TestCrashLoopAfterFiveShortRuns(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	for i := range 4 {
		r.exitAfter(n, time.Minute, 1)
		if st := r.state(n, "a"); st.Observed != node.ObservedBackingOff {
			t.Fatalf("short run %d: %+v", i+1, st)
		}
		if len(n.Notices()) != 0 {
			t.Fatalf("short run %d: notices %+v", i+1, n.Notices())
		}
		r.waitOut(n)
	}
	r.exitAfter(n, time.Minute, 7)
	st := r.state(n, "a")
	if st.Observed != node.ObservedCrashLoop || !st.NextAttemptAt.IsZero() || st.Wire().NextAttemptAt != nil {
		t.Fatalf("after five short runs: %+v", st)
	}
	want := []node.Notice{{ID: "a", Kind: wire.NoticeCrashLoop, Run: 5, AgentSessionID: "conv-a", LastExit: 7, Runs: 5}}
	if got := n.Notices(); !slices.Equal(got, want) {
		t.Fatalf("notices %+v, want %+v", got, want)
	}

	// No more attempts, however long it waits; still one notice.
	launches := len(r.launches())
	r.now = r.now.Add(time.Hour)
	r.reconcile(n)
	r.reconcile(n)
	if len(r.launches()) != launches || len(n.Notices()) != 1 || r.state(n, "a").Observed != node.ObservedCrashLoop {
		t.Fatalf("crash loop relaunched: %d launches, notices %+v", len(r.launches())-launches, n.Notices())
	}

	// Start clears it and launches at once, resuming the conversation.
	a.StartGen++
	n.Assign([]node.Record{a})
	r.reconcile(n)
	got := r.launches()
	if len(got) != launches+1 || got[len(got)-1][1] != "--resume" || got[len(got)-1][2] != "conv-a" {
		t.Fatalf("start after crash loop: %q", got[launches:])
	}
	if len(n.Notices()) != 0 || r.state(n, "a").Observed != node.ObservedStarting {
		t.Fatalf("crash loop kept: %+v %+v", r.state(n, "a"), n.Notices())
	}
	// And the count starts over.
	r.exitAfter(n, time.Second, 1)
	if st := r.state(n, "a"); st.Observed != node.ObservedBackingOff || !st.NextAttemptAt.Equal(r.now.Add(node.BackoffBase)) {
		t.Fatalf("after start: %+v", st)
	}
}

func TestRestartClearsCrashLoop(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	for range 4 {
		r.exitAfter(n, 30*time.Second, 1)
		r.waitOut(n)
	}
	r.exitAfter(n, 30*time.Second, 1)
	if r.state(n, "a").Observed != node.ObservedCrashLoop {
		t.Fatal("no crash loop")
	}
	if err := n.Restart(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if st := r.state(n, "a"); st.Observed != node.ObservedStarting || len(n.Notices()) != 0 {
		t.Fatalf("after restart: %+v %+v", st, n.Notices())
	}

	// The host's way: RestartGen grows on the record.
	for range 4 {
		r.exitAfter(n, 30*time.Second, 1)
		r.waitOut(n)
	}
	r.exitAfter(n, 30*time.Second, 1)
	a.RestartGen++
	n.Assign([]node.Record{a})
	launches := len(r.launches())
	r.reconcile(n)
	if len(r.launches()) != launches+1 || r.state(n, "a").Observed != node.ObservedStarting {
		t.Fatalf("restart gen: %d launches, %+v", len(r.launches())-launches, r.state(n, "a"))
	}
}

func TestRestartGenStopsAndResumesAtOnce(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	r.reconcile(n)
	a.RestartGen = 3
	n.Assign([]node.Record{a})
	r.reconcile(n)
	if stops := r.fake.Stops(); len(stops) != 1 || stops[0].Name != "sb-a" || stops[0].Grace != node.StopGrace {
		t.Fatalf("stops %+v", stops)
	}
	want := [][]string{
		{"/opt/agent/claude", "--session-id", "conv-a", "--model", "opus"},
		{"/opt/agent/claude", "--resume", "conv-a", "--model", "opus"},
	}
	if got := r.launches(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("launches %q", got)
	}
	// The same counter again is not another restart, nor is a new node
	// seeing it for the first time.
	r.reconcile(n)
	n2 := r.node(a)
	r.reconcile(n2)
	if len(r.launches()) != 2 {
		t.Fatalf("restarted again: %q", r.launches())
	}
}

func TestStartRelaunchesSessionStoppedByPolicy(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	a.Restart = node.RestartOnFailure
	n := r.node(a)
	r.reconcile(n)
	r.exitAfter(n, time.Minute, 0)
	r.reconcile(n)
	if st := r.state(n, "a"); st.Observed != node.ObservedStopped || len(r.launches()) != 1 {
		t.Fatalf("on-failure exit 0: %+v", st)
	}
	a.StartGen++
	n.Assign([]node.Record{a})
	r.reconcile(n)
	if got := r.launches(); len(got) != 2 || got[1][1] != "--resume" {
		t.Fatalf("start: %q", got)
	}
}

func TestResumeFailureFallsBackToNewConversation(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	a.Launched = true
	n := r.node(a)
	r.reconcile(n)

	// A failed resume once is an ordinary exit.
	r.exitAfter(n, 3*time.Second, 1)
	if st := r.state(n, "a"); st.Observed != node.ObservedBackingOff || len(n.Notices()) != 0 {
		t.Fatalf("first failed resume: %+v %+v", st, n.Notices())
	}
	r.waitOut(n)

	// Twice in a row: the node stops and asks for a new conversation.
	r.exitAfter(n, 3*time.Second, 1)
	st := r.state(n, "a")
	if st.Observed != node.ObservedResumeFailed {
		t.Fatalf("second failed resume: %+v", st)
	}
	want := []node.Notice{{ID: "a", Kind: wire.NoticeResumeFailed, Run: 2, AgentSessionID: "conv-a", LastExit: 1, Runs: 2}}
	if got := n.Notices(); !slices.Equal(got, want) {
		t.Fatalf("notices %+v, want %+v", got, want)
	}
	launches := len(r.launches())
	r.now = r.now.Add(time.Hour)
	r.reconcile(n)
	if len(r.launches()) != launches {
		t.Fatal("launched before a new conversation arrived")
	}

	// The host assigns a new conversation id: it starts at once.
	a.AgentSessionID, a.Launched = "conv-b", false
	n.Assign([]node.Record{a})
	r.reconcile(n)
	got := r.launches()
	if last := got[len(got)-1]; len(got) != launches+1 || last[1] != "--session-id" || last[2] != "conv-b" {
		t.Fatalf("after new id: %q", got[launches:])
	}
	st = r.state(n, "a")
	if !st.ResumeFailed || !st.Wire().ResumeFailed || len(n.Notices()) != 0 || st.Observed != node.ObservedStarting {
		t.Fatalf("after new id: %+v %+v", st, n.Notices())
	}
	// Later launches resume the new conversation.
	r.exitAfter(n, time.Minute, 1)
	r.waitOut(n)
	if got := r.launches(); got[len(got)-1][1] != "--resume" || got[len(got)-1][2] != "conv-b" {
		t.Fatalf("later launch %q", got[len(got)-1])
	}
	noContinue(t, r.launches())
}

func TestResumeFailureNeedsTwoFastFailuresInARow(t *testing.T) {
	cases := []struct {
		name string
		ran  time.Duration
		code int
	}{
		{"slow", node.ResumeFailRun, 1},
		{"clean", time.Second, 0},
	}
	for _, c := range cases {
		r := newRig(t)
		a := rec("a")
		a.Launched = true
		n := r.node(a)
		r.reconcile(n)
		r.exitAfter(n, time.Second, 1)
		r.waitOut(n)
		r.exitAfter(n, c.ran, c.code)
		r.waitOut(n)
		r.exitAfter(n, time.Second, 1)
		if st := r.state(n, "a"); st.Observed != node.ObservedBackingOff || len(n.Notices()) != 0 {
			t.Errorf("%s: %+v %+v", c.name, st, n.Notices())
		}
	}
	// A --session-id run failing fast is not a resume failure.
	r := newRig(t)
	n := r.node(rec("a"))
	r.reconcile(n)
	r.exitAfter(n, time.Second, 1)
	if len(n.Notices()) != 0 {
		t.Fatalf("first launch counted: %+v", n.Notices())
	}
}

func TestNewConversationReplacesRunningSession(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	n := r.node(a)
	r.reconcile(n)
	r.reconcile(n)
	a.AgentSessionID, a.Launched = "conv-new", false
	n.Assign([]node.Record{a})
	r.reconcile(n)
	if stops := r.fake.Stops(); len(stops) != 1 || stops[0].Name != "sb-a" {
		t.Fatalf("stops %+v", stops)
	}
	got := r.launches()
	if len(got) != 2 || got[1][1] != "--session-id" || got[1][2] != "conv-new" {
		t.Fatalf("launches %q", got)
	}
	if st := r.state(n, "a"); st.ResumeFailed {
		t.Fatalf("asked-for conversation marked as a fallback: %+v", st)
	}
	r.reconcile(n)
	r.exitAfter(n, time.Minute, 1)
	r.waitOut(n)
	if got := r.launches(); len(got) != 3 || got[2][1] != "--resume" || got[2][2] != "conv-new" {
		t.Fatalf("launches %q", got)
	}
	noContinue(t, r.launches())
}

// A terminal holder that is gone (tmux server killed, keeper replaced)
// leaves no sessions: every record is launched again at once with resume.
func TestHolderLossRelaunchesWithResume(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"), rec("b"))
	r.reconcile(n)
	r.reconcile(n)
	for _, name := range []string{"sb-a", "sb-b"} {
		_ = r.fake.Stop(context.Background(), name, 0)
		_ = r.fake.Forget(context.Background(), name)
	}
	r.reconcile(n)
	got := r.launches()
	if len(got) != 4 {
		t.Fatalf("launches %q", got)
	}
	for _, a := range got[2:] {
		if a[1] != "--resume" || (a[2] != "conv-a" && a[2] != "conv-b") {
			t.Fatalf("relaunch %q", a)
		}
	}
}

func TestRestartSessionsRelaunchesEveryRecordWithResume(t *testing.T) {
	r := newRig(t)
	stopped := rec("c")
	stopped.Desired = node.DesiredStopped
	n := r.node(rec("a"), rec("b"), stopped)
	r.reconcile(n)
	r.reconcile(n)

	// switchboard node --restart-sessions: a new node process stops what
	// runs before its first assign.
	n2, err := node.New(r.cfg, r.fake)
	if err != nil {
		t.Fatal(err)
	}
	if err := n2.RestartSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stops := r.fake.Stops(); len(stops) != 2 || stops[0].Grace != node.StopGrace {
		t.Fatalf("stops %+v", stops)
	}
	// The host has recorded both as launched by now.
	a, b := rec("a"), rec("b")
	a.Launched, b.Launched = true, true
	n2.Assign([]node.Record{a, b, stopped})
	r.reconcile(n2)
	got := r.launches()
	if len(got) != 4 || got[2][1] != "--resume" || got[3][1] != "--resume" {
		t.Fatalf("launches %q", got)
	}
	if st := r.state(n2, "c"); st.Observed != node.ObservedStopped {
		t.Fatalf("stopped record %+v", st)
	}
}
