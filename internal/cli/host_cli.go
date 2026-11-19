package cli

import (
	"io"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// cliHost checks `host ACTION [ID] [options] [-- ARGS…]` and hands it to the
// binary's host command.
func cliHost(p parsedArgs, stdout io.Writer) (int, error) {
	action, id := p.pos[0], ""
	if len(p.pos) > 1 {
		id = p.pos[1]
	}
	addOnly := []string{"--node", "--base", "--profile", "--dir", "--label", "--parent", "--restart"}
	switch action {
	case "add":
		if id != "" {
			return 0, usagef("host add: unexpected argument %q", id)
		}
		for _, o := range []string{"--node", "--base", "--profile", "--dir"} {
			if p.values[o] == "" {
				return 0, usagef("host add requires --node, --base, --profile and --dir (missing %s)", o)
			}
		}
	case "list", "start", "stop", "restart", "new-conversation", "remove":
		if action == "list" && id != "" {
			return 0, usagef("host list: unexpected argument %q", id)
		}
		if action != "list" && id == "" {
			return 0, usagef("host %s requires ID", action)
		}
		for _, o := range addOnly {
			if _, ok := p.values[o]; ok {
				return 0, usagef("host %s: %s is for host add", action, o)
			}
		}
		if len(p.extra) > 0 {
			return 0, usagef("host %s: arguments after -- are for host add", action)
		}
	default:
		return 0, usagef("host: unknown action %q (add, list, start, stop, restart, new-conversation, remove)", action)
	}
	add := wire.HostedSessionCreate{
		Node: p.values["--node"], Base: p.values["--base"], Profile: p.values["--profile"], Directory: p.values["--dir"],
		Label: p.values["--label"], Parent: p.values["--parent"], Restart: p.values["--restart"], Args: p.extra,
	}
	return host(action, id, add, p.bool("--json"), stdout)
}
