package mail

import (
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// maxReplyChain bounds the walk up a reply chain. Threads are short; the cap
// only guards against a loop in damaged data.
const maxReplyChain = 64

// flowMail loads message id as FlowMail.
func (s *Service) flowMail(id int64) (FlowMail, bool) {
	rows, err := s.db.Query(`SELECT `+FlowCols+` FROM messages WHERE id=?`, id)
	if err != nil {
		return FlowMail{}, false
	}
	ms, err := ScanFlowMail(rows)
	if err != nil || len(ms) != 1 {
		return FlowMail{}, false
	}
	return ms[0], true
}

// directiveStateOf is what the answered rule's Open reads for directive id,
// apart from Replied: done by its recipient (complete) or closed (no_reply).
func (s *Service) directiveStateOf(id int64) (DirectiveState, bool) {
	var status string
	var noReply bool
	if s.db.QueryRow(`SELECT `+StatusExpr+`,m.no_reply FROM messages m `+StatusJoin+` WHERE m.id=?`, id).Scan(&status, &noReply) != nil {
		return DirectiveState{}, false
	}
	return DirectiveState{Completed: IsCompleteStatus(status), Closed: noReply}, true
}

// stateName is the wire name of a directive state under the answered rule:
// open while CurrentAnsweredRule.Open holds, else done.
func stateName(st DirectiveState) string {
	if CurrentAnsweredRule.Open(st) {
		return wire.DirectiveOpen
	}
	return wire.DirectiveDone
}

// AnswersDirective is the directive message id answers, for reader (#275):
// set when id is a reply (mail.Replies) in the thread of a directive that
// reader sent. The directive is the nearest one up id's reply chain (the
// replied-to mail, or what that replied to, up to the thread root). Its state
// is the answered rule's Open, so a sender sees that an answer left the
// directive open. nil for the directive's recipient, for mail that answers
// no directive, and for a reader who did not send it.
func (s *Service) AnswersDirective(reader string, id int64) *wire.DirectiveRef {
	m, ok := s.flowMail(id)
	if !ok || m.ReplyTo == 0 || m.Recipient != reader {
		return nil
	}
	next := m.ReplyTo
	for i := 0; i < maxReplyChain && next != 0; i++ {
		d, ok := s.flowMail(next)
		if !ok {
			return nil
		}
		if !d.IsDirective() {
			next = d.ReplyTo
			continue
		}
		if d.Sender != reader || !CurrentAnsweredRule.Replies(d, m, s.base) {
			return nil
		}
		st, ok := s.directiveStateOf(d.ID)
		if !ok {
			return nil
		}
		st.Replied = true
		return &wire.DirectiveRef{ID: d.ID, State: stateName(st)}
	}
	return nil
}

func (s *Service) base(role string) string { return identity.BaseOf(s.db, role) }

// withAnswers sets AnswersDirective on each message reader received.
func (s *Service) withAnswers(reader string, ms []wire.Message) {
	for i := range ms {
		if ms[i].ReplyTo != 0 && ms[i].Recipient == reader {
			ms[i].AnswersDirective = s.AnswersDirective(reader, ms[i].ID)
		}
	}
}
