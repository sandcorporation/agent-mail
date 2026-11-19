package asks

import (
	"database/sql"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Where an answer goes.
//
// A question asked from a live slot remembers that session (machine,
// directory, label). Its answer goes to that session's live slot under
// whatever number it holds now; else to the most recently registered live
// slot of the base with the same label; else it is held and handed to the
// next session of the base that is that identity or has that label
// (internal/mail/handoff.go). While a slot's lease is alive the answer stays
// with it: a crashed session and a live one in the same folder look alike.
// When that lease ends with the answer unread, it is held the same way. A
// session with another label works on
// something else and never receives it. A question asked without a slot is
// answered at the asking address.

// askingSlot is the live slot p asks from, or the zero view.
func (s *Service) askingSlot(tx *sql.Tx, p identity.Principal, now time.Time) (wire.SlotView, error) {
	if s.slots.Asking == nil {
		return wire.SlotView{}, nil
	}
	v, ok, err := s.slots.Asking(tx, p.Base, p.Role, now)
	if err != nil || !ok {
		return wire.SlotView{}, err
	}
	return v, nil
}

// answerBox is the mailbox the answer to v goes to now ("" when the asker has
// no mailbox, or when nobody may take it now). w, when not nil, names who may
// take the answer if it waits: now when box is "", else when box's slot ends
// with it unread (mail.HandOffUnread).
func (s *Service) answerBox(tx *sql.Tx, v wire.QuestionView, now time.Time) (box string, w *mail.Handoff, err error) {
	var asking wire.SlotView
	if err = tx.QueryRow(`SELECT asker_machine,asker_directory,asker_label FROM questions WHERE id=?`, v.ID).Scan(&asking.Machine, &asking.Directory, &asking.Label); err != nil {
		return "", nil, err
	}
	to := v.Asker
	if asking.Machine != "" && s.slots.Answer != nil {
		role, ok, err := s.slots.Answer(tx, v.AskerBase, asking, now)
		if err != nil {
			return "", nil, err
		}
		w = &mail.Handoff{Base: v.AskerBase, Machine: asking.Machine, Directory: asking.Directory, Label: asking.Label}
		if !ok {
			return "", w, nil
		}
		to = role
	}
	box, _ = identity.Mailbox(tx, to)
	return box, w, nil
}
