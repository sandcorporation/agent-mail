//go:build unix

package server

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// jobHarness is the CLI harness configured as role a, with every runner the
// test started reaped by PID at the end.
func jobHarness(t *testing.T) *cliHarness {
	t.Helper()
	h := newCLIHarness(t)
	h.configure(nil, "a")
	t.Cleanup(func() {
		pids, _ := filepath.Glob(filepath.Join(h.dir, "jobs", "default", "*", "pid"))
		for _, f := range pids {
			b, _ := os.ReadFile(f)
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return h
}

func TestJobRunWaitStreamsAndExitsWithTheCode(t *testing.T) {
	h := jobHarness(t)
	r := h.run(nil, "job", "run", "--wait", "--label", "build", "--", "sh", "-c", "echo out; echo err >&2; exit 3")
	if r.code != 3 || r.stdout != "out\nerr" {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	id := jobID(t, r.stderr)
	j := h.ended(id)
	if j.State != wire.JobExited || *j.ExitCode != 3 || j.Tail != "out\nerr" || j.Command != "sh -c 'echo out; echo err >&2; exit 3'" {
		t.Fatalf("job: %+v", j)
	}
	if m := jobMail(t, h.s, "a"); len(m) != 0 {
		t.Fatalf("a waiter saw the end, yet mail: %+v", m)
	}
	st, err := os.Stat(filepath.Join(h.jobDir(id), "output"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("output file: %v %v", st, err)
	}
}

func TestJobRunUnwatchedMailsTheEndOnce(t *testing.T) {
	h := jobHarness(t)
	out := h.mustRun(nil, "job", "run", "--idem", "k", "--", "sh", "-c", "echo boom; exit 4")
	id, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		t.Fatalf("job run printed %q", out)
	}
	h.ended(id)
	if again := h.mustRun(nil, "job", "run", "--idem", "k", "--", "sh", "-c", "echo boom; exit 4"); again != out {
		t.Fatalf("idem retry: %q want %q", again, out)
	}
	m := jobMail(t, h.s, "a")
	if len(m) != 1 || m[0].Priority != "urgent" || !strings.HasSuffix(m[0].Subject, "exit 4") || !strings.HasSuffix(m[0].Body, "boom") {
		t.Fatalf("end mail: %+v", m)
	}
	// wait on an ended job returns its code at once, with no second mail.
	if r := h.run(nil, "job", "wait", out); r.code != 4 || r.stdout != "boom" {
		t.Fatalf("wait after the end: %d %q %s", r.code, r.stdout, r.stderr)
	}
	if m = jobMail(t, h.s, "a"); len(m) != 1 {
		t.Fatalf("mail after wait: %+v", m)
	}
	var jobs []wire.Job
	if err = json.Unmarshal([]byte(h.mustRun(nil, "job", "list", "--json")), &jobs); err != nil || len(jobs) != 1 {
		t.Fatalf("list: %v %+v", err, jobs)
	}
}

func TestJobSurvivesAKilledWaiterAndWaitReattaches(t *testing.T) {
	h := jobHarness(t)
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	args, _ := json.Marshal([]string{"job", "run", "--wait", "--", "sh", "-c",
		`echo started; while [ ! -e "$0" ]; do sleep 0.05; done; echo finished; exit 7`, ready})
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
	// Kill the waiter (a session restart) while the job runs.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if j, _ := h.s.jobs().Get(id); wire.JobEnded(j.State) {
		t.Fatalf("job ended with its waiter: %+v", j)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(ready, nil, 0o600)
	}()
	r := h.run(nil, "job", "wait", strconv.FormatInt(id, 10))
	if r.code != 7 || r.stdout != "started\nfinished" {
		t.Fatalf("job wait: code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if j := h.ended(id); *j.ExitCode != 7 {
		t.Fatalf("job: %+v", j)
	}
	if m := jobMail(t, h.s, "a"); len(m) != 0 {
		t.Fatalf("the reattached waiter saw the end, yet mail: %+v", m)
	}
}

func TestJobStop(t *testing.T) {
	h := jobHarness(t)
	out := h.mustRun(nil, "job", "run", "--", "sh", "-c", "echo up; sleep 30")
	id, _ := strconv.ParseInt(out, 10, 64)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if j, _ := h.s.jobs().Get(id); j.State == wire.JobRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := h.mustRun(nil, "job", "stop", out); got != "#"+out+" stopping" {
		t.Fatalf("stop: %q", got)
	}
	if r := h.run(nil, "job", "wait", out); r.code != 128+int(syscall.SIGTERM) {
		t.Fatalf("wait after stop: %d %q %s", r.code, r.stdout, r.stderr)
	}
	j := h.ended(id)
	if j.State != wire.JobStopped || !j.StopRequested {
		t.Fatalf("stopped job: %+v", j)
	}
	if got := h.mustRun(nil, "job", "stop", out); got != "#"+out+" already stopped" {
		t.Fatalf("stop again: %q", got)
	}
}
func (h *cliHarness) runnerGone(id int64) {
	h.t.Helper()
	b, _ := os.ReadFile(filepath.Join(h.jobDir(id), "pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(10 * time.Second)
	for pid > 0 && syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			h.t.Fatalf("runner %d still alive", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A stop from the dashboard reaches the runner through the server record
// alone: nothing on this machine signals it.
func TestJobStopFromTheDashboardReachesTheRunner(t *testing.T) {
	h := jobHarness(t)
	h.s.admin.ProxyToken = "proxy-secret"
	out := h.mustRun(fastPoll, "job", "run", "--", "sh", "-c", "echo up; sleep 30")
	id, _ := strconv.ParseInt(out, 10, 64)
	h.running(id)
	if w := adminWrite(h.s, "POST", "/api/admin/jobs/"+out+"/stop", ""); w.Code != 200 {
		t.Fatalf("dashboard stop: %d %s", w.Code, w.Body)
	}
	j := h.ended(id)
	if j.State != wire.JobStopped || *j.ExitCode != 128+int(syscall.SIGTERM) || j.Tail != "up" {
		t.Fatalf("stopped job: %+v", j)
	}
	h.runnerGone(id)
	if m := jobMail(t, h.s, "a"); len(m) != 1 || !strings.HasSuffix(m[0].Subject, "중지됨") {
		t.Fatalf("stop mail: %+v", m)
	}
}

// At its expiry the runner terminates the command: state expired, one mail.
func TestJobExpiresAtItsDeadline(t *testing.T) {
	h := jobHarness(t)
	out := h.mustRun(fastPoll, "job", "run", "--expires", "1s", "--label", "slow", "--", "sh", "-c", "echo up; sleep 30")
	id, _ := strconv.ParseInt(out, 10, 64)
	j := h.ended(id)
	if j.State != wire.JobExpired || *j.ExitCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("expired job: %+v", j)
	}
	h.runnerGone(id)
	if m := jobMail(t, h.s, "a"); len(m) != 1 || m[0].Subject != "[작업 끝남] slow 만료" || m[0].Priority != "urgent" {
		t.Fatalf("expiry mail: %+v", m)
	}
}

// The runner follows an expiry the server moved forward (auto-extension).
func TestJobRunnerFollowsAMovedExpiry(t *testing.T) {
	h := jobHarness(t)
	h.s.admin.ProxyToken = "proxy-secret"
	out := h.mustRun(fastPoll, "job", "run", "--expires", "2s", "--", "sh", "-c", "sleep 30")
	id, _ := strconv.ParseInt(out, 10, 64)
	h.running(id)
	if _, err := h.s.db.Exec(`UPDATE jobs SET expires_at=? WHERE id=?`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	if j, _ := h.s.jobs().Get(id); j.State != wire.JobRunning {
		t.Fatalf("job ended at its first expiry: %+v", j)
	}
	adminWrite(h.s, "POST", "/api/admin/jobs/"+out+"/stop", "")
	if j := h.ended(id); j.State != wire.JobStopped {
		t.Fatalf("job: %+v", j)
	}
}
