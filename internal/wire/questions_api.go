package wire

import (
	"fmt"
	"strings"
	"time"
)

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

type QuestionItem struct {
	Header      string           `json:"header"`
	Question    string           `json:"question"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

type QuestionAnswer struct {
	Selected []string `json:"selected"`
	Other    string   `json:"other,omitempty"`
}

// QuestionView is the JSON of one question. Answers, AnsweredAt and
// WithdrawnAt are null and AnsweredBy is "" until they apply.
type QuestionView struct {
	ID          string           `json:"id"`
	Asker       string           `json:"asker"`
	CreatedAt   time.Time        `json:"created_at"`
	Status      string           `json:"status"` // pending | answered | withdrawn
	Questions   []QuestionItem   `json:"questions"`
	Answers     []QuestionAnswer `json:"answers"`
	AnsweredBy  string           `json:"answered_by"` // a role, "dashboard" or "panel"
	AnsweredAt  *time.Time       `json:"answered_at"`
	WithdrawnAt *time.Time       `json:"withdrawn_at"`
	Seq         int64            `json:"seq"`
	CanAnswer   bool             `json:"can_answer"` // the caller may answer it now
	// Answerer is AnswererOwner when no boss role could answer at ask time,
	// so a person answers on the dashboard; empty (omitted) otherwise.
	Answerer string `json:"answerer,omitempty"`

	AskerBase string `json:"-"` // server-side only
}

// AnswererOwner marks a question that only a person on the dashboard answers.
const AnswererOwner = "owner"

type QuestionList struct {
	Questions []QuestionView `json:"questions"`
}

// QuestionText is the full question with its options, for watcher mail.
func QuestionText(v QuestionView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks (%s):\n", v.Asker, v.ID)
	for i, q := range v.Questions {
		kind := "one choice"
		if q.MultiSelect {
			kind = "multiple choice"
		}
		fmt.Fprintf(&b, "\n%d. [%s] %s (%s)\n", i+1, q.Header, q.Question, kind)
		for _, o := range q.Options {
			fmt.Fprintf(&b, "   - %s", o.Label)
			if o.Description != "" {
				fmt.Fprintf(&b, ": %s", o.Description)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// AnswerText: one line per question, then who answered.
func AnswerText(v QuestionView) string {
	var b strings.Builder
	for i, q := range v.Questions {
		a := v.Answers[i]
		fmt.Fprintf(&b, "%s: %s = %s", q.Header, q.Question, strings.Join(a.Selected, ", "))
		if a.Other != "" {
			if len(a.Selected) > 0 {
				b.WriteString(" / ")
			}
			b.WriteString("기타: " + a.Other)
		}
		b.WriteString("\n")
	}
	b.WriteString("답한 쪽: " + v.AnsweredBy)
	return b.String()
}
