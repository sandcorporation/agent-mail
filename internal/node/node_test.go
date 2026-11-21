package node_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
)

func TestMain(m *testing.M) {
	fakeagent.MainIfRequested()
	os.Exit(m.Run())
}

var t0 = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

type rig struct {
	t    *testing.T
	fake *termtest.Fake
	now  time.Time
	cfg  node.Config
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, fake: termtest.New(), now: t0}
	r.fake.SetClock(func() time.Time { return r.now })
	r.cfg = node.Config{
		Agent:    "/opt/agent/claude",
		Machine:  "pc-1",
		Endpoint: "http://127.0.0.1:8090",
		Now:      func() time.Time { return r.now },
	}
	return r
}

// node starts a node process over the rig's backend (a "node restart" is a
// second call: the backend and its sessions survive).
func (r *rig) node(recs ...node.Record) *node.Node {
	r.t.Helper()
	n, err := node.New(r.cfg, r.fake)
	if err != nil {
		r.t.Fatal(err)
	}
	n.Assign(recs)
	return n
}

func (r *rig) reconcile(n *node.Node) {
	r.t.Helper()
	r.now = r.now.Add(time.Second)
	if err := n.Reconcile(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// relaunch passes the exit, waits out the first backoff and reconciles.
func (r *rig) relaunch(n *node.Node) {
	r.t.Helper()
	r.reconcile(n)
	r.now = r.now.Add(node.BackoffBase)
	r.reconcile(n)
}

func (r *rig) state(n *node.Node, id string) node.State {
	r.t.Helper()
	for _, s := range n.States() {
		if s.ID == id {
			return s
		}
	}
	r.t.Fatalf("no state for %s", id)
	return node.State{}
}

func (r *rig) launches() [][]string {
	var out [][]string
	for _, s := range r.fake.Launches() {
		out = append(out, s.Argv)
	}
	return out
}

func rec(id string) node.Record {
	return node.Record{
		ID: id, Profile: "team-worker", Directory: "/work/" + id,
		AgentSessionID: "conv-" + id, Args: []string{"--model", "opus"},
		Restart: node.RestartAlways, Desired: node.DesiredRunning,
	}
}

func TestStateWire(t *testing.T) {
	code := 2
	rt := node.State{Observed: node.ObservedRunning, PID: 7, Run: 3, StartedAt: t0, LastExit: &code}.Wire()
	if rt.State != "running" || rt.PID != 7 || rt.Run != 3 || rt.StartedAt == nil || *rt.StartedAt != t0.UnixMilli() || *rt.LastExit != 2 {
		t.Fatalf("%+v", rt)
	}
	if (node.State{}).Wire().StartedAt != nil {
		t.Fatal("zero start time reported")
	}
}

func TestFirstLaunchUsesSessionIDThenResume(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"))
	r.reconcile(n)
	if st := r.state(n, "a"); st.Observed != node.ObservedStarting || st.Run != 1 || !st.StartedAt.Equal(r.now) || !st.Launched {
		t.Fatalf("after launch: %+v", st)
	}
	r.reconcile(n)
	st := r.state(n, "a")
	if st.Observed != node.ObservedRunning || st.PID == 0 {
		t.Fatalf("after second pass: %+v", st)
	}

	if err := r.fake.Exit("sb-a", 1); err != nil {
		t.Fatal(err)
	}
	r.relaunch(n)
	want := [][]string{
		{"/opt/agent/claude", "--session-id", "conv-a", "--model", "opus"},
		{"/opt/agent/claude", "--resume", "conv-a", "--model", "opus"},
	}
	if got := r.launches(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("launches %q, want %q", got, want)
	}
	st = r.state(n, "a")
	if st.Run != 2 || st.LastExit == nil || *st.LastExit != 1 {
		t.Fatalf("after relaunch: %+v", st)
	}
	spec := r.fake.Launches()[0]
	if spec.Name != "sb-a" || spec.Dir != "/work/a" || spec.Cols != node.DefaultCols || spec.Rows != node.DefaultRows {
		t.Fatalf("spec %+v", spec)
	}
}

func TestLaunchedRecordResumesOnFirstLaunch(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	a.Launched = true
	r.reconcile(r.node(a))
	if got := r.launches(); len(got) != 1 || got[0][1] != "--resume" {
		t.Fatalf("launches %q", got)
	}
}

func TestLiveSessionSurvivesNodeRestart(t *testing.T) {
	r := newRig(t)
	n1 := r.node(rec("a"))
	r.reconcile(n1)
	r.reconcile(n1)
	pid := r.state(n1, "a").PID

	n2 := r.node(rec("a")) // the host still says launched=0
	r.reconcile(n2)
	if got := r.launches(); len(got) != 1 {
		t.Fatalf("relaunched after node restart: %q", got)
	}
	st := r.state(n2, "a")
	if st.Observed != node.ObservedRunning || st.PID != pid || st.Run != 1 || !st.Launched {
		t.Fatalf("adopted state %+v", st)
	}

	// A later exit resumes the same conversation.
	if err := r.fake.Exit("sb-a", 0); err != nil {
		t.Fatal(err)
	}
	r.relaunch(n2)
	if got := r.launches(); len(got) != 2 || got[1][1] != "--resume" || got[1][2] != "conv-a" {
		t.Fatalf("launches %q", got)
	}
}

func TestOrphanSessionIsStoppedAndForgotten(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, name := range []string{"sb-gone", "other"} {
		if err := r.fake.Launch(ctx, term.Spec{Name: name, Argv: []string{"x"}, Cols: 80, Rows: 24}); err != nil {
			t.Fatal(err)
		}
	}
	n := r.node(rec("a"))
	r.reconcile(n)
	if stops := r.fake.Stops(); len(stops) != 1 || stops[0].Name != "sb-gone" || stops[0].Grace != node.StopGrace {
		t.Fatalf("stops %+v", stops)
	}
	infos, err := r.fake.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range infos {
		names = append(names, in.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"other", "sb-a"}) {
		t.Fatalf("sessions %q", names)
	}
}

func TestRestartPolicies(t *testing.T) {
	cases := []struct {
		policy   string
		code     int
		relaunch bool
	}{
		{node.RestartAlways, 0, true},
		{node.RestartAlways, 3, true},
		{node.RestartOnFailure, 0, false},
		{node.RestartOnFailure, 3, true},
		{node.RestartNever, 3, false},
	}
	for _, c := range cases {
		r := newRig(t)
		a := rec("a")
		a.Restart = c.policy
		n := r.node(a)
		r.reconcile(n)
		if err := r.fake.Exit("sb-a", c.code); err != nil {
			t.Fatal(err)
		}
		r.relaunch(n)
		st := r.state(n, "a")
		if got := len(r.launches()) == 2; got != c.relaunch {
			t.Errorf("%s exit %d: relaunched %v, want %v", c.policy, c.code, got, c.relaunch)
		}
		if !c.relaunch && (st.Observed != node.ObservedStopped || st.LastExit == nil || *st.LastExit != c.code) {
			t.Errorf("%s exit %d: state %+v", c.policy, c.code, st)
		}
	}
}

func TestDesiredStoppedSurvivesNodeRestart(t *testing.T) {
	r := newRig(t)
	n := r.node(rec("a"))
	r.reconcile(n)

	stopped := rec("a")
	stopped.Desired = node.DesiredStopped
	n.Assign([]node.Record{stopped})
	r.reconcile(n)
	if stops := r.fake.Stops(); len(stops) != 1 || stops[0].Name != "sb-a" || stops[0].Grace != node.StopGrace {
		t.Fatalf("stops %+v", stops)
	}
	r.reconcile(n)
	if st := r.state(n, "a"); st.Observed != node.ObservedStopped || st.LastExit == nil || *st.LastExit != termtest.StopExitCode {
		t.Fatalf("state %+v", st)
	}

	n2 := r.node(stopped)
	r.reconcile(n2)
	r.reconcile(n2)
	if got := r.launches(); len(got) != 1 {
		t.Fatalf("stopped record relaunched: %q", got)
	}
	if st := r.state(n2, "a"); st.Observed != node.ObservedStopped {
		t.Fatalf("state after restart %+v", st)
	}

	// Started again: resumes the conversation.
	n2.Assign([]node.Record{rec("a")})
	r.reconcile(n2)
	if got := r.launches(); len(got) != 2 || got[1][1] != "--resume" {
		t.Fatalf("launches %q", got)
	}
}

func TestStoppedRecordWithoutSessionIsNotLaunched(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	a.Desired = node.DesiredStopped
	n := r.node(a)
	r.reconcile(n)
	if len(r.launches()) != 0 || r.state(n, "a").Observed != node.ObservedStopped {
		t.Fatalf("launches %q state %+v", r.launches(), r.state(n, "a"))
	}
}

func TestArgsNeverCarryPrintOrContinue(t *testing.T) {
	r := newRig(t)
	a := rec("a")
	a.Args = []string{"-p", "--continue", "--print=x", "--resume", "--session-id", "--model", "opus"}
	r.reconcile(r.node(a))
	want := []string{"/opt/agent/claude", "--session-id", "conv-a", "--model", "opus"}
	if got := r.launches(); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestChildEnvironmentDropsAgentSessionMarkers(t *testing.T) {
	r := newRig(t)
	r.cfg.Environ = []string{
		"PATH=/usr/bin", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_SESSION_ID=abc",
		"CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_PLUGIN_DIRS=/p", "CLAUDE_EFFORT=high",
	}
	if got := strings.Join(r.cfg.DroppedMarkers(), ","); got != "CLAUDECODE,CLAUDE_CODE_CHILD_SESSION,CLAUDE_CODE_ENTRYPOINT,CLAUDE_CODE_SESSION_ID" {
		t.Errorf("DroppedMarkers = %q", got)
	}
	a := rec("a")
	a.Env = map[string]string{"CLAUDECODE": "user-set"}
	r.reconcile(r.node(a))
	m := envMap(r.fake.Launches()[0].Env)
	for _, k := range []string{"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT"} {
		if _, ok := m[k]; ok {
			t.Errorf("child env has marker %s", k)
		}
	}
	if m["CLAUDECODE"] != "user-set" {
		t.Errorf("record env CLAUDECODE=%q, want kept", m["CLAUDECODE"])
	}
	if m["CLAUDE_CODE_PLUGIN_DIRS"] != "/p" || m["CLAUDE_EFFORT"] != "high" || m["PATH"] != "/usr/bin" {
		t.Errorf("user settings dropped: %v", m)
	}
}

func TestChildEnvironment(t *testing.T) {
	r := newRig(t)
	r.cfg.NodeToken = "node-credential-value"
	r.cfg.Term = "xterm-256color"
	r.cfg.Environ = []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"SWITCHBOARD_TOKEN=role-token-value",
		"AGENT_MAIL_TOKEN=old-role-token-value",
		"SWITCHBOARD_LABEL=node-own-label",
		"AGENT_MAIL_PARENT=node-own-parent",
		"NODE_CREDENTIAL=node-credential-value",
		"TERM=dumb",
	}
	a := rec("a")
	a.Label, a.Parent = "frontend", "team/lead"
	a.Env = map[string]string{"FOO": "bar", "API_SECRET": "x", "HOME": "/home/override"}
	r.reconcile(r.node(a))
	env := r.fake.Launches()[0].Env
	m := envMap(env)
	want := map[string]string{
		"SWITCHBOARD_PROFILE":  "team-worker",
		"SWITCHBOARD_ENDPOINT": "http://127.0.0.1:8090",
		"SWITCHBOARD_MACHINE":  "pc-1",
		"SWITCHBOARD_LABEL":    "frontend",
		"SWITCHBOARD_PARENT":   "team/lead",
		"COLORTERM":            "truecolor",
		"TERM":                 "xterm-256color",
		"PATH":                 "/usr/bin",
		"FOO":                  "bar",
		"HOME":                 "/home/override",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s=%q, want %q", k, m[k], v)
		}
	}
	for _, k := range []string{"SWITCHBOARD_TOKEN", "AGENT_MAIL_TOKEN", "AGENT_MAIL_PARENT", "NODE_CREDENTIAL", "API_SECRET"} {
		if _, ok := m[k]; ok {
			t.Errorf("child env has %s", k)
		}
	}
	for _, kv := range env {
		if strings.Contains(kv, "node-credential-value") || strings.Contains(kv, "role-token-value") {
			t.Errorf("child env leaks %q", kv)
		}
	}
	if len(env) != len(m) {
		t.Errorf("duplicate names in %q", env)
	}

	// Unset label and parent are not inherited from the node either.
	b := rec("b")
	r.reconcile(r.node(b))
	m = envMap(r.fake.Launches()[1].Env)
	for _, k := range []string{"SWITCHBOARD_LABEL", "SWITCHBOARD_PARENT"} {
		if _, ok := m[k]; ok {
			t.Errorf("unlabelled child has %s=%q", k, m[k])
		}
	}
}

func host(env map[string]string, path map[string]string) node.Host {
	return node.Host{
		Getenv: func(k string) string { return env[k] },
		LookPath: func(name string) (string, error) {
			if p, ok := path[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		Hostname: func() (string, error) { return "box", nil },
		Environ:  func() []string { return []string{"PATH=/usr/bin"} },
	}
}

func TestResolve(t *testing.T) {
	cfg, err := node.Resolve(host(nil, map[string]string{"claude": "/usr/bin/claude"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != "/usr/bin/claude" || cfg.Machine != "box" || cfg.History != node.DefaultHistory || len(cfg.Environ) != 1 {
		t.Fatalf("defaults %+v", cfg)
	}

	cfg, err = node.Resolve(host(map[string]string{
		"SWITCHBOARD_HOSTING_AGENT":   "fake",
		"SWITCHBOARD_HOSTING_MACHINE": "pc-1",
		"SWITCHBOARD_HOSTING_HISTORY": "500",
	}, map[string]string{"fake": "/x/fake"}))
	if err != nil || cfg.Agent != "/x/fake" || cfg.Machine != "pc-1" || cfg.History != 500 {
		t.Fatalf("%+v %v", cfg, err)
	}

	if _, err := node.Resolve(host(map[string]string{"SWITCHBOARD_HOSTING_HISTORY": "lots"}, map[string]string{"claude": "/c"})); err == nil {
		t.Fatal("bad history accepted")
	}
}

func TestNodeRefusesUnresolvedAgent(t *testing.T) {
	_, err := node.Resolve(host(nil, nil))
	if !errors.Is(err, node.ErrNoAgent) {
		t.Fatalf("err %v, want ErrNoAgent", err)
	}
	// Hosting variables have no old-name alias.
	_, err = node.Resolve(host(map[string]string{"AGENT_MAIL_HOSTING_AGENT": "fake"}, map[string]string{"fake": "/x/fake"}))
	if !errors.Is(err, node.ErrNoAgent) {
		t.Fatalf("old name used: %v", err)
	}
	if _, err := node.New(node.Config{}, termtest.New()); err == nil {
		t.Fatal("node started without an agent")
	}
}

// The command the node builds runs the fake agent: a first launch creates
// the conversation, the second resumes it.
func TestLaunchCommandRunsFakeAgent(t *testing.T) {
	r := newRig(t)
	r.cfg.Agent = os.Args[0]
	dir := t.TempDir()
	r.cfg.Environ = append(config.WithoutOwnEnv(os.Environ()), fakeagent.Env(dir)...)
	n := r.node(rec("a"))
	r.reconcile(n)
	if err := r.fake.Exit("sb-a", 0); err != nil {
		t.Fatal(err)
	}
	r.relaunch(n)
	for i, spec := range r.fake.Launches() {
		cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
		cmd.Env, cmd.Dir = spec.Env, dir
		cmd.Stdin = strings.NewReader("hi\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("launch %d %q: %v\n%s", i, spec.Argv, err, out)
		}
		if !strings.Contains(string(out), "ready conv-a") || !strings.Contains(string(out), "env SWITCHBOARD_MACHINE=pc-1") {
			t.Fatalf("launch %d output:\n%s", i, out)
		}
	}
}
