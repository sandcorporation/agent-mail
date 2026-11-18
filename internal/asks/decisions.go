package asks

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Decision ledger.
//
// A decision is an answered question, seen as what was chosen: GET
// /api/admin/decisions (the dashboard) and GET /api/decisions (a boss sees
// everything, other roles their own base) list them newest answer first,
// filtered by answer time, asker and text, as JSON or as an ndjson download.
// A new question also gets up to three related past decisions, in the 201
// response and in the watchers' create mail.

const (
	maxDecisions   = 1000
	maxRelated     = 3
	decisionDate   = "2006-01-02"
	ndjsonMimeType = "application/x-ndjson"
)

// asDecision turns an answered question into a decision.
func asDecision(v wire.QuestionView) wire.Decision {
	d := wire.Decision{ID: v.ID, Asker: v.Asker, AskedAt: v.CreatedAt, AnsweredBy: v.AnsweredBy, Items: make([]wire.DecisionItem, len(v.Questions))}
	if v.AnsweredAt != nil {
		d.AnsweredAt = *v.AnsweredAt
	}
	for i, q := range v.Questions {
		it := wire.DecisionItem{Header: q.Header, Question: q.Question, Chosen: []string{}}
		if i < len(v.Answers) {
			if v.Answers[i].Selected != nil {
				it.Chosen = v.Answers[i].Selected
			}
			it.Other = v.Answers[i].Other
		}
		d.Items[i] = it
	}
	return d
}

// decisionFilter is a parsed ?from=&to=&role=&q=&format= query.
type decisionFilter struct {
	where    string // " AND …" clauses on the questions table
	args     []any
	needle   string // lower-cased q, "" for none
	ndjson   bool
	fromName string // file name parts of the export
	toName   string
}

// parseDecisionTime reads a date (UTC day) or an RFC 3339 time. For a date,
// end moves it to the start of the next day so that "to" covers the day.
func parseDecisionTime(raw string, end bool) (t time.Time, isDate bool, err error) {
	if d, e := time.Parse(decisionDate, raw); e == nil {
		if end {
			d = d.AddDate(0, 0, 1)
		}
		return d, true, nil
	}
	t, err = time.Parse(time.RFC3339, raw)
	return t, false, err
}

func readDecisionFilter(r *http.Request, now time.Time) (decisionFilter, error) {
	query := r.URL.Query()
	f := decisionFilter{fromName: "start", toName: now.UTC().Format(decisionDate)}
	if raw := strings.TrimSpace(query.Get("from")); raw != "" {
		t, _, err := parseDecisionTime(raw, false)
		if err != nil {
			return f, badQuestion("from must be a date (YYYY-MM-DD) or an RFC 3339 time")
		}
		f.where += ` AND answered_at>=?`
		f.args = append(f.args, ms(t))
		f.fromName = t.UTC().Format(decisionDate)
	}
	if raw := strings.TrimSpace(query.Get("to")); raw != "" {
		t, isDate, err := parseDecisionTime(raw, true)
		if err != nil {
			return f, badQuestion("to must be a date (YYYY-MM-DD) or an RFC 3339 time")
		}
		if isDate {
			f.where += ` AND answered_at<?`
			f.toName = raw
		} else {
			f.where += ` AND answered_at<=?`
			f.toName = t.UTC().Format(decisionDate)
		}
		f.args = append(f.args, ms(t))
	}
	if role := strings.TrimSpace(query.Get("role")); role != "" {
		if p, ok := strings.CutSuffix(role, "/*"); ok {
			if !wire.ValidRole(p) {
				return f, badQuestion("invalid role")
			}
			f.where += ` AND substr(asker_base,1,?)=?`
			f.args = append(f.args, len(p)+1, p+"/")
		} else {
			if !wire.ValidRole(role) {
				return f, badQuestion("invalid role")
			}
			f.where += ` AND (asker=? OR asker_base=?)`
			f.args = append(f.args, role, role)
		}
	}
	f.needle = strings.ToLower(strings.TrimSpace(query.Get("q")))
	switch query.Get("format") {
	case "", "json":
	case "ndjson":
		f.ndjson = true
	default:
		return f, badQuestion("format must be json or ndjson")
	}
	return f, nil
}

// queryDecisions runs base (a SELECT of questionColumns ending in a WHERE)
// with the filter, newest answer first, at most maxDecisions.
func (s *Service) queryDecisions(base string, args []any, f decisionFilter) ([]wire.Decision, error) {
	q := base + ` AND status='answered'` + f.where + ` ORDER BY answered_at DESC, seq DESC`
	args = append(append([]any{}, args...), f.args...)
	if f.needle == "" {
		q += ` LIMIT ?`
		args = append(args, maxDecisions)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.Decision{}
	for rows.Next() && len(out) < maxDecisions {
		v, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		if d := asDecision(v); f.needle == "" || decisionMatches(d, f.needle) {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// DecisionsAPI serves GET /api/decisions (p set) and GET
// /api/admin/decisions (p nil: everything).
func (s *Service) DecisionsAPI(w http.ResponseWriter, r *http.Request, p *identity.Principal) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f, err := readDecisionFilter(r, s.now())
	if err != nil {
		questionError(w, err)
		return
	}
	base, args := `SELECT `+questionColumns+` FROM questions WHERE 1=1`, []any(nil)
	if p != nil {
		base, args = s.visibleQuestions(*p)
	}
	out, err := s.queryDecisions(base, args, f)
	if err != nil {
		http.Error(w, "could not list decisions", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !f.ndjson {
		writeJSON(w, wire.DecisionList{Decisions: out})
		return
	}
	w.Header().Set("Content-Type", ndjsonMimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="decisions-%s-%s.ndjson"`, f.fromName, f.toName))
	for _, d := range out {
		writeJSON(w, d) // json.Encoder ends each value with a newline
	}
}

// rowsQueryer is what relatedDecisions reads through: *sql.DB or *sql.Tx.
type rowsQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryDecisionRows(q rowsQueryer, where string, args ...any) ([]wire.Decision, error) {
	rows, err := q.Query(`SELECT `+questionColumns+` FROM questions WHERE status='answered' AND `+where+` ORDER BY answered_at DESC, seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wire.Decision
	for rows.Next() {
		v, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, asDecision(v))
	}
	return out, rows.Err()
}

// relatedDecisions picks up to maxRelated past decisions for a new question
// of askerBase: the base's own, newest first; then, when the asker's
// settings came from a "<prefix>/*" target, other bases under that prefix;
// then any whose first header equals header (ASCII case-insensitive). A
// non-empty group keeps the last two to askers of that group (internal/identity/groups.go).
func relatedDecisions(q rowsQueryer, askerBase, target, header, group string) ([]wire.Decision, error) {
	scope, scopeArgs := "", []any(nil)
	if group != "" {
		scope, scopeArgs = ` AND substr(asker_base,1,?)=?`, []any{len(group) + 1, group + "/"}
	}
	out, err := queryDecisionRows(q, `asker_base=?`, askerBase, maxRelated)
	if err != nil {
		return nil, err
	}
	if p, ok := strings.CutSuffix(target, "/*"); ok && len(out) < maxRelated {
		more, err := queryDecisionRows(q, `substr(asker_base,1,?)=? AND asker_base<>?`+scope, append(append([]any{len(p) + 1, p + "/", askerBase}, scopeArgs...), maxRelated-len(out))...)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	if len(out) < maxRelated {
		where, args := `json_extract(questions,'$[0].header')=? COLLATE NOCASE`+scope, append([]any{header}, scopeArgs...)
		for _, d := range out {
			where += ` AND id<>?`
			args = append(args, d.ID)
		}
		more, err := queryDecisionRows(q, where, append(args, maxRelated-len(out))...)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	if out == nil {
		out = []wire.Decision{}
	}
	return out, nil
}

// relatedText is the 「관련 지난 결정」 block of the watcher create mail, ""
// when there are none: one line per decision, header → chosen.
func relatedText(ds []wire.Decision) string {
	if len(ds) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n관련 지난 결정:")
	for _, d := range ds {
		parts := make([]string, len(d.Items))
		for i, it := range d.Items {
			chosen := strings.Join(it.Chosen, ", ")
			if it.Other != "" {
				if chosen != "" {
					chosen += " / "
				}
				chosen += "기타: " + it.Other
			}
			parts[i] = it.Header + " → " + chosen
		}
		fmt.Fprintf(&b, "\n- %s %s: %s", d.ID, d.AnsweredAt.Format(decisionDate), strings.Join(parts, "; "))
	}
	return b.String()
}

// decisionMatches reports whether needle (already lower-cased) is in a header, a
// question or a chosen label.
func decisionMatches(d wire.Decision, needle string) bool {
	for _, it := range d.Items {
		if strings.Contains(strings.ToLower(it.Header), needle) || strings.Contains(strings.ToLower(it.Question), needle) {
			return true
		}
		for _, c := range it.Chosen {
			if strings.Contains(strings.ToLower(c), needle) {
				return true
			}
		}
	}
	return false
}
