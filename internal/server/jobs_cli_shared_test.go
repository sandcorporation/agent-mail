package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func (h *cliHarness) jobDir(id int64) string {
	return filepath.Join(h.dir, "jobs", "default", strconv.FormatInt(id, 10))
}

// ended waits until the server recorded the job's end.
func (h *cliHarness) ended(id int64) wire.Job {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := h.s.jobs().Get(id)
		if err == nil && wire.JobEnded(j.State) {
			return j
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(h.jobDir(id), "runner.log"))
			h.t.Fatalf("job %d did not end: %+v %v\nrunner log: %s", id, j, err, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func jobID(t *testing.T, stderr string) int64 {
	t.Helper()
	first, _, _ := strings.Cut(stderr, "\n")
	id, err := strconv.ParseInt(strings.TrimPrefix(first, "switchboard job "), 10, 64)
	if err != nil || !strings.HasPrefix(first, "switchboard job ") {
		t.Fatalf("first stderr line %q", first)
	}
	return id
}

// fastPoll makes the runner ask the server every 100 ms.
var fastPoll = []string{"SWITCHBOARD_JOB_POLL=100ms"}

func (h *cliHarness) running(id int64) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if j, _ := h.s.jobs().Get(id); j.State == wire.JobRunning {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("job never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
