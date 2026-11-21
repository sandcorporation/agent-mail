//go:build unix

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandcorporation/switchboard/internal/cli"
)

func TestJobPingPassesWhenAHandOverCanWork(t *testing.T) {
	t.Parallel()
	h := jobHarness(t)
	o := h.ping(nil)
	if !o.OK || o.Exit != 0 || len(o.Checks) != 5 {
		t.Fatalf("ping: %+v", o)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.dir, "jobs", "default")); len(entries) != 0 {
		t.Fatalf("ping left files: %v", entries)
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("jobs registered: %d %v", n, err)
	}
}

func TestJobPingReadOnlyDataDirectoryIsADataFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only mode")
	}
	h := jobHarness(t)
	jobs := filepath.Join(h.dir, "jobs")
	if err := os.Mkdir(jobs, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jobs, 0o700) })
	o := h.ping(nil)
	if o.OK || o.Exit != cli.ExitPingData || !failed(o, "data") || failed(o, "server") || failed(o, "token") {
		t.Fatalf("ping: %+v", o)
	}
}
