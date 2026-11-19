package server

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
)

// A port held by another process without a listener (#217): the doc's port
// check refuses it, and up exits at once without starting the node's
// backend.

// docPortCheck is the port check line of W3 (Windows) or L3 (Linux) and the
// script that runs it on port.
func docPortCheck(t *testing.T) (shell string, script func(port string) []string) {
	t.Helper()
	step, mark := "L3.", "is in use"
	if runtime.GOOS == "windows" {
		step, mark = "W3.", "TcpListener"
	}
	var line string
	for _, b := range readDocBlocks(t) {
		if !strings.HasPrefix(b.step, step) {
			continue
		}
		for _, l := range b.lines {
			if strings.Contains(l, mark) {
				line = l
			}
		}
	}
	if line == "" {
		t.Fatalf("%s has no port check line (%q)", step, mark)
	}
	if runtime.GOOS == "windows" {
		ps, err := exec.LookPath("powershell.exe")
		if err != nil {
			e2eSkip(t, "powershell.exe unavailable: %v", err)
		}
		return ps, func(port string) []string {
			return []string{"-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference = 'Stop'\n$port = " + port + "\n" + line}
		}
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			e2eSkip(t, "%s unavailable: %v", tool, err)
		}
	}
	return "bash", func(port string) []string {
		return []string{"-c", "set -euo pipefail\nSBPORT=" + port + "\n" + line}
	}
}

func TestE2E_HostingDocPortCheckRefusesABoundPort(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("the doc covers Linux and Windows")
	}
	shell, script := docPortCheck(t)
	held := boundPort(t)
	out, err := exec.Command(shell, script(held)...).CombinedOutput()
	if err == nil {
		t.Errorf("the port check passed on port %s, which another socket holds bound:\n%s", held, out)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	if out, err := exec.Command(shell, script(free)...).CombinedOutput(); err != nil {
		t.Errorf("the port check refused free port %s: %v\n%s", free, err, out)
	}
}

func TestE2E_UpOnABoundPortExitsAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("end to end: not in -short")
	}
	b := e2eBackendFor(t)
	dir, err := os.MkdirTemp("", "sbheld")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	instance := instanceFor(filepath.Join(data, upNodeToken))
	t.Cleanup(func() {
		b.end(instance)
		deadline := time.Now().Add(15 * time.Second)
		for os.RemoveAll(dir) != nil && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
	})
	bin := e2eBinary(t, filepath.Join(dir, "bin"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(cliEnv(),
		"SWITCHBOARD_ADMIN_AUTH=builtin",
		"SWITCHBOARD_HOSTING_AGENT="+exe,
		"SWITCHBOARD_HOSTING_MACHINE=pc-1")
	env = append(append(env, fakeagent.Env(filepath.Join(dir, "transcripts"))...), b.env...)

	port := boundPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), e2eWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "up", "--data", data, "--port", port)
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	out, err := cmd.CombinedOutput()
	took := time.Since(start)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("up on bound port %s: %v (context %v) after %s, want a non-zero exit:\n%s", port, err, ctx.Err(), took, out)
	}
	if took > 10*time.Second {
		t.Errorf("up took %s to exit, want a few seconds", took)
	}
	if want := "up: listen tcp 127.0.0.1:" + port + ": bind"; !strings.Contains(string(out), want) {
		t.Errorf("up's output lacks %q:\n%s", want, out)
	}
	if backendRunning(env, instance) {
		t.Errorf("up left the backend (tmux server, keeper) of %s running:\n%s", instance, out)
	}
}
