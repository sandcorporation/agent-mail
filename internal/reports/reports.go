// Package reports holds a boss role's reports (#262): a title, an optional
// request line and a body, linking the directive threads the work went
// through. A report's status is derived from those threads under the
// answered rule (internal/mail), never stored.
package reports

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Service serves reports over one database.
type Service struct {
	db   *sql.DB
	now  func() time.Time
	ids  *identity.Service
	base func(string) string // mailbox name → base role
	rule mail.AnsweredRule
}

// New builds the service. base maps a mailbox name to its base role (the
// answered rule compares bases).
func New(db *sql.DB, now func() time.Time, ids *identity.Service, base func(string) string) *Service {
	return &Service{db: db, now: now, ids: ids, base: base, rule: mail.CurrentAnsweredRule}
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

// API serves POST /api/reports for a boss role (403 otherwise). A retry
// with the same idempotency key (header Idempotency-Key, else the body's
// idempotency_key) returns the first report with 200.
func (s *Service) API(w http.ResponseWriter, r *http.Request, p identity.Principal) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.ids.IsBoss(p) {
		http.Error(w, "reports are for boss roles (SWITCHBOARD_BOSS_ROLES or a group's boss role)", http.StatusForbidden)
		return
	}
	var q wire.ReportRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&q) != nil {
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
		if id, ok := s.byIdem(p.Role, key); ok {
			s.writeReport(w, http.StatusOK, id)
			return
		}
	}
	roots, bad, err := s.resolve(p, q.Mail)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if len(bad) > 0 {
		http.Error(w, "unknown mail ID(s): "+joinIDs(bad), http.StatusBadRequest)
		return
	}
	var id int64
	err = store.InTx(r.Context(), s.db, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO reports(author,title,request,body,created_at,idem) VALUES(?,?,?,?,?,?)`,
			p.Role, q.Title, q.Request, q.Body, s.now().UTC().Format(time.RFC3339Nano), nullable(key))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		for _, m := range roots {
			if _, err = tx.Exec(`INSERT OR IGNORE INTO report_mail(report_id,message_id) VALUES(?,?)`, id, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// A concurrent retry with the same key won the insert.
		if existing, ok := s.byIdem(p.Role, key); key != "" && ok {
			s.writeReport(w, http.StatusOK, existing)
			return
		}
		http.Error(w, "could not store report", http.StatusInternalServerError)
		return
	}
	s.writeReport(w, http.StatusCreated, id)
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func (s *Service) byIdem(author, key string) (int64, bool) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM reports WHERE author=? AND idem=?`, author, key).Scan(&id)
	return id, err == nil
}

func (s *Service) writeReport(w http.ResponseWriter, code int, id int64) {
	d, err := s.Detail(id)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(code)
	writeJSON(w, d.Report)
}

// resolve maps each mail ID to its thread's root, in the order given and
// without repeats. An ID that does not exist or whose sender and recipient
// are both outside p's view is bad.
func (s *Service) resolve(p identity.Principal, ids []int64) (roots, bad []int64, err error) {
	seen := map[int64]bool{}
	for _, id := range ids {
		var sender, recipient string
		var thread int64
		err = s.db.QueryRow(`SELECT sender,recipient,COALESCE(thread_id,0) FROM messages WHERE id=?`, id).Scan(&sender, &recipient, &thread)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !s.ids.MaySee(p, identity.BaseOf(s.db, sender)) && !s.ids.MaySee(p, identity.BaseOf(s.db, recipient)) {
			bad, err = append(bad, id), nil
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		root := id
		if thread != 0 {
			root = thread
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots, bad, nil
}

// AdminAPI serves GET /api/admin/reports (rest "") and
// GET /api/admin/reports/{id}.
func (s *Service) AdminAPI(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rest == "" {
		list, err := s.List()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, list)
		return
	}
	id, err := wire.ParseID(rest)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := s.Detail(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, d)
}

// List is every report, newest first, without bodies.
func (s *Service) List() (wire.ReportList, error) {
	out := wire.ReportList{Reports: []wire.Report{}}
	rows, err := s.db.Query(`SELECT id,author,title,request,created_at FROM reports ORDER BY id DESC`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var rp wire.Report
		var created string
		if err = rows.Scan(&rp.ID, &rp.Author, &rp.Title, &rp.Request, &created); err != nil {
			rows.Close()
			return out, err
		}
		rp.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out.Reports = append(out.Reports, rp)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	links, err := s.links(0)
	if err != nil {
		return out, err
	}
	var all []int64
	for _, ids := range links {
		all = append(all, ids...)
	}
	threads, err := s.threads(all, false)
	if err != nil {
		return out, err
	}
	for i := range out.Reports {
		rp := &out.Reports[i]
		rp.Mail = nonNil(links[rp.ID])
		summarize(rp, threads)
	}
	return out, nil
}

// Detail is one report with its body and linked threads; sql.ErrNoRows when
// there is no such report.
func (s *Service) Detail(id int64) (wire.ReportDetail, error) {
	var d wire.ReportDetail
	var created string
	err := s.db.QueryRow(`SELECT id,author,title,request,body,created_at FROM reports WHERE id=?`, id).Scan(&d.ID, &d.Author, &d.Title, &d.Request, &d.Body, &created)
	if err != nil {
		return d, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	links, err := s.links(id)
	if err != nil {
		return d, err
	}
	d.Mail = nonNil(links[id])
	threads, err := s.threads(d.Mail, true)
	if err != nil {
		return d, err
	}
	summarize(&d.Report, threads)
	d.Threads = []wire.ReportThread{}
	for _, m := range d.Mail {
		if t, ok := threads[m]; ok {
			d.Threads = append(d.Threads, t)
		}
	}
	return d, nil
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// summarize sets the report's status and latest reply time from its threads.
// A report with no (remaining) linked thread is done.
func summarize(rp *wire.Report, threads map[int64]wire.ReportThread) {
	rp.Status = wire.ReportDone
	for _, m := range rp.Mail {
		t, ok := threads[m]
		if !ok {
			continue
		}
		if t.Open {
			rp.Status = wire.ReportOpen
		}
		if n := len(t.Replies); n > 0 {
			at := t.Replies[n-1].CreatedAt
			if rp.LastReplyAt == nil || at.After(*rp.LastReplyAt) {
				rp.LastReplyAt = &at
			}
		}
	}
}

// links reads report_mail for one report, or for all when id is 0, in the
// order the report named them.
func (s *Service) links(id int64) (map[int64][]int64, error) {
	q, args := `SELECT report_id,message_id FROM report_mail ORDER BY report_id, rowid`, []any{}
	if id != 0 {
		q, args = `SELECT report_id,message_id FROM report_mail WHERE report_id=? ORDER BY rowid`, []any{id}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var rid, mid int64
		if err = rows.Scan(&rid, &mid); err != nil {
			return nil, err
		}
		out[rid] = append(out[rid], mid)
	}
	return out, rows.Err()
}

// threadMail is a message as a linked thread reads it.
type threadMail struct {
	mail.FlowMail
	Status  string
	NoReply bool
}

const threadCols = `m.id,m.sender,m.recipient,m.subject,m.body,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0),COALESCE(m.reply_to,0),m.directive,m.no_reply,`

func (s *Service) queryMail(where string, args ...any) ([]threadMail, error) {
	rows, err := s.db.Query(`SELECT `+threadCols+mail.StatusExpr+` FROM messages m `+mail.StatusJoin+` WHERE `+where+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []threadMail
	for rows.Next() {
		var m threadMail
		var created string
		if err = rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &created, &m.TaskID, &m.ThreadID, &m.ReplyTo, &m.Directive, &m.NoReply, &m.Status); err != nil {
			return nil, err
		}
		m.Created, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func placeholders(n int) string { return "?" + strings.Repeat(",?", n-1) }

func idArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func toReportMail(m threadMail, withBody bool) wire.ReportMail {
	rm := wire.ReportMail{ID: m.ID, Sender: m.Sender, Recipient: m.Recipient, Subject: m.Subject, CreatedAt: m.Created, Status: m.Status}
	if withBody {
		rm.Body = m.Body
	}
	return rm
}

// threads builds the linked threads rooted at roots, keyed by root ID. A
// root that no longer exists is left out. The directive is open unless the
// answered rule (mail.CurrentAnsweredRule) finds a reply to it, its
// recipient completed it, or it was closed (no_reply). Bodies are read only
// for the detail.
func (s *Service) threads(roots []int64, withBody bool) (map[int64]wire.ReportThread, error) {
	out := map[int64]wire.ReportThread{}
	set := map[int64]bool{}
	var ids []int64
	for _, id := range roots {
		if !set[id] {
			set[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	const chunk = 400
	for i := 0; i < len(ids); i += chunk {
		part := idArgs(ids[i:min(i+chunk, len(ids))])
		ms, err := s.queryMail(`m.id IN (`+placeholders(len(part))+`) OR m.thread_id IN (`+placeholders(len(part))+`)`, append(part, part...)...)
		if err != nil {
			return nil, err
		}
		byThread := map[int64][]threadMail{}
		for _, m := range ms {
			byThread[m.Thread()] = append(byThread[m.Thread()], m)
		}
		for _, p := range part {
			id := p.(int64)
			all := byThread[id]
			if len(all) == 0 || all[0].ID != id {
				continue // the root is gone
			}
			d := all[0]
			t := wire.ReportThread{Directive: toReportMail(d, withBody), Replies: []wire.ReportMail{}}
			for _, m := range all[1:] {
				rm := toReportMail(m, withBody)
				if s.rule.Replies(d.FlowMail, m.FlowMail, s.base) {
					rm.Answers, t.Answered = true, true
				}
				t.Replies = append(t.Replies, rm)
			}
			t.Completed = d.Status == "complete" || d.Status == "completed"
			t.Closed = d.NoReply
			t.Open = !t.Answered && !t.Completed && !t.Closed
			out[id] = t
		}
	}
	return out, nil
}
