//go:build linux

package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// descendants lists every process below pid, read from /proc.
func descendants(pid int) []int {
	children := map[int][]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces, so cut at the last ')'.
		s := string(b)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil {
			children[ppid] = append(children[ppid], p)
		}
	}
	var out []int
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// A Monitor that ends kills the waiting client's whole process tree: the
// runner is not in it, so the job keeps running and ends exited.
func TestJobSurvivesATerminatedClientTree(t *testing.T) {
	h := jobHarness(t)
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	args, _ := json.Marshal([]string{"job", "run", "--wait", "--", "sh", "-c",
		`echo started; while [ ! -e "$0" ]; do sleep 0.05; done; echo finished`, ready})
	cmd.Env = append(cliEnv(), "SWITCHBOARD_TEST_CLI=1", "AGENT_MAIL_CONFIG_DIR="+h.dir, "SWITCHBOARD_TEST_ARGS="+string(args))
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	id := jobID(t, line)
	h.running(id)
	for _, p := range append(descendants(cmd.Process.Pid), cmd.Process.Pid) {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}
	_ = cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	if j, _ := h.s.jobs().Get(id); wire.JobEnded(j.State) {
		t.Fatalf("job ended with its client's tree: %+v", j)
	}
	_ = os.WriteFile(ready, nil, 0o600)
	if j := h.ended(id); j.State != wire.JobExited || *j.ExitCode != 0 {
		t.Fatalf("job: %+v", j)
	}
}

// noEscape is a caller inside a systemd scope with no user manager to leave it.
func noEscape(t *testing.T) []string {
	cg := filepath.Join(t.TempDir(), "cgroup")
	line := "0::/user.slice/user-1000.slice/user@1000.service/app.slice/term_7.scope\n"
	if err := os.WriteFile(cg, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"SWITCHBOARD_JOB_CGROUP=" + cg, "XDG_RUNTIME_DIR=" + t.TempDir(), "DBUS_SESSION_BUS_ADDRESS="}
}

func TestJobPingRunnerFailsInAScopeWithNoWayOut(t *testing.T) {
	h := jobHarness(t)
	o := h.ping(noEscape(t))
	if o.OK || o.Exit != cli.ExitPingRunner || !failed(o, "runner") {
		t.Fatalf("ping: %+v", o)
	}
}

func TestJobRunInAScopeWithNoWayOutIsNotHandedOver(t *testing.T) {
	h := jobHarness(t)
	for _, args := range [][]string{{"job", "run", "--", "true"}, {"watch", "start", "--", "true"}} {
		r := h.run(noEscape(t), args...)
		if r.code != cli.ExitNoRunner || !strings.Contains(r.stderr, "job not handed over") || !strings.Contains(r.stderr, "scope") {
			t.Fatalf("%v: code %d stderr %q", args, r.code, r.stderr)
		}
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("jobs registered: %d %v", n, err)
	}
}

// userManager skips the test where no systemd user manager answers.
func userManager(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("no systemd-run: the runner leaves a scope only through a systemd user manager")
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skipf("no systemd user manager here (%v): nothing to start a scope or the runner unit", err)
	}
}

// A terminal host stops the terminal's scope when it closes: the runner,
// started as a unit of its own, lives on and the job ends exited.
func TestJobRunnerSurvivesItsCallersScopeStop(t *testing.T) {
	userManager(t)
	h := jobHarness(t)
	ready := filepath.Join(t.TempDir(), "ready")
	scope := fmt.Sprintf("switchboard-test-%d-%d.scope", os.Getpid(), time.Now().UnixNano())
	args, _ := json.Marshal([]string{"job", "run", "--", "sh", "-c",
		`echo started; while [ ! -e "$0" ]; do sleep 0.05; done; echo finished`, ready})
	cmd := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "--unit", scope, "--",
		"sh", "-c", `"$0" -test.run='^TestCLIHelperProcess$'; exec sleep 60`, os.Args[0])
	cmd.Env = append(cliEnv(), "SWITCHBOARD_TEST_CLI=1", "AGENT_MAIL_CONFIG_DIR="+h.dir, "SWITCHBOARD_TEST_ARGS="+string(args))
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "stop", scope).Run()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("job run in the scope: %v", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "PASS")), 10, 64)
	if err != nil {
		t.Fatalf("job run printed %q", line)
	}
	h.running(id)
	if b, err := exec.Command("systemctl", "--user", "stop", scope).CombinedOutput(); err != nil {
		t.Fatalf("stop %s: %v %s", scope, err, b)
	}
	time.Sleep(300 * time.Millisecond)
	if j, _ := h.s.jobs().Get(id); wire.JobEnded(j.State) {
		t.Fatalf("job ended with the scope: %+v", j)
	}
	_ = os.WriteFile(ready, nil, 0o600)
	if j := h.ended(id); j.State != wire.JobExited || *j.ExitCode != 0 || j.Tail != "started\nfinished" {
		t.Fatalf("job: %+v", j)
	}
}
