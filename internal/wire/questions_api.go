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

// QuestionRefusal is one role refused a question (can_ask off) in the last
// 24 hours, for the dashboard's question settings (#286): the settings
// target it matched ("none" when no row did), the role it was told to ask
// instead, when it was last refused and how often since the row began.
type QuestionRefusal struct {
	Role   string `json:"role"`
	Target string `json:"target"`
	AskVia string `json:"ask_via"`
	At     string `json:"at"`
	Count  int    `json:"count"`
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
	// Delivery is DeliveryWaiting while the answer mail waits for a session
	// of the asker to register; empty (omitted) otherwise.
	Delivery string `json:"delivery,omitempty"`

	AskerBase string `json:"-"` // server-side only
}

// AnswererOwner marks a question that only a person on the dashboard answers.
const AnswererOwner = "owner"

// DeliveryWaiting marks an answered question whose answer mail no session
// has received yet.
const DeliveryWaiting = "waiting"

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
