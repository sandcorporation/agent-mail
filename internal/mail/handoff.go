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
// same base that is the same identity or carries the same label (no label
// matches no label). The same identity is the same machine and folder, where
// a directory and its session-marked form "<folder> #<8 hex digits>" (the
// plugin's directory for a second session in a folder another session holds)
// are one folder, unless both carry labels that differ. Held mail is stored
// with recipient "handoff:<task>" and status "held" (so no inbox shows it)
// plus a handoffs row naming who may take it. The first matching session that
// registers or heartbeats receives it in that transaction, under a new ID as
// label mail does, so exactly one session gets it.
//
// Mail delivered to a live session that may be handed off later (an answer
// sent to the asking slot) keeps its handoffs row while it sits in that
// mailbox. When the slot's lease ends with it still unread, HandOffUnread
// holds it again for the next matching session; read mail stays.

const handoffPrefix = "handoff:"

// Handoff names the sessions of Base that may take held mail: the identity
// Machine+Directory, or any session with Label.
type Handoff struct {
	Base, Machine, Directory, Label string
}

// takes reports whether a session at machine and directory with label may
// take mail held for w.
func (w Handoff) takes(machine, directory, label string) bool {
	if w.Label == label {
		return true
	}
	if w.Machine != machine {
		return false
	}
	if w.Directory == directory {
		return true
	}
	return folder(w.Directory) == folder(directory) && (w.Label == "" || label == "")
}

// folder is dir without a trailing session mark " #<8 hex digits>".
func folder(dir string) string {
	const mark = len(" #") + 8
	i := len(dir) - mark
	if i <= 0 || dir[i] != ' ' || dir[i+1] != '#' {
		return dir
	}
	for _, c := range dir[i+2:] {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return dir
		}
	}
	return dir[:i]
}

// InsertHandoffMail delivers system mail to box and keeps w with it, so the
// mail is held for the next session w matches if box's slot ends with it
// unread (HandOffUnread).
func InsertHandoffMail(tx *sql.Tx, box string, w Handoff, subject, body, task string, now time.Time) error {
	res, err := tx.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,priority,status) VALUES(?,?,?,?,?,?,'normal','pending')`,
		identity.SystemSender, box, subject, body, now.UTC().Format(time.RFC3339Nano), nullable(task))
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

// HandOffUnread holds again the unread mail in the mailbox of slot number of
// base that carries a handoffs row, for the next session its row matches. The
// caller has seen that slot's lease end. Mail a role marked in any way (read,
// acknowledged, complete) stays. It returns how many messages it held.
func HandOffUnread(tx *sql.Tx, base string, number int, now time.Time) (int, error) {
	box, ok := identity.Mailbox(tx, identity.SlotRole(base, number))
	if !ok {
		return 0, nil
	}
	rows, err := tx.Query(`SELECT m.id,COALESCE(m.task_id,'') FROM messages m JOIN handoffs h ON h.message_id=m.id `+StatusJoin+` WHERE m.recipient=? AND m.status='pending' AND COALESCE(rs.status,'pending')='pending' ORDER BY m.id`, box)
	if err != nil {
		return 0, err
	}
	type unread struct {
		id   int64
		task string
	}
	var due []unread
	for rows.Next() {
		var u unread
		if err = rows.Scan(&u.id, &u.task); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, u)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	at := now.UTC().Format(time.RFC3339Nano)
	for _, u := range due {
		// held_notice_at is set: label stall notices are not for handed-off mail.
		if _, err = tx.Exec(`UPDATE messages SET recipient=?,status=?,addressed_to=COALESCE(addressed_to,?),held_at=?,held_notice_at=? WHERE id=?`,
			handoffPrefix+u.task, statusHeld, box, at, at, u.id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`DELETE FROM role_states WHERE message_id=? AND role=?`, u.id, box); err != nil {
			return 0, err
		}
	}
	return len(due), nil
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
	rows, err := tx.Query(`SELECT h.message_id,h.machine,h.directory,h.label FROM handoffs h JOIN messages m ON m.id=h.message_id WHERE h.base=? AND (h.machine=? OR h.label=?) AND m.status=? ORDER BY h.message_id`,
		base, machine, label, statusHeld)
	if err != nil {
		return "", 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		w := Handoff{Base: base}
		if err = rows.Scan(&id, &w.Machine, &w.Directory, &w.Label); err != nil {
			rows.Close()
			return "", 0, err
		}
		if w.takes(machine, directory, label) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(ids) == 0 {
		return "", 0, err
	}
	box, ok := identity.Mailbox(tx, identity.SlotRole(base, number))
	if !ok {
		return "", 0, nil
	}
	// The handoffs rows stay (moveHeld renumbers them with the mail): the
	// taker's lease may end with the mail unread as well.
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
