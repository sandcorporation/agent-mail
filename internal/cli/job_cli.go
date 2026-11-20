package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// runnerCommand is the hidden command the detached job runner runs as.
const runnerCommand = "__job-runner"

// SelfCommand builds a command that runs this binary with args (the job
// runner), from the runner copy where there is one (RunnerExecutable).
// Tests that run the client from a test binary replace it.
var SelfCommand = func(args ...string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return exec.Command(RunnerExecutable(exe), args...)
}

// ExitNoRunner is job run's (and watch start's) exit code when no runner
// was started on this machine: the caller (the plugin hook) then runs the
// command itself. A line on stderr says why.
const ExitNoRunner = 125

// errNoBreakaway: the runner would stay in the caller's job object and end
// with the terminal (Windows).
var errNoBreakaway = errors.New("the terminal's job does not let the runner leave it")

// noRunner tells the caller that the command was not handed over.
func noRunner(err error) (int, error) {
	fmt.Fprintf(os.Stderr, "%s: job not handed over, run the command yourself: %v\n", progName(), err)
	return ExitNoRunner, nil
}

// jobRetention is how long a finished job's local output is kept.
const jobRetention = 7 * 24 * time.Hour

var jobCmd = command{name: "job", args: "ACTION [ID]", nargs: 2, optArgs: true, extra: true,
	synopsis: "(ACTION: run | list | show ID | tail ID | stop ID | wait ID | ping) [-- CMD ARGS...]",
	summary:  "Hand a command to a detached runner on this machine that outlives the session, and list, show, tail, stop or wait for it; ping checks that a hand-over can work here",
	options: []option{
		{"--wait", "", "run: stream the output and exit with the command's exit code (killing the waiter leaves the job running)"},
		{"--expires", "DURATION", "run: how long it may run, e.g. 90m, 24h, 7d (default 24h)"},
		{"--no-extend", "", "run, watch start: the expiry is fixed; a presence sweep never moves it"},
		{"--cwd", "DIR", "run: working directory (default the current one)"},
		{"--label", "TEXT", "run: label (default the first 60 characters of the command)"},
		{"--idem", "KEY", "run: retry-safe key; running again with the same key returns the first job"},
		{"--state", "STATE", "list: only jobs in this state"},
		{"--owner", "ROLE", "list: only this role's jobs (boss roles)"},
		{"-f", "", "tail: follow the output until the job ends"},
		optJSON, optProfile,
	}}

func init() { commands = append(commands, jobCmd) }

// dataDir is the client's data directory (the config directory).
func dataDir() string {
	d := config.EnvAlias("CONFIG_DIR")
	if d == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = "."
		}
		d = filepath.Join(base, config.ConfigDirName)
	}
	return d
}

// jobsRoot is where this profile's job output lives: <data dir>/jobs/<profile>.
func jobsRoot() string { return filepath.Join(dataDir(), "jobs", profileName()) }

func jobDir(id int64) string { return filepath.Join(jobsRoot(), strconv.FormatInt(id, 10)) }

// jobSpec is what the runner reads from the job directory: the command never
// travels back from the server.
type jobSpec struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	// Expires is the job's expiry when it was created; the runner then
	// follows the server's (a watch's moves forward while its role is
	// attached), and keeps the last one it saw when the server is away.
	Expires time.Time `json:"expires"`
	// Kind is the job's kind; a watch's output lines are sent in batches
	// of Batch.
	Kind  string        `json:"kind,omitempty"`
	Batch time.Duration `json:"batch,omitempty"`
}

// jobExit is the runner's local end record (exit.json).
type jobExit struct {
	State string `json:"state"`
	Code  int    `json:"code"`
}

func cliJob(c Config, p parsedArgs) (int, error) {
	action := p.pos[0]
	needID := map[string]bool{"show": true, "tail": true, "stop": true, "wait": true}
	var id int64
	if needID[action] {
		if len(p.pos) < 2 {
			return 0, usagef("job %s needs a job ID", action)
		}
		var err error
		if id, err = strconv.ParseInt(strings.TrimPrefix(p.pos[1], "#"), 10, 64); err != nil || id <= 0 {
			return 0, usagef("job: invalid job ID %q", p.pos[1])
		}
	} else if len(p.pos) > 1 {
		return 0, usagef("job %s takes no ID", action)
	}
	if action != "run" && len(p.extra) > 0 {
		return 0, usagef("job %s takes no command", action)
	}
	switch action {
	case "run":
		return jobRun(c, p)
	case "list":
		return jobList(c, p)
	case "show":
		return jobShow(c, id, p.bool("--json"))
	case "tail":
		return jobTail(c, id, p.bool("-f"))
	case "stop":
		return jobStop(c, id, p.bool("--json"))
	case "wait":
		return jobWait(c, id)
	}
	return 0, usagef("job: unknown action %q (run, list, show, tail, stop, wait, ping)", action)
}

func getJob(c Config, id int64) (wire.Job, error) {
	var j wire.Job
	b, err := slotCall(c, "GET", "/api/jobs/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(b, &j); err != nil {
		return j, fmt.Errorf("unexpected response from server: %v", err)
	}
	return j, nil
}

func jobRun(c Config, p parsedArgs) (int, error) {
	return startJob(c, p, wire.JobCommand, 0, p.bool("--wait"))
}

// startJob registers a job of kind (job run, watch start), starts its runner
// and, with wait, streams its output and exits with its code.
func startJob(c Config, p parsedArgs, kind string, batch time.Duration, wait bool) (int, error) {
	name := "job run"
	if kind == wire.JobWatch {
		name = "watch start"
	}
	if len(p.extra) == 0 {
		return 0, usagef("%s needs a command after --", name)
	}
	expires := wire.DefaultExpiry(kind)
	if v := p.values["--expires"]; v != "" {
		d, err := parseDays(v)
		if err != nil || d <= 0 {
			return 0, usagef("%s: --expires takes a duration such as 90m, 24h or 7d", name)
		}
		expires = d
	}
	cwd := p.values["--cwd"]
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return 0, err
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return 0, usagef("%s: --cwd %s is not a directory", name, cwd)
	}
	if err = checkRunner(); err != nil {
		return noRunner(err)
	}
	machine, _ := os.Hostname()
	sweepJobs(jobsRoot(), time.Now())
	reportMachine(c, machine)
	q := wire.JobCreate{Kind: kind, Label: p.values["--label"], Command: shellJoin(p.extra), Cwd: cwd, Machine: machine,
		ExpiresIn: int64(expires / time.Second), IdempotencyKey: p.values["--idem"], NoExtend: p.bool("--no-extend")}
	b, created, err := createJob(c, q)
	if err != nil {
		return 0, err
	}
	var j wire.Job
	if err = json.Unmarshal(b, &j); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if created {
		if err = startRunner(j.ID, jobSpec{Argv: p.extra, Cwd: cwd, Expires: j.ExpiresAt, Kind: kind, Batch: batch}); err != nil {
			// Watched: the caller sees the error here, so no end mail.
			report := wire.JobReport{State: wire.JobFailed, ExitCode: intPtr(127), Error: "runner did not start: " + err.Error(), Watched: true}
			_, _ = slotCall(c, "POST", fmt.Sprintf("/api/jobs/%d/report", j.ID), report)
			return noRunner(fmt.Errorf("job %d: runner did not start: %w", j.ID, err))
		}
	}
	if wait {
		fmt.Fprintf(os.Stderr, "%s job %d\n", progName(), j.ID)
		return jobWait(c, j.ID)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
	} else {
		fmt.Println(j.ID)
	}
	return ExitOK, nil
}

// createJob posts the job; created is false when the idem key returned an
// existing one (its runner is not started again).
func createJob(c Config, q wire.JobCreate) ([]byte, bool, error) {
	b, _ := json.Marshal(q)
	req, _ := http.NewRequest("POST", endpoint(c, "/api/jobs"), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	authorize(c, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, resp.StatusCode == 201, nil
}

// startRunner writes the job directory and starts the runner detached from
// this process (its own session on Unix, stdin from /dev/null).
func startRunner(id int64, spec jobSpec) error {
	dir := jobDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "waiters"), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(spec)
	if err := writeUserFile(filepath.Join(dir, "spec.json"), b); err != nil {
		return err
	}
	var args []string
	if profileFlag != "" {
		args = append(args, "--profile", profileFlag)
	}
	if roleFlag != "" {
		args = append(args, "--role", roleFlag)
	}
	cmd := SelfCommand(append(args, runnerCommand, strconv.FormatInt(id, 10))...)
	cmd.Dir = dir
	logf, err := openUserFile(filepath.Join(dir, "runner.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf
	if err = startDetached(cmd); err != nil {
		return err
	}
	// Reap it if it ends while this process still runs (job run --wait), so
	// a finished runner is not seen as alive.
	go func() { _ = cmd.Wait() }()
	return nil
}

func jobList(c Config, p parsedArgs) (int, error) {
	v := url.Values{}
	if s := p.values["--state"]; s != "" {
		v.Set("state", s)
	}
	if r := p.values["--owner"]; r != "" {
		v.Set("role", r)
	}
	path := "/api/jobs"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	b, err := slotCall(c, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	var jobs []wire.Job
	if err = json.Unmarshal(b, &jobs); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	for _, j := range jobs {
		fmt.Printf("#%d  %s  %s@%s  %s\n", j.ID, stateText(j), j.Role, j.Machine, j.Label)
	}
	return ExitOK, nil
}

func stateText(j wire.Job) string {
	if j.ExitCode != nil && j.State == wire.JobExited {
		return fmt.Sprintf("exited %d", *j.ExitCode)
	}
	if j.StopRequested && !wire.JobEnded(j.State) {
		return j.State + " (stopping)"
	}
	return j.State
}

func jobShow(c Config, id int64, jsonOut bool) (int, error) {
	if jsonOut {
		b, err := slotCall(c, "GET", "/api/jobs/"+strconv.FormatInt(id, 10), nil)
		if err != nil {
			return 0, err
		}
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	j, err := getJob(c, id)
	if err != nil {
		return 0, err
	}
	fmt.Printf("job      #%d\nstate    %s\nlabel    %s\nowner    %s@%s\ncwd      %s\ncommand  %s\ncreated  %s\nexpires  %s\nextends  %t\n",
		j.ID, stateText(j), j.Label, j.Role, j.Machine, j.Cwd, j.Command, j.CreatedAt.Format(time.RFC3339), j.ExpiresAt.Format(time.RFC3339), j.Extends)
	if j.EndedAt != nil {
		fmt.Printf("ended    %s\n", j.EndedAt.Format(time.RFC3339))
	}
	if j.Error != "" {
		fmt.Printf("error    %s\n", j.Error)
	}
	return ExitOK, nil
}

func jobStop(c Config, id int64, jsonOut bool) (int, error) {
	b, err := slotCall(c, "POST", fmt.Sprintf("/api/jobs/%d/stop", id), nil)
	if err != nil {
		return 0, err
	}
	var j wire.Job
	if err = json.Unmarshal(b, &j); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	local := false
	if !wire.JobEnded(j.State) {
		if pid, ok := runnerPID(jobDir(id)); ok {
			local = stopRunner(pid) == nil
		}
	}
	if jsonOut {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	switch {
	case wire.JobEnded(j.State):
		fmt.Printf("#%d already %s\n", id, stateText(j))
	case local:
		fmt.Printf("#%d stopping\n", id)
	default:
		fmt.Printf("#%d stop recorded; its runner is on %s\n", id, j.Machine)
	}
	return ExitOK, nil
}

// jobWait streams the job's output from the start and returns its exit
// code. Without local output (another machine, or removed) it waits on the
// server and prints the tail it holds.
func jobWait(c Config, id int64) (int, error) {
	dir := jobDir(id)
	if _, err := os.Stat(filepath.Join(dir, "spec.json")); err != nil {
		return waitServer(c, id)
	}
	return waitLocal(c, id, dir, os.Stdout)
}

func waitServer(c Config, id int64) (int, error) {
	for {
		j, err := getJob(c, id)
		if err != nil {
			return 0, err
		}
		if wire.JobEnded(j.State) {
			if j.Tail != "" {
				fmt.Println(j.Tail)
			}
			return endCode(j.State, j.ExitCode), nil
		}
		time.Sleep(2 * time.Second)
	}
}

func endCode(state string, code *int) int {
	if code != nil {
		return *code
	}
	if state == wire.JobFailed {
		return 127
	}
	return ExitError
}

// waitLocal registers as a waiter, copies the output file to out as it grows
// and, once the runner wrote exit.json, drains the rest, marks the end seen
// (so the runner reports it watched and no end mail is sent) and returns the
// exit code.
func waitLocal(c Config, id int64, dir string, out io.Writer) (int, error) {
	me := filepath.Join(dir, "waiters", strconv.Itoa(os.Getpid()))
	if err := writeUserFile(me, nil); err != nil {
		return 0, err
	}
	defer os.Remove(me)
	f, err := openOutput(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	start, lastCheck := time.Now(), time.Now()
	for {
		ex, done := readExit(dir)
		if _, err = io.Copy(out, f); err != nil {
			return 0, err
		}
		if done {
			_ = writeUserFile(filepath.Join(dir, "seen"), nil)
			return ex.Code, nil
		}
		if time.Since(lastCheck) > time.Second {
			lastCheck = time.Now()
			pid, ok := runnerPID(dir)
			if (ok && !pidAlive(pid) || !ok && time.Since(start) > 15*time.Second) && !exitWritten(dir) {
				// The runner is gone without an end record: the server may
				// still know the end (it reported, then the file was lost).
				j, err := getJob(c, id)
				if err == nil && wire.JobEnded(j.State) {
					return endCode(j.State, j.ExitCode), nil
				}
				return 0, fmt.Errorf("job %d: the runner is gone without an end record", id)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openOutput opens the output file, waiting briefly for the runner to make it.
func openOutput(dir string) (*os.File, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		f, err := os.Open(filepath.Join(dir, "output"))
		if err == nil || !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return f, err
		}
		if exitWritten(dir) {
			// Ended without output (a failed start writes its error there,
			// so this is only a race with removal).
			return os.Open(os.DevNull)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readExit(dir string) (jobExit, bool) {
	var ex jobExit
	b, err := os.ReadFile(filepath.Join(dir, "exit.json"))
	if err != nil || json.Unmarshal(b, &ex) != nil {
		return ex, false
	}
	return ex, true
}

func exitWritten(dir string) bool { _, ok := readExit(dir); return ok }

func runnerPID(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}

func jobTail(c Config, id int64, follow bool) (int, error) {
	dir := jobDir(id)
	path := filepath.Join(dir, "output")
	f, err := os.Open(path)
	if err != nil {
		if follow {
			if j, e := getJob(c, id); e == nil && !wire.JobEnded(j.State) {
				return 0, fmt.Errorf("job %d: its output is on %s", id, j.Machine)
			}
		}
		j, err := getJob(c, id)
		if err != nil {
			return 0, err
		}
		if j.Tail != "" {
			fmt.Println(j.Tail)
		}
		return ExitOK, nil
	}
	defer f.Close()
	st, _ := f.Stat()
	from := st.Size() - 64<<10
	if from < 0 {
		from = 0
	}
	buf := make([]byte, st.Size()-from)
	n, _ := f.ReadAt(buf, from)
	if t := lastLines(string(buf[:n]), wire.MaxJobTailLines); t != "" {
		fmt.Println(t)
	}
	if !follow {
		return ExitOK, nil
	}
	if _, err = f.Seek(from+int64(n), io.SeekStart); err != nil {
		return 0, err
	}
	for {
		done := exitWritten(dir)
		if _, err = io.Copy(os.Stdout, f); err != nil || done {
			return ExitOK, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// sweepJobs removes local job directories whose job ended more than
// jobRetention ago.
func sweepJobs(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, err := strconv.ParseInt(e.Name(), 10, 64); err != nil || !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if st, err := os.Stat(filepath.Join(dir, "exit.json")); err == nil && now.Sub(st.ModTime()) > jobRetention {
			_ = os.RemoveAll(dir)
		}
	}
}

// parseDays is time.ParseDuration that also takes whole days ("7d").
func parseDays(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil {
			return 0, err
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellJoin is the command as one line a POSIX shell would read back.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if shellSafe.MatchString(a) {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

func intPtr(v int) *int { return &v }
