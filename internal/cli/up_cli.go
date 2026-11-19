package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sandcorporation/switchboard/internal/config"
)

// cliUp checks `up --data DIR --port PORT [--node NAME]` and runs the
// binary's up command.
func cliUp(p parsedArgs) (int, error) {
	dir, port := p.values["--data"], p.values["--port"]
	if dir == "" || port == "" {
		return 0, usagef("up requires --data and --port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, usagef("up: invalid port %q", port)
	}
	name := p.values["--node"]
	if name == "" {
		name = "local"
	}
	if binary.Up == nil {
		return 0, usagef("up is not available in this build")
	}
	if err := binary.Up(dir, n, name); err != nil {
		return 0, err
	}
	return ExitOK, nil
}

// cliNode checks `node --host URL [--join CODE] [--token-file PATH]
// [--restart-sessions]` and
// `node invite NAME`, and runs the binary's command.
func cliNode(p parsedArgs, stdout io.Writer) (int, error) {
	if len(p.pos) > 0 {
		if p.pos[0] != "invite" || len(p.pos) != 2 {
			return 0, usagef("node: unknown subcommand %q (only 'invite NAME')", strings.Join(p.pos, " "))
		}
		if p.values["--host"] != "" || p.values["--join"] != "" || p.values["--token-file"] != "" || p.bool("--restart-sessions") {
			return 0, usagef("node invite takes only NAME")
		}
		if binary.NodeInvite == nil {
			return 0, usagef("node invite is not available in this build")
		}
		return binary.NodeInvite(p.pos[1], stdout)
	}
	hostURL, tokenFile := p.values["--host"], p.values["--token-file"]
	if hostURL == "" {
		return 0, usagef("node requires --host")
	}
	if tokenFile == "" {
		tokenFile = nodeTokenPath()
	}
	if binary.Node == nil {
		return 0, usagef("node is not available in this build")
	}
	if err := binary.Node(hostURL, tokenFile, p.values["--join"], p.bool("--restart-sessions")); err != nil {
		return 0, err
	}
	return ExitOK, nil
}

// nodeTokenPath is the node's default token file: node.token in the client's
// config directory (SWITCHBOARD_CONFIG_DIR, else <user config>/switchboard).
func nodeTokenPath() string {
	if d := config.EnvAlias("CONFIG_DIR"); d != "" {
		return filepath.Join(d, "node.token")
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, config.ConfigDirName, "node.token")
}
