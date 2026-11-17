package main

import (
	"net/http"

	"github.com/sandcorporation/agent-mail/internal/config"
)

// header returns X-Switchboard-<name>, else X-Agent-Mail-<name>. Using the
// old header counts toward the alias metrics.
func (s *Server) header(r *http.Request, name string) string {
	if v := r.Header.Get(config.HeaderPrefix + name); v != "" {
		return v
	}
	v := r.Header.Get(config.LegacyHeaderPrefix + name)
	if v != "" {
		s.oldHeaders.Add(config.LegacyHeaderPrefix + name)
	}
	return v
}

func (s *Server) aliasUse() aliasUse {
	return aliasUse{OldHeaders: s.oldHeaders.Snapshot(), OldEnv: config.OldEnvNames()}
}

// legacySystemSender is the system sender's old name. It stays reserved, and
// mail sent under it still counts as system mail.
const legacySystemSender = "agent-mail/system"

// isSystemSender reports whether name is the system sender under either name.
func isSystemSender(name string) bool {
	return name == systemSender || name == legacySystemSender
}
