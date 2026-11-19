package cli

import (
	"strconv"
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

// cliNode checks `node --host URL --token-file PATH` and runs the binary's
// node command.
func cliNode(p parsedArgs) (int, error) {
	hostURL, tokenFile := p.values["--host"], p.values["--token-file"]
	if hostURL == "" || tokenFile == "" {
		return 0, usagef("node requires --host and --token-file")
	}
	if binary.Node == nil {
		return 0, usagef("node is not available in this build")
	}
	if err := binary.Node(hostURL, tokenFile); err != nil {
		return 0, err
	}
	return ExitOK, nil
}
