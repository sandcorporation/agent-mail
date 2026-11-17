package asks

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// AdminQuestionsAPI serves the dashboard's question endpoints behind the
// admin proxy secret (writes also behind the server's adminWriteAllowed, checked by
// adminAPI):
//
//	GET  /api/admin/questions[?status=pending|answered|withdrawn][&limit=N]
//	POST /api/admin/questions/{id}/answer  {answers:[…]}
//
// The list holds every question, newest first; without a status the pending
// ones come first. An answer goes through the shared core as "dashboard":
// no boss check, but the question must still be pending (else 409).
func (s *Service) AdminQuestionsAPI(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.adminListQuestions(w, r)
	case len(parts) == 2 && parts[0] != "" && parts[1] == "answer" && r.Method == http.MethodPost:
		s.adminAnswerQuestion(w, r, parts[0])
	case rest == "" || (len(parts) == 2 && parts[1] == "answer"):
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// forDashboard marks a pending question answerable: the dashboard may answer
// any pending question.
func forDashboard(v wire.QuestionView) wire.QuestionView {
	v.CanAnswer = v.Status == "pending"
	return v
}

func (s *Service) adminListQuestions(w http.ResponseWriter, r *http.Request) {
	where, args, limit, err := questionListFilter(r)
	if err != nil {
		questionError(w, err)
		return
	}
	order := ` ORDER BY created_at DESC, seq DESC LIMIT ?`
	if where == "" {
		order = ` ORDER BY status<>'pending', created_at DESC, seq DESC LIMIT ?`
	}
	rows, err := s.db.Query(`SELECT `+questionColumns+` FROM questions WHERE 1=1`+where+order, append(args, limit)...)
	if err != nil {
		http.Error(w, "could not list questions", 500)
		return
	}
	defer rows.Close()
	out := []wire.QuestionView{}
	for rows.Next() {
		v, err := scanQuestion(rows)
		if err != nil {
			http.Error(w, "could not list questions", 500)
			return
		}
		out = append(out, forDashboard(v))
	}
	if rows.Err() != nil {
		http.Error(w, "could not list questions", 500)
		return
	}
	writeJSON(w, wire.QuestionList{Questions: out})
}

func (s *Service) adminAnswerQuestion(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Answers []wire.QuestionAnswer `json:"answers"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQuestionBody)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	v, err := s.AnswerQuestion(r.Context(), id, QuestionByDashboard, req.Answers)
	if err != nil {
		questionError(w, err)
		return
	}
	writeJSON(w, forDashboard(v))
}
