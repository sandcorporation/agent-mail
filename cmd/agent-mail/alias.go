package main

// Switchboard is the product's name; agent-mail is the old one. Until the
// aliases are removed (#71 §5, phase P3), every environment variable, header
// and the profile directory answer to both names. The new name wins when both
// are set. Old-name use is counted so the removal can wait until it is zero.

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	envPrefix          = "SWITCHBOARD_"
	legacyEnvPrefix    = "AGENT_MAIL_"
	headerPrefix       = "X-Switchboard-"
	legacyHeaderPrefix = "X-Agent-Mail-"
	configDirName      = "switchboard"
	legacyConfigDir    = "agent-mail"
)

// oldEnvRead holds the old environment names read since start.
var oldEnvRead = struct {
	sync.Mutex
	names map[string]bool
}{names: map[string]bool{}}

// envAlias returns SWITCHBOARD_<name>, else AGENT_MAIL_<name>.
func envAlias(name string) string {
	v, _ := envAliasFrom(os.Getenv, name)
	return v
}

// envAliasFrom reads <name> through getenv under the new prefix, then the old
// one. from is the variable that held the value (the new name when neither
// is set), for messages. Reading an old name records it.
func envAliasFrom(getenv func(string) string, name string) (value, from string) {
	if v := getenv(envPrefix + name); v != "" {
		return v, envPrefix + name
	}
	if v := getenv(legacyEnvPrefix + name); v != "" {
		oldEnvRead.Lock()
		oldEnvRead.names[legacyEnvPrefix+name] = true
		oldEnvRead.Unlock()
		return v, legacyEnvPrefix + name
	}
	return "", envPrefix + name
}

// oldEnvNames lists the old environment names read since start, sorted.
func oldEnvNames() []string {
	oldEnvRead.Lock()
	defer oldEnvRead.Unlock()
	out := make([]string, 0, len(oldEnvRead.names))
	for n := range oldEnvRead.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// aliasCounter counts requests that carried an old header, by header name.
type aliasCounter struct {
	mu   sync.Mutex
	seen map[string]int64
}

func (c *aliasCounter) add(name string) {
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]int64{}
	}
	c.seen[name]++
	c.mu.Unlock()
}

func (c *aliasCounter) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.seen))
	for k, v := range c.seen {
		out[k] = v
	}
	return out
}

// header returns X-Switchboard-<name>, else X-Agent-Mail-<name>. Using the
// old header counts toward the alias metrics.
func (s *Server) header(r *http.Request, name string) string {
	if v := r.Header.Get(headerPrefix + name); v != "" {
		return v
	}
	v := r.Header.Get(legacyHeaderPrefix + name)
	if v != "" {
		s.oldHeaders.add(legacyHeaderPrefix + name)
	}
	return v
}

// aliasUse is the "aliases" block of the admin metrics.
type aliasUse struct {
	OldHeaders map[string]int64 `json:"old_headers"`
	OldEnv     []string         `json:"old_env"`
}

func (s *Server) aliasUse() aliasUse {
	return aliasUse{OldHeaders: s.oldHeaders.snapshot(), OldEnv: oldEnvNames()}
}

// legacySystemSender is the system sender's old name. It stays reserved, and
// mail sent under it still counts as system mail.
const legacySystemSender = "agent-mail/system"

// isSystemSender reports whether name is the system sender under either name.
func isSystemSender(name string) bool {
	return name == systemSender || name == legacySystemSender
}

// progName is the name the CLI was invoked as, for help and messages:
// agent-mail when run under the old name, otherwise switchboard.
func progName() string {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if base == legacyConfigDir {
		return legacyConfigDir
	}
	return "switchboard"
}
