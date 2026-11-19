// Package wire holds the JSON shapes of the HTTP API, which the server writes
// and the client reads, and the checks both sides make on them.
package wire

import (
	"strconv"
	"strings"
	"time"
)

type Message struct {
	ID             int64      `json:"id"`
	Sender         string     `json:"sender"`
	Recipient      string     `json:"recipient"`
	Subject        string     `json:"subject"`
	Body           string     `json:"body"`
	CreatedAt      time.Time  `json:"created_at"`
	TaskID         string     `json:"task_id,omitempty"`
	ThreadID       int64      `json:"thread_id,omitempty"`
	ReplyTo        int64      `json:"reply_to,omitempty"`
	Status         string     `json:"status"`
	Priority       string     `json:"priority"`               // "normal" or "urgent"
	AddressedTo    string     `json:"addressed_to,omitempty"` // label address the sender used (see labels.go)
	HeldID         int64      `json:"held_id,omitempty"`      // ID while held, when the message moved to a session
	HeldAt         *time.Time `json:"held_at,omitempty"`      // when the message was held for a label
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"` // when held mail moved to a session
	Directive      bool       `json:"directive,omitempty"`    // a reply that carries new work (reply --directive); plain sends are directives anyway
	NoReply        bool       `json:"no_reply,omitempty"`     // needs no answer (send/reply --no-reply): never an open directive
	IdempotencyKey string     `json:"-"`
}

func ValidRole(role string) bool {
	if len(role) < 1 || len(role) > 128 {
		return false
	}
	for _, c := range role {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-", c)) {
			return false
		}
	}
	return true
}

func ParseID(x string) (int64, error) { return strconv.ParseInt(x, 10, 64) }
