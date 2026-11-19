package node

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
)

// Defaults for the node-side hosting variables.
const (
	DefaultAgent   = "claude"
	DefaultHistory = 10000
)

// Config is the node's launch settings.
type Config struct {
	// Agent is the resolved path of the agent CLI.
	Agent string
	// Machine is SWITCHBOARD_MACHINE for every child, the same on every
	// launch so a session's slot comes back.
	Machine string
	// Endpoint is the host URL as the node reaches it.
	Endpoint string
	// History is the scrollback lines kept per session.
	History int
	// Max caps the sessions running at once (0: config.DefaultHostingMax);
	// records beyond it wait, oldest-created first (#190).
	Max int
	// NodeToken is the node's credential; no child sees it.
	NodeToken string
	// Environ is the node's environment ("KEY=value"), the base of every
	// child's.
	Environ []string
	// Term is the TERM a child gets; empty keeps what the backend sets
	// (tmux sets its own).
	Term string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Host is what Resolve needs from the machine; Machine fills it from the OS.
type Host struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
	Hostname func() (string, error)
	Environ  func() []string
}

// Machine is the real Host.
func Machine() Host {
	return Host{Getenv: os.Getenv, LookPath: exec.LookPath, Hostname: os.Hostname, Environ: os.Environ}
}

// ErrNoAgent is returned when the agent CLI cannot be resolved; the node
// refuses to start.
var ErrNoAgent = errors.New("node: agent CLI not found")

// Resolve reads the node-side hosting variables (new names only) and
// resolves the agent CLI. It fails when the agent cannot be found.
func Resolve(h Host) (Config, error) {
	agent, from := config.EnvFrom(h.Getenv, config.HostingAgentName)
	if agent = strings.TrimSpace(agent); agent == "" {
		agent = DefaultAgent
	}
	path, err := h.LookPath(agent)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %s=%q: %v", ErrNoAgent, from, agent, err)
	}
	machine, _ := config.EnvFrom(h.Getenv, config.HostingMachineName)
	if machine = strings.TrimSpace(machine); machine == "" {
		if machine, err = h.Hostname(); err != nil || machine == "" {
			return Config{}, fmt.Errorf("node: machine name: set %s%s (hostname: %v)", config.EnvPrefix, config.HostingMachineName, err)
		}
	}
	history := DefaultHistory
	if v, from := config.EnvFrom(h.Getenv, config.HostingHistoryName); strings.TrimSpace(v) != "" {
		if history, err = strconv.Atoi(strings.TrimSpace(v)); err != nil || history < 0 {
			return Config{}, fmt.Errorf("node: %s=%q is not a line count", from, v)
		}
	}
	max := config.DefaultHostingMax
	if v, from := config.EnvFrom(h.Getenv, config.HostingMaxName); strings.TrimSpace(v) != "" {
		if max, err = strconv.Atoi(strings.TrimSpace(v)); err != nil || max < 1 {
			return Config{}, fmt.Errorf("node: %s=%q is not a session count (1 or more)", from, v)
		}
	}
	return Config{Agent: path, Machine: machine, History: history, Max: max, Environ: h.Environ()}, nil
}

// Argv is the command for a record: --session-id on its first launch,
// --resume after it, then the record's args. Never -p, never --continue.
func (c Config) Argv(r Record, launched bool) []string {
	flag := "--session-id"
	if launched {
		flag = "--resume"
	}
	argv := []string{c.Agent, flag, r.AgentSessionID}
	for _, a := range r.Args {
		if refusedArg(a) {
			continue
		}
		argv = append(argv, a)
	}
	return argv
}

// refusedArg drops flags that would break the launch contract; the host
// refuses them already, the node does not trust that.
func refusedArg(a string) bool {
	name, _, _ := strings.Cut(a, "=")
	switch name {
	case "-p", "--print", "-c", "--continue", "--resume", "-r", "--session-id":
		return true
	}
	return false
}

// SecretName reports whether an environment name looks like a credential.
func SecretName(name string) bool {
	u := strings.ToUpper(name)
	for _, s := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD"} {
		if strings.Contains(u, s) {
			return true
		}
	}
	return false
}

// ChildEnv is a record's environment: the node's, minus the role token and
// the node token, with the identity variables set from the record, the
// colour variables, then the record's non-secret env.
func (c Config) ChildEnv(r Record) []string {
	identity := []struct{ name, value string }{
		{config.ChildProfileName, r.Profile},
		{config.ChildEndpointName, c.Endpoint},
		{config.ChildMachineName, c.Machine},
		{config.ChildLabelName, r.Label},
		{config.ChildParentName, r.Parent},
	}
	drop := map[string]bool{
		config.EnvPrefix + config.TokenName: true,
		config.LegacyEnv(config.TokenName):  true,
		"COLORTERM":                         true,
	}
	if c.Term != "" {
		drop["TERM"] = true
	}
	for _, v := range identity {
		drop[config.EnvPrefix+v.name] = true
		drop[config.LegacyEnv(v.name)] = true
	}
	extra := map[string]string{}
	for k, v := range r.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || SecretName(k) {
			continue
		}
		extra[k] = v
		drop[k] = true
	}
	var env []string
	for _, kv := range c.Environ {
		k, v, _ := strings.Cut(kv, "=")
		if drop[k] || (c.NodeToken != "" && v == c.NodeToken) {
			continue
		}
		env = append(env, kv)
	}
	for _, v := range identity {
		if v.value != "" {
			env = append(env, config.EnvPrefix+v.name+"="+v.value)
		}
	}
	env = append(env, "COLORTERM=truecolor")
	if c.Term != "" {
		env = append(env, "TERM="+c.Term)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c.NodeToken != "" && extra[k] == c.NodeToken {
			continue
		}
		env = append(env, k+"="+extra[k])
	}
	return env
}
