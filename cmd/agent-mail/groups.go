package main

import (
	"slices"

	"github.com/sandcorporation/agent-mail/internal/identity"
)

// visibleSessions keeps the presence rows p may see.
func (s *Server) visibleSessions(p identity.Principal, in []presenceView) []presenceView {
	if s.ids().Unrestricted(p) {
		return in
	}
	return slices.DeleteFunc(in, func(v presenceView) bool { return !s.ids().MaySee(p, v.Base) })
}
