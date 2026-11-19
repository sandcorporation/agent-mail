package presence

import (
	"sort"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Idle open directives (#268, #269).
//
// An open directive (the answered rule's Open: not done and not closed,
// however many replies it got) whose recipient sessions went idle is work
// that may have stopped, for example after an acknowledgement. The check
// takes, per recipient mailbox, only its newest directive from a boss role
// (the work the session was last given), when that directive is open, the
// recipient is not a boss role, and the live session reading the mailbox
// (slot 1 for <base>, slot N for <base>-N) has been idle, with no sub-agent
// running, for SWITCHBOARD_REMIND_IDLE (default 1h, 0 off) since the latest
// of the directive, its newest reply and the start of the idle stretch. The
// 진행 view lists it under Stalled (대기 멈춤 rows); the reminder sweep, when
// reminders are on, tells its sender up to SWITCHBOARD_REMIND_IDLE_MAX times
// (default 1; idle-after:<id>, then idle-after:<id>:<n>), one threshold
// apart, never earlier than SWITCHBOARD_REMIND_IDLE after the reminders were
// switched on.

const defaultRemindIdle = time.Hour

// stall is an open directive whose recipient is idle.
type stall struct {
	progressMailRow
	Since    time.Time // the latest of the directive, its newest reply and the idle stretch start
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

// idleOpen lists, per recipient mailbox, its newest open directive when the
// recipient is idle past the threshold as of now, oldest first; none when the check is off. sessions is liveSessions(now).
func (s *Service) idleOpen(now time.Time, sessions []liveSession, base func(string) string, isBossName func(string) bool) ([]stall, error) {
	idle := s.cfg.Remind.idle()
	if idle <= 0 {
		return nil, nil
	}
	open, err := s.openDirectives(base, isBossName)
	if err != nil {
		return nil, err
	}
	newest := map[string]int{} // recipient -> index in ds of its newest open directive
	var ds []progressMailRow
	for _, d := range open { // oldest first
		if isBossName(d.Recipient) {
			continue
		}
		if i, ok := newest[d.Recipient]; ok {
			ds[i] = d
			continue
		}
		newest[d.Recipient] = len(ds)
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
	var out []stall
	for _, d := range ds {
		var mine []liveSession
		for _, v := range sessions {
			if v.Mailbox() == d.Recipient {
				mine = append(mine, v)
			}
		}
		if len(mine) == 0 {
			continue // no live session: the offline and label stall notices cover it
		}
		st := stall{progressMailRow: d, Since: d.At}
		if d.LastAnswer.After(st.Since) {
			st.Since = d.LastAnswer
		}
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

// mailboxState is the live state of the sessions reading mailbox (the worst,
// as progressBadge ranks them), or offline when none is live.
func mailboxState(sessions []liveSession, mailbox string) string {
	var mine []wire.ProgressSession
	for _, v := range sessions {
		if v.Mailbox() == mailbox {
			mine = append(mine, wire.ProgressSession{State: v.State, Subagents: v.Subagents})
		}
	}
	if len(mine) == 0 {
		return "offline"
	}
	return progressBadge(mine)
}
