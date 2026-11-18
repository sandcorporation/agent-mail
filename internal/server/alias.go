package server

import (
	"net/http"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// header returns X-Switchboard-<name>, else the old-prefix header. Using the
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

func (s *Server) aliasUse() wire.AliasUse {
	return wire.AliasUse{OldHeaders: s.oldHeaders.Snapshot(), OldEnv: config.OldEnvNames()}
}
