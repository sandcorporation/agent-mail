package asks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// A can_ask refusal outside the asking session (#286): one log line, one
// system mail to the role the asker was told to ask instead, and a row the
// dashboard's question settings list for 24 hours.

// noQuestionTarget is the settings target of a refusal no row matched.
const noQuestionTarget = "none"

// refusalShown is how long the dashboard lists a refused role.
const refusalShown = 24 * time.Hour

// refuseCanAsk records a can_ask refusal of p's question in r. Failures are
// logged; the refusal itself still goes out.
func (s *Service) refuseCanAsk(r *http.Request, p identity.Principal, target, askVia string) {
	via := askVia
	if via == "" {
		via = "none"
	}
	log.Printf("questions: question refused role=%s code=%s target=%s ask_via=%s", p.Role, CodeCanAskOff, target, via)
	var req struct {
		Questions []wire.QuestionItem `json:"questions"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxQuestionBody))
	_ = json.Unmarshal(raw, &req)
	now := s.now()
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		sum := sha256.Sum256(raw)
		key = "sha256:" + hex.EncodeToString(sum[:])
	}
	idem := "can-ask-off:" + p.Role + ":" + key
	var box string
	err := store.InTx(context.Background(), s.db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO question_refusals(role,target,ask_via,last_at,count) VALUES(?,?,?,?,1) ON CONFLICT(role) DO UPDATE SET target=excluded.target,ask_via=excluded.ask_via,last_at=excluded.last_at,count=count+1`,
			p.Role, target, askVia, now.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if askVia == "" {
			return nil
		}
		to, ok := identity.Mailbox(tx, askVia)
		if !ok {
			return nil
		}
		stored, err := mail.InsertSystemMailOnce(tx, to, "[질문 거절] "+p.Role+" can_ask 꺼짐", refusalBody(p.Role, target, req.Questions), "", "normal", idem, now)
		if stored {
			box = to
		}
		return err
	})
	if err != nil {
		log.Printf("questions: refusal of %s: %v", p.Role, err)
	}
	if box != "" {
		s.hub.Publish(box)
	}
}

// refusalBody is the mail to ask_via: who was refused, by which settings
// target, and the questions as plain text.
func refusalBody(asker, target string, qs []wire.QuestionItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "질문한 역할: %s\n설정 대상: %s (can_ask 꺼짐)\n", asker, target)
	if len(qs) == 0 {
		b.WriteString("\n질문 본문을 읽지 못했다.\n")
	}
	for _, q := range qs {
		pick := "하나 고름"
		if q.MultiSelect {
			pick = "여럿 고름"
		}
		fmt.Fprintf(&b, "\n[%s] %s (%s)\n", q.Header, q.Question, pick)
		for _, o := range q.Options {
			if o.Description != "" {
				fmt.Fprintf(&b, "- %s: %s\n", o.Label, o.Description)
			} else {
				fmt.Fprintf(&b, "- %s\n", o.Label)
			}
		}
	}
	return b.String()
}

// QuestionRefusals are the roles refused in the last 24 hours, newest first.
func (s *Service) QuestionRefusals() ([]wire.QuestionRefusal, error) {
	since := s.now().Add(-refusalShown).UTC().Format(time.RFC3339Nano)
	rows, err := s.db.Query(`SELECT role,target,ask_via,last_at,count FROM question_refusals WHERE last_at>=? ORDER BY last_at DESC, role`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.QuestionRefusal{}
	for rows.Next() {
		var v wire.QuestionRefusal
		if err := rows.Scan(&v.Role, &v.Target, &v.AskVia, &v.At, &v.Count); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// QuestionRefusalsAPI serves GET /api/admin/question-refusals.
func (s *Service) QuestionRefusalsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out, err := s.QuestionRefusals()
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}
