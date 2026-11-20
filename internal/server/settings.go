package server

import (
	"context"
	"log"
	"net/http"

	"github.com/sandcorporation/switchboard/internal/presence"
)

// adminSettings is GET /api/admin/settings (#223): the server version and
// the settings an operator needs to confirm after a deploy, read-only. It
// holds no secret (no token, webhook address or client secret).
type adminSettings struct {
	Version   string                `json:"version"`
	Hosting   bool                  `json:"hosting"`
	Reminders presence.RemindStatus `json:"reminders"`
	Rule      presence.RuleStatus   `json:"answered_rule"`
}

func (s *Server) settingsAPI(w http.ResponseWriter, r *http.Request) {
	rs, err := s.presence().RemindStatus(r.Context())
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	rule, err := s.presence().RuleStatus()
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, adminSettings{Version: version, Hosting: s.hosting, Reminders: rs, Rule: rule})
}

// logRemindStart logs the one start-up line for directive reminders, next
// to the "admin auth: …" line: "directive reminders: on (after 2h, max 3,
// since first sweep)" or "directive reminders: off".
func (s *Server) logRemindStart() error {
	rs, err := s.presence().RemindStatus(context.Background())
	if err != nil {
		return err
	}
	log.Print(rs.StartLine())
	return nil
}
