package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
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
// take mail held for w. strict is set for mail a role sent (#354): there no
// label does not match no label, so only the same folder or a real label
// takes it.
func (w Handoff) takes(machine, directory, label string, strict bool) bool {
	if w.Label == label && (!strict || label != "") {
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

// Folder is dir without a trailing session mark " #<8 hex digits>": the
// folder a plugin directory names.
func Folder(dir string) string { return folder(dir) }

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

// HandOffUnread holds the unread mail in the mailbox of slot number of base
// for the next session its handoffs row matches. The caller has seen that
// slot's lease end. Mail with a handoffs row (system mail meant for one
// session) is held again under that row; mail a role sent (#354) is held for
// the slot's ended identity (machine, folder, label), so the next session that
// gets the number does not inherit it, and its sender is told once. Other
// system mail and mail a role marked in any way (read, acknowledged,
// complete) stay. It returns how many messages it held and the mailboxes of
// the senders it told (to wake after commit).
func HandOffUnread(tx *sql.Tx, base string, number int, now time.Time) (int, []string, error) {
	box, ok := identity.Mailbox(tx, identity.SlotRole(base, number))
	if !ok {
		return 0, nil, nil
	}
	rows, err := tx.Query(`SELECT m.id,COALESCE(m.task_id,''),m.sender,m.subject,h.message_id IS NOT NULL FROM messages m LEFT JOIN handoffs h ON h.message_id=m.id `+StatusJoin+
		` WHERE m.recipient=? AND m.status='pending' AND COALESCE(rs.status,'pending')='pending' AND (h.message_id IS NOT NULL OR m.sender NOT IN (?,?)) ORDER BY m.id`,
		box, identity.SystemSender, identity.LegacySystemSender)
	if err != nil {
		return 0, nil, err
	}
	var due []unread
	for rows.Next() {
		var u unread
		if err = rows.Scan(&u.id, &u.task, &u.sender, &u.subject, &u.hasRow); err != nil {
			rows.Close()
			return 0, nil, err
		}
		due = append(due, u)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(due) == 0 {
		return 0, nil, err
	}
	var ended *Handoff
	var told []string
	held := 0
	for _, u := range due {
		if !u.hasRow && ended == nil {
			w := Handoff{Base: base}
			if err = tx.QueryRow(`SELECT machine,directory,label FROM slots WHERE base=? AND number=?`, base, number).Scan(&w.Machine, &w.Directory, &w.Label); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue // no session ever held the number: the mail stays
				}
				return 0, nil, err
			}
			ended = &w
		}
		var w *Handoff
		if !u.hasRow {
			w = ended
		}
		to, err := hold(tx, u, box, w, now)
		if err != nil {
			return 0, nil, err
		}
		held++
		if to != "" {
			told = append(told, to)
		}
	}
	return held, told, nil
}

// unread is mail HandOffUnread or HoldIfEnded holds.
type unread struct {
	id              int64
	task            string
	sender, subject string
	hasRow          bool // a handoffs row exists
}

// hold moves u, unread in box, to the held handoff state. w, when set, is
// stored as u's handoffs row (mail a role sent); its sender is then told
// once and hold returns that sender's mailbox.
func hold(tx *sql.Tx, u unread, box string, w *Handoff, now time.Time) (string, error) {
	at := now.UTC().Format(time.RFC3339Nano)
	// held_notice_at is set: label stall notices are not for handed-off mail.
	if _, err := tx.Exec(`UPDATE messages SET recipient=?,status=?,addressed_to=COALESCE(addressed_to,?),held_at=?,held_notice_at=? WHERE id=?`,
		handoffPrefix+u.task, statusHeld, box, at, at, u.id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM role_states WHERE message_id=? AND role=?`, u.id, box); err != nil {
		return "", err
	}
	if w == nil {
		return "", nil
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO handoffs(message_id,base,machine,directory,label) VALUES(?,?,?,?,?)`, u.id, w.Base, w.Machine, w.Directory, w.Label); err != nil {
		return "", err
	}
	to, ok := identity.Mailbox(tx, u.sender)
	if !ok || identity.Reserved(u.sender) {
		return "", nil
	}
	stored, err := InsertSystemMailOnce(tx, to, fmt.Sprintf("[미수신] %s 받는 세션 없음 — #%d %s", box, u.id, u.subject),
		fmt.Sprintf("%s 앞으로 보낸 #%d 「%s」 를 읽을 세션이 없어 붙잡아 두었습니다. 받을 세션(같은 기계·폴더 또는 같은 라벨)이 다시 붙으면 전달됩니다. 다른 세션에 맡기려면 산 슬롯 이름으로 다시 보내세요.", box, u.id, u.subject),
		u.task, "normal", fmt.Sprintf("handoff-held:%d", u.id), now)
	if err != nil || !stored {
		return "", err
	}
	return to, nil
}

// HoldIfEnded holds mail id, just sent by sender to box, when box is the
// mailbox of a slot whose last session's lease has ended (#354): it waits for
// that session as HandOffUnread would have held it. A number no session ever
// held keeps the mail in box, for the first session that gets it. It
// reports whether it held the mail and the sender's mailbox it told.
func HoldIfEnded(tx *sql.Tx, id int64, box, sender, subject, task string, now time.Time) (bool, string, error) {
	base, number, ok := SlotOf(tx, box)
	if !ok {
		return false, "", nil
	}
	w := Handoff{Base: base}
	var until int64
	err := tx.QueryRow(`SELECT machine,directory,label,lease_until FROM slots WHERE base=? AND number=?`, base, number).Scan(&w.Machine, &w.Directory, &w.Label, &until)
	if errors.Is(err, sql.ErrNoRows) || err == nil && until > ms(now) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	to, err := hold(tx, unread{id: id, task: task, sender: sender, subject: subject}, box, &w, now)
	return err == nil, to, err
}

// SlotOf is the base and slot number whose session reads mailbox box: a role
// row is its own base's slot 1, "<base>-N" is slot N of base.
func SlotOf(q identity.Queryer, box string) (string, int, bool) {
	if identity.Reserved(box) {
		return "", 0, false
	}
	if identity.RoleExists(q, box) {
		return box, 1, true
	}
	base, n, ok := wire.SplitSlot(box)
	if !ok || n < 2 || !identity.RoleExists(q, base) {
		return "", 0, false
	}
	return base, n, true
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
	rows, err := tx.Query(`SELECT h.message_id,h.machine,h.directory,h.label,m.sender NOT IN (?,?) FROM handoffs h JOIN messages m ON m.id=h.message_id WHERE h.base=? AND (h.machine=? OR h.label=?) AND m.status=? ORDER BY h.message_id`,
		identity.SystemSender, identity.LegacySystemSender, base, machine, label, statusHeld)
	if err != nil {
		return "", 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var strict bool
		w := Handoff{Base: base}
		if err = rows.Scan(&id, &w.Machine, &w.Directory, &w.Label, &strict); err != nil {
			rows.Close()
			return "", 0, err
		}
		if w.takes(machine, directory, label, strict) {
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

// HandoffWaitingSQL is an SQL expression, true while held handed-off system
// mail with task_id equal to the expression task waits for a session.
func HandoffWaitingSQL(task string) string {
	return `EXISTS(SELECT 1 FROM handoffs h JOIN messages hm ON hm.id=h.message_id WHERE hm.status='held' AND hm.sender IN ('` +
		identity.SystemSender + `','` + identity.LegacySystemSender + `') AND hm.task_id=` + task + `)`
}

// ExpireHandoffs ends mail a role sent that has been held for a session
// (handoff) longer than after (#354): it is marked complete where it is held,
// kept, never delivered, and its sender gets one line. It returns how many
// messages it ended.
func (s *Service) ExpireHandoffs(ctx context.Context, after time.Duration) (int, error) {
	now := s.now()
	limit := now.Add(-after)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.sender,m.subject,COALESCE(m.addressed_to,''),COALESCE(m.task_id,''),COALESCE(m.held_at,'') FROM messages m JOIN handoffs h ON h.message_id=m.id WHERE m.status=? AND m.sender NOT IN (?,?) ORDER BY m.id`,
		statusHeld, identity.SystemSender, identity.LegacySystemSender)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id                         int64
		sender, subject, box, task string
	}
	var due []expired
	for rows.Next() {
		var x expired
		var at string
		if err = rows.Scan(&x.id, &x.sender, &x.subject, &x.box, &x.task, &at); err != nil {
			rows.Close()
			return 0, err
		}
		// held_at does not sort as text (see SweepLabels).
		if t, err := time.Parse(time.RFC3339Nano, at); err != nil || t.After(limit) {
			continue
		}
		due = append(due, x)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	ended := 0
	for _, x := range due {
		var to string
		var done bool
		err := store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			to, done = "", false
			res, err := tx.Exec(`UPDATE messages SET status='complete' WHERE id=? AND status=?`, x.id, statusHeld)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			done = true
			box, ok := identity.Mailbox(tx, x.sender)
			if !ok || identity.Reserved(x.sender) {
				return nil
			}
			stored, err := InsertSystemMailOnce(tx, box, fmt.Sprintf("[미수신] #%d 끝냄 — %s 받는 세션 없음", x.id, x.box),
				fmt.Sprintf("%s 앞으로 보낸 #%d 「%s」 를 %s 동안 받을 세션이 붙지 않아 끝냈습니다(지우지 않음). 아직 필요하면 산 세션에 다시 보내세요.", x.box, x.id, x.subject, after),
				x.task, "normal", fmt.Sprintf("handoff-expired:%d", x.id), now)
			if stored {
				to = box
			}
			return err
		})
		if err != nil {
			return ended, err
		}
		if done {
			ended++
		}
		if to != "" {
			s.hub.Publish(to)
		}
	}
	return ended, nil
}
