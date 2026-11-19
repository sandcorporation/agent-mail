// Package presence is the session side of numbered slots: the slot lease
// lifecycle (register, heartbeat, takeover, release), presence reports and
// their history (presence_log), offline notices and the progress view.
package presence

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Session presence.
//
// A session holding a numbered slot reports presence through the slot
// heartbeat: state, context tokens and last turn time ride on the same body,
// and each report renews the lease. There is one rule for offline: a session
// is offline exactly when its slot lease has ended. The lease length is
// SWITCHBOARD_PRESENCE_INTERVAL × SWITCHBOARD_PRESENCE_MISSED (60s × 3 = 3m by
// default), so missing that many heartbeats both marks the session offline
// and frees its number. SWITCHBOARD_SLOT_LEASE, when set, overrides the
// product directly. Offline rows stay listed for SWITCHBOARD_PRESENCE_RETAIN
// (default 24h) unless another identity takes the number first.

const (
	defaultPresenceInterval = 60 * time.Second
	defaultPresenceMissed   = 3
	defaultPresenceRetain   = 24 * time.Hour
)

// Config is the presence and lease settings read from the environment.
type Config struct {
	SlotLease time.Duration  // SWITCHBOARD_SLOT_LEASE; 0 means Interval × Missed
	Interval  time.Duration  // expected heartbeat interval; 0 means defaultPresenceInterval
	Missed    int            // heartbeats a session may miss; 0 means defaultPresenceMissed
	Retain    *time.Duration // how long offline sessions stay listed; nil means defaultPresenceRetain
	Remind    RemindConfig   // directive reminders (remind.go)
	// Rule is the answered rule open directives follow; the zero value means
	// mail.CurrentAnsweredRule. Only tests set another (a later version).
	Rule mail.AnsweredRule
	// Rules is every version of the rule by number, for MigrateAnsweredRule;
	// nil means mail.AnsweredRules. Only tests set it.
	Rules map[int]mail.AnsweredRule
}

// rule is the answered rule in force (Config.Rule, else the current one).
func (s *Service) rule() mail.AnsweredRule {
	if s.cfg != nil && s.cfg.Rule.Replies != nil {
		return s.cfg.Rule
	}
	return mail.CurrentAnsweredRule
}

// pastRule is version v of the answered rule, false when this build does not
// know it.
func (s *Service) pastRule(v int) (mail.AnsweredRule, bool) {
	rules := mail.AnsweredRules
	if s.cfg != nil && s.cfg.Rules != nil {
		rules = s.cfg.Rules
	}
	r, ok := rules[v]
	return r, ok && r.Replies != nil
}

// Service is the presence rules over the slots and presence_log tables. It
// holds only what those rules need, so callers build one per use.
type Service struct {
	db    *sql.DB
	clock func() time.Time
	cfg   *Config
	ids   *identity.Service
	wake  func(box string)
}

// New returns the presence rules over db. clock is the current time, cfg the
// presence settings, ids the identity rules, and wake tells event subscribers
// of a mailbox that mail arrived (the mail hub's Publish).
func New(db *sql.DB, clock func() time.Time, cfg *Config, ids *identity.Service, wake func(box string)) *Service {
	return &Service{db: db, clock: clock, cfg: cfg, ids: ids, wake: wake}
}

func (s *Service) now() time.Time { return s.clock() }

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// Configure reads the presence settings from the environment into c.
func Configure(c *Config, getenv func(string) string) error {
	if v, from := config.EnvAliasFrom(getenv, "PRESENCE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return errors.New(from + " must be a duration of at least 1s such as 60s")
		}
		c.Interval = d
	}
	if v, from := config.EnvAliasFrom(getenv, "PRESENCE_MISSED"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New(from + " must be a whole number of at least 1 such as 3")
		}
		c.Missed = n
	}
	if v, from := config.EnvAliasFrom(getenv, "PRESENCE_RETAIN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return errors.New(from + " must be a duration such as 24h (0 hides offline sessions)")
		}
		c.Retain = &d
	}
	return configureRemind(&c.Remind, getenv)
}

func (s *Service) heartbeatInterval() time.Duration {
	if s.cfg.Interval > 0 {
		return s.cfg.Interval
	}
	return defaultPresenceInterval
}

func (s *Service) missedHeartbeats() int {
	if s.cfg.Missed > 0 {
		return s.cfg.Missed
	}
	return defaultPresenceMissed
}

// OfflineRetention is how long offline sessions stay listed.
func (s *Service) OfflineRetention() time.Duration {
	if s.cfg.Retain != nil {
		return *s.cfg.Retain
	}
	return defaultPresenceRetain
}

// applyPresence records a report on a slot row. A fresh claim (register or
// takeover) starts from the given fields only; a heartbeat keeps fields it
// does not send. A change of state is written to presence_log.
func applyPresence(tx *sql.Tx, base string, number int, req wire.SlotRequest, now time.Time, fresh bool) error {
	last, err := req.Presence()
	if err != nil {
		return err
	}
	// subagents is not kept like the other fields: a report without it means
	// none are running (the plugin sends it only when N > 0).
	set, args := `reported_at=?,offline_noticed_at=0,subagents=?`, []any{ms(now), req.SubagentCount()}
	if fresh {
		set += `,state='',context_tokens=-1,last_turn_at=0`
	}
	if req.State != "" {
		set += `,state=?`
		args = append(args, req.State)
	}
	if req.ContextTokens != nil {
		set += `,context_tokens=?`
		args = append(args, *req.ContextTokens)
	}
	if !last.IsZero() {
		set += `,last_turn_at=?`
		args = append(args, ms(last))
	}
	if _, err = tx.Exec(`UPDATE slots SET `+set+` WHERE base=? AND number=?`, append(args, base, number)...); err != nil {
		return err
	}
	return logPresenceReport(tx, base, number, now)
}

func presenceCols() string {
	return slotCols() + `,state,context_tokens,last_turn_at,reported_at,subagents,` + stretchStartSQL
}

func scanPresence(sc interface{ Scan(...any) error }, now time.Time) (wire.PresenceView, error) {
	var v wire.PresenceView
	var reg, until, tokens, last, reported, subagents, started int64
	err := sc.Scan(&v.Base, &v.Number, &v.Machine, &v.Directory, &v.SessionID, &v.Label, &v.Parent, &reg, &until, &v.State, &tokens, &last, &reported, &subagents, &started)
	v.Role = identity.SlotRole(v.Base, v.Number)
	v.RegisteredAt, v.LeaseExpiresAt = fromMS(reg), fromMS(until)
	v.Online = until > ms(now)
	switch {
	case !v.Online:
		v.Status = "offline"
		t := v.LeaseExpiresAt
		v.OfflineSince = &t
		v.Since = &t
	case v.State != "":
		v.Status = v.State
	default:
		v.Status = "unknown"
	}
	if v.Online {
		v.Subagents = subagents
		t := stretchStart(started, reg)
		v.Since = &t
	}
	if tokens >= 0 {
		v.ContextTokens = &tokens
	}
	if last > 0 {
		t := fromMS(last)
		v.LastTurnAt = &t
	}
	if reported > 0 {
		t := fromMS(reported)
		v.ReportedAt = &t
	}
	return v, err
}

// matchRole selects sessions for ?role=: empty matches everything, "x*"
// matches roles starting with x, otherwise the value matches a base (all its
// numbers) or one numbered role ("team/worker-1").
func matchRole(filter string, v wire.PresenceView) bool {
	switch {
	case filter == "":
		return true
	case strings.HasSuffix(filter, "*"):
		return strings.HasPrefix(v.Role, strings.TrimSuffix(filter, "*"))
	default:
		return filter == v.Base || filter == v.Role
	}
}

// List is the sessions matching filter (see matchRole) with the presence
// settings.
func (s *Service) List(filter string) (wire.PresenceList, error) {
	now := s.now()
	out := wire.PresenceList{
		Now:             now.UTC(),
		IntervalSeconds: int(s.heartbeatInterval() / time.Second),
		Missed:          s.missedHeartbeats(),
		LeaseSeconds:    int(s.LeaseLength() / time.Second),
		RetainSeconds:   int(s.OfflineRetention() / time.Second),
		Sessions:        []wire.PresenceView{},
	}
	rows, err := s.db.Query(`SELECT `+presenceCols()+` FROM slots s WHERE lease_until>? ORDER BY base,number`, ms(now.Add(-s.OfflineRetention())))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanPresence(rows, now)
		if err != nil {
			return out, err
		}
		if matchRole(filter, v) {
			out.Sessions = append(out.Sessions, v)
		}
	}
	return out, rows.Err()
}

// API serves GET /api/presence. Internal roles and global bosses read
// every session; a group role reads its own group's (internal/identity/groups.go). p is nil
// for the admin API, which sees all.
func (s *Service) API(w http.ResponseWriter, r *http.Request, p *identity.Principal) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	out, err := s.List(r.URL.Query().Get("role"))
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	if p != nil {
		out.Sessions = s.visibleSessions(*p, out.Sessions)
	}
	writeJSON(w, out)
}

// visibleSessions keeps the presence rows p may see.
func (s *Service) visibleSessions(p identity.Principal, in []wire.PresenceView) []wire.PresenceView {
	if s.ids.Unrestricted(p) {
		return in
	}
	return slices.DeleteFunc(in, func(v wire.PresenceView) bool { return !s.ids.MaySee(p, v.Base) })
}

// OfflineUnnoticed lists sessions whose lease has ended (offline) and whose
// offline transition nobody has marked handled since their last report. It
// is the hook for acting once per offline transition (notifying a parent):
// call it periodically and mark each handled row with MarkOfflineNoticed.
// Rows from before presence (never reported) are left out.
func (s *Service) OfflineUnnoticed() ([]wire.PresenceView, error) {
	now := s.now()
	rows, err := s.db.Query(`SELECT `+presenceCols()+` FROM slots s WHERE lease_until<=? AND lease_until>? AND reported_at>0 AND offline_noticed_at=0 ORDER BY base,number`, ms(now), ms(now.Add(-s.OfflineRetention())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.PresenceView{}
	for rows.Next() {
		v, err := scanPresence(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MarkOfflineNoticed records that v's offline transition was handled. It does
// nothing (and returns false) when the session reported again since v was
// read, so a renewal racing with the handler is not hidden.
func (s *Service) MarkOfflineNoticed(v wire.PresenceView) (bool, error) {
	res, err := s.db.Exec(`UPDATE slots SET offline_noticed_at=? WHERE base=? AND number=? AND lease_until=? AND lease_until<=? AND offline_noticed_at=0`,
		ms(s.now()), v.Base, v.Number, ms(v.LeaseExpiresAt), ms(s.now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
