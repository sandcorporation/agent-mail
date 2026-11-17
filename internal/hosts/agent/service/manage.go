package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// runFunc runs a command and returns its combined output.
type runFunc func(name string, args ...string) (string, error)

// host is the machine Install, Uninstall and Status act on. Tests swap the
// command runner and home directory.
type host struct {
	goos string
	home string
	uid  int
	user string // Windows DOMAIN\name of the current user
	run  runFunc
}

func currentHost() (host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return host{}, err
	}
	h := host{goos: runtime.GOOS, home: home, uid: os.Getuid(), run: runCommand}
	if h.goos == "windows" {
		h.user = os.Getenv("USERNAME")
		if d := os.Getenv("USERDOMAIN"); d != "" && h.user != "" {
			h.user = d + `\` + h.user
		}
	}
	return h, nil
}

func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, s)
	}
	return s, nil
}

// Install writes u's service definition for the current OS and starts it.
// Installing again replaces the definition and restarts the service.
func Install(u Unit) (Installed, error) {
	h, err := currentHost()
	if err != nil {
		return Installed{}, err
	}
	return h.install(u)
}

// Uninstall stops the service and removes its definition. Uninstalling a
// service that is not installed is not an error.
func Uninstall(name string) error {
	h, err := currentHost()
	if err != nil {
		return err
	}
	return h.uninstall(name)
}

// Status reports whether the service is installed and running.
func Status(name string) (State, error) {
	h, err := currentHost()
	if err != nil {
		return State{}, err
	}
	return h.status(name)
}

func (h host) abs(rel string) string { return filepath.Join(h.home, filepath.FromSlash(rel)) }

func (h host) launchdTarget(name string) string {
	return "gui/" + strconv.Itoa(h.uid) + "/" + name
}

func (h host) install(u Unit) (Installed, error) {
	if h.goos == "windows" && u.User == "" {
		u.User = h.user
	}
	files, err := Render(h.goos, u)
	if err != nil {
		return Installed{}, err
	}
	var res Installed
	if u.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(u.LogPath), 0o755); err != nil {
			return res, err
		}
	}
	for _, f := range files {
		p := h.abs(f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(p, f.Content, 0o644); err != nil {
			return res, err
		}
		res.Files = append(res.Files, p)
	}

	switch h.goos {
	case "linux":
		unit := u.Name + ".service"
		for _, args := range [][]string{
			{"--user", "daemon-reload"},
			{"--user", "enable", unit},
			{"--user", "restart", unit},
		} {
			if _, err := h.run("systemctl", args...); err != nil {
				return res, err
			}
		}
		res.Notes = append(res.Notes,
			"systemd stops user services when you log out. To keep this one running, run: loginctl enable-linger")
	case "darwin":
		target := h.launchdTarget(u.Name)
		// Replace a loaded copy; bootout fails harmlessly when none is loaded.
		_, _ = h.run("launchctl", "bootout", target)
		if _, err := h.run("launchctl", "enable", target); err != nil {
			return res, err
		}
		if _, err := h.run("launchctl", "bootstrap", "gui/"+strconv.Itoa(h.uid), h.abs(launchdPath(u.Name))); err != nil {
			return res, err
		}
	case "windows":
		if _, err := h.run("schtasks", "/Create", "/TN", u.Name, "/XML", h.abs(windowsTaskPath(u.Name)), "/F"); err != nil {
			return res, err
		}
		if _, err := h.run("schtasks", "/Run", "/TN", u.Name); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (h host) definitionFiles(name string) []string {
	switch h.goos {
	case "linux":
		return []string{h.abs(systemdPath(name))}
	case "darwin":
		return []string{h.abs(launchdPath(name))}
	case "windows":
		return []string{h.abs(windowsScriptPath(name)), h.abs(windowsTaskPath(name))}
	}
	return nil
}

func (h host) checkHost(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("service: invalid name %q", name)
	}
	switch h.goos {
	case "linux", "darwin", "windows":
		return nil
	}
	return fmt.Errorf("service: unsupported OS %q", h.goos)
}

func (h host) uninstall(name string) error {
	if err := h.checkHost(name); err != nil {
		return err
	}
	installed := h.anyFile(name)
	switch h.goos {
	case "linux":
		unit := name + ".service"
		if installed {
			// disable --now fails when the unit is already gone from
			// systemd's view; the file removal below is what matters.
			_, _ = h.run("systemctl", "--user", "disable", "--now", unit)
		}
	case "darwin":
		_, _ = h.run("launchctl", "bootout", h.launchdTarget(name))
	case "windows":
		_, _ = h.run("schtasks", "/End", "/TN", name)
		if out, err := h.run("schtasks", "/Delete", "/TN", name, "/F"); err != nil && installed {
			return fmt.Errorf("service: delete task: %w (%s)", err, out)
		}
	}
	for _, p := range h.definitionFiles(name) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if h.goos == "windows" {
		_ = os.Remove(h.abs(windowsDir(name))) // only if empty
	}
	if h.goos == "linux" && installed {
		if _, err := h.run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	return nil
}

func (h host) anyFile(name string) bool {
	for _, p := range h.definitionFiles(name) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func (h host) status(name string) (State, error) {
	if err := h.checkHost(name); err != nil {
		return State{}, err
	}
	st := State{Installed: h.anyFile(name)}
	switch h.goos {
	case "linux":
		// is-active exits non-zero for anything but "active"; its output is
		// still the state word.
		out, _ := h.run("systemctl", "--user", "is-active", name+".service")
		st.Detail = firstLine(out)
		st.Running = st.Detail == "active"
	case "darwin":
		out, err := h.run("launchctl", "print", h.launchdTarget(name))
		if err != nil {
			st.Detail = "not loaded"
			return st, nil
		}
		st.Detail = launchdState(out)
		st.Running = st.Detail == "running"
	case "windows":
		out, err := h.run("schtasks", "/Query", "/TN", name, "/FO", "CSV", "/NH")
		if err != nil {
			st.Detail = "not registered"
			return st, nil
		}
		st.Detail = schtasksStatus(out)
		st.Running = strings.EqualFold(st.Detail, "Running")
	}
	return st, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

// launchdState pulls the top-level "state = ..." line from launchctl print.
func launchdState(out string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if ok && k == "state" {
			return strings.TrimSpace(v)
		}
	}
	return "loaded"
}

// schtasksStatus reads the Status column from `schtasks /Query /FO CSV /NH`
// ("TaskName","Next Run Time","Status"). The word is localized on
// non-English Windows.
func schtasksStatus(out string) string {
	fields := strings.Split(firstLine(out), `","`)
	if len(fields) < 3 {
		return firstLine(out)
	}
	return strings.Trim(fields[len(fields)-1], `"`)
}
