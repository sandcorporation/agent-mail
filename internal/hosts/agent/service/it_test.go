package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRoundTripOnThisOS installs a real service on this machine, checks it
// runs, then removes it. Opt in with HOSTS_SERVICE_IT=1. Supported here:
// macOS (launchd) and Linux (systemd user manager).
func TestRoundTripOnThisOS(t *testing.T) {
	if os.Getenv("HOSTS_SERVICE_IT") != "1" {
		t.Skip("set HOSTS_SERVICE_IT=1 to install a real service")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("round trip not scripted for %s", runtime.GOOS)
	}
	dir := t.TempDir()
	name := "dev.hosts-service-it." + strconv.Itoa(os.Getpid())
	log := filepath.Join(dir, "logs", "it.log")
	u := Unit{
		Name:    name,
		Exec:    "/bin/sh",
		Args:    []string{"-c", `echo "started FOO=$FOO in $(pwd)"; exec sleep 300`},
		Env:     []string{"FOO=bar 100%"},
		WorkDir: dir,
		LogPath: log,
	}
	t.Cleanup(func() { _ = Uninstall(name) })

	res, err := Install(u)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Logf("installed %v, notes %q", res.Files, res.Notes)

	var st State
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err = Status(name)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Installed && st.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service never ran: %+v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}

	realDir, _ := filepath.EvalSymlinks(dir)
	want := "started FOO=bar 100% in " + realDir
	for {
		b, _ := os.ReadFile(log)
		if strings.Contains(string(b), want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %q lacks %q", b, want)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := Uninstall(name); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if st, err = Status(name); err != nil || st.Installed || st.Running {
		t.Fatalf("after Uninstall: %+v, %v", st, err)
	}
	if err := Uninstall(name); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
}
