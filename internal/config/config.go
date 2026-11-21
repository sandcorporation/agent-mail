// Package config reads the environment: both the new SWITCHBOARD_* and the
// old AGENT_MAIL_* names, and the header and profile-directory names.
package config

// Switchboard is the product's name; LegacyConfigDir holds the old one. Until the
// aliases are removed (#71 §5, phase P3), every environment variable, header
// and the profile directory answer to both names. The new name wins when both
// are set. Old-name use is counted so the removal can wait until it is zero.

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	EnvPrefix          = "SWITCHBOARD_"
	legacyEnvPrefix    = "AGENT_MAIL_"
	HeaderPrefix       = "X-Switchboard-"
	LegacyHeaderPrefix = "X-Agent-Mail-" // remove in phase 3 (3단계에 제거): the admin proxy still sends it
	ConfigDirName      = "switchboard"
	LegacyConfigDir    = "agent-mail" // remove in phase 3 (3단계에 제거): old binary and profile directory name
)

// VersionHeaderName is HeaderPrefix + this: the CLI build version every
// client request carries (#308). It has no old-prefix alias; a request
// without it is an older CLI.
const VersionHeaderName = "Version"

// NodeCredentialsHeaderName is HeaderPrefix + this on GET /api/node/socket
// (#343): a node that takes role credentials sends it with the credential
// ids it holds, comma separated, or "-" for none. Without it the host sends
// no credentials and the node's sessions use the profiles their records name.
const NodeCredentialsHeaderName = "Node-Credentials"

// CLIMinName is SWITCHBOARD_CLI_MIN without the prefix: the minimum client
// major.minor (#308), read with EnvFrom. Unset or invalid: the server's own.
const CLIMinName = "CLI_MIN"

// ProductName is the only place the product name lives (dashboard, login
// pages, TOTP issuer), so a rename is a one-line change.
const ProductName = "Switchboard"

// oldEnvRead holds the old environment names read since start.
var oldEnvRead = struct {
	sync.Mutex
	names map[string]bool
}{names: map[string]bool{}}

// EnvAlias returns SWITCHBOARD_<name>, else AGENT_MAIL_<name>.
func EnvAlias(name string) string {
	v, _ := EnvAliasFrom(os.Getenv, name)
	return v
}

// EnvAliasFrom reads <name> through getenv under the new prefix, then the old
// one. from is the variable that held the value (the new name when neither
// is set), for messages. Reading an old name records it.
func EnvAliasFrom(getenv func(string) string, name string) (value, from string) {
	if v := getenv(EnvPrefix + name); v != "" {
		return v, EnvPrefix + name
	}
	if v := getenv(legacyEnvPrefix + name); v != "" {
		oldEnvRead.Lock()
		oldEnvRead.names[legacyEnvPrefix+name] = true
		oldEnvRead.Unlock()
		return v, legacyEnvPrefix + name
	}
	return "", EnvPrefix + name
}

// OldEnvNames lists the old environment names read since start, sorted.
func OldEnvNames() []string {
	oldEnvRead.Lock()
	defer oldEnvRead.Unlock()
	out := make([]string, 0, len(oldEnvRead.names))
	for n := range oldEnvRead.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AliasCounter counts requests that carried an old header, by header name.
type AliasCounter struct {
	mu   sync.Mutex
	seen map[string]int64
}

func (c *AliasCounter) Add(name string) {
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]int64{}
	}
	c.seen[name]++
	c.mu.Unlock()
}

func (c *AliasCounter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.seen))
	for k, v := range c.seen {
		out[k] = v
	}
	return out
}

// Env returns the environment variable k, else d.
func Env(k, d string) string {
	if x := os.Getenv(k); x != "" {
		return x
	}
	return d
}

// HostingName is SWITCHBOARD_HOSTING without the prefix: terminal hosting
// (#173). The operations instance never sets it; a test keeps it out of the
// deployment files.
const HostingName = "HOSTING"

// HostingFrom reports whether terminal hosting is on: only the value "on"
// (under either prefix, new name first) enables it. from names the variable
// that held the value, for messages.
func HostingFrom(getenv func(string) string) (on bool, from string) {
	v, from := EnvAliasFrom(getenv, HostingName)
	return strings.TrimSpace(v) == "on", from
}

// EnvFrom reads SWITCHBOARD_<name> through getenv, with no old-name alias:
// variables added after the rename (hosting, #173) answer only to the new
// name. from is the full variable name, for messages.
func EnvFrom(getenv func(string) string, name string) (value, from string) {
	return getenv(EnvPrefix + name), EnvPrefix + name
}

// HostingAllowChecksName is SWITCHBOARD_HOSTING_ALLOW_CHECKS: "on" lets a
// node run scheduled runs' prechecks (#334), like `node --allow-checks`;
// `up`'s own node reads it. Off by default.
const HostingAllowChecksName = "HOSTING_ALLOW_CHECKS"

// HostingAllowChecks reads SWITCHBOARD_HOSTING_ALLOW_CHECKS through getenv.
func HostingAllowChecks(getenv func(string) string) bool {
	v, _ := EnvFrom(getenv, HostingAllowChecksName)
	return strings.TrimSpace(v) == "on"
}

// DefaultHostingMax is the running-session cap per node when
// SWITCHBOARD_HOSTING_MAX is unset (#190).
const DefaultHostingMax = 4

// Remote nodes (#329): a hosting host may open a second listener on a
// private-network address for nodes on other machines, and both sides accept
// only addresses inside the allowed ranges.
const (
	// HostingRemoteListenName is the host's second listener (IP:port, unset:
	// none). It serves the node channel and the role API only.
	HostingRemoteListenName = "HOSTING_REMOTE_LISTEN"
	// HostingRemoteAllowName lists the CIDR ranges (comma-separated) the
	// remote listener's address and peers, and a node's --host, must be in.
	HostingRemoteAllowName = "HOSTING_REMOTE_ALLOW"
	// DefaultHostingRemoteAllow is the Tailscale address ranges.
	DefaultHostingRemoteAllow = "100.64.0.0/10,fd7a:115c:a1e9::/48" // leak-guard:allow
)

// HostingRemoteAllow reads SWITCHBOARD_HOSTING_REMOTE_ALLOW through getenv
// (DefaultHostingRemoteAllow when unset). from names the variable, for
// messages.
func HostingRemoteAllow(getenv func(string) string) (allow []netip.Prefix, from string, err error) {
	v, from := EnvFrom(getenv, HostingRemoteAllowName)
	if strings.TrimSpace(v) == "" {
		v = DefaultHostingRemoteAllow
	}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, perr := netip.ParsePrefix(part)
		if perr != nil {
			return nil, from, fmt.Errorf("%s: %q is not a CIDR range such as 100.64.0.0/10", from, part) // leak-guard:allow
		}
		allow = append(allow, p.Masked())
	}
	if len(allow) == 0 {
		return nil, from, fmt.Errorf("%s names no CIDR range", from)
	}
	return allow, from, nil
}

// AddrAllowed reports whether a is inside one of allow (an IPv4-mapped IPv6
// address counts as its IPv4 address; a zone is ignored).
func AddrAllowed(a netip.Addr, allow []netip.Prefix) bool {
	a = a.Unmap().WithZone("")
	for _, p := range allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Node-side hosting variables (#173), read with EnvFrom.
const (
	// HostingAgentName names the agent CLI the node launches (default
	// "claude"), resolved through PATH at node start.
	HostingAgentName = "HOSTING_AGENT"
	// HostingMachineName is the machine name given to hosted sessions
	// (default os.Hostname()).
	HostingMachineName = "HOSTING_MACHINE"
	// HostingHistoryName is the scrollback lines kept per session.
	HostingHistoryName = "HOSTING_HISTORY"
	// HostingMaxName caps the sessions a node runs at once (#190, default
	// DefaultHostingMax); the node tells the host in its hello.
	HostingMaxName = "HOSTING_MAX"
	// FakeAgentName set to "1" turns a test binary into the fake agent.
	FakeAgentName = "FAKE_AGENT"
	// FakeAgentDirName is where the fake agent keeps its transcripts.
	FakeAgentDirName = "FAKE_AGENT_DIR"
	// FakeAgentScenesName is the directory of *.vt scenes "draw" paints.
	FakeAgentScenesName = "FAKE_AGENT_SCENES"
)

// Variables a hosted session's child gets from the node (#173). Each is
// EnvPrefix + the name.
const (
	ChildProfileName  = "PROFILE"
	ChildEndpointName = "ENDPOINT"
	ChildMachineName  = "MACHINE"
	ChildLabelName    = "LABEL"
	ChildParentName   = "PARENT"
	TokenName         = "TOKEN"
)

// IsOwnEnv reports whether the environment variable name is one this package
// reads: anything under the new or the old prefix. Windows names are
// case-insensitive, so the match ignores case. Tests use it to keep a
// machine's own setup out of the processes they start (#246).
func IsOwnEnv(name string) bool {
	n := strings.ToUpper(name)
	return strings.HasPrefix(n, EnvPrefix) || strings.HasPrefix(n, legacyEnvPrefix)
}

// WithoutOwnEnv returns environ (KEY=value entries) without the variables
// IsOwnEnv matches.
func WithoutOwnEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if name, _, _ := strings.Cut(kv, "="); !IsOwnEnv(name) {
			out = append(out, kv)
		}
	}
	return out
}

// LegacyEnv returns the old-prefix spelling of name, so callers that must
// strip a variable under both names (a credential kept out of a child) can
// do so without hard-coding the prefix.
func LegacyEnv(name string) string { return legacyEnvPrefix + name }
