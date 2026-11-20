package wire

import (
	"errors"
	"strings"
	"time"
)

// Job kinds (#288). Only command jobs are started in this version; watch
// jobs come with `switchboard watch start`.
const (
	JobCommand = "command"
	JobWatch   = "watch"
)

// Job states. handed: registered, the runner not yet reporting; running:
// the runner started the command; the rest are end states and never change.
const (
	JobHanded  = "handed"
	JobRunning = "running"
	JobExited  = "exited"
	JobFailed  = "failed"
	JobStopped = "stopped"
	JobExpired = "expired"
	JobLost    = "lost"
)

// JobEnded reports whether state is an end state.
func JobEnded(state string) bool {
	switch state {
	case JobExited, JobFailed, JobStopped, JobExpired, JobLost:
		return true
	}
	return false
}

// Limits of a job record.
const (
	MaxJobLabel     = 200
	MaxJobCommand   = 16 << 10
	MaxJobTailBytes = 8 << 10 // the server keeps at most this much tail
	MaxJobTailLines = 50
	// DefaultJobExpiry is how long a command job may run unless --expires
	// says otherwise; DefaultWatchExpiry a watch (and a schedule).
	DefaultJobExpiry   = 24 * time.Hour
	DefaultWatchExpiry = 7 * 24 * time.Hour
)

// DefaultExpiry is a kind's expiry when the creator gave none.
func DefaultExpiry(kind string) time.Duration {
	if kind == JobCommand {
		return DefaultJobExpiry
	}
	return DefaultWatchExpiry
}

// AutoExtends reports whether a kind's expiry moves forward while a session
// of its owner role is attached: watches and schedules do, commands do not.
func AutoExtends(kind string) bool { return kind != JobCommand }

// Job is one command handed over to a runner (GET /api/jobs/ID).
type Job struct {
	ID            int64      `json:"id"`
	Role          string     `json:"role"`
	Machine       string     `json:"machine"`
	Kind          string     `json:"kind"`
	Label         string     `json:"label"`
	Command       string     `json:"command"`
	Cwd           string     `json:"cwd"`
	State         string     `json:"state"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	StopRequested bool       `json:"stop_requested,omitempty"`
	Tail          string     `json:"tail,omitempty"`
}

// JobCreate is the body of POST /api/jobs (switchboard job run).
type JobCreate struct {
	Kind           string `json:"kind,omitempty"` // default command
	Label          string `json:"label,omitempty"`
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	Machine        string `json:"machine"`
	ExpiresIn      int64  `json:"expires_in,omitempty"` // seconds; 0 is DefaultExpiry(kind)
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// Check fills the defaults (kind, label from the command) and refuses an
// empty command, machine or directory, an unknown kind and over-long fields.
func (q *JobCreate) Check() error {
	q.Label, q.Machine = oneLine(q.Label), strings.TrimSpace(q.Machine)
	if q.Kind == "" {
		q.Kind = JobCommand
	}
	switch {
	case q.Kind != JobCommand && q.Kind != JobWatch:
		return errors.New("kind must be command or watch")
	case strings.TrimSpace(q.Command) == "":
		return errors.New("command required")
	case len(q.Command) > MaxJobCommand:
		return errors.New("command over 16 KB")
	case q.Machine == "" || strings.TrimSpace(q.Cwd) == "":
		return errors.New("machine and cwd required")
	case q.ExpiresIn < 0:
		return errors.New("expires_in must not be negative")
	}
	if q.Label == "" {
		q.Label = oneLine(q.Command)
		if r := []rune(q.Label); len(r) > 60 {
			q.Label = string(r[:60])
		}
	}
	if len(q.Label) > MaxJobLabel {
		return errors.New("label over 200 bytes")
	}
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// JobReport is the body of POST /api/jobs/ID/report: the runner's state.
// running records the start; an end state records the exit code, the error
// of a failed start, the tail and whether a waiter saw the end (watched:
// then no end mail is sent).
type JobReport struct {
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Tail     string `json:"tail,omitempty"`
	Watched  bool   `json:"watched,omitempty"`
}

// Check refuses a state the runner cannot report and caps the tail.
func (q *JobReport) Check() error {
	switch q.State {
	case JobRunning, JobStopped, JobExpired:
	case JobExited:
		if q.ExitCode == nil {
			return errors.New("exited needs exit_code")
		}
	case JobFailed:
	default:
		return errors.New("state must be running, exited, failed, stopped or expired")
	}
	q.Tail = JobTail(q.Tail)
	if len(q.Error) > 1000 {
		q.Error = q.Error[:1000]
	}
	return nil
}

// JobTail is the last MaxJobTailLines lines of out, at most MaxJobTailBytes
// (cut at a line start when it can be).
func JobTail(out string) string {
	out = strings.TrimRight(out, "\n")
	if len(out) > MaxJobTailBytes {
		out = out[len(out)-MaxJobTailBytes:]
		if i := strings.IndexByte(out, '\n'); i >= 0 && i < len(out)-1 {
			out = out[i+1:]
		}
	}
	lines := strings.Split(out, "\n")
	if len(lines) > MaxJobTailLines {
		lines = lines[len(lines)-MaxJobTailLines:]
	}
	return strings.ToValidUTF8(strings.Join(lines, "\n"), "�")
}
