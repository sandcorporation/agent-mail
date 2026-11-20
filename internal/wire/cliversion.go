package wire

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CLI versions (#308). Every CLI request carries its build version in the
// X-Switchboard-Version header; a session's heartbeat records it on the slot.
// Versions compare by major.minor only. A version that is not a release tag
// (vX.Y.Z) — a development build ("dev"), a git describe string — is "판 모름"
// like a request without the header, and never counts as old.

// CLIUnknown is the version recorded when a slot is claimed without the
// header; a later heartbeat without it keeps the recorded version (#319).
const CLIUnknown = "unknown"

// CLI states of a recorded version against the server's minimum.
const (
	CLICurrent = "current" // at or above the minimum
	CLIOld     = "old"     // below the minimum: 옛 판
	CLINoInfo  = "unknown" // no header, a development build, or no minimum
)

// MinorVersion is a major.minor version.
type MinorVersion struct{ Major, Minor int }

func (v MinorVersion) String() string { return fmt.Sprintf("v%d.%d", v.Major, v.Minor) }

// Less reports whether v is below o.
func (v MinorVersion) Less(o MinorVersion) bool {
	return v.Major < o.Major || v.Major == o.Major && v.Minor < o.Minor
}

var (
	releaseRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	// serverRE is a server's own build: a release tag or git describe output
	// (vX.Y.Z-N-gSHA).
	serverRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(-\d+-g[0-9a-f]+)?$`)
	minRE    = regexp.MustCompile(`^v?(\d+)\.(\d+)(\.\d+)?$`)
)

// ReleaseMinor is the major.minor of a release tag vX.Y.Z; false for anything
// else (dev, unknown, git describe output).
func ReleaseMinor(version string) (MinorVersion, bool) {
	m := releaseRE.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return MinorVersion{}, false
	}
	return minorOf(m[1], m[2])
}

// ParseMinVersion reads a minimum: vX.Y, X.Y or vX.Y.Z (the patch is ignored).
func ParseMinVersion(s string) (MinorVersion, bool) {
	m := minRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return MinorVersion{}, false
	}
	return minorOf(m[1], m[2])
}

func minorOf(a, b string) (MinorVersion, bool) {
	x, e1 := strconv.Atoi(a)
	y, e2 := strconv.Atoi(b)
	return MinorVersion{x, y}, e1 == nil && e2 == nil
}

// CLIState is the state of a recorded client version against min (ok false:
// the server has no minimum, as a development build).
func CLIState(version string, min MinorVersion, ok bool) string {
	v, known := ReleaseMinor(version)
	switch {
	case !ok || !known:
		return CLINoInfo
	case v.Less(min):
		return CLIOld
	}
	return CLICurrent
}

// CLIMinimum is the server's minimum client version: a valid
// SWITCHBOARD_CLI_MIN, else the server's own major.minor. ok is false when
// neither is known (a development server). bad is the setting when it was
// set but invalid, for one log line.
func CLIMinimum(setting, serverVersion string) (min MinorVersion, ok bool, bad string) {
	if s := strings.TrimSpace(setting); s != "" {
		if m, ok := ParseMinVersion(s); ok {
			return m, true, ""
		}
		bad = s
	}
	if m := serverRE.FindStringSubmatch(strings.TrimSpace(serverVersion)); m != nil {
		min, ok = minorOf(m[1], m[2])
	}
	return min, ok, bad
}

// CLIBehind is one machine whose session CLI is below the minimum (#308): the
// 진행 대기 row and the daily mail. Version is the lowest version among Roles.
// Unknown versions are not listed (#319).
type CLIBehind struct {
	Machine string   `json:"machine"`
	Version string   `json:"version"`
	Roles   []string `json:"roles"`
	// RoleVersions is each role's version, in Roles order.
	RoleVersions []string `json:"role_versions"`
}

// ServerVersionInfo is GET /health with the client minimum (#308): version
// --check compares against it.
type ServerVersionInfo struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	// CLIMin is the minimum client major.minor ("v0.26"); empty when the
	// server has none (a development build).
	CLIMin string `json:"cli_min,omitempty"`
}

// ReleaseAsset is the release archive name for a version and GOOS/GOARCH
// (.github/workflows/release.yml).
func ReleaseAsset(version, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "switchboard_" + version + "_" + goos + "_" + goarch + ext
}
