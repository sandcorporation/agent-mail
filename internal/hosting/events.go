package hosting

import (
	"net/http"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Session history (#252): each hosted session's starts, restarts, stops,
// crashes, exits and adoptions, as its node reported them (wire.HostedEvent).

// EventKeep is how many events the host keeps per session.
const EventKeep = 200

// recordEvent stores e for its session unless it is a repeat or the
// session is gone, then drops the session's events beyond EventKeep.
func (s *Service) recordEvent(e wire.HostedEvent) error {
	var exit any
	if e.ExitCode != nil {
		exit = *e.ExitCode
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO hosted_events(session,at,kind,exit_code,reason,restarts,actor) SELECT ?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM hosted_sessions WHERE id=?)`,
		e.ID, e.At, e.Kind, exit, e.Reason, e.Restarts, e.Actor, e.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	_, err = s.db.Exec(`DELETE FROM hosted_events WHERE session=? AND id NOT IN (SELECT id FROM hosted_events WHERE session=? ORDER BY at DESC, id DESC LIMIT ?)`, e.ID, e.ID, EventKeep)
	return err
}

// Events is session's history, newest first.
func (s *Service) Events(session string) ([]wire.HostedEvent, error) {
	rows, err := s.db.Query(`SELECT at,kind,exit_code,reason,restarts,actor FROM hosted_events WHERE session=? ORDER BY at DESC, id DESC LIMIT ?`, session, EventKeep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.HostedEvent{}
	for rows.Next() {
		var e wire.HostedEvent
		var exit *int
		if err := rows.Scan(&e.At, &e.Kind, &exit, &e.Reason, &e.Restarts, &e.Actor); err != nil {
			return nil, err
		}
		e.ExitCode = exit
		out = append(out, e)
	}
	return out, rows.Err()
}

// eventsAPI serves GET /api/admin/terminals/{id}/events.
func (h *Hub) eventsAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.svc.Get(id); err != nil {
		writeResult(w, http.StatusOK, wire.HostedSession{}, err)
		return
	}
	out, err := h.svc.Events(id)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
