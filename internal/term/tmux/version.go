// Package tmux is the Linux term.Backend: every session is a tmux session on
// a tmux server of its own (-L switchboard-<instance>), so sessions outlive
// the node and the operator's own tmux is never touched.
//
// tmux 3.2 or newer is required (new-session -e, control-mode
// subscriptions); Check refuses anything older with install commands.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Version is a tmux release as reported by tmux -V.
type Version struct {
	Major, Minor int
	// Dev marks a development build ("tmux master"), taken as new enough.
	Dev bool
	// Raw is the version text without the "tmux " prefix.
	Raw string
}

// MinVersion is the oldest tmux the backend runs on.
var MinVersion = Version{Major: 3, Minor: 2, Raw: "3.2"}

func (v Version) String() string { return v.Raw }

// AtLeast reports whether v is min or newer.
func (v Version) AtLeast(min Version) bool {
	if v.Dev {
		return true
	}
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	return v.Minor >= min.Minor
}

var versionRE = regexp.MustCompile(`^(?:[a-z]+-)?(\d+)\.(\d+)`)

// ParseVersion reads the output of tmux -V: "tmux 3.3a", "tmux next-3.4",
// "tmux 3.2-rc2", "tmux master".
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "tmux"))
	if raw == "master" {
		return Version{Dev: true, Raw: raw}, nil
	}
	m := versionRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("tmux: unrecognised version %q", strings.TrimSpace(s))
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return Version{}, fmt.Errorf("tmux: unrecognised version %q", strings.TrimSpace(s))
	}
	return Version{Major: major, Minor: minor, Raw: raw}, nil
}

// ErrUnavailable is returned by Check when tmux is missing or too old.
var ErrUnavailable = errors.New("tmux 3.2 or newer is required for terminal hosting on Linux")

// InstallHint lists the install commands Check puts in its errors.
const InstallHint = "install it with: sudo apt-get install tmux (Debian, Ubuntu)" +
	" | sudo dnf install tmux (Fedora, RHEL)" +
	" | sudo pacman -S tmux (Arch)" +
	" | sudo zypper install tmux (openSUSE)" +
	" | sudo apk add tmux (Alpine)"

// Check finds the tmux binary (a name looked up in PATH, or a path) and
// refuses one older than MinVersion. Errors wrap ErrUnavailable and carry
// InstallHint.
func Check(ctx context.Context, binary string) (Version, error) {
	if binary == "" {
		binary = "tmux"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return Version{}, fmt.Errorf("%w: %s not found; %s", ErrUnavailable, binary, InstallHint)
	}
	out, err := exec.CommandContext(ctx, path, "-V").Output()
	if err != nil {
		return Version{}, fmt.Errorf("%w: %s -V failed (%v); %s", ErrUnavailable, path, err, InstallHint)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		return Version{}, fmt.Errorf("%w: %v; %s", ErrUnavailable, err, InstallHint)
	}
	if !v.AtLeast(MinVersion) {
		return v, fmt.Errorf("%w: %s is tmux %s; %s", ErrUnavailable, path, v, InstallHint)
	}
	return v, nil
}
