package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// ExitBehind: version --check found this CLI below the server's minimum
// (#308).
const ExitBehind = 3

// versionCheckTimeout bounds the /health request of version --check.
const versionCheckTimeout = 10 * time.Second

// setVersionHeader sends this build's version (#308) so the server can tell
// an old CLI.
func setVersionHeader(req *http.Request) {
	req.Header.Set(config.HeaderPrefix+config.VersionHeaderName, version)
}

// versionCheck asks c's server for its version and minimum client version and
// prints "server vY, this vX", plus "update: <asset>" when this CLI is behind
// (exit ExitBehind). A server it cannot reach is an error (exit 1).
// A development build (not a release tag) is never behind.
func versionCheck(c Config, this string, out io.Writer) (int, error) {
	info, err := serverVersion(c)
	if err != nil {
		return 0, fmt.Errorf("version --check: cannot reach the server: %v", err)
	}
	fmt.Fprintf(out, "server %s, this %s\n", info.Version, this)
	min, ok := wire.ParseMinVersion(info.CLIMin)
	if !ok {
		// A server before #308 reports no minimum: its own major.minor.
		min, ok = wire.ReleaseMinor(info.Version)
	}
	if wire.CLIState(this, min, ok) != wire.CLIOld {
		return ExitOK, nil
	}
	fmt.Fprintf(out, "update: %s\n", wire.ReleaseAsset(info.Version, runtime.GOOS, runtime.GOARCH))
	return ExitBehind, nil
}

func serverVersion(c Config) (wire.ServerVersionInfo, error) {
	var info wire.ServerVersionInfo
	req, err := http.NewRequest("GET", strings.TrimRight(c.Endpoint, "/")+"/health", nil)
	if err != nil {
		return info, err
	}
	setVersionHeader(req)
	resp, err := (&http.Client{Timeout: versionCheckTimeout}).Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("server returned %s", resp.Status)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info); err != nil {
		return info, err
	}
	if info.Version == "" {
		return info, fmt.Errorf("server reported no version")
	}
	return info, nil
}
