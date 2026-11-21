package cli

import (
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// cliHost checks `host ACTION [ID] [options] [-- ARGS…]` and hands it to the
// binary's host command.
func cliHost(p parsedArgs, stdout io.Writer) (int, error) {
	action, id := p.pos[0], ""
	if len(p.pos) > 1 {
		id = p.pos[1]
	}
	if action == "auto" {
		return cliHostAuto(p, stdout)
	}
	if len(p.pos) > 2 {
		return 0, usagef("host %s: unexpected argument %q", action, p.pos[2])
	}
	for _, o := range autoOnly {
		if _, ok := p.values[o]; ok {
			return 0, usagef("host %s: %s is for host auto", action, o)
		}
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

// autoOnly are the options of `host auto` alone.
var autoOnly = []string{"--cron", "--prompt", "--resume", "--notify", "--max-run", "--runs"}

// cliHostAuto checks `host auto ACTION [REF] [options]` and hands it to the
// binary's host auto command.
func cliHostAuto(p parsedArgs, stdout io.Writer) (int, error) {
	if len(p.pos) < 2 {
		return 0, usagef("host auto requires ACTION (add, list, show, run, pause, resume, rm)")
	}
	action, ref := p.pos[1], ""
	if len(p.pos) > 2 {
		ref = p.pos[2]
	}
	if len(p.extra) > 0 {
		return 0, usagef("host auto: arguments after -- are for host add")
	}
	for _, o := range []string{"--node", "--base", "--profile", "--dir", "--parent", "--restart", "--class", "--ask-permissions", "--skip-permissions"} {
		if _, ok := p.values[o]; ok {
			return 0, usagef("host auto: %s is for host add", o)
		}
	}
	allowed := map[string]bool{}
	switch action {
	case "add":
		if ref == "" {
			return 0, usagef("host auto add requires the hosted session ID")
		}
		if p.values["--cron"] == "" || p.values["--prompt"] == "" {
			return 0, usagef("host auto add requires --cron and --prompt")
		}
		for _, o := range []string{"--cron", "--prompt", "--label", "--resume", "--notify", "--max-run"} {
			allowed[o] = true
		}
	case "list":
		if ref != "" {
			return 0, usagef("host auto list: unexpected argument %q", ref)
		}
	case "show", "run", "pause", "resume", "rm":
		if ref == "" {
			return 0, usagef("host auto %s requires AUTO-ID", action)
		}
		allowed["--runs"] = action == "show"
	default:
		return 0, usagef("host auto: unknown action %q (add, list, show, run, pause, resume, rm)", action)
	}
	for _, o := range append([]string{"--label"}, autoOnly...) {
		if _, ok := p.values[o]; ok && !allowed[o] {
			return 0, usagef("host auto %s: %s is not one of its options", action, o)
		}
	}
	runs := 20
	if v, ok := p.values["--runs"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return 0, usagef("host auto show: --runs wants 1 to 500, not %q", v)
		}
		runs = n
	}
	c := wire.HostAutoCreate{Cron: p.values["--cron"], Prompt: p.values["--prompt"], Label: p.values["--label"], Resume: p.bool("--resume")}
	if v, ok := p.values["--max-run"]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 7*24*time.Hour {
			return 0, usagef("host auto add: --max-run wants a duration from 1m to 168h, not %q", v)
		}
		c.MaxRunMS = d.Milliseconds()
	}
	for _, r := range p.multi["--notify"] {
		for _, role := range strings.Split(r, ",") {
			if role = strings.TrimSpace(role); role != "" {
				c.Notify = append(c.Notify, role)
			}
		}
	}
	if hostAuto == nil {
		return 0, usagef("host auto: not available in this binary")
	}
	return hostAuto(action, ref, c, runs, p.bool("--json"), stdout)
}
