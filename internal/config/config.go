// Package config reads the environment: both the new SWITCHBOARD_* and the
// old AGENT_MAIL_* names, and the header and profile-directory names.
package config

// Switchboard is the product's name; LegacyConfigDir holds the old one. Until the
// aliases are removed (#71 §5, phase P3), every environment variable, header
// and the profile directory answer to both names. The new name wins when both
// are set. Old-name use is counted so the removal can wait until it is zero.

import (
	"os"
	"sort"
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
