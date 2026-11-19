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
	addOnly := []string{"--node", "--base", "--profile", "--dir", "--label", "--parent", "--restart", "--class", "--ask-permissions"}
	var skip *bool
	if p.bool("--ask-permissions") {
		skip = new(bool)
	}
	if v, ok := p.values["--skip-permissions"]; ok && action != "set" {
		return 0, usagef("host %s: --skip-permissions is for host set (host add skips by default; --ask-permissions turns it off)", action)
	} else if ok && v != "on" && v != "off" {
		return 0, usagef("host set: --skip-permissions wants on or off, not %q", v)
	} else if ok {
		on := v == "on"
		skip = &on
	}
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
	case "set":
		if id == "" {
			return 0, usagef("host set requires ID")
		}
		if skip == nil {
			return 0, usagef("host set requires --skip-permissions on|off")
		}
		for _, o := range addOnly {
			if _, ok := p.values[o]; ok {
				return 0, usagef("host set: %s is for host add", o)
			}
		}
		if len(p.extra) > 0 {
			return 0, usagef("host set: arguments after -- are for host add")
		}
	case "limit":
		if id != "" && id != "on" && id != "off" {
			return 0, usagef("host limit: want on, off or nothing, not %q", id)
		}
		for _, o := range addOnly {
			if _, ok := p.values[o]; ok {
				return 0, usagef("host limit: %s is for host add", o)
			}
		}
		if len(p.extra) > 0 {
			return 0, usagef("host limit: arguments after -- are for host add")
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
		return 0, usagef("host: unknown action %q (add, list, set, start, stop, restart, new-conversation, remove, limit)", action)
	}
	add := wire.HostedSessionCreate{
		Node: p.values["--node"], Base: p.values["--base"], Profile: p.values["--profile"], Directory: p.values["--dir"],
		Label: p.values["--label"], Parent: p.values["--parent"], Restart: p.values["--restart"], Class: p.values["--class"], Args: p.extra,
		SkipPermissions: skip,
	}
	return host(action, id, add, p.bool("--json"), stdout)
}
