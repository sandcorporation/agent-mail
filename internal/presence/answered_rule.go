package presence

import (
	"database/sql"
	"errors"
	"time"
)

// RuleMigration is what MigrateAnsweredRule did at start-up.
type RuleMigration struct {
	From    int  // stored version before; 0 when none was stored
	To      int  // version stored now
	Marked  int  // old directives marked as needing no reply
	Changed bool // the stored version was written
}

// MigrateAnsweredRule settles the stored directives under the answered rule
// in force (Config.Rule, else mail.CurrentAnsweredRule) once per version. The
// server runs it at start-up, before it serves requests. base maps a mailbox
// name to its base role.
//
//   - No stored version (a new database, or one from before this table): the
//     current version is stored and nothing is marked. A database without
//     the row was already running the current rule (version 1, #214).
//   - A stored version older than the rule: every directive created before
//     now that is open under the rule gets no_reply=1, the flag `send
//     --no-reply` and `close` set, so a rule change does not reopen old
//     directives at once; the rule's version is stored.
//   - The same version, or a newer one (an older build started on a newer
//     database): nothing changes.
func (s *Service) MigrateAnsweredRule(base func(string) string) (RuleMigration, error) {
	rule := s.rule()
	out := RuleMigration{To: rule.Version}
	err := s.db.QueryRow(`SELECT version FROM answered_rule WHERE id=1`).Scan(&out.From)
	if errors.Is(err, sql.ErrNoRows) {
		out.Changed = true
		_, err = s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,?,?)`, rule.Version, s.now().UTC().Format(time.RFC3339Nano))
		return out, err
	}
	if err != nil || out.From >= rule.Version {
		out.To = out.From
		return out, err
	}
	now := s.now()
	open, err := s.openDirectives(base, s.ids.IsBossName(base))
	if err != nil {
		return out, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, d := range open {
		if !d.At.Before(now) {
			continue
		}
		res, err := tx.Exec(`UPDATE messages SET no_reply=1 WHERE id=? AND no_reply=0`, d.ID)
		if err != nil {
			return out, err
		}
		n, _ := res.RowsAffected()
		out.Marked += int(n)
	}
	if _, err = tx.Exec(`UPDATE answered_rule SET version=?, updated_at=? WHERE id=1`, rule.Version, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return out, err
	}
	out.Changed = true
	return out, tx.Commit()
}
