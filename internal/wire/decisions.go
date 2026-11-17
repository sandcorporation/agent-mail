package wire

import (
	"time"
)

// DecisionItem is one question of a decision and what was chosen.
type DecisionItem struct {
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Chosen   []string `json:"chosen"` // option labels
	Other    string   `json:"other"`  // free text, "" when none
}

type Decision struct {
	ID         string         `json:"id"`
	Asker      string         `json:"asker"`
	AskedAt    time.Time      `json:"asked_at"`
	AnsweredAt time.Time      `json:"answered_at"`
	AnsweredBy string         `json:"answered_by"`
	Items      []DecisionItem `json:"items"`
}

type DecisionList struct {
	Decisions []Decision `json:"decisions"`
}
