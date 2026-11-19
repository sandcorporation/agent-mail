package mail

import (
	"net/http"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Close serves POST /api/messages/{id}/close (role is the caller, who must
// be the sender) and POST /api/admin/messages/{id}/close (role is empty: an
// admin may close any directive). Closing marks a directive as needing no
// answer: it stores no_reply=1, the flag `send --no-reply` sets, so the
// progress view, the directive reminders and the sweep that sends them
// (all through presence openDirectives) drop it on their next read.
// Closing again answers the same. 404 for no such message, 403 for a
// caller that is not the sender, 400 for mail that is not a directive (a
// plain reply, or system mail).
func (s *Service) Close(w http.ResponseWriter, role string, id int64) {
	m, err := s.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if role != "" && m.Sender != role {
		http.Error(w, "only the sender may close a directive", http.StatusForbidden)
		return
	}
	if identity.IsSystemSender(m.Sender) || !(FlowMail{ReplyTo: m.ReplyTo, Directive: m.Directive}).IsDirective() {
		http.Error(w, "not a directive", http.StatusBadRequest)
		return
	}
	if _, err = s.db.Exec(`UPDATE messages SET no_reply=1 WHERE id=? AND no_reply=0`, id); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, wire.Closed{ID: id, Closed: true})
}
