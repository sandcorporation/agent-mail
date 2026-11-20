package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// A job is lost when its runner is gone without reporting an end: the
// machine rebooted, or the runner was killed. Two things tell: the machine
// reports in without the job (machine), or the runner has not reported for
// wire.JobSilentAfter (SweepLost). Either way the owner gets one urgent
// mail. A runner that reports again takes the job back (report).

// liveJob is what a lost check reads of a live job.
type liveJob struct {
	job  wire.Job
	seen time.Time
}

// liveJobs returns the live (handed or running) jobs matching where.
func (s *Service) liveJobs(ctx context.Context, where string, args ...any) ([]liveJob, error) {
	q := `SELECT ` + columns + `,COALESCE(seen_at, created_at) FROM jobs WHERE state IN (?,?)`
	if where != "" {
		q += " AND " + where
	}
	rows, err := s.db.QueryContext(ctx, q, append([]any{wire.JobHanded, wire.JobRunning}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []liveJob
	for rows.Next() {
		var x liveJob
		var seen string
		if x.job, err = scan(withSeen{rows, &seen}); err != nil {
			return nil, err
		}
		x.seen = parse(seen)
		out = append(out, x)
	}
	return out, rows.Err()
}

// withSeen scans the columns and then the seen time.
type withSeen struct {
	r    scanner
	seen *string
}

func (w withSeen) Scan(dst ...any) error { return w.r.Scan(append(dst, w.seen)...) }

// machine records a machine reporting in: the role's live jobs there that
// are not in q.Live and older than wire.JobLostGrace become lost.
func (s *Service) machine(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	var q wire.JobMachine
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&q) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := q.Check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	live := make(map[int64]bool, len(q.Live))
	for _, id := range q.Live {
		live[id] = true
	}
	jobs, err := s.liveJobs(r.Context(), "role=? AND machine=?", p.Role, q.Machine)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	now := s.now().UTC()
	n := 0
	for _, x := range jobs {
		if live[x.job.ID] || now.Sub(x.job.CreatedAt) < wire.JobLostGrace {
			continue
		}
		ok, err := s.markLost(r.Context(), x.job, "its machine reported in without it (restarted, or the runner was killed)")
		if err != nil {
			http.Error(w, "could not record state", http.StatusInternalServerError)
			return
		}
		if ok {
			n++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"lost": n})
}

// SweepLost marks lost every live job whose runner has not reported for
// wire.JobSilentAfter. It returns how many it marked.
func (s *Service) SweepLost(ctx context.Context) (int, error) {
	jobs, err := s.liveJobs(ctx, "")
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	n := 0
	for _, x := range jobs {
		if now.Sub(x.seen) < wire.JobSilentAfter {
			continue
		}
		ok, err := s.markLost(ctx, x.job, fmt.Sprintf("no report from its runner for %s", wire.JobSilentAfter))
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// markLost ends a live job as lost with one urgent mail to its owner
// (idempotent on the job, apart from the end mail). ok is false when the job
// was no longer live.
func (s *Service) markLost(ctx context.Context, j wire.Job, why string) (bool, error) {
	now := s.now().UTC()
	var mailed string
	var ok bool
	err := store.InTx(ctx, s.db, func(tx *sql.Tx) error {
		mailed, ok = "", false
		res, err := tx.Exec(`UPDATE jobs SET state=?, error=?, ended_at=? WHERE id=? AND state IN (?,?)`,
			wire.JobLost, why, stamp(now), j.ID, wire.JobHanded, wire.JobRunning)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		box, found := identity.Mailbox(tx, j.Role)
		if !found {
			return nil
		}
		subject, body, priority := endMail(j, wire.JobReport{State: wire.JobLost, Error: why, Tail: j.Tail})
		stored, err := mail.InsertSystemMailOnce(tx, box, subject, body, "", priority, "job-lost:"+strconv.FormatInt(j.ID, 10), now)
		if stored {
			mailed = box
		}
		return err
	})
	if err == nil && mailed != "" && s.wake != nil {
		s.wake(mailed)
	}
	return ok, err
}
