package hosting

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/cron"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Scheduled runs of a hosted session (#333): the records and their history.
// The host fires them (autorun.go); the CLI and the admin API only write
// records, so `host auto` works without the server.

const (
	// AutoRunsKept is how many history lines a scheduled run keeps.
	AutoRunsKept  = 500
	maxAutoPrompt = 16 << 10
)

// ErrNoSuchAuto means no scheduled run has the given id (or it belongs to
// another hosted session).
var ErrNoSuchAuto = errors.New("no such scheduled run")

const autoColumns = `id,session,cron,tz,prompt,label,resume,notify,state,next_fire_at,run_requested_at,created_at,updated_at`

func scanAuto(r rowScanner) (wire.HostAuto, error) {
	var a wire.HostAuto
	var resume int
	var notify string
	var next, req sql.NullInt64
	if err := r.Scan(&a.ID, &a.Session, &a.Cron, &a.TZ, &a.Prompt, &a.Label, &resume, &notify, &a.State, &next, &req, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	a.Resume = resume != 0
	if err := json.Unmarshal([]byte(notify), &a.Notify); err != nil || a.Notify == nil {
		a.Notify = []string{}
	}
	if next.Valid {
		a.NextFireAt = &next.Int64
	}
	if req.Valid {
		a.RunRequestedAt = &req.Int64
	}
	return a, nil
}

// nextFire is the first time of expr in tz after now, or nil when it has
// none.
func nextFire(expr, tz string, now time.Time) (*int64, error) {
	spec, err := cron.Parse(expr)
	if err != nil {
		return nil, invalidf("cron: %v", err)
	}
	loc, err := cron.Zone(tz)
	if err != nil {
		return nil, invalidf("%v", err)
	}
	at := spec.Next(now, loc)
	if at.IsZero() {
		return nil, nil
	}
	ms := at.UnixMilli()
	return &ms, nil
}

func (s *Service) checkAuto(c wire.HostAutoCreate) error {
	if strings.TrimSpace(c.Prompt) == "" {
		return invalidf("prompt: required")
	}
	if len(c.Prompt) > maxAutoPrompt || strings.ContainsRune(c.Prompt, 0) {
		return invalidf("prompt: at most %d bytes of text", maxAutoPrompt)
	}
	if err := checkLabel(c.Label); err != nil {
		return err
	}
	for _, r := range c.Notify {
		if s.roleExists == nil || !s.roleExists(r) {
			return invalidf("notify: %q is not a registered role", r)
		}
	}
	return nil
}

// CreateAuto adds a scheduled run to the hosted session id, read in this
// machine's time zone (the host's, like `schedule add`).
func (s *Service) CreateAuto(id string, c wire.HostAutoCreate) (wire.HostAuto, error) {
	if _, err := s.Get(id); err != nil {
		return wire.HostAuto{}, err
	}
	if err := s.checkAuto(c); err != nil {
		return wire.HostAuto{}, err
	}
	now := s.clock()
	tz := cron.LocalZone()
	next, err := nextFire(c.Cron, tz, now)
	if err != nil {
		return wire.HostAuto{}, err
	}
	if next == nil {
		return wire.HostAuto{}, invalidf("cron: %q never fires", c.Cron)
	}
	notify := c.Notify
	if notify == nil {
		notify = []string{}
	}
	nb, _ := json.Marshal(notify)
	ms := now.UnixMilli()
	res, err := s.db.Exec(`INSERT INTO host_autos(session,cron,tz,prompt,label,resume,notify,state,next_fire_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, c.Cron, tz, c.Prompt, c.Label, boolInt(c.Resume), string(nb), wire.HostAutoActive, *next, ms, ms)
	if err != nil {
		return wire.HostAuto{}, err
	}
	aid, err := res.LastInsertId()
	if err != nil {
		return wire.HostAuto{}, err
	}
	return s.GetAuto(aid)
}

// GetAuto reads one scheduled run with its newest history line.
func (s *Service) GetAuto(id int64) (wire.HostAuto, error) {
	a, err := scanAuto(s.db.QueryRow(`SELECT `+autoColumns+` FROM host_autos WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNoSuchAuto
	}
	if err != nil {
		return a, err
	}
	runs, err := s.AutoRuns(id, 1)
	if err == nil && len(runs) == 1 {
		a.LastRun = &runs[0]
	}
	return a, err
}

// Autos lists the scheduled runs of the hosted session id, or of every
// session when id is empty, by id, each with its newest history line.
func (s *Service) Autos(id string) ([]wire.HostAuto, error) {
	q, args := `SELECT `+autoColumns+` FROM host_autos`, []any{}
	if id != "" {
		q, args = q+` WHERE session=?`, append(args, id)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	out := []wire.HostAuto{}
	for rows.Next() {
		a, err := scanAuto(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		runs, err := s.AutoRuns(out[i].ID, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) == 1 {
			out[i].LastRun = &runs[0]
		}
	}
	return out, nil
}

// AutoRuns is the newest n history lines of the scheduled run id, newest
// first.
func (s *Service) AutoRuns(id int64, n int) ([]wire.HostAutoRun, error) {
	if n <= 0 || n > AutoRunsKept {
		n = AutoRunsKept
	}
	rows, err := s.db.Query(`SELECT id,auto,at,result,manual,exit_code,duration_ms,output,mail_id,launched,agent_session_id,detail FROM host_auto_runs WHERE auto=? ORDER BY id DESC LIMIT ?`, id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.HostAutoRun{}
	for rows.Next() {
		var r wire.HostAutoRun
		var manual, launched int
		var exit sql.NullInt64
		var dur, mailID sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Auto, &r.At, &r.Result, &manual, &exit, &dur, &r.Output, &mailID, &launched, &r.AgentSessionID, &r.Detail); err != nil {
			return nil, err
		}
		r.Manual, r.Launched = manual != 0, launched != 0
		if exit.Valid {
			v := int(exit.Int64)
			r.ExitCode = &v
		}
		if dur.Valid {
			r.DurationMS = &dur.Int64
		}
		if mailID.Valid {
			r.MailID = &mailID.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AutoView is the scheduled run id with its newest n history lines.
func (s *Service) AutoView(id int64, n int) (wire.HostAutoView, error) {
	a, err := s.GetAuto(id)
	if err != nil {
		return wire.HostAutoView{}, err
	}
	runs, err := s.AutoRuns(id, n)
	return wire.HostAutoView{HostAuto: a, Runs: runs}, err
}

// PauseAuto stops the scheduled run id from firing at its times.
func (s *Service) PauseAuto(id int64) (wire.HostAuto, error) {
	if err := s.autoExec(`UPDATE host_autos SET state=?,next_fire_at=NULL,updated_at=? WHERE id=?`, wire.HostAutoPaused, s.clock().UnixMilli(), id); err != nil {
		return wire.HostAuto{}, err
	}
	return s.GetAuto(id)
}

// ResumeAuto lets a paused scheduled run fire again from its next time
// after now; times passed while it was paused are not made up.
func (s *Service) ResumeAuto(id int64) (wire.HostAuto, error) {
	a, err := s.GetAuto(id)
	if err != nil {
		return a, err
	}
	now := s.clock()
	next, err := nextFire(a.Cron, a.TZ, now)
	if err != nil {
		return a, err
	}
	var nv any
	if next != nil {
		nv = *next
	}
	if err := s.autoExec(`UPDATE host_autos SET state=?,next_fire_at=?,updated_at=? WHERE id=?`, wire.HostAutoActive, nv, now.UnixMilli(), id); err != nil {
		return a, err
	}
	return s.GetAuto(id)
}

// RequestAutoRun asks the host to run the scheduled run id once, now,
// whatever its time and state; the host does it on its next sweep.
func (s *Service) RequestAutoRun(id int64) (wire.HostAuto, error) {
	if err := s.autoExec(`UPDATE host_autos SET run_requested_at=?,updated_at=? WHERE id=?`, s.clock().UnixMilli(), s.clock().UnixMilli(), id); err != nil {
		return wire.HostAuto{}, err
	}
	return s.GetAuto(id)
}

// DeleteAuto removes the scheduled run id and its history.
func (s *Service) DeleteAuto(id int64) (wire.HostAuto, error) {
	a, err := s.GetAuto(id)
	if err != nil {
		return a, err
	}
	if err := s.autoExec(`DELETE FROM host_autos WHERE id=?`, id); err != nil {
		return a, err
	}
	_, err = s.db.Exec(`DELETE FROM host_auto_runs WHERE auto=?`, id)
	return a, err
}

// deleteAutosOf removes the scheduled runs of the hosted session id and
// their history (the record is being removed).
func (s *Service) deleteAutosOf(id string) error {
	if _, err := s.db.Exec(`DELETE FROM host_auto_runs WHERE auto IN (SELECT id FROM host_autos WHERE session=?)`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM host_autos WHERE session=?`, id)
	return err
}

func (s *Service) autoExec(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchAuto
	}
	return nil
}

// addAutoRun records one history line and keeps the newest AutoRunsKept.
func (s *Service) addAutoRun(r wire.HostAutoRun) (wire.HostAutoRun, error) {
	var mailID any
	if r.MailID != nil {
		mailID = *r.MailID
	}
	var exit, dur any
	if r.ExitCode != nil {
		exit = *r.ExitCode
	}
	if r.DurationMS != nil {
		dur = *r.DurationMS
	}
	res, err := s.db.Exec(`INSERT INTO host_auto_runs(auto,at,result,manual,exit_code,duration_ms,output,mail_id,launched,agent_session_id,detail) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.Auto, r.At, r.Result, boolInt(r.Manual), exit, dur, r.Output, mailID, boolInt(r.Launched), r.AgentSessionID, r.Detail)
	if err != nil {
		return r, err
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return r, err
	}
	_, err = s.db.Exec(`DELETE FROM host_auto_runs WHERE auto=? AND id NOT IN (SELECT id FROM host_auto_runs WHERE auto=? ORDER BY id DESC LIMIT ?)`, r.Auto, r.Auto, AutoRunsKept)
	return r, err
}

// claimAuto takes the scheduled run id's due time (or its manual request)
// for one firing: it moves next_fire_at past now and clears the request, so
// a missed stretch fires once and a second sweep finds nothing. It reports
// whether there was anything to fire and whether it was the manual request.
func (s *Service) claimAuto(a wire.HostAuto, now time.Time) (fire, manual bool, at int64, err error) {
	ms := now.UnixMilli()
	switch {
	case a.RunRequestedAt != nil:
		res, err := s.db.Exec(`UPDATE host_autos SET run_requested_at=NULL WHERE id=? AND run_requested_at=?`, a.ID, *a.RunRequestedAt)
		if err != nil {
			return false, false, 0, err
		}
		n, _ := res.RowsAffected()
		return n > 0, true, *a.RunRequestedAt, nil
	case a.State == wire.HostAutoActive && a.NextFireAt != nil && *a.NextFireAt <= ms:
		next, err := nextFire(a.Cron, a.TZ, now)
		if err != nil {
			return false, false, 0, err
		}
		var nv any
		if next != nil {
			nv = *next
		}
		res, err := s.db.Exec(`UPDATE host_autos SET next_fire_at=? WHERE id=? AND next_fire_at=?`, nv, a.ID, *a.NextFireAt)
		if err != nil {
			return false, false, 0, err
		}
		n, _ := res.RowsAffected()
		return n > 0, false, *a.NextFireAt, nil
	}
	return false, false, 0, nil
}

// dueAutos lists the scheduled runs with a time at or before now or a
// manual request.
func (s *Service) dueAutos(now time.Time) ([]wire.HostAuto, error) {
	rows, err := s.db.Query(`SELECT `+autoColumns+` FROM host_autos WHERE run_requested_at IS NOT NULL OR (state=? AND next_fire_at<=?) ORDER BY id`,
		wire.HostAutoActive, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wire.HostAuto
	for rows.Next() {
		a, err := scanAuto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
