package wire

import "time"

// MailEvent is the data of an SSE "message" event. Body is present only for
// urgent mail (always present for urgent mail, even when empty).
type MailEvent struct {
	ID        int64     `json:"id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Priority  string    `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
	TaskID    string    `json:"task_id,omitempty"`
	ThreadID  int64     `json:"thread_id,omitempty"`
	// AddressedTo is the label address the sender used, for label mail.
	AddressedTo string  `json:"addressed_to,omitempty"`
	Body        *string `json:"body,omitempty"`
	// AnswersDirective: the mail answers a directive the recipient sent, and
	// that directive's state (#275), as on wire.Message.
	AnswersDirective *DirectiveRef `json:"answers_directive,omitempty"`
}
