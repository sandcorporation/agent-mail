// Package jobs is the job ledger (#288, #289): commands a role handed to a
// detached runner on its own machine (`switchboard job run`). The server
// records them, their state and the end (exit code, a short output tail) and
// mails the owner role an end nobody waited for. Nothing here sends a
// command to a machine: the runner reports up, and the only thing that
// travels down is a stop request.
package jobs

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

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Service serves the job ledger over one database.
type Service struct {
	db   *sql.DB
	now  func() time.Time
	ids  *identity.Service
	wake func(mailbox string) // wakes the mailbox's waiters after an end mail
}

// New builds the service. wake is called with the owner's mailbox after an
// end mail was stored.
func New(db *sql.DB, now func() time.Time, ids *identity.Service, wake func(string)) *Service {
	return &Service{db: db, now: now, ids: ids, wake: wake}
}

var errNotFound = errors.New("not found")

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// API serves /api/jobs (rest is the path after it, without the slash):
//
//	POST ""          create (idempotent on the role and key)
//	GET  ""          list: the role's own jobs, every role's for a boss role
//	GET  "ID"        show (owner only)
//	POST "ID/report" the runner's state (owner only)
//	POST "ID/stop"   ask the runner to stop (owner only)
func (s *Service) API(w http.ResponseWriter, r *http.Request, p identity.Principal, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodPost:
			s.create(w, r, p)
		case http.MethodGet:
			s.list(w, r, p)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	idPart, action, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job ID", http.StatusBadRequest)
		return
	}
	j, err := s.Get(id)
	if err != nil || j.Role != p.Role {
		// Another role's job is as unknown as a missing one.
		if err != nil && !errors.Is(err, errNotFound) {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, j)
	case action == "report" && r.Method == http.MethodPost:
		s.report(w, r, j)
	case action == "stop" && r.Method == http.MethodPost:
		s.stop(w, j)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) create(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var q wire.JobCreate
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
		if j, ok := s.byIdem(p.Role, key); ok {
			writeJSON(w, http.StatusOK, j)
			return
		}
	}
	expires := wire.DefaultExpiry(q.Kind)
	if q.ExpiresIn > 0 {
		expires = time.Duration(q.ExpiresIn) * time.Second
	}
	now := s.now().UTC()
	res, err := s.db.Exec(`INSERT INTO jobs(role,machine,kind,label,command,cwd,state,created_at,expires_at,duration_s,idem) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		p.Role, q.Machine, q.Kind, q.Label, q.Command, q.Cwd, wire.JobHanded, stamp(now), stamp(now.Add(expires)), int64(expires/time.Second), nullable(key))
	if err != nil {
		// A concurrent retry with the same key won the insert.
		if j, ok := s.byIdem(p.Role, key); key != "" && ok {
			writeJSON(w, http.StatusOK, j)
			return
		}
		http.Error(w, "could not store job", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	j, err := s.Get(id)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	role := r.URL.Query().Get("role")
	if !s.ids.IsBoss(p) {
		if role != "" && role != p.Role {
			http.Error(w, "only boss roles list other roles' jobs", http.StatusForbidden)
			return
		}
		role = p.Role
	}
	out, err := s.query(role, r.URL.Query().Get("state"))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// StateLive is the list filter for every job not yet ended.
const StateLive = "live"

// query lists jobs newest first, of role and in state when given (StateLive:
// handed or running).
func (s *Service) query(role, state string) ([]wire.Job, error) {
	where, args := []string{}, []any{}
	if role != "" {
		where, args = append(where, "role=?"), append(args, role)
	}
	switch state {
	case "":
	case StateLive:
		where, args = append(where, "state IN (?,?)"), append(args, wire.JobHanded, wire.JobRunning)
	default:
		where, args = append(where, "state=?"), append(args, state)
	}
	q := `SELECT ` + columns + ` FROM jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.Job{}
	for rows.Next() {
		j, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// AdminAPI serves the dashboard's /api/admin/jobs (rest is the path after
// it, without the slash). The admin guard and the write guard ran already.
//
//	GET    ""        every role's jobs, ?role= and ?state= (live: not ended);
//	                 the command text is left out
//	POST   "ID/stop" ask the runner to stop
//	DELETE "ID"      remove an ended job's record
func (s *Service) AdminAPI(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out, err := s.query(r.URL.Query().Get("role"), r.URL.Query().Get("state"))
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		for i := range out {
			out[i].Command = ""
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	idPart, action, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid job ID", http.StatusBadRequest)
		return
	}
	j, err := s.Get(id)
	if err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "unknown job", http.StatusNotFound)
		} else {
			http.Error(w, "query failed", http.StatusInternalServerError)
		}
		return
	}
	switch {
	case action == "stop" && r.Method == http.MethodPost:
		s.stop(w, j)
	case action == "" && r.Method == http.MethodDelete:
		if !wire.JobEnded(j.State) {
			http.Error(w, "stop the job first", http.StatusConflict)
			return
		}
		if _, err := s.db.Exec(`DELETE FROM jobs WHERE id=?`, id); err != nil {
			http.Error(w, "could not remove job", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// report records the runner's state. running only moves a handed job; an end
// state is recorded once (a repeated end report changes nothing and sends no
// second mail). The end mail goes to the owner when no waiter saw the end.
func (s *Service) report(w http.ResponseWriter, r *http.Request, j wire.Job) {
	var q wire.JobReport
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&q) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := q.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := s.now().UTC()
	var mailed string
	err := store.InTx(r.Context(), s.db, func(tx *sql.Tx) error {
		mailed = ""
		if q.State == wire.JobRunning {
			_, err := tx.Exec(`UPDATE jobs SET state=?, started_at=? WHERE id=? AND state=?`, wire.JobRunning, stamp(now), j.ID, wire.JobHanded)
			return err
		}
		res, err := tx.Exec(`UPDATE jobs SET state=?, exit_code=?, error=?, tail=?, ended_at=?, started_at=COALESCE(started_at, ?) WHERE id=? AND state IN (?,?)`,
			q.State, q.ExitCode, q.Error, q.Tail, stamp(now), stamp(now), j.ID, wire.JobHanded, wire.JobRunning)
		if err != nil {
			return err
		}
		// An expiry is always told: the waiter saw only a killed command.
		if n, _ := res.RowsAffected(); n == 0 || (q.Watched && q.State != wire.JobExpired) {
			return nil
		}
		box, ok := identity.Mailbox(tx, j.Role)
		if !ok {
			return nil
		}
		subject, body, priority := endMail(j, q)
		stored, err := mail.InsertSystemMailOnce(tx, box, subject, body, "", priority, "job:"+strconv.FormatInt(j.ID, 10), now)
		if stored {
			mailed = box
		}
		return err
	})
	if err != nil {
		http.Error(w, "could not record state", http.StatusInternalServerError)
		return
	}
	if mailed != "" && s.wake != nil {
		s.wake(mailed)
	}
	s.writeJob(w, j.ID)
}

// endMail is the end notice: normal for exit 0 or a stop, urgent for a
// non-zero exit, a failed start or an expiry.
func endMail(j wire.Job, q wire.JobReport) (subject, body, priority string) {
	priority = "urgent"
	var what string
	switch q.State {
	case wire.JobExited:
		what = fmt.Sprintf("exit %d", *q.ExitCode)
		if *q.ExitCode == 0 {
			priority = "normal"
		}
	case wire.JobFailed:
		what = "시작 실패"
	case wire.JobStopped:
		what, priority = "중지됨", "normal"
	case wire.JobExpired:
		what = "만료"
	}
	subject = fmt.Sprintf("[작업 끝남] %s %s", j.Label, what)
	body = fmt.Sprintf("작업 %d · %s · %s\n%s", j.ID, j.Machine, j.Cwd, what)
	if q.Error != "" {
		body += "\n" + q.Error
	}
	if q.Tail != "" {
		body += "\n\n" + q.Tail
	}
	return subject, body, priority
}

func (s *Service) stop(w http.ResponseWriter, j wire.Job) {
	if !wire.JobEnded(j.State) {
		if _, err := s.db.Exec(`UPDATE jobs SET stop_requested_at=COALESCE(stop_requested_at, ?) WHERE id=?`, stamp(s.now().UTC()), j.ID); err != nil {
			http.Error(w, "could not record stop", http.StatusInternalServerError)
			return
		}
	}
	s.writeJob(w, j.ID)
}

func (s *Service) writeJob(w http.ResponseWriter, id int64) {
	j, err := s.Get(id)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

const columns = `id,role,machine,kind,label,command,cwd,state,exit_code,error,created_at,started_at,ended_at,expires_at,stop_requested_at,tail`

type scanner interface{ Scan(...any) error }

func scan(r scanner) (wire.Job, error) {
	var j wire.Job
	var code sql.NullInt64
	var created, expires string
	var started, ended, stop sql.NullString
	err := r.Scan(&j.ID, &j.Role, &j.Machine, &j.Kind, &j.Label, &j.Command, &j.Cwd, &j.State, &code, &j.Error,
		&created, &started, &ended, &expires, &stop, &j.Tail)
	if err != nil {
		return j, err
	}
	if code.Valid {
		c := int(code.Int64)
		j.ExitCode = &c
	}
	j.CreatedAt, j.ExpiresAt = parse(created), parse(expires)
	j.StartedAt, j.EndedAt = parseNull(started), parseNull(ended)
	j.StopRequested = stop.Valid
	return j, nil
}

// Get returns one job.
func (s *Service) Get(id int64) (wire.Job, error) {
	j, err := scan(s.db.QueryRowContext(context.Background(), `SELECT `+columns+` FROM jobs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return j, errNotFound
	}
	return j, err
}

func (s *Service) byIdem(role, key string) (wire.Job, bool) {
	j, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM jobs WHERE role=? AND idem=?`, role, key))
	return j, err == nil
}

func stamp(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parse(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }

func parseNull(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t := parse(v.String)
	return &t
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ExtendAttached moves the expiry of every live job whose kind auto-extends
// (wire.AutoExtends) to now plus the duration it was created with, when a
// session of its owner role is attached. base maps a role to its base role;
// attached reports a live session of a base. It returns how many moved.
func (s *Service) ExtendAttached(ctx context.Context, base func(string) string, attached func(context.Context, string) (bool, error)) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,role,kind,duration_s FROM jobs WHERE state IN (?,?) AND duration_s>0`, wire.JobHanded, wire.JobRunning)
	if err != nil {
		return 0, err
	}
	type live struct {
		id       int64
		role     string
		duration int64
	}
	var due []live
	for rows.Next() {
		var x live
		var kind string
		if err = rows.Scan(&x.id, &x.role, &kind, &x.duration); err != nil {
			rows.Close()
			return 0, err
		}
		if wire.AutoExtends(kind) {
			due = append(due, x)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	now := s.now().UTC()
	seen := map[string]bool{}
	moved := 0
	for _, x := range due {
		b := base(x.role)
		on, ok := seen[b]
		if !ok {
			if on, err = attached(ctx, b); err != nil {
				return moved, err
			}
			seen[b] = on
		}
		if !on {
			continue
		}
		res, err := s.db.ExecContext(ctx, `UPDATE jobs SET expires_at=? WHERE id=? AND state IN (?,?)`,
			stamp(now.Add(time.Duration(x.duration)*time.Second)), x.id, wire.JobHanded, wire.JobRunning)
		if err != nil {
			return moved, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			moved++
		}
	}
	return moved, nil
}
