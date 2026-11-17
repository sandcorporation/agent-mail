package main

import (
	"database/sql"
	"errors"
	"time"
)

// Presence history.
//
// slots keeps only a session's current state, so every change is also written
// to presence_log: one row per (slot, state) stretch, with ended_at NULL while
// it is the slot's current state. A report (register, heartbeat, takeover)
// whose effective state differs from the open row closes that row and opens a
// new one; going offline (the sweep seeing a lapsed lease, a release, or the
// slot row being dropped) does the same with the state "offline". A session
// online without a reported state is logged as "unknown". Closed rows older
// than presenceLogRetain are pruned by the sweep.
//
// The metrics (metrics.go) read error:* and blocked:* time from this table.

const (
	presenceLogRetain = 60 * 24 * time.Hour
	presenceOffline   = "offline"
	presenceUnknown   = "unknown"
)

func migratePresenceLog(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS presence_log(id INTEGER PRIMARY KEY AUTOINCREMENT,base TEXT NOT NULL,number INTEGER NOT NULL,role TEXT NOT NULL,state TEXT NOT NULL,started_at INTEGER NOT NULL,ended_at INTEGER);
CREATE INDEX IF NOT EXISTS idx_presence_log_slot ON presence_log(base,number,ended_at);
CREATE INDEX IF NOT EXISTS idx_presence_log_started ON presence_log(started_at);`)
	return err
}

// openPresenceState is the state of (base, number)'s open presence_log row;
// ok is false when there is none.
func openPresenceState(q queryer, base string, number int) (state string, ok bool, err error) {
	err = q.QueryRow(`SELECT state FROM presence_log WHERE base=? AND number=? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, base, number).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return state, err == nil, err
}

// logPresenceState makes state the open presence_log row of (base, number)
// from at on. It does nothing when that is already the open state; otherwise
// it closes the open row (ended_at = at, never before its start) and opens a
// new one.
func logPresenceState(tx *sql.Tx, base string, number int, state string, at time.Time) error {
	cur, ok, err := openPresenceState(tx, base, number)
	if err != nil || (ok && cur == state) {
		return err
	}
	if err = closePresenceLog(tx, base, number, at); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO presence_log(base,number,role,state,started_at) VALUES(?,?,?,?,?)`, base, number, slotRole(base, number), state, ms(at))
	return err
}

// logPresenceOffline records that (base, number) went offline at at. A slot
// with no open row (never reported, or a free number) is left alone.
func logPresenceOffline(tx *sql.Tx, base string, number int, at time.Time) error {
	if _, ok, err := openPresenceState(tx, base, number); err != nil || !ok {
		return err
	}
	return logPresenceState(tx, base, number, presenceOffline, at)
}

// logPresenceReport logs the state a report left on the slot row: the stored
// state, or "unknown" when none was reported.
func logPresenceReport(tx *sql.Tx, base string, number int, at time.Time) error {
	var state string
	if err := tx.QueryRow(`SELECT state FROM slots WHERE base=? AND number=?`, base, number).Scan(&state); err != nil {
		return err
	}
	if state == "" {
		state = presenceUnknown
	}
	return logPresenceState(tx, base, number, state, at)
}

// movePresenceLog follows a slot renumbered from one number to another: the
// open row at from is closed and its state continues at to.
func movePresenceLog(tx *sql.Tx, base string, from, to int, at time.Time) error {
	state, ok, err := openPresenceState(tx, base, from)
	if err != nil || !ok {
		return err
	}
	if err = closePresenceLog(tx, base, from, at); err != nil {
		return err
	}
	return logPresenceState(tx, base, to, state, at)
}

func closePresenceLog(tx *sql.Tx, base string, number int, at time.Time) error {
	_, err := tx.Exec(`UPDATE presence_log SET ended_at=MAX(?,started_at) WHERE base=? AND number=? AND ended_at IS NULL`, ms(at), base, number)
	return err
}

// prunePresenceLog drops closed rows that ended more than presenceLogRetain
// before now. Open rows stay however old they are.
func prunePresenceLog(db *sql.DB, now time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM presence_log WHERE ended_at IS NOT NULL AND ended_at<?`, ms(now.Add(-presenceLogRetain)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
