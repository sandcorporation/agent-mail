//go:build windows

package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/winjob"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// jobHarness is the CLI harness configured as role a. At the end it kills,
// by PID, every runner the test started and every PID a job command wrote
// to a *.pid file under the test's folders.
func jobHarness(t *testing.T) *cliHarness {
	t.Helper()
	if err := winjob.CheckBreakaway(); err != nil {
		t.Fatalf("this test process may not break away from its job: %v", err)
	}
	h := newCLIHarness(t)
	h.configure(nil, "a")
	t.Cleanup(func() {
		pids, _ := filepath.Glob(filepath.Join(h.dir, "jobs", "default", "*", "pid"))
		more, _ := filepath.Glob(filepath.Join(h.dir, "*.pid"))
		for _, f := range append(pids, more...) {
			b, _ := os.ReadFile(f)
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})
	return h
}

// cliCommand is the CLI (the test binary in helper mode, or exe) with args,
// not started.
func (h *cliHarness) cliCommand(exe string, env []string, args ...string) *exec.Cmd {
	if exe == "" {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "-test.run=^TestCLIHelperProcess$")
	argJSON, _ := json.Marshal(args)
	cmd.Env = append(cliEnv(), "SWITCHBOARD_TEST_CLI=1", "AGENT_MAIL_CONFIG_DIR="+h.dir, "SWITCHBOARD_TEST_ARGS="+string(argJSON))
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// jobCommand is a job's command: this test binary as TestJobCommandHelper.
func jobCommand(args ...string) []string {
	return append([]string{os.Args[0], "-test.run=^TestJobCommandHelper$", "jobcmd"}, args...)
}

// TestJobCommandHelper is a job's command when run with "jobcmd":
//
//	wait FILE CODE  print started, wait for FILE, print finished, exit CODE
//	tree PIDFILE    start "sleep" in a console group of its own (no CTRL_BREAK
//	                reaches it), write its PID to PIDFILE, sleep
//	sleep           sleep
func TestJobCommandHelper(t *testing.T) {
	t.Parallel()
	a := flag.Args()
	if len(a) < 2 || a[0] != "jobcmd" {
		return
	}
	switch a[1] {
	case "wait":
		os.Stdout.WriteString("started\n")
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(a[2]); err == nil {
				os.Stdout.WriteString("finished\n")
				code, _ := strconv.Atoi(a[3])
				os.Exit(code)
			}
		}
		os.Exit(99)
	case "tree":
		c := exec.Command(os.Args[0], "-test.run=^TestJobCommandHelper$", "jobcmd", "sleep")
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
		if err := c.Start(); err != nil {
			os.Exit(98)
		}
		_ = os.WriteFile(a[2], []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
}

// startInJob starts cmd inside a new kill-on-close job object (a terminal's)
// that lets processes break away or not. Closing the returned handle ends
// cmd and everything still in the job.
func startInJob(t *testing.T, cmd *exec.Cmd, breakaway bool) windows.Handle {
	t.Helper()
	job, err := winjob.New(true, breakaway)
	if err != nil {
		t.Fatal(err)
	}
	if err = winjob.Start(cmd, job); err != nil {
		_ = windows.CloseHandle(job)
		t.Fatal(err)
	}
	return job
}

func jobOutput(t *testing.T, h *cliHarness, id int64) string {
	b, _ := os.ReadFile(filepath.Join(h.jobDir(id), "runner.log"))
	return string(b)
}

// The terminal goes away (its job object ends the CLI and its console)
// while the job runs: the runner, outside that job and on a console of its
// own, keeps the job running to its end and reports it; nobody waited, so
// the owner gets the end mail.
func TestJobOutlivesItsTerminalWindows(t *testing.T) {
	t.Parallel()
	if outsideJob(t) {
		return
	}
	h := jobHarness(t)
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := h.cliCommand("", fastPoll, append([]string{"job", "run", "--wait", "--label", "survivor", "--"}, jobCommand("wait", ready, "7")...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	terminal := startInJob(t, cmd, true)
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil {
		_ = windows.CloseHandle(terminal)
		t.Fatalf("first stderr line: %v", err)
	}
	id := jobID(t, line)
	h.running(id)
	// Close the terminal.
	if err = windows.TerminateJobObject(terminal, 1); err != nil {
		t.Fatal(err)
	}
	_ = windows.CloseHandle(terminal)
	_ = cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	if j, _ := h.s.jobs().Get(id); wire.JobEnded(j.State) {
		t.Fatalf("job ended with its terminal: %+v\nrunner log: %s", j, jobOutput(t, h, id))
	}
	if err = os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	j := h.ended(id)
	if j.State != wire.JobExited || *j.ExitCode != 7 || !strings.Contains(j.Tail, "finished") {
		t.Fatalf("job: %+v", j)
	}
	if m := jobMail(t, h.s, "a"); len(m) != 1 || m[0].Subject != "[작업 끝남] survivor exit 7" {
		t.Fatalf("end mail: %+v", m)
	}
}

// A terminal whose job forbids breakaway: job run starts no runner and
// registers nothing; it exits with ExitNoRunner and says so on stderr, so
// the hook runs the command itself.
func TestJobRunRefusesWhenTheTerminalForbidsBreakawayWindows(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure(nil, "a")
	cmd := h.cliCommand("", nil, "job", "run", "--", "cmd", "/c", "exit 0")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	terminal := startInJob(t, cmd, false)
	defer windows.CloseHandle(terminal)
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != cli.ExitNoRunner {
		t.Fatalf("exit: %v\nstdout %q\nstderr %q", err, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "job not handed over, run the command yourself") {
		t.Fatalf("stderr: %q", errOut.String())
	}
	var n int
	if err = h.s.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("jobs registered: %d %v", n, err)
	}
}

// job ping in a terminal whose job forbids breakaway: the runner check
// fails with its own code, and nothing is registered.
func TestJobPingBreakawayRefusedIsARunnerFailureWindows(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure(nil, "a")
	cmd := h.cliCommand("", nil, "job", "ping", "--json")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	terminal := startInJob(t, cmd, false)
	defer windows.CloseHandle(terminal)
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != cli.ExitPingRunner {
		t.Fatalf("exit: %v\nstdout %q\nstderr %q", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"check":"runner","ok":false`) || !strings.Contains(out.String(), `"check":"token","ok":true`) {
		t.Fatalf("stdout: %q", out.String())
	}
}

// Outside any job that forbids breakaway, every check passes.
func TestJobPingPassesWindows(t *testing.T) {
	t.Parallel()
	if outsideJob(t) {
		return
	}
	h := jobHarness(t)
	if o := h.ping(nil); !o.OK || o.Exit != 0 || len(o.Checks) != 5 {
		t.Fatalf("ping: %+v", o)
	}
}

// The runner runs from a copy in the data directory: the installed binary
// can be replaced while a job runs.
func TestJobRunnerLeavesTheInstalledBinaryReplaceableWindows(t *testing.T) {
	t.Parallel()
	if outsideJob(t) {
		return
	}
	h := jobHarness(t)
	exe := filepath.Join(t.TempDir(), "switchboard.exe")
	copyFile(t, os.Args[0], exe)
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := h.cliCommand(exe, fastPoll, append([]string{"job", "run", "--"}, jobCommand("wait", ready, "0")...)...)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("job run: %v %s", err, b)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(b)), "PASS")), 10, 64)
	if err != nil {
		t.Fatalf("job run output %q", b)
	}
	h.running(id)
	if err = os.WriteFile(exe, []byte("a newer switchboard"), 0o700); err != nil {
		t.Fatalf("replace the installed binary while a job runs: %v", err)
	}
	copies, _ := filepath.Glob(filepath.Join(h.dir, "runner", "switchboard-*.exe"))
	if len(copies) != 1 {
		t.Fatalf("runner copies: %v", copies)
	}
	_ = os.WriteFile(ready, nil, 0o600)
	if j := h.ended(id); j.State != wire.JobExited || *j.ExitCode != 0 {
		t.Fatalf("job: %+v", j)
	}
}

// job stop ends the command and every process it started, even one no
// console event reaches.
func TestJobStopEndsTheCommandTreeWindows(t *testing.T) {
	t.Parallel()
	if outsideJob(t) {
		return
	}
	h := jobHarness(t)
	pidFile := filepath.Join(h.dir, "grandchild.pid")
	out := h.mustRun(fastPoll, append([]string{"job", "run", "--"}, jobCommand("tree", pidFile)...)...)
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("job run output %q", out)
	}
	h.running(id)
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if pid == 0 && time.Now().After(deadline) {
			t.Fatal("the command did not start its child")
		}
	}
	if !winjob.Alive(pid) {
		t.Fatal("grandchild not running")
	}
	h.mustRun(nil, "job", "stop", strconv.FormatInt(id, 10))
	if j := h.ended(id); j.State != wire.JobStopped {
		t.Fatalf("job: %+v", j)
	}
	for deadline := time.Now().Add(10 * time.Second); winjob.Alive(pid); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d outlived the stop", pid)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err = dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// outsideJob runs the calling test again outside this process's job object
// when that job forbids breakaway (a CI runner's, a terminal's), since no
// runner could start here: a process WMI creates (Win32_Process.Create)
// belongs to no job of ours. It returns true when it ran the test that way
// (and failed t if the copy failed); false means run the test here.
func outsideJob(t *testing.T) bool {
	t.Helper()
	if winjob.CheckBreakaway() == nil || os.Getenv("SWITCHBOARD_TEST_OUTSIDE") == "1" {
		return false
	}
	dir := t.TempDir()
	out, done := filepath.Join(dir, "out.txt"), filepath.Join(dir, "done.txt")
	script := filepath.Join(dir, "run.cmd")
	body := "@echo off\r\nset SWITCHBOARD_TEST_OUTSIDE=1\r\n" +
		`"` + os.Args[0] + `" -test.run "^` + t.Name() + `$" -test.v -test.count=1 -test.timeout 150s > "` + out + `" 2>&1` + "\r\n" +
		`echo %ERRORLEVEL% > "` + done + `.tmp"` + "\r\n" +
		`move /y "` + done + `.tmp" "` + done + `" > nul` + "\r\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$env:SB_CL; CurrentDirectory=$env:SB_CWD}; if ($r.ReturnValue -ne 0) { exit 1 }`)
	ps.Env = append(os.Environ(), `SB_CL=cmd.exe /c "`+script+`"`, "SB_CWD="+dir)
	if b, err := ps.CombinedOutput(); err != nil {
		t.Fatalf("start the test outside the job: %v %s", err, b)
	}
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(done); err == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(out)
			t.Fatalf("the test outside the job did not finish:\n%s", b)
		}
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "--- PASS: "+t.Name()) {
		t.Fatalf("outside the job:\n%s", b)
	}
	t.Logf("ran outside this process's job:\n%s", b)
	return true
}
