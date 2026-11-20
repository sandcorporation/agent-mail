package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
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
	// defaultJobPoll is how often the runner asks the server whether the job
	// was stopped (dashboard, another machine) and when it expires.
	defaultJobPoll = 5 * time.Second
)

// jobPoll is the poll interval, SWITCHBOARD_JOB_POLL when set (tests).
func jobPoll() time.Duration {
	if v, _ := config.EnvFrom(os.Getenv, "JOB_POLL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultJobPoll
}

// jobRunner is the hidden `__job-runner ID`: it runs the job's command as its
// child (in a process group of its own), writes the output to the job
// directory, reports running and the end to the server, and stops the
// command on SIGTERM or SIGINT (`job stop` on this machine), on a stop the
// server recorded (the dashboard, `job stop` elsewhere) and at its expiry.
// The stop request and the expiry are all it reads from the server: the
// command comes from the local spec only. It never logs the command.
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
	var lines *lineBatcher
	if spec.Kind == wire.JobWatch {
		// A watch's output also goes through the line batcher; one writer
		// for both streams keeps their lines whole.
		lines = newLineBatcher()
		w := io.MultiWriter(out, lines)
		cmd.Stdout, cmd.Stderr = w, w
		// A child left holding the pipe does not keep the end from the runner.
		cmd.WaitDelay = stopGrace
	}
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(out, "%s: %v\n", progName(), err)
		finishJob(dir, wire.JobReport{State: wire.JobFailed, ExitCode: intPtr(127), Error: err.Error()}, report)
		return ExitOK, nil
	}
	report(wire.JobReport{State: wire.JobRunning}, 3)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	batched := make(chan struct{})
	if lines != nil {
		window := spec.Batch
		if window <= 0 {
			window = wire.DefaultWatchBatch
		}
		go func() { runBatches(lines, window, time.After, watchSender(c, id, dir)); close(batched) }()
	} else {
		close(batched)
	}
	polled := make(chan wire.Job)
	go pollJob(c, id, dir, done, polled)
	expiry := time.NewTimer(time.Until(spec.Expires))
	defer expiry.Stop()
	if spec.Expires.IsZero() {
		expiry.Stop()
	}
	expires := spec.Expires
	endState := "" // stopped or expired once the runner began stopping
	var kill <-chan time.Time
	stop := func(state string) {
		if endState == "" {
			endState = state
			_ = signalGroup(cmd.Process.Pid, false)
			kill = time.After(stopGrace)
		}
	}
wait:
	for {
		select {
		case <-sigs:
			stop(wire.JobStopped)
		case j := <-polled:
			if j.StopRequested {
				stop(wire.JobStopped)
			} else if !j.ExpiresAt.IsZero() && !j.ExpiresAt.Equal(expires) {
				expires = j.ExpiresAt
				expiry.Stop()
				expiry.Reset(time.Until(expires))
			}
		case <-expiry.C:
			stop(wire.JobExpired)
		case <-kill:
			_ = signalGroup(cmd.Process.Pid, true)
		case <-done:
			break wait
		}
	}
	// The last batch goes out before the end.
	if lines != nil {
		lines.Close()
	}
	<-batched
	q := wire.JobReport{State: wire.JobExited, ExitCode: intPtr(exitCodeOf(cmd.ProcessState))}
	if endState != "" {
		q.State = endState
	}
	finishJob(dir, q, report)
	return ExitOK, nil
}

// pollJob sends the server's record of the job to out every jobPoll until
// done closes. A failed poll is skipped: the runner keeps the last expiry.
// When the output grew it first reports the tail (the dashboard's last line
// of a running job).
func pollJob(c Config, id int64, dir string, done <-chan struct{}, out chan<- wire.Job) {
	t := time.NewTicker(jobPoll())
	defer t.Stop()
	var size int64
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if st, err := os.Stat(filepath.Join(dir, "output")); err == nil && st.Size() != size {
			if tail := readTail(filepath.Join(dir, "output")); tail != "" {
				if _, err = slotCall(c, "POST", fmt.Sprintf("/api/jobs/%d/report", id), wire.JobReport{State: wire.JobRunning, Tail: tail}); err == nil {
					size = st.Size()
				}
			}
		}
		j, err := getJob(c, id)
		if err != nil {
			continue
		}
		select {
		case out <- j:
		case <-done:
			return
		}
	}
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
