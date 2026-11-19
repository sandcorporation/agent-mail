package wire

import (
	"errors"
	"strings"
	"time"
)

// Report status, derived from the linked threads (never stored).
const (
	ReportOpen = "open" // a linked directive is still open
	ReportDone = "done" // every linked directive is done or closed; or no links
)

// Report limits.
const (
	MaxReportTitle = 200
	MaxReportLine  = 500
	MaxReportMail  = 100
)

// ReportRequest is the body of POST /api/reports (switchboard report).
type ReportRequest struct {
	Title          string  `json:"title"`
	Request        string  `json:"request,omitempty"`
	Body           string  `json:"body"`
	Mail           []int64 `json:"mail,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

// Check trims the one-line fields and refuses an empty title or body, a
// title or request over its limit or with a line break, and more than
// MaxReportMail mail IDs.
func (q *ReportRequest) Check() error {
	q.Title, q.Request = strings.TrimSpace(q.Title), strings.TrimSpace(q.Request)
	switch {
	case q.Title == "":
		return errors.New("title required")
	case strings.TrimSpace(q.Body) == "":
		return errors.New("body required")
	case len(q.Title) > MaxReportTitle || strings.ContainsAny(q.Title, "\r\n"):
		return errors.New("title must be one line of at most 200 bytes")
	case len(q.Request) > MaxReportLine || strings.ContainsAny(q.Request, "\r\n"):
		return errors.New("request must be one line of at most 500 bytes")
	case len(q.Mail) > MaxReportMail:
		return errors.New("at most 100 mail IDs")
	}
	for _, id := range q.Mail {
		if id <= 0 {
			return errors.New("mail IDs must be positive")
		}
	}
	return nil
}

// Report is one report. Mail lists the linked threads by their root
// directive's ID. Body is left out of the admin list.
type Report struct {
	ID          int64      `json:"id"`
	Author      string     `json:"author"`
	Title       string     `json:"title"`
	Request     string     `json:"request"`
	Body        string     `json:"body,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Mail        []int64    `json:"mail"`
	Status      string     `json:"status"`
	LastReplyAt *time.Time `json:"last_reply_at"`
}

// ReportList is GET /api/admin/reports, newest first.
type ReportList struct {
	Reports []Report `json:"reports"`
}

// ReportMail is one message of a linked thread. Status is the recipient's
// state; Answers is true for a reply the answered rule counts as answering
// the thread's directive.
type ReportMail struct {
	ID        int64     `json:"id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`
	Answers   bool      `json:"answers,omitempty"`
}

// ReportThread is a linked thread: its root directive and the replies in
// order. Answered: it got a reply (mail.Replies). Open is the thread's share
// of the report status, the answered rule's Open: since version 2 not done
// by the recipient and not closed (no_reply), however many replies.
type ReportThread struct {
	Directive ReportMail   `json:"directive"`
	Replies   []ReportMail `json:"replies"`
	Answered  bool         `json:"answered"`
	Completed bool         `json:"completed"`
	Closed    bool         `json:"closed"`
	Open      bool         `json:"open"`
}

// ReportDetail is GET /api/admin/reports/{id}.
type ReportDetail struct {
	Report
	Threads []ReportThread `json:"threads"`
}
