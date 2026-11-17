// Package service installs a long-running program as a per-user OS service:
// a systemd user unit on Linux, a launchd agent on macOS and a Task
// Scheduler logon task on Windows. Nothing here needs root or admin rights.
package service

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Unit describes the program to run as a service.
type Unit struct {
	// Name identifies the service: the systemd unit name (without
	// ".service"), the launchd label and the scheduled task name. Letters,
	// digits, '.', '_' and '-' only.
	Name string
	// Exec is the absolute path of the program.
	Exec string
	// Args are passed to Exec after the program name.
	Args []string
	// Env is the program's extra environment, as KEY=VALUE pairs.
	Env []string
	// WorkDir is the working directory; empty leaves the OS default.
	WorkDir string
	// LogPath receives stdout and stderr (appended); empty discards them.
	LogPath string
	// User is the Windows account (DOMAIN\name) whose logon starts the task.
	// Install fills it with the current user when empty. Other OSes ignore it.
	User string
}

// File is one rendered file, to be written under the user's home directory.
type File struct {
	// Path is relative to the home directory and uses '/' separators.
	Path    string
	Content []byte
}

// Installed reports what Install did.
type Installed struct {
	// Files are the absolute paths written.
	Files []string
	// Notes are things the user should know but that Install never does
	// for them (such as enabling systemd linger).
	Notes []string
}

// State is the result of Status.
type State struct {
	// Installed is true when the service definition is present.
	Installed bool
	// Running is true when the OS reports the program as running.
	Running bool
	// Detail is the OS's own short status, such as "active" or "running".
	Detail string
}

var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidName reports whether name can be used as Unit.Name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

func validate(goos string, u Unit) error {
	if !ValidName(u.Name) {
		return fmt.Errorf("service: invalid name %q", u.Name)
	}
	if u.Exec == "" {
		return errors.New("service: Exec is required")
	}
	if !isAbs(goos, u.Exec) {
		return fmt.Errorf("service: Exec %q is not an absolute path", u.Exec)
	}
	for _, p := range []struct{ field, v string }{{"WorkDir", u.WorkDir}, {"LogPath", u.LogPath}} {
		if p.v != "" && !isAbs(goos, p.v) {
			return fmt.Errorf("service: %s %q is not an absolute path", p.field, p.v)
		}
	}
	all := append([]string{u.Exec, u.WorkDir, u.LogPath, u.User}, u.Args...)
	all = append(all, u.Env...)
	for _, v := range all {
		if strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("service: value %q contains a newline or NUL", v)
		}
		if goos == "windows" && strings.Contains(v, `"`) {
			return fmt.Errorf("service: value %q contains a double quote, which the Windows wrapper cannot carry", v)
		}
	}
	for _, kv := range u.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !envKeyRE.MatchString(k) {
			return fmt.Errorf("service: Env entry %q is not KEY=VALUE", kv)
		}
	}
	return nil
}

func isAbs(goos, p string) bool {
	if goos == "windows" {
		if strings.HasPrefix(p, `\\`) {
			return true
		}
		return len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
	}
	return strings.HasPrefix(p, "/")
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// Render returns the files that define u as a service on goos ("linux",
// "darwin" or "windows"). It is a pure function: it reads nothing from the
// machine it runs on.
func Render(goos string, u Unit) ([]File, error) {
	if err := validate(goos, u); err != nil {
		return nil, err
	}
	switch goos {
	case "linux":
		return []File{{Path: systemdPath(u.Name), Content: renderSystemd(u)}}, nil
	case "darwin":
		return []File{{Path: launchdPath(u.Name), Content: renderLaunchd(u)}}, nil
	case "windows":
		return []File{
			{Path: windowsScriptPath(u.Name), Content: renderWindowsScript(u)},
			{Path: windowsTaskPath(u.Name), Content: renderWindowsTask(u)},
		}, nil
	}
	return nil, fmt.Errorf("service: unsupported OS %q", goos)
}
