package hosting

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Terminal attach/input log (#173, #192): who attached to, detached from or
// typed into a terminal and when. Input is a byte count per socket per
// minute; its content is never stored.

// Log kinds.
const (
	LogAttach = "attach"
	LogDetach = "detach"
	LogInput  = "input"
)

// Log limits.
const (
	LogRetain = 30 * 24 * time.Hour // rows older than this are pruned
	LogWindow = time.Minute         // input bytes of one socket are summed over this
	logShown  = 100                 // rows the log route returns
)

// logTerminal appends one row; at is Unix ms.
func (s *Service) logTerminal(session, user, kind string, at int64, bytes int) error {
	_, err := s.db.Exec(`INSERT INTO terminal_log(session,admin_user,kind,at,bytes) VALUES(?,?,?,?,?)`, session, user, kind, at, bytes)
	return err
}

// TerminalLog is the newest rows of session's log, newest first.
func (s *Service) TerminalLog(session string) ([]wire.TerminalLogEntry, error) {
	rows, err := s.db.Query(`SELECT admin_user,kind,at,bytes FROM terminal_log WHERE session=? ORDER BY at DESC, id DESC LIMIT ?`, session, logShown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.TerminalLogEntry{}
	for rows.Next() {
		var e wire.TerminalLogEntry
		if err := rows.Scan(&e.AdminUser, &e.Kind, &e.At, &e.Bytes); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneLog deletes terminal_log rows older than LogRetain; the server's
// sweep calls it whether or not hosting is on.
func PruneLog(db *sql.DB, now time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM terminal_log WHERE at<?`, now.Add(-LogRetain).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// logAPI serves GET /api/admin/terminals/{id}/log.
func (h *Hub) logAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.svc.Get(id); err != nil {
		writeResult(w, http.StatusOK, wire.HostedSession{}, err)
		return
	}
	out, err := h.svc.TerminalLog(id)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// tally sums one socket's input bytes over a LogWindow.
type tally struct {
	start int64 // Unix ms of the window's first input
	bytes int
}

// add counts n bytes typed at now and returns a finished window to log
// (bytes 0: none).
func (t *tally) add(now int64, n int) (done tally) {
	if t.bytes > 0 && now-t.start >= LogWindow.Milliseconds() {
		done, *t = *t, tally{}
	}
	if t.bytes == 0 {
		t.start = now
	}
	t.bytes += n
	return done
}

// due takes the window when it is over at now, or at once when force.
func (t *tally) due(now int64, force bool) (done tally) {
	if t.bytes > 0 && (force || now-t.start >= LogWindow.Milliseconds()) {
		done, *t = *t, tally{}
	}
	return done
}
