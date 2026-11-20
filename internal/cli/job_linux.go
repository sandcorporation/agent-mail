//go:build linux

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
)

// unitStartTimeout bounds systemd-run, which returns once the unit started.
const unitStartTimeout = 10 * time.Second

// cgroupFile is where this process's cgroup is read, SWITCHBOARD_JOB_CGROUP
// when set (tests).
func cgroupFile() string {
	if v, _ := config.EnvFrom(os.Getenv, "JOB_CGROUP"); v != "" {
		return v
	}
	return "/proc/self/cgroup"
}

// inScope reports whether this process runs in a systemd scope: a terminal
// host may stop it when the terminal closes, and that kills every process in
// it, whatever its session or parent.
func inScope() bool {
	f, err := os.Open(cgroupFile())
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// cgroup v2 "0::/path"; v1 the systemd hierarchy "N:name=systemd:/path".
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 || !(parts[0] == "0" && parts[1] == "" || parts[1] == "name=systemd") {
			continue
		}
		if strings.HasSuffix(parts[2], ".scope") {
			return true
		}
	}
	return false
}

// userManager reports whether a systemd user manager looks reachable:
// systemd-run is installed and the user bus or the manager's private socket
// exists.
func userManager() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return false
	}
	for _, s := range []string{"bus", "systemd/private"} {
		if _, err := os.Stat(filepath.Join(rt, s)); err == nil {
			return true
		}
	}
	return false
}

// scopeCheck fails where the runner would stay in the caller's scope.
func scopeCheck() error {
	if inScope() && !userManager() {
		return errNoEscape
	}
	return nil
}

// startUnit starts cmd as a transient service of the user manager, outside
// the caller's cgroup, when one is reachable. done is false when there is
// none (or it refused and the caller is in no scope): the caller detaches
// the runner itself. The unit is named after the job directory, never the
// command; its environment values travel by name (-E NAME), not on argv.
func startUnit(cmd *exec.Cmd) (done bool, err error) {
	if !userManager() {
		if inScope() {
			return true, errNoEscape
		}
		return false, nil
	}
	exe, err := filepath.Abs(cmd.Path)
	if err != nil {
		return true, err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	sum := sha256.Sum256([]byte(cmd.Dir))
	unit := fmt.Sprintf("switchboard-job-%s-%s", filepath.Base(cmd.Dir), hex.EncodeToString(sum[:4]))
	log := filepath.Join(cmd.Dir, "runner.log")
	args := []string{"--user", "--quiet", "--collect", "--unit", unit, "--working-directory", cmd.Dir,
		// The runner ends the command's process group itself; what the
		// command left running in the background is left alone, as before.
		"-p", "KillMode=process",
		"-p", "StandardOutput=append:" + log, "-p", "StandardError=append:" + log}
	seen := map[string]bool{}
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && name != "" && !seen[name] {
			seen[name] = true
			args = append(args, "-E", name)
		}
	}
	args = append(append(args, "--", exe), cmd.Args[1:]...)
	ctx, cancel := context.WithTimeout(context.Background(), unitStartTimeout)
	defer cancel()
	run := exec.CommandContext(ctx, "systemd-run", args...)
	run.Env = env
	out, err := run.CombinedOutput()
	if err == nil {
		return true, nil
	}
	err = fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	if inScope() {
		return true, errors.Join(errNoEscape, err)
	}
	return false, nil
}
