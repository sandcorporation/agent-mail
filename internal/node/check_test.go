package node_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// The check helper is this test binary run by a precheck: it writes its pid
// to SB_CHECK_PIDS, starts one more copy of itself when SB_CHECK_SPAWN is
// set, prints SB_CHECK_PRINT lines, then sleeps SB_CHECK_SLEEP.
func checkHelperIfRequested() {
	pids := os.Getenv("SB_CHECK_PIDS")
	if pids == "" {
		return
	}
	f, err := os.OpenFile(pids, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintln(f, os.Getpid())
		f.Close()
	}
	if os.Getenv("SB_CHECK_SPAWN") != "" {
		exe, _ := os.Executable()
		c := exec.Command(exe)
		c.Env = append(os.Environ(), "SB_CHECK_SPAWN=")
		if err := c.Start(); err != nil {
			os.Exit(9)
		}
	}
	n, _ := strconv.Atoi(os.Getenv("SB_CHECK_PRINT"))
	for i := 0; i < n; i++ {
		fmt.Printf("line %05d\n", i)
	}
	if d, err := time.ParseDuration(os.Getenv("SB_CHECK_SLEEP")); err == nil {
		time.Sleep(d)
	}
	os.Exit(0)
}

// helperCommand is the shell command line that runs the check helper.
func helperCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return `"` + exe + `"`
	}
	return "'" + exe + "'"
}

func helperEnv(pids string, extra ...string) []string {
	return append(append(os.Environ(), "SB_CHECK_PIDS="+pids), extra...)
}

func TestRunCheckExitCodes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pwd := "pwd"
	if runtime.GOOS == "windows" {
		pwd = "cd"
	}
	cases := []struct {
		name, command string
		code          int
		out           []string
	}{
		{"ok", pwd + " && echo ready", 0, []string{filepath.Base(dir), "ready"}},
		{"skip", "exit 1", 1, nil},
		{"other", "echo bad && exit 3", 3, []string{"bad"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res := node.RunCheck(context.Background(), dir, os.Environ(), c.command, 30*time.Second)
			if res.Error != "" || res.TimedOut || res.ExitCode == nil || *res.ExitCode != c.code {
				t.Fatalf("%q: %+v", c.command, res)
			}
			for _, w := range c.out {
				if !strings.Contains(res.Output, w) {
					t.Fatalf("%q: output %q lacks %q", c.command, res.Output, w)
				}
			}
		})
	}
}

func TestRunCheckKeepsTheEndOfStdout(t *testing.T) {
	t.Parallel()
	pids := filepath.Join(t.TempDir(), "pids")
	res := node.RunCheck(context.Background(), t.TempDir(), helperEnv(pids, "SB_CHECK_PRINT=2000"), helperCommand(t), 30*time.Second)
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	out := strings.ReplaceAll(res.Output, "\r\n", "\n")
	if len(res.Output) > wire.CheckOutputMax || !strings.HasSuffix(out, "line 01999\n") || strings.Contains(out, "line 00000") {
		t.Fatalf("output of %d bytes: ...%q", len(res.Output), out[max(0, len(out)-40):])
	}
}

// A precheck past its limit is killed with everything it started.
func TestRunCheckTimeoutKillsTheProcessTree(t *testing.T) {
	t.Parallel()
	pids := filepath.Join(t.TempDir(), "pids")
	start := time.Now()
	res := node.RunCheck(context.Background(), t.TempDir(), helperEnv(pids, "SB_CHECK_SPAWN=1", "SB_CHECK_SLEEP=2m"), helperCommand(t), 3*time.Second)
	if !res.TimedOut || res.ExitCode != nil || res.Error != "" {
		t.Fatalf("%+v", res)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("returned after %s", took)
	}
	b, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(b))
	if len(lines) != 2 {
		t.Fatalf("helper pids %q: the helper and its child did not both start in time", lines)
	}
	for _, l := range lines {
		pid, _ := strconv.Atoi(l)
		deadline := time.Now().Add(10 * time.Second)
		for processAlive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running after the timeout", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestRunCheckStopsWithItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	pids := filepath.Join(t.TempDir(), "pids")
	res := node.RunCheck(ctx, t.TempDir(), helperEnv(pids, "SB_CHECK_SLEEP=2m"), helperCommand(t), time.Minute)
	if res.Error == "" || res.ExitCode != nil {
		t.Fatalf("%+v", res)
	}
}

func TestNodeCheckNeedsAllowChecks(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	a := rec("a")
	a.Directory = t.TempDir()
	r.cfg.Environ = os.Environ()
	res := r.node(a).Check(context.Background(), "a", wire.NodeCheck{Ref: "c1", Command: "exit 0", TimeoutMS: 10_000})
	if res.Ref != "c1" || res.Error != node.ErrChecksOff || res.ExitCode != nil {
		t.Fatalf("checks off: %+v", res)
	}
	r.cfg.AllowChecks = true
	n := r.node(a)
	if res = n.Check(context.Background(), "a", wire.NodeCheck{Ref: "c2", Command: "exit 0", TimeoutMS: 10_000}); res.Ref != "c2" || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("checks on: %+v", res)
	}
	if res = n.Check(context.Background(), "b", wire.NodeCheck{Ref: "c3", Command: "exit 0"}); !strings.Contains(res.Error, "not on this node") {
		t.Fatalf("unknown record: %+v", res)
	}
}

// A precheck gets the session's environment without its role profile or
// any token.
func TestCheckEnvHasNoCredentials(t *testing.T) {
	cfg := node.Config{Endpoint: "http://127.0.0.1:8090", Machine: "pc-1", NodeToken: "node-credential-value",
		Environ: []string{"PATH=/usr/bin", "SWITCHBOARD_TOKEN=role-token-value", "AGENT_MAIL_TOKEN=old", "SWITCHBOARD_PROFILE=node-own", "NODE_CREDENTIAL=node-credential-value"}}
	a := rec("a")
	a.Env = map[string]string{"FOO": "bar"}
	m := envMap(cfg.CheckEnv(a))
	for _, k := range []string{"SWITCHBOARD_TOKEN", "AGENT_MAIL_TOKEN", "SWITCHBOARD_PROFILE", "NODE_CREDENTIAL"} {
		if v, ok := m[k]; ok {
			t.Fatalf("%s=%q in a precheck's environment", k, v)
		}
	}
	if m["FOO"] != "bar" || m["SWITCHBOARD_ENDPOINT"] != "http://127.0.0.1:8090" || m["PATH"] != "/usr/bin" {
		t.Fatalf("env %v", m)
	}
}
