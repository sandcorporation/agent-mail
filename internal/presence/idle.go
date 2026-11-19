package presence

import (
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
)

// Idle after an answer (#268).
//
// A directive whose recipient answered it (a plain reply, so it is no longer
// open) but never completed it, and whose recipient sessions then went idle,
// is work that may have stopped after an acknowledgement. Nothing else
// watches it: the reminders stop at the answer. The check takes, per
// recipient mailbox, only its newest directive from a boss role (the work
// the session was last given), when mail.DirectiveState says
// AwaitingCompletion, the recipient is not a boss role, and the live session
// reading the mailbox (slot 1 for <base>, slot N for <base>-N) has been idle, with no sub-agent running, for SWITCHBOARD_REMIND_IDLE (default 1h, 0 off)
// since the later of the answer and the start of the idle stretch. The 진행
// view lists it under Stalled (대기 멈춤 rows); the reminder sweep, when
// reminders are on, tells its sender once (idle-after:<id>), never earlier
// than SWITCHBOARD_REMIND_IDLE after the reminders were switched on.

const defaultRemindIdle = time.Hour

// stall is a directive idle after its answer.
type stall struct {
	progressMailRow
	Since    time.Time // the later of the answer and the idle stretch start
	Sessions []string
}

// liveSession is a numbered slot whose lease has not expired.
type liveSession struct {
	Base, Directory, State string
	Number                 int
	Subagents              int64
	Since, LastSeen        time.Time
}

func (v liveSession) Role() string { return identity.SlotRole(v.Base, v.Number) }

// Mailbox is the mailbox the session reads: <base> for slot 1 (identity.Mailbox
// stores <base>-1 as <base>), else <base>-N.
func (v liveSession) Mailbox() string {
	if v.Number == 1 {
		return v.Base
	}
	return v.Role()
}

// liveSessions lists the live slots with the start of their current
// presence_log stretch, by base and number.
func (s *Service) liveSessions(now time.Time) ([]liveSession, error) {
	rows, err := s.db.Query(`SELECT s.base,s.number,s.directory,s.state,s.subagents,s.registered_at,s.reported_at,`+stretchStartSQL+` FROM slots s WHERE s.lease_until>? ORDER BY s.base,s.number`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []liveSession
	for rows.Next() {
		var v liveSession
		var reg, reported, since int64
		if err = rows.Scan(&v.Base, &v.Number, &v.Directory, &v.State, &v.Subagents, &reg, &reported, &since); err != nil {
			return nil, err
		}
		if v.State == "" {
			v.State = presenceUnknown
		}
		v.Since, v.LastSeen = stretchStart(since, reg), fromMS(max(reported, reg))
		out = append(out, v)
	}
	return out, rows.Err()
}

// idleAfterAnswer lists the directives idle after their answer as of now,
// oldest first; none when the check is off. sessions is liveSessions(now).
func (s *Service) idleAfterAnswer(now time.Time, sessions []liveSession, base func(string) string, isBossName func(string) bool) ([]stall, error) {
	idle := s.cfg.Remind.idle()
	if idle <= 0 {
		return nil, nil
	}
	bossSenders, err := s.bossSenders(isBossName)
	if err != nil || len(bossSenders) == 0 {
		return nil, err
	}
	in := `(` + placeholders(len(bossSenders)) + `)`
	newest, err := s.progressMailQuery(`m.id IN (SELECT MAX(id) FROM messages WHERE sender IN `+in+` AND (COALESCE(reply_to,0)=0 OR directive=1) GROUP BY recipient)`, anySlice(bossSenders)...)
	if err != nil {
		return nil, err
	}
	var ds []progressMailRow
	for _, d := range newest {
		if !isBossName(d.Recipient) {
			ds = append(ds, d)
		}
	}
	if err = s.settle(s.rule(), false, ds, base, isBossName); err != nil {
		return nil, err
	}
	var out []stall
	for _, d := range ds {
		if !d.State.AwaitingCompletion() {
			continue
		}
		var mine []liveSession
		for _, v := range sessions {
			if v.Mailbox() == d.Recipient {
				mine = append(mine, v)
			}
		}
		if len(mine) == 0 {
			continue // no live session: the offline and label stall notices cover it
		}
		st := stall{progressMailRow: d, Since: d.LastAnswer}
		allIdle := true
		for _, v := range mine {
			if v.State != "idle" || v.Subagents > 0 {
				allIdle = false
				break
			}
			if v.Since.After(st.Since) {
				st.Since = v.Since
			}
			st.Sessions = append(st.Sessions, v.Role())
		}
		if allIdle && !now.Before(st.Since.Add(idle)) {
			out = append(out, st)
		}
	}
	return out, nil
}
