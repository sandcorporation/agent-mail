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
