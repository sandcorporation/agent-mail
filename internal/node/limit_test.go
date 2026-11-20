package node_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
)

// six records created a minute apart, assigned newest first.
func sixRecords() []node.Record {
	var recs []node.Record
	for i := 6; i >= 1; i-- {
		r := rec(fmt.Sprintf("r%d", i))
		r.CreatedAt = t0.Add(time.Duration(i) * time.Minute).UnixMilli()
		recs = append(recs, r)
	}
	return recs
}

func (r *rig) observed(n *node.Node) map[string]string {
	out := map[string]string{}
	for _, s := range n.States() {
		out[s.ID] = s.Observed
	}
	return out
}

func TestCapRunsFourAndStartsTheOldestWaitingWhenOneStops(t *testing.T) {
	r := newRig(t)
	recs := sixRecords()
	n := r.node(recs...)
	r.reconcile(n)
	r.reconcile(n)
	got := r.observed(n)
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		if got[id] != node.ObservedRunning {
			t.Fatalf("%s %q, want running: %v", id, got[id], got)
		}
	}
	for _, id := range []string{"r5", "r6"} {
		if got[id] != node.ObservedWaiting {
			t.Fatalf("%s %q, want waiting: %v", id, got[id], got)
		}
	}
	if len(r.fake.Launches()) != 4 {
		t.Fatalf("%d launches, want 4", len(r.fake.Launches()))
	}

	// Stop r2: r5, the oldest waiting, starts; r6 still waits.
	for i := range recs {
		if recs[i].ID == "r2" {
			recs[i].Desired = node.DesiredStopped
		}
	}
	n.Assign(recs)
	r.reconcile(n)
	got = r.observed(n)
	if got["r2"] != node.ObservedStopped || got["r5"] != node.ObservedStarting || got["r6"] != node.ObservedWaiting {
		t.Fatalf("after stopping r2: %v", got)
	}
	if l := r.fake.Launches(); l[len(l)-1].Name != "sb-r5" {
		t.Fatalf("last launch %s, want sb-r5", l[len(l)-1].Name)
	}
}

func TestBackingOffDoesNotHoldACapSlot(t *testing.T) {
	r := newRig(t)
	recs := sixRecords()[1:] // r5 … r1
	n := r.node(recs...)
	r.reconcile(n)
	if got := r.observed(n); got["r5"] != node.ObservedWaiting {
		t.Fatalf("r5 %q, want waiting", got["r5"])
	}
	// r1 exits: it backs off and r5 takes its slot.
	if err := r.fake.Exit("sb-r1", 1); err != nil {
		t.Fatal(err)
	}
	r.reconcile(n)
	got := r.observed(n)
	if got["r1"] != node.ObservedBackingOff || got["r5"] != node.ObservedStarting {
		t.Fatalf("after r1 exits: %v", got)
	}
	// Its backoff over, r1 waits for a slot instead of exceeding the cap.
	r.now = r.now.Add(node.BackoffBase)
	r.reconcile(n)
	if got := r.observed(n); got["r1"] != node.ObservedWaiting {
		t.Fatalf("r1 after backoff %q, want waiting", got["r1"])
	}
	if len(r.fake.Launches()) != 5 {
		t.Fatalf("%d launches, want 5", len(r.fake.Launches()))
	}
}

func TestCapFromConfig(t *testing.T) {
	r := newRig(t)
	r.cfg.Max = 2
	n := r.node(sixRecords()...)
	r.reconcile(n)
	if len(r.fake.Launches()) != 2 {
		t.Fatalf("%d launches, want 2", len(r.fake.Launches()))
	}
	cfg, err := node.Resolve(host(map[string]string{"SWITCHBOARD_HOSTING_MAX": "6"}, map[string]string{"claude": "/c"}))
	if err != nil || cfg.Max != 6 {
		t.Fatalf("max %d %v", cfg.Max, err)
	}
	if cfg, _ := node.Resolve(host(nil, map[string]string{"claude": "/c"})); cfg.Max != 4 {
		t.Fatalf("default max %d, want 4", cfg.Max)
	}
	for _, bad := range []string{"0", "-1", "four"} {
		if _, err := node.Resolve(host(map[string]string{"SWITCHBOARD_HOSTING_MAX": bad}, map[string]string{"claude": "/c"})); err == nil {
			t.Fatalf("max %q accepted", bad)
		}
	}
}

func TestParseReset(t *testing.T) {
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no zone data")
	}
	for _, c := range []struct {
		msg  string
		want time.Time
	}{
		{"5-hour limit reached ∙ resets 3pm", time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)},
		{"Claude usage limit reached. Your limit will reset at 10:30pm.", time.Date(2026, 11, 19, 22, 30, 0, 0, time.UTC)},
		{"limit reached ∙ resets 8am", time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)},
		{"limit reached ∙ resets 12am", time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)},
		{"limit reached ∙ resets 21:00", time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC)},
		{"limit reached ∙ resets 7pm (Asia/Seoul)", time.Date(2026, 11, 19, 19, 0, 0, 0, seoul)},
		{"Weekly limit reached ∙ resets Nov 21, 5pm", time.Date(2026, 11, 21, 17, 0, 0, 0, time.UTC)},
		{"Claude AI usage limit reached|1763600000", time.Unix(1763600000, 0)},
		{"Claude usage limit reached.", time.Time{}},
		{"resets 13pm", time.Time{}},
	} {
		if got := node.ParseReset(c.msg, at); !got.Equal(c.want) {
			t.Errorf("%q: %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestDetectLimitOnScriptedScreens(t *testing.T) {
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		rows  []string
		found bool
	}{
		{[]string{"> fix the bug", "", "  ⎿  5-hour limit reached ∙ resets 3pm", "     /upgrade to increase your usage limit."}, true},
		{[]string{"Claude usage limit reached. Your limit will reset at", "3pm (Asia/Seoul)."}, true},
		{[]string{"You've hit your limit · resets 3pm"}, true},
		{[]string{"> what is a usage limit?", "A usage limit caps requests."}, false},
		{nil, false},
	} {
		hit, found := node.DetectLimit(c.rows, at)
		if found != c.found {
			t.Errorf("%q: found %v, want %v", c.rows, found, c.found)
		}
		if found && hit.Reset.IsZero() {
			t.Errorf("%q: no reset time in %q", c.rows, hit.Text)
		}
	}
}

// A fake agent printing the limit message puts the node in limited with the
// reset time; nothing is stopped; it clears at the reset time and stays
// clear while the spent message is still on the screen; a fresh message
// limits again.
func TestFakeAgentLimitMessageLimitsTheNode(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"), rec("b"))
	r.reconcile(n)
	if n.Limit().Limited {
		t.Fatal("limited before any message")
	}
	if err := r.fake.Output("sb-a", []byte(fakeagent.LimitMessage("3pm")+"\r\n")); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	l := n.Limit()
	reset := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	if !l.Limited || l.ResetAt == nil || *l.ResetAt != reset.UnixMilli() || l.Since != r.now.UnixMilli() {
		t.Fatalf("limit %+v, want limited until %v", l, reset)
	}
	if len(r.fake.Stops()) != 0 {
		t.Fatalf("stopped %v", r.fake.Stops())
	}
	r.now = reset
	r.reconcile(n)
	if n.Limit().Limited {
		t.Fatal("still limited after the reset time")
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	if n.Limit().Limited {
		t.Fatal("the spent message limited again")
	}
	// The next day's message limits again.
	if err := r.fake.Output("sb-b", []byte(fakeagent.LimitMessage("8pm")+"\r\n")); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	if !n.Limit().Limited {
		t.Fatal("a new message did not limit")
	}
}

// A message without a reset time holds until the screen stops showing it.
func TestLimitClearsWhenTheScreenNoLongerShowsIt(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"))
	r.reconcile(n)
	if err := r.fake.Output("sb-a", []byte(fakeagent.LimitMessage("")+"\r\n")); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	if l := n.Limit(); !l.Limited || l.ResetAt != nil {
		t.Fatalf("limit %+v, want limited without reset", l)
	}
	if err := r.fake.Output("sb-a", []byte("\x1b[2J\x1b[Hnext turn\r\n")); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	if n.Limit().Limited {
		t.Fatal("still limited after the screen moved on")
	}
	// A session that ends takes its showing with it.
	if err := r.fake.Output("sb-a", []byte(fakeagent.LimitMessage("")+"\r\n")); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	if err := r.fake.Exit("sb-a", 0); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(node.LimitScan)
	r.reconcile(n)
	if n.Limit().Limited {
		t.Fatal("an ended session still limits")
	}
}

// A session that hits the limit again after a reset prints a fresh notice
// below the spent one still on its screen: the fresh one limits again,
// whether it names another reset time or the same text (a day later), and
// the spent one alone never does (#210).
func TestARepeatedNoticeOnTheSameScreenLimitsAgain(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"))
	r.reconcile(n)
	limitedTimes := 0
	was := false
	step := func() {
		r.now = r.now.Add(node.LimitScan)
		r.reconcile(n)
		if l := n.Limit().Limited; l != was {
			if l {
				limitedTimes++
			}
			was = l
		}
	}
	say := func(s string) {
		if err := r.fake.Output("sb-a", []byte(s+"\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	say(fakeagent.LimitMessage("3pm"))
	step()
	r.now = time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	step()
	say("> carry on")
	step()
	if was {
		t.Fatal("the spent notice still limits")
	}
	say(fakeagent.LimitMessage("8pm"))
	step()
	if !was || limitedTimes != 2 {
		t.Fatalf("a fresh notice under a spent one: limited %v, %d times", was, limitedTimes)
	}
	r.now = time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	step()
	step()
	if was {
		t.Fatal("still limited after 8pm")
	}
	// The next day, the same words again.
	r.now = time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	say(fakeagent.LimitMessage("8pm"))
	step()
	if !was || limitedTimes != 3 {
		t.Fatalf("the same notice again: limited %v, %d times", was, limitedTimes)
	}
	l := n.Limit()
	if want := time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC); l.ResetAt == nil || *l.ResetAt != want.UnixMilli() {
		t.Fatalf("reset %v, want %v", l.ResetAt, want)
	}
	// Scanning on with nothing new keeps it at one showing.
	step()
	if limitedTimes != 3 {
		t.Fatalf("limited %d times without a new notice", limitedTimes)
	}
}
