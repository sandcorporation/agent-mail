package jobs

// Schedules (#288, #292): prompts the server delivers to their owner role as
// mail at fire times. Nothing runs on a machine: the sweep fires them. A fire
// while a session of the role is attached is one normal mail; otherwise it is
// recorded as missed, and the next sweep that finds a session attached sends
// one mail with the prompt and the count. At most one fire mail of a
// schedule is unread at a time: fires while it is unread are counted too.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/cron"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// maxCatchUp bounds the fires one sweep counts for one schedule (a minute
// cron after a long server outage); past it the count is a lower bound.
const maxCatchUp = 100000

// ScheduleAPI serves /api/schedules (rest is the path after it, without the
// slash):
//
//	POST   ""   create (idempotent on the role and key)
//	GET    ""   list: the role's own, every role's for a boss role
//	GET    "ID" show (owner only)
//	DELETE "ID" remove (owner only)
func (s *Service) ScheduleAPI(w http.ResponseWriter, r *http.Request, p identity.Principal, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodPost:
			s.createSchedule(w, r, p)
		case http.MethodGet:
			role := r.URL.Query().Get("role")
			if !s.ids.IsBoss(p) {
				if role != "" && role != p.Role {
					http.Error(w, "only boss roles list other roles' schedules", http.StatusForbidden)
					return
				}
				role = p.Role
			}
			s.writeSchedules(w, role, r.URL.Query().Get("state"), false)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid schedule ID", http.StatusBadRequest)
		return
	}
	sc, err := s.GetSchedule(id)
	if err != nil || sc.Role != p.Role {
		// Another role's schedule is as unknown as a missing one.
		if err != nil && !errors.Is(err, errNotFound) {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		http.Error(w, "unknown schedule", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, sc)
	case http.MethodDelete:
		s.removeSchedule(w, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ScheduleAdminAPI serves the dashboard's /api/admin/schedules (rest is the
// path after it, without the slash). The admin guard and the write guard ran
// already.
//
//	GET    ""   every role's schedules, ?role= and ?state=; no prompt text
//	DELETE "ID" remove
func (s *Service) ScheduleAdminAPI(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.writeSchedules(w, r.URL.Query().Get("role"), r.URL.Query().Get("state"), true)
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid schedule ID", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.GetSchedule(id); err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "unknown schedule", http.StatusNotFound)
		} else {
			http.Error(w, "query failed", http.StatusInternalServerError)
		}
		return
	}
	s.removeSchedule(w, id)
}

func (s *Service) createSchedule(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var q wire.ScheduleCreate
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&q) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := q.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = q.IdempotencyKey
	}
	if key != "" {
		if sc, ok := s.scheduleByIdem(p.Role, key); ok {
			writeJSON(w, http.StatusOK, sc)
			return
		}
	}
	now := s.now().UTC()
	expires := wire.DefaultWatchExpiry
	if q.ExpiresIn > 0 {
		expires = time.Duration(q.ExpiresIn) * time.Second
	}
	var next time.Time
	switch q.Kind {
	case wire.ScheduleCron:
		spec, err := cron.Parse(q.Cron)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		loc, err := cron.Zone(q.TZ)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if next = spec.Next(now, loc); next.IsZero() {
			http.Error(w, "cron never fires", http.StatusBadRequest)
			return
		}
	case wire.ScheduleOnce:
		if q.TZ == "" {
			q.TZ = "UTC"
		}
		if _, err := cron.Zone(q.TZ); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		next = q.At.UTC()
		switch {
		case next.Before(now.Add(-time.Minute)):
			http.Error(w, "at is in the past", http.StatusBadRequest)
			return
		case next.After(now.Add(expires)):
			http.Error(w, "at is after the expiry; give a longer expires_in", http.StatusBadRequest)
			return
		}
	}
	res, err := s.db.Exec(`INSERT INTO schedules(role,machine,kind,label,prompt,cron,tz,state,created_at,expires_at,duration_s,next_fire_at,idem) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Role, q.Machine, q.Kind, q.Label, q.Prompt, q.Cron, q.TZ, wire.ScheduleActive, stamp(now), stamp(now.Add(expires)), int64(expires/time.Second), stamp(next.UTC()), nullable(key))
	if err != nil {
		// A concurrent retry with the same key won the insert.
		if sc, ok := s.scheduleByIdem(p.Role, key); key != "" && ok {
			writeJSON(w, http.StatusOK, sc)
			return
		}
		http.Error(w, "could not store schedule", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	sc, err := s.GetSchedule(id)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Service) removeSchedule(w http.ResponseWriter, id int64) {
	if _, err := s.db.Exec(`DELETE FROM schedules WHERE id=?`, id); err != nil {
		http.Error(w, "could not remove schedule", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": true})
}

func (s *Service) writeSchedules(w http.ResponseWriter, role, state string, hidePrompt bool) {
	out, err := s.Schedules(role, state)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if hidePrompt {
		for i := range out {
			out[i].Prompt = ""
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// Schedules lists schedules newest first, of role and in state when given
// (StateLive: active).
func (s *Service) Schedules(role, state string) ([]wire.Schedule, error) {
	where, args := []string{}, []any{}
	if role != "" {
		where, args = append(where, "role=?"), append(args, role)
	}
	switch state {
	case "":
	case StateLive:
		where, args = append(where, "state=?"), append(args, wire.ScheduleActive)
	default:
		where, args = append(where, "state=?"), append(args, state)
	}
	q := `SELECT ` + scheduleColumns + ` FROM schedules`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.Schedule{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc.Schedule)
	}
	return out, rows.Err()
}

const scheduleColumns = `id,role,machine,kind,label,prompt,cron,tz,state,created_at,expires_at,next_fire_at,last_fire_at,missed,missed_last_at,duration_s,last_mail_id`

// scheduleRow is a schedule with the columns the sweep needs.
type scheduleRow struct {
	wire.Schedule
	duration time.Duration
	lastMail sql.NullInt64
}

func scanSchedule(r scanner) (scheduleRow, error) {
	var sc scheduleRow
	var created, expires string
	var next, last, missedLast sql.NullString
	var duration int64
	err := r.Scan(&sc.ID, &sc.Role, &sc.Machine, &sc.Kind, &sc.Label, &sc.Prompt, &sc.Cron, &sc.TZ, &sc.State,
		&created, &expires, &next, &last, &sc.Missed, &missedLast, &duration, &sc.lastMail)
	if err != nil {
		return sc, err
	}
	sc.CreatedAt, sc.ExpiresAt = parse(created), parse(expires)
	sc.NextFireAt, sc.LastFireAt, sc.MissedLastAt = parseNull(next), parseNull(last), parseNull(missedLast)
	sc.duration = time.Duration(duration) * time.Second
	return sc, nil
}

// GetSchedule returns one schedule.
func (s *Service) GetSchedule(id int64) (wire.Schedule, error) {
	sc, err := scanSchedule(s.db.QueryRow(`SELECT `+scheduleColumns+` FROM schedules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return sc.Schedule, errNotFound
	}
	return sc.Schedule, err
}

func (s *Service) scheduleByIdem(role, key string) (wire.Schedule, bool) {
	sc, err := scanSchedule(s.db.QueryRow(`SELECT `+scheduleColumns+` FROM schedules WHERE role=? AND idem=?`, role, key))
	return sc.Schedule, err == nil
}

// SweepSchedules handles every active schedule at the clock's now: moves the
// expiry forward while a session of the owner is attached, counts the fires
// due since the last sweep, delivers them (or records them as missed) and
// ends a delivered once schedule or an expired one. Each schedule is one
// transaction, and every mail's idempotency key names the schedule and the
// fire, so a sweep repeated after a restart sends nothing twice. base maps
// a role to its base role; attached reports a live session of a base. It
// returns how many schedules changed.
func (s *Service) SweepSchedules(ctx context.Context, base func(string) string, attached func(context.Context, string) (bool, error)) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,role FROM schedules WHERE state=?`, wire.ScheduleActive)
	if err != nil {
		return 0, err
	}
	type due struct {
		id   int64
		role string
	}
	var list []due
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.id, &d.role); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	now := s.now().UTC()
	seen := map[string]bool{}
	changed := 0
	for _, d := range list {
		b := base(d.role)
		on, ok := seen[b]
		if !ok {
			if on, err = attached(ctx, b); err != nil {
				return changed, err
			}
			seen[b] = on
		}
		var box string
		var moved bool
		err = store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			var e error
			box, moved, e = sweepSchedule(tx, d.id, on, now)
			return e
		})
		if err != nil {
			return changed, err
		}
		if moved {
			changed++
		}
		if box != "" && s.wake != nil {
			s.wake(box)
		}
	}
	return changed, nil
}

// sweepSchedule handles one schedule; box is the mailbox that got a mail.
func sweepSchedule(tx *sql.Tx, id int64, attached bool, now time.Time) (box string, changed bool, err error) {
	sc, err := scanSchedule(tx.QueryRow(`SELECT `+scheduleColumns+` FROM schedules WHERE id=? AND state=?`, id, wire.ScheduleActive))
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil // removed or ended meanwhile
	}
	if err != nil {
		return "", false, err
	}
	expires := sc.ExpiresAt
	if attached && sc.duration > 0 {
		expires = now.Add(sc.duration)
	}
	// Fires up to the expiry count; later ones never happen.
	limit := now
	if expires.Before(limit) {
		limit = expires
	}
	count, last, next, err := dueFires(sc.Schedule, limit)
	if err != nil {
		return "", false, err
	}
	missed, missedLast := sc.Missed+count, sc.MissedLastAt
	if count > 0 {
		missedLast = &last
	}
	lastMail := sc.lastMail
	mailbox, hasBox := identity.Mailbox(tx, sc.Role)
	if attached && hasBox && missed > 0 {
		unread, err := mailUnread(tx, lastMail)
		if err != nil {
			return "", false, err
		}
		if !unread {
			subject, body := fireMail(sc.Schedule, missed, *missedLast, sc.Missed == 0 && count == 1)
			key := fmt.Sprintf("schedule:%d:%s", sc.ID, stamp(missedLast.UTC()))
			stored, err := mail.InsertSystemMailOnce(tx, mailbox, subject, body, "", "normal", key, now)
			if err != nil {
				return "", false, err
			}
			if stored {
				box = mailbox
			}
			if err = tx.QueryRow(`SELECT id FROM messages WHERE sender=? AND idem=?`, identity.SystemSender, key).Scan(&lastMail); err != nil {
				return "", false, err
			}
			missed, missedLast = 0, nil
		}
	}
	state := wire.ScheduleActive
	switch {
	case !expires.After(now):
		state = wire.ScheduleExpired
		if hasBox {
			subject, body := expiryMail(sc.Schedule, missed, missedLast)
			stored, err := mail.InsertSystemMailOnce(tx, mailbox, subject, body, "", "normal", fmt.Sprintf("schedule-expired:%d", sc.ID), now)
			if err != nil {
				return "", false, err
			}
			if stored {
				box = mailbox
			}
		}
	case sc.Kind == wire.ScheduleOnce && next == nil && missed == 0:
		state = wire.ScheduleEnded
	}
	lastFire := sc.LastFireAt
	if count > 0 {
		lastFire = &last
	}
	changed = count > 0 || box != "" || state != sc.State || missed != sc.Missed || !expires.Equal(sc.ExpiresAt)
	if !changed {
		return "", false, nil
	}
	_, err = tx.Exec(`UPDATE schedules SET state=?, expires_at=?, next_fire_at=?, last_fire_at=?, missed=?, missed_last_at=?, last_mail_id=? WHERE id=?`,
		state, stamp(expires), stampNull(next), stampNull(lastFire), missed, stampNull(missedLast), lastMail, sc.ID)
	return box, true, err
}

// dueFires counts the fires of sc at or before limit, from its next fire on:
// the count, the last of them, and the next fire after them (nil when
// nothing is left: a once schedule after its fire).
func dueFires(sc wire.Schedule, limit time.Time) (count int, last time.Time, next *time.Time, err error) {
	next = sc.NextFireAt
	if next == nil || next.After(limit) {
		return 0, last, next, nil
	}
	if sc.Kind == wire.ScheduleOnce {
		return 1, *next, nil, nil
	}
	spec, err := cron.Parse(sc.Cron)
	if err != nil {
		return 0, last, next, err
	}
	loc, err := cron.Zone(sc.TZ)
	if err != nil {
		return 0, last, next, err
	}
	at := *next
	for !at.After(limit) {
		count, last = count+1, at
		if count >= maxCatchUp {
			at = spec.Next(limit, loc)
		} else {
			at = spec.Next(at, loc)
		}
		if at.IsZero() {
			return count, last, nil, nil
		}
		at = at.UTC()
	}
	return count, last, &at, nil
}

// mailUnread reports whether the schedule's previous fire mail is still
// unread by its recipient.
func mailUnread(tx *sql.Tx, id sql.NullInt64) (bool, error) {
	if !id.Valid {
		return false, nil
	}
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM messages m `+mail.StatusJoin+` WHERE m.id=? AND `+mail.StatusExpr+` IN ('pending','held')`, id.Int64).Scan(&n)
	return n > 0, err
}

// fireMail is the mail of n undelivered fires, the last at last: the prompt
// alone for one fire on time, else the prompt and `밀린 N회, 마지막 <time>`
// (the time on the schedule's zone).
func fireMail(sc wire.Schedule, n int, last time.Time, onTime bool) (subject, body string) {
	if onTime {
		return "[주기] " + sc.Label, sc.Prompt
	}
	return fmt.Sprintf("[주기] %s · 밀린 %d회", sc.Label, n), fmt.Sprintf("%s\n\n밀린 %d회, 마지막 %s", sc.Prompt, n, localTime(sc, last))
}

func expiryMail(sc wire.Schedule, missed int, missedLast *time.Time) (subject, body string) {
	body = fmt.Sprintf("주기 %d · %s 만료", sc.ID, sc.Label)
	if missed > 0 && missedLast != nil {
		body += fmt.Sprintf("\n밀린 %d회, 마지막 %s\n\n%s", missed, localTime(sc, *missedLast), sc.Prompt)
	}
	return "[주기 만료] " + sc.Label, body
}

func localTime(sc wire.Schedule, t time.Time) string {
	if loc, err := cron.Zone(sc.TZ); err == nil {
		t = t.In(loc)
	}
	return t.Format("2006-01-02 15:04 MST")
}

func stampNull(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(t.UTC())
}
