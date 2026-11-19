package presence

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RuleMigration is what MigrateAnsweredRule did at start-up.
type RuleMigration struct {
	From    int  // stored version before; 0 when none was stored
	To      int  // version stored now
	Marked  int  // old directives marked as needing no reply
	Changed bool // the stored version was written
	Known   bool // this build knows the stored version's rule (mail.AnsweredRules)
}

// Line is the start-up log line of a migration that wrote the version:
//
//	answered rule: version 2 stored (none before); 0 directives marked closed (no_reply=1)
//	answered rule: version 1 -> 2; 3 directives closed under version 1 but open under version 2 marked closed (no_reply=1); open directives stay open
//	answered rule: version 7 -> 2; version 7 unknown to this build, 4 open directives marked closed (no_reply=1)
func (m RuleMigration) Line() string {
	switch {
	case m.From == 0:
		return fmt.Sprintf("answered rule: version %d stored (none before); %d directives marked closed (no_reply=1)", m.To, m.Marked)
	case !m.Known:
		return fmt.Sprintf("answered rule: version %d -> %d; version %d unknown to this build, %d open directives marked closed (no_reply=1)", m.From, m.To, m.From, m.Marked)
	}
	return fmt.Sprintf("answered rule: version %d -> %d; %d directives closed under version %d but open under version %d marked closed (no_reply=1); open directives stay open", m.From, m.To, m.Marked, m.From, m.To)
}

// MigrateAnsweredRule settles the stored directives under the answered rule
// in force (Config.Rule, else mail.CurrentAnsweredRule) once per version. The
// server runs it at start-up, before it serves requests. base maps a mailbox
// name to its base role.
//
//   - No stored version: a new database without mail stores the current
//     version and marks nothing. A database with mail from before this table
//     ran version 1 (#214): it is settled as version 1, below.
//   - A stored version older than the rule: every directive created before
//     now that is open under the rule but was not open under the stored
//     version's rule gets no_reply=1, the flag `send --no-reply` and `close`
//     set, so a rule change does not reopen old directives at once, and a
//     directive that was open stays open (#268). For 1 -> 2 (#269) that is
//     every directive answered by a reply and not done: it is closed. When this build does not
//     know the stored version (mail.AnsweredRules), every open directive is
//     marked. The rule's version is stored.
//   - The same version, or a newer one (an older build started on a newer
//     database): nothing changes.
func (s *Service) MigrateAnsweredRule(base func(string) string) (RuleMigration, error) {
	rule := s.rule()
	out := RuleMigration{To: rule.Version}
	err := s.db.QueryRow(`SELECT version FROM answered_rule WHERE id=1`).Scan(&out.From)
	if errors.Is(err, sql.ErrNoRows) {
		var mailed bool
		if err = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM messages)`).Scan(&mailed); err != nil {
			return out, err
		}
		if !mailed || rule.Version == 1 {
			out.Changed = true
			_, err = s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,?,?)`, rule.Version, s.now().UTC().Format(time.RFC3339Nano))
			return out, err
		}
		// Mail from before the version table ran version 1 (#214).
		if _, err = s.db.Exec(`INSERT INTO answered_rule(id,version,updated_at) VALUES(1,1,?)`, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
			return out, err
		}
		out.From = 1
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
	wasOpen := map[int64]bool{}
	if old, ok := s.pastRule(out.From); ok {
		out.Known = true
		before, err := s.openDirectivesUnder(old, true, base, s.ids.IsBossName(base))
		if err != nil {
			return out, err
		}
		for _, d := range before {
			wasOpen[d.ID] = true
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, d := range open {
		if !d.At.Before(now) || wasOpen[d.ID] {
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
