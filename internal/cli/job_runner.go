package cli

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/ownerfile"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Runner timings.
const (
	stopGrace   = 5 * time.Second  // terminate, then kill after this
	seenWait    = 3 * time.Second  // how long a live waiter has to mark the end seen
	reportTries = 12               // end report retries (backoff 0.5 s doubling, capped)
	reportCap   = 30 * time.Second // longest pause between end report retries
	tailRead    = int64(wire.MaxJobTailBytes) * 2
)

// jobRunner is the hidden `__job-runner ID`: it runs the job's command as its
// child (in a process group of its own), writes the output to the job
// directory, reports running and the end to the server, and stops the
// command on SIGTERM or SIGINT (`job stop`). It never logs the command.
func jobRunner(c Config, arg string) (int, error) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 {
		return 0, usagef("invalid job ID %q", arg)
	}
	dir := jobDir(id)
	var spec jobSpec
	b, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil || json.Unmarshal(b, &spec) != nil || len(spec.Argv) == 0 {
		return 0, fmt.Errorf("job %d: no spec in %s", id, dir)
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, stopSignals...)
	signal.Ignore(ignoredSignals...)
	if err = writeUserFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid()))); err != nil {
		return 0, err
	}
	out, err := openUserFile(filepath.Join(dir, "output"))
	if err != nil {
		return 0, err
	}
	defer out.Close()
	report := func(q wire.JobReport, tries int) {
		path := fmt.Sprintf("/api/jobs/%d/report", id)
		pause := 500 * time.Millisecond
		for i := 0; ; i++ {
			_, err := slotCall(c, "POST", path, q)
			if err == nil || i+1 >= tries {
				if err != nil {
					log.Printf("job %d: report %s failed: %v", id, q.State, err)
				}
				return
			}
			time.Sleep(pause)
			if pause *= 2; pause > reportCap {
				pause = reportCap
			}
		}
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = spec.Cwd, out, out
	cmd.SysProcAttr = childAttr()
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(out, "%s: %v\n", progName(), err)
		finishJob(dir, wire.JobReport{State: wire.JobFailed, ExitCode: intPtr(127), Error: err.Error()}, report)
		return ExitOK, nil
	}
	report(wire.JobReport{State: wire.JobRunning}, 3)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopping := false
	var kill <-chan time.Time
wait:
	for {
		select {
		case <-sigs:
			if !stopping {
				stopping = true
				_ = signalGroup(cmd.Process.Pid, false)
				kill = time.After(stopGrace)
			}
		case <-kill:
			_ = signalGroup(cmd.Process.Pid, true)
		case <-done:
			break wait
		}
	}
	q := wire.JobReport{State: wire.JobExited, ExitCode: intPtr(exitCodeOf(cmd.ProcessState))}
	if stopping {
		q.State = wire.JobStopped
	}
	finishJob(dir, q, report)
	return ExitOK, nil
}

// finishJob writes exit.json (what local waiters wait for), gives a live
// waiter time to mark the end seen, and reports the end with the tail.
func finishJob(dir string, q wire.JobReport, report func(wire.JobReport, int)) {
	ex, _ := json.Marshal(jobExit{State: q.State, Code: *q.ExitCode})
	tmp := filepath.Join(dir, "exit.json.tmp")
	if writeUserFile(tmp, ex) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "exit.json"))
	}
	q.Watched = awaitSeen(dir)
	q.Tail = readTail(filepath.Join(dir, "output"))
	report(q, reportTries)
}

// awaitSeen reports whether a waiter saw the end: none alive means no; with
// one alive, yes once it wrote "seen" within seenWait.
func awaitSeen(dir string) bool {
	deadline := time.Now().Add(seenWait)
	for {
		if _, err := os.Stat(filepath.Join(dir, "seen")); err == nil {
			return true
		}
		if !liveWaiter(dir) || time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func liveWaiter(dir string) bool {
	entries, _ := os.ReadDir(filepath.Join(dir, "waiters"))
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid != os.Getpid() && pidAlive(pid) {
			return true
		}
	}
	return false
}

func readTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	from := st.Size() - tailRead
	if from < 0 {
		from = 0
	}
	buf := make([]byte, st.Size()-from)
	n, _ := f.ReadAt(buf, from)
	s := string(buf[:n])
	if from > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return wire.JobTail(s)
}

// writeUserFile writes path with owner-only access.
func writeUserFile(path string, data []byte) error { return ownerfile.Write(path, data) }

// openUserFile opens path for appending, created with owner-only access.
func openUserFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err = ownerfile.RestrictToUser(path); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
