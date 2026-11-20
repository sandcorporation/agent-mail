package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// reportMachine tells the server which jobs have a live runner on this
// machine, so the role's jobs it ran here that ended with the machine (a
// reboot) or with a killed runner become lost. Best effort: a failure only
// leaves them to the server's silence limit.
func reportMachine(c Config, machine string) {
	if machine == "" {
		return
	}
	_, _ = slotCall(c, "POST", "/api/jobs/machine", wire.JobMachine{Machine: machine, Live: liveLocalJobs(filepath.Join(dataDir(), "jobs"), time.Now())})
}

// liveLocalJobs lists the job IDs under root (every profile's folder) that
// have not ended here: no exit.json, and a live runner, or no runner pid yet
// in a folder younger than wire.JobLostGrace.
func liveLocalJobs(root string, now time.Time) []int64 {
	dirs, _ := filepath.Glob(filepath.Join(root, "*", "*"))
	live := []int64{}
	for _, dir := range dirs {
		id, err := strconv.ParseInt(filepath.Base(dir), 10, 64)
		if err != nil {
			continue
		}
		if _, ok := readExit(dir); ok {
			continue
		}
		if pid, ok := runnerPID(dir); ok {
			if pidAlive(pid) {
				live = append(live, id)
			}
			continue
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() && now.Sub(st.ModTime()) < wire.JobLostGrace {
			live = append(live, id)
		}
	}
	return live
}
