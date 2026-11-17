package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Questions.
//
// A session whose resolved question settings allow it (can_ask) asks one to
// four multiple-choice questions (POST /api/questions). Like permission
// requests, only a boss role (AGENT_MAIL_BOSS_ROLES) of a base other than the
// asker's may answer, and the first answer wins. The asker's base may
// withdraw a pending question. The dashboard answers through the same core
// (answerQuestion with by="dashboard"), which skips the boss check.
//
// Every create, answer and withdraw bumps a global change counter (seq);
// boss sessions long-poll GET /api/questions/poll?after=<seq> to fill their
// panel and to close items answered or withdrawn elsewhere, so bosses get no
// mail on create. The asker gets normal-priority mail with the answer;
// watchers from the asker's resolved settings get one mail on create and one
// on answer. Nobody is mailed on withdraw.

const (
	questionIDPrefix    = "q_"
	questionByDashboard = "dashboard" // answered_by for answers given in the dashboard
	maxQuestionBody     = 32 << 10    // bytes, for the whole request body
	maxQuestionItems    = 4
	minQuestionOptions  = 2
	maxQuestionOptions  = 4
	maxQuestionHeader   = 12   // runes
	maxQuestionText     = 1000 // bytes
	maxQuestionLabel    = 100  // bytes
	maxQuestionDesc     = 500  // bytes
	maxQuestionPreview  = 4000 // bytes
	maxQuestionOther    = 1000 // bytes
	defaultQuestionList = 100
	maxQuestionList     = 500
	// questionHubKey is the mailHub topic woken on every question change. It
	// is not a valid role name, so no mailbox shares it.
	questionHubKey = "#questions"
)

type questionPoll struct {
	Questions []questionView `json:"questions"`
	Cursor    int64          `json:"cursor"`
}

// questionInputError is a 400: the body does not have the required shape.
type questionInputError struct{ msg string }

func (e questionInputError) Error() string { return e.msg }

func badQuestion(format string, a ...any) error { return questionInputError{fmt.Sprintf(format, a...)} }

var (
	errQuestionNotFound   = errors.New("unknown question")
	errQuestionForbidden  = errors.New("not allowed to act on this question")
	errQuestionNotPending = errors.New("question is not pending")
)

const questionColumns = `id,asker,asker_base,questions,answers,status,answered_by,created_at,answered_at,withdrawn_at,seq`

func scanQuestion(sc interface{ Scan(...any) error }) (questionView, error) {
	var v questionView
	var qs, as string
	var created, answered, withdrawn int64
	if err := sc.Scan(&v.ID, &v.Asker, &v.AskerBase, &qs, &as, &v.Status, &v.AnsweredBy, &created, &answered, &withdrawn, &v.Seq); err != nil {
		return v, err
	}
	if err := json.Unmarshal([]byte(qs), &v.Questions); err != nil {
		return v, err
	}
	if as != "" {
		if err := json.Unmarshal([]byte(as), &v.Answers); err != nil {
			return v, err
		}
	}
	v.CreatedAt = fromMS(created)
	if answered > 0 {
		t := fromMS(answered)
		v.AnsweredAt = &t
	}
	if withdrawn > 0 {
		t := fromMS(withdrawn)
		v.WithdrawnAt = &t
	}
	return v, nil
}

func loadQuestion(q queryer, id string) (questionView, error) {
	if !strings.HasPrefix(id, questionIDPrefix) || len(id) > 64 {
		return questionView{}, errQuestionNotFound
	}
	v, err := scanQuestion(q.QueryRow(`SELECT `+questionColumns+` FROM questions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, errQuestionNotFound
	}
	return v, err
}

// mayAnswerQuestion: the asker's boss (bossesFor), never its own base.
func (s *Server) mayAnswerQuestion(p principal, v questionView) bool {
	return p.Base != v.AskerBase && s.isBossFor(p, v.AskerBase)
}

// mayReadQuestion: the asker's base or the asker's boss.
func (s *Server) mayReadQuestion(p principal, v questionView) bool {
	return p.Base == v.AskerBase || s.isBossFor(p, v.AskerBase)
}

func (s *Server) forCaller(p principal, v questionView) questionView {
	v.CanAnswer = v.Status == "pending" && s.mayAnswerQuestion(p, v)
	return v
}

// questionAPI serves /api/questions, /api/questions/poll and
// /api/questions/{id}[/answer|/withdraw].
func (s *Server) questionAPI(w http.ResponseWriter, r *http.Request, p principal, rest string) {
	parts := strings.Split(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		s.askQuestion(w, r, p)
	case rest == "" && r.Method == http.MethodGet:
		s.listQuestions(w, r, p)
	case rest == "poll" && r.Method == http.MethodGet:
		s.pollQuestions(w, r, p)
	case len(parts) == 1 && rest != "poll" && r.Method == http.MethodGet:
		v, err := loadQuestion(s.db, parts[0])
		if err == nil && !s.mayReadQuestion(p, v) {
			err = errQuestionForbidden
		}
		if err != nil {
			questionError(w, err)
			return
		}
		writeJSON(w, s.forCaller(p, v))
	case len(parts) == 2 && parts[1] == "answer" && r.Method == http.MethodPost:
		s.answerQuestionAPI(w, r, p, parts[0])
	case len(parts) == 2 && parts[1] == "withdraw" && r.Method == http.MethodPost:
		s.withdrawQuestion(w, r, p, parts[0])
	case rest == "" || len(parts) == 1 || (len(parts) == 2 && (parts[1] == "answer" || parts[1] == "withdraw")):
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// questionError writes the HTTP error for an error from the question code.
func questionError(w http.ResponseWriter, err error) {
	var in questionInputError
	switch {
	case errors.As(err, &in):
		http.Error(w, in.msg, http.StatusBadRequest)
	case errors.Is(err, errQuestionNotFound):
		http.Error(w, errQuestionNotFound.Error(), http.StatusNotFound)
	case errors.Is(err, errQuestionForbidden):
		http.Error(w, errQuestionForbidden.Error(), http.StatusForbidden)
	case errors.Is(err, errQuestionNotPending):
		http.Error(w, errQuestionNotPending.Error(), http.StatusConflict)
	default:
		http.Error(w, "question store failed", http.StatusInternalServerError)
	}
}

func newQuestionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return questionIDPrefix + hex.EncodeToString(b), nil
}

// validateQuestions checks the shape and size limits of a question list.
func validateQuestions(qs []questionItem) error {
	if len(qs) < 1 || len(qs) > maxQuestionItems {
		return badQuestion("questions: 1-%d items required", maxQuestionItems)
	}
	for i, q := range qs {
		n := i + 1
		if q.Header == "" || utf8.RuneCountInString(q.Header) > maxQuestionHeader {
			return badQuestion("question %d: header must be 1-%d characters", n, maxQuestionHeader)
		}
		if q.Question == "" || len(q.Question) > maxQuestionText {
			return badQuestion("question %d: question must be 1-%d bytes", n, maxQuestionText)
		}
		if len(q.Options) < minQuestionOptions || len(q.Options) > maxQuestionOptions {
			return badQuestion("question %d: %d-%d options required", n, minQuestionOptions, maxQuestionOptions)
		}
		seen := map[string]bool{}
		for _, o := range q.Options {
			switch {
			case o.Label == "" || len(o.Label) > maxQuestionLabel:
				return badQuestion("question %d: option label must be 1-%d bytes", n, maxQuestionLabel)
			case seen[o.Label]:
				return badQuestion("question %d: duplicate option label %q", n, o.Label)
			case len(o.Description) > maxQuestionDesc:
				return badQuestion("question %d: option description must be at most %d bytes", n, maxQuestionDesc)
			case len(o.Preview) > maxQuestionPreview:
				return badQuestion("question %d: option preview must be at most %d bytes", n, maxQuestionPreview)
			}
			seen[o.Label] = true
		}
	}
	return nil
}

// validateAnswers checks answers against the questions they answer and
// returns them with every selected list non-nil.
func validateAnswers(qs []questionItem, as []questionAnswer) ([]questionAnswer, error) {
	if len(as) != len(qs) {
		return nil, badQuestion("answers: exactly %d entries required, one per question", len(qs))
	}
	out := make([]questionAnswer, len(as))
	for i, a := range as {
		n, q := i+1, qs[i]
		if len(a.Other) > maxQuestionOther {
			return nil, badQuestion("answer %d: other must be at most %d bytes", n, maxQuestionOther)
		}
		if len(a.Selected) == 0 && a.Other == "" {
			return nil, badQuestion("answer %d: select a label or give other", n)
		}
		if !q.MultiSelect && len(a.Selected) > 1 {
			return nil, badQuestion("answer %d: question takes at most one label", n)
		}
		seen := map[string]bool{}
		for _, l := range a.Selected {
			if seen[l] {
				return nil, badQuestion("answer %d: label %q selected twice", n, l)
			}
			seen[l] = true
			if !slices.ContainsFunc(q.Options, func(o questionOption) bool { return o.Label == l }) {
				return nil, badQuestion("answer %d: unknown label %q", n, l)
			}
		}
		out[i] = questionAnswer{Selected: append([]string{}, a.Selected...), Other: a.Other}
	}
	return out, nil
}

// nextQuestionSeq is the next value of the global change counter. Writers
// are serialised by the single database connection; the unique index on seq
// backs that up.
func nextQuestionSeq(tx *sql.Tx) (int64, error) {
	var seq int64
	err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM questions`).Scan(&seq)
	return seq, err
}

// questionWatchers are the mailboxes of the asker's resolved watchers,
// without the asker's own base and roles that do not resolve.
func questionWatchers(tx *sql.Tx, asker, askerBase string) ([]string, error) {
	settings, err := resolveQuestionSettingsIn(tx, asker)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, role := range settings.Watchers {
		b := baseOf(tx, role)
		if b == "" || b == askerBase {
			continue
		}
		if box, ok := mailbox(tx, role); ok && !slices.Contains(out, box) {
			out = append(out, box)
		}
	}
	return out, nil
}

func (s *Server) askQuestion(w http.ResponseWriter, r *http.Request, p principal) {
	settings, err := s.resolveQuestionSettings(p.Role)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "could not read question settings", 500)
		return
	}
	if !settings.CanAsk {
		http.Error(w, "this role may not ask questions (can_ask is off)", http.StatusForbidden)
		return
	}
	var req struct {
		Questions []questionItem `json:"questions"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQuestionBody)).Decode(&req) != nil {
		http.Error(w, fmt.Sprintf("invalid JSON (at most %d bytes)", maxQuestionBody), 400)
		return
	}
	if err = validateQuestions(req.Questions); err != nil {
		questionError(w, err)
		return
	}
	id, err := newQuestionID()
	if err != nil {
		http.Error(w, "could not create question", 500)
		return
	}
	encoded, _ := json.Marshal(req.Questions)
	now := s.now()
	var v questionView
	var told []string
	var related []decision
	errNobody := errors.New("nobody")
	err = s.inTx(r.Context(), func(tx *sql.Tx) error {
		nobody := true
		for _, boss := range s.bossesFor(p.Base) {
			if b := baseOf(tx, boss); b != "" && b != p.Base {
				nobody = false
				break
			}
		}
		if nobody {
			return errNobody
		}
		seq, e := nextQuestionSeq(tx)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO questions(id,asker,asker_base,questions,created_at,seq) VALUES(?,?,?,?,?,?)`,
			id, p.Role, p.Base, string(encoded), ms(now), seq); e != nil {
			return e
		}
		if v, e = loadQuestion(tx, id); e != nil {
			return e
		}
		if told, e = questionWatchers(tx, p.Role, p.Base); e != nil {
			return e
		}
		if related, e = relatedDecisions(tx, p.Base, settings.Target, v.Questions[0].Header, s.scopeGroup(p)); e != nil {
			return e
		}
		subject := fmt.Sprintf("[질문] %s %s %s", id, p.Role, v.Questions[0].Header)
		body := wire.QuestionText(v) + relatedText(related)
		for _, box := range told {
			if e = insertSystemMail(tx, box, subject, body, id, "normal", "", now); e != nil {
				return e
			}
		}
		return nil
	})
	if errors.Is(err, errNobody) {
		http.Error(w, "nobody can answer: no boss role of another base configured", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "could not store question", 500)
		return
	}
	s.publishQuestion(told)
	if settings.Discord {
		s.discord.noticeQuestion(id, p.Role, v.Questions[0].Header)
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, askedQuestion{questionView: s.forCaller(p, v), Related: related})
}

// askedQuestion is the 201 body of a new question: the question and up to
// maxRelated related past decisions.
type askedQuestion struct {
	questionView
	Related []decision `json:"related"`
}

func (s *Server) publishQuestion(boxes []string) {
	for _, box := range boxes {
		s.hub.publish(box)
	}
	s.hub.publish(questionHubKey)
}

// answerQuestion is the answering core shared by the role endpoint and the
// dashboard. by is the answering role, or questionByDashboard, which skips
// the boss check. A role must be a boss of a base other than the asker's.
// The question must still be pending; the check and the update run in one
// transaction, so the first answer wins. It mails the asker and the
// watchers. Errors: errQuestionNotFound, errQuestionForbidden,
// errQuestionNotPending, questionInputError (bad answers), or a storage
// error.
func (s *Server) answerQuestion(ctx context.Context, id, by string, answers []questionAnswer) (questionView, error) {
	now := s.now()
	var v questionView
	var told []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var e error
		if v, e = loadQuestion(tx, id); e != nil {
			return e
		}
		if by != questionByDashboard {
			base := baseOf(tx, by)
			if base == "" || !s.mayAnswerQuestion(principal{Role: by, Base: base}, v) {
				return errQuestionForbidden
			}
		}
		if v.Status != "pending" {
			return errQuestionNotPending
		}
		clean, e := validateAnswers(v.Questions, answers)
		if e != nil {
			return e
		}
		seq, e := nextQuestionSeq(tx)
		if e != nil {
			return e
		}
		encoded, _ := json.Marshal(clean)
		res, e := tx.Exec(`UPDATE questions SET status='answered',answers=?,answered_by=?,answered_at=?,seq=? WHERE id=? AND status='pending'`,
			string(encoded), by, ms(now), seq, id)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errQuestionNotPending
		}
		if v, e = loadQuestion(tx, id); e != nil {
			return e
		}
		body := wire.AnswerText(v)
		first := v.Questions[0].Header
		if box, ok := mailbox(tx, v.Asker); ok {
			if e = insertSystemMail(tx, box, fmt.Sprintf("[질문 답] %s %s", id, first), body, id, "normal", "", now); e != nil {
				return e
			}
			told = append(told, box)
		}
		watchers, e := questionWatchers(tx, v.Asker, v.AskerBase)
		if e != nil {
			return e
		}
		subject := fmt.Sprintf("[질문 답] %s %s %s", id, v.Asker, first)
		for _, box := range watchers {
			if e = insertSystemMail(tx, box, subject, body, id, "normal", "", now); e != nil {
				return e
			}
		}
		told = append(told, watchers...)
		return nil
	})
	if err != nil {
		return questionView{}, err
	}
	s.publishQuestion(told)
	return v, nil
}

func (s *Server) answerQuestionAPI(w http.ResponseWriter, r *http.Request, p principal, id string) {
	var req struct {
		Answers []questionAnswer `json:"answers"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQuestionBody)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if p.Role == questionByDashboard { // a role cannot pose as the dashboard
		questionError(w, errQuestionForbidden)
		return
	}
	v, err := s.answerQuestion(r.Context(), id, p.Role, req.Answers)
	if err != nil {
		questionError(w, err)
		return
	}
	writeJSON(w, s.forCaller(p, v))
}

func (s *Server) withdrawQuestion(w http.ResponseWriter, r *http.Request, p principal, id string) {
	now := s.now()
	var v questionView
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		var e error
		if v, e = loadQuestion(tx, id); e != nil {
			return e
		}
		if p.Base != v.AskerBase {
			return errQuestionForbidden
		}
		if v.Status != "pending" {
			return errQuestionNotPending
		}
		seq, e := nextQuestionSeq(tx)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`UPDATE questions SET status='withdrawn',withdrawn_at=?,seq=? WHERE id=? AND status='pending'`, ms(now), seq, id); e != nil {
			return e
		}
		v, e = loadQuestion(tx, id)
		return e
	})
	if err != nil {
		questionError(w, err)
		return
	}
	s.publishQuestion(nil)
	writeJSON(w, s.forCaller(p, v))
}

// visibleQuestions is the query for questions p may see: the caller's own
// base, and those of the askers p is boss for (answerableSQL).
func (s *Server) visibleQuestions(p principal) (string, []any) {
	where, args := `asker_base=?`, []any{p.Base}
	if cond, more := s.answerableSQL(p, "questions.asker_base"); cond != "" {
		where += ` OR ` + cond
		args = append(args, more...)
	}
	return `SELECT ` + questionColumns + ` FROM questions WHERE (` + where + `)`, args
}

func (s *Server) queryQuestions(p principal, q string, args ...any) ([]questionView, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []questionView{}
	for rows.Next() {
		v, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.forCaller(p, v))
	}
	return out, rows.Err()
}

func (s *Server) listQuestions(w http.ResponseWriter, r *http.Request, p principal) {
	q, args := s.visibleQuestions(p)
	where, wargs, limit, err := questionListFilter(r)
	if err != nil {
		questionError(w, err)
		return
	}
	out, err := s.queryQuestions(p, q+where+` ORDER BY created_at DESC, seq DESC LIMIT ?`, append(append(args, wargs...), limit)...)
	if err != nil {
		http.Error(w, "could not list questions", 500)
		return
	}
	writeJSON(w, questionList{Questions: out})
}

// questionListFilter reads ?status= and ?limit= of a question list: an
// " AND status=?" clause (empty without a status) and the row limit. A bad
// value is a questionInputError.
func questionListFilter(r *http.Request) (where string, args []any, limit int, err error) {
	switch st := r.URL.Query().Get("status"); st {
	case "":
	case "pending", "answered", "withdrawn":
		where, args = ` AND status=?`, []any{st}
	default:
		return "", nil, 0, badQuestion("status must be pending, answered or withdrawn")
	}
	limit = defaultQuestionList
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return "", nil, 0, badQuestion("invalid limit")
		}
		limit = min(n, maxQuestionList)
	}
	return where, args, limit, nil
}

// pollQuestions answers at once with the questions visible to p whose seq is
// greater than ?after=, oldest change first. When there are none it waits
// up to ?timeout= seconds (as /api/events/poll) for a change.
func (s *Server) pollQuestions(w http.ResponseWriter, r *http.Request, p principal) {
	var after int64
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		n, err := wire.ParseID(raw)
		if err != nil || n < 0 {
			http.Error(w, "invalid cursor", 400)
			return
		}
		after = n
	}
	wait, ok := s.pollWait(r)
	if !ok {
		http.Error(w, "invalid timeout", 400)
		return
	}
	// Subscribe before the first query so a change between the query and the
	// wait still wakes this request.
	wake, unsubscribe := s.hub.subscribe(questionHubKey)
	defer unsubscribe()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	q, args := s.visibleQuestions(p)
	var out []questionView
	for {
		var err error
		if out, err = s.queryQuestions(p, q+` AND seq>? ORDER BY seq LIMIT ?`, append(slices.Clone(args), after, maxQuestionList)...); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		if len(out) > 0 || wait == 0 {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
		case <-timer.C:
		case <-wake:
			continue
		}
		break
	}
	cursor := after
	if len(out) > 0 {
		cursor = out[len(out)-1].Seq
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, questionPoll{Questions: out, Cursor: cursor})
}
