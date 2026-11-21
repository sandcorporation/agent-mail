package mail

import (
	"database/sql"
	"net/http"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/store"
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
//
// Given the ID of an answer the caller received to a directive it sent,
// Close closes that directive and marks the answer complete for the caller
// in one transaction (#327).
func (s *Service) Close(w http.ResponseWriter, r *http.Request, role string, id int64) {
	m, err := s.Get(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if d, ok := s.answeredDirective(role, m); ok {
		err = store.InTx(r.Context(), s.db, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE messages SET no_reply=1 WHERE id=? AND no_reply=0`, d); err != nil {
				return err
			}
			return markComplete(tx, role, []int64{id}, s.now().UTC())
		})
		if err != nil {
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, wire.Closed{ID: d, Closed: true, Completed: id})
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

// answeredDirective is the directive m answers when role received m and
// sent that directive: m replies to it and is not a directive itself, and
// neither is system mail.
func (s *Service) answeredDirective(role string, m wire.Message) (int64, bool) {
	if role == "" || m.Recipient != role || m.ReplyTo == 0 || identity.IsSystemSender(m.Sender) || (FlowMail{ReplyTo: m.ReplyTo, Directive: m.Directive}).IsDirective() {
		return 0, false
	}
	d, err := s.Get(m.ReplyTo)
	if err != nil || d.Sender != role || identity.IsSystemSender(d.Sender) || !(FlowMail{ReplyTo: d.ReplyTo, Directive: d.Directive}).IsDirective() {
		return 0, false
	}
	return d.ID, true
}
