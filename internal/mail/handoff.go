package mail

import (
	"database/sql"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
)

// Handed-off mail.
//
// System mail meant for one session that is no longer live (the answer to a
// question whose asking session left) is held for the next session of the
// same base that is the same identity (machine and directory) or carries the
// same label (no label matches no label). It is stored with recipient
// "handoff:<task>" and status "held" (so no inbox shows it) plus a handoffs
// row naming who may take it. The first matching session that registers or
// heartbeats receives it in that transaction, under a new ID as label mail
// does, and the handoffs row goes away, so exactly one session gets it.

const handoffPrefix = "handoff:"

// Handoff names the sessions of Base that may take held mail: the identity
// Machine+Directory, or any session with Label.
type Handoff struct {
	Base, Machine, Directory, Label string
}

// HoldForHandoff stores system mail for the next session w matches.
// addressedTo keeps the address the mail was meant for.
func HoldForHandoff(tx *sql.Tx, w Handoff, addressedTo, subject, body, task string, now time.Time) error {
	at := now.UTC().Format(time.RFC3339Nano)
	// held_notice_at is set: label stall notices are not for handed-off mail.
	res, err := tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,priority,status,addressed_to,held_at,held_notice_at) VALUES(?,?,?,?,?,?,'normal',?,?,?,?)`,
		identity.SystemSender, handoffPrefix+task, subject, body, at, nullable(task), statusHeld, nullable(addressedTo), at, at)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO handoffs(message_id,base,machine,directory,label) VALUES(?,?,?,?,?)`, id, w.Base, w.Machine, w.Directory, w.Label)
	return err
}

// DeliverHandoff moves the held mail a session of base at number (identity
// machine+directory, label) may take to that slot's mailbox. It returns that
// mailbox and how many messages moved.
func DeliverHandoff(tx *sql.Tx, base string, number int, machine, directory, label string, now time.Time) (string, int, error) {
	rows, err := tx.Query(`SELECT h.message_id FROM handoffs h JOIN messages m ON m.id=h.message_id WHERE h.base=? AND ((h.machine=? AND h.directory=?) OR h.label=?) AND m.status=? ORDER BY h.message_id`,
		base, machine, directory, label, statusHeld)
	if err != nil {
		return "", 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return "", 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(ids) == 0 {
		return "", 0, err
	}
	box, ok := identity.Mailbox(tx, identity.SlotRole(base, number))
	if !ok {
		return "", 0, nil
	}
	for _, id := range ids {
		if _, err = tx.Exec(`DELETE FROM handoffs WHERE message_id=?`, id); err != nil {
			return "", 0, err
		}
	}
	if err = moveHeld(tx, ids, box, now); err != nil {
		return "", 0, err
	}
	return box, len(ids), nil
}

// HandoffWaitingSQL is an SQL expression, true while held handed-off mail
// with task_id equal to the expression task waits for a session.
func HandoffWaitingSQL(task string) string {
	return `EXISTS(SELECT 1 FROM handoffs h JOIN messages hm ON hm.id=h.message_id WHERE hm.status='held' AND hm.task_id=` + task + `)`
}
