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
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// watchMails waits until role a has n watch mails and returns them.
func (h *cliHarness) watchMails(n int) []wire.Message {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		m := watchMail(h.t, h.s, "a")
		if len(m) >= n {
			return m
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d watch mails, want %d: %+v", len(m), n, m)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *cliHarness) startWatch(args ...string) int64 {
	h.t.Helper()
	out := h.mustRun(fastPoll, append([]string{"watch", "start"}, args...)...)
	id, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		h.t.Fatalf("watch start printed %q", out)
	}
	h.t.Cleanup(func() { h.run(nil, "job", "stop", strconv.FormatInt(id, 10)) })
	return id
}

func TestWatchLinesWithinTheWindowMakeOneMail(t *testing.T) {
	t.Parallel()
	h := jobHarness(t)
	id := h.startWatch("--batch", "300ms", "--label", "app log", "--", "sh", "-c", "for i in 1 2 3 4 5; do echo l$i; done; sleep 30")
	m := h.watchMails(1)
	if m[0].Subject != "[감시] app log: 5줄" || m[0].Priority != "urgent" || !strings.HasSuffix(m[0].Body, "l1\nl2\nl3\nl4\nl5") {
		t.Fatalf("watch mail: %+v", m[0])
	}
	if j, _ := h.s.jobs().Get(id); j.Kind != wire.JobWatch || j.State != wire.JobRunning {
		t.Fatalf("watch job: %+v", j)
	}
	// The running watch's last line reaches the server (the dashboard's
	// last line), and no second mail follows.
	deadline := time.Now().Add(10 * time.Second)
	for j, _ := h.s.jobs().Get(id); !strings.HasSuffix(j.Tail, "l5"); j, _ = h.s.jobs().Get(id) {
		if time.Now().After(deadline) {
			t.Fatalf("running tail: %q", j.Tail)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m = watchMail(t, h.s, "a"); len(m) != 1 {
		t.Fatalf("watch mails: %+v", m)
	}
}

func TestWatchLinesInTwoWindowsMakeTwoMailsAndTheExitOneEndMail(t *testing.T) {
	t.Parallel()
	h := jobHarness(t)
	ready := filepath.Join(t.TempDir(), "ready")
	id := h.startWatch("--batch", "200ms", "--label", "w", "--", "sh", "-c",
		`echo a; echo b; while [ ! -e "$0" ]; do sleep 0.05; done; echo c; exit 0`, ready)
	if m := h.watchMails(1); m[0].Subject != "[감시] w: 2줄" {
		t.Fatalf("first window: %+v", m)
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	j := h.ended(id)
	m := h.watchMails(2)
	if len(m) != 2 || m[1].Subject != "[감시] w: 1줄" || !strings.HasSuffix(m[1].Body, "c") {
		t.Fatalf("second window: %+v", m)
	}
	if j.State != wire.JobExited || *j.ExitCode != 0 {
		t.Fatalf("watch end: %+v", j)
	}
	if end := jobMail(t, h.s, "a"); len(end) != 1 || !strings.HasSuffix(end[0].Subject, "exit 0") {
		t.Fatalf("end mail: %+v", end)
	}
}

// While attached, lines stream to stdout and no mail goes out; a killed
// attacher leaves the watch running and mail takes over.
func TestWatchAttachStreamsThenAKilledAttacherHandsOverToMail(t *testing.T) {
	t.Parallel()
	h := jobHarness(t)
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	args, _ := json.Marshal([]string{"watch", "start", "--attach", "--batch", "100ms", "--", "sh", "-c",
		`echo first; while [ ! -e "$0" ]; do sleep 0.05; done; echo after; sleep 30`, ready})
	cmd.Env = append(cliEnv(), "SWITCHBOARD_TEST_CLI=1", "AGENT_MAIL_CONFIG_DIR="+h.dir, "SWITCHBOARD_TEST_ARGS="+string(args))
	stderr, _ := cmd.StderrPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	id := jobID(t, line)
	t.Cleanup(func() { h.run(nil, "job", "stop", strconv.FormatInt(id, 10)) })
	if got, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || got != "first\n" {
		t.Fatalf("attached stdout: %q %v", got, err)
	}
	// The batch window passed while attached (the runner judges it after
	// the window): still no mail.
	time.Sleep(400 * time.Millisecond)
	if m := watchMail(t, h.s, "a"); len(m) != 0 {
		t.Fatalf("mail while attached: %+v", m)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := h.watchMails(1)
	if m[0].Subject != "[감시] "+mustJob(t, h, id).Label+": 1줄" || !strings.HasSuffix(m[0].Body, "after") {
		t.Fatalf("mail after the attacher died: %+v", m)
	}
	if j := mustJob(t, h, id); j.State != wire.JobRunning {
		t.Fatalf("the watch ended with its attacher: %+v", j)
	}
}

func mustJob(t *testing.T, h *cliHarness, id int64) wire.Job {
	t.Helper()
	j, err := h.s.jobs().Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
