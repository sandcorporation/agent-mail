package mail

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Label addresses.
//
// A sender may address mail to "label:<base>/<label>" before any session of
// <base> carries that label. The address names the base role on purpose: a
// label alone (an issue number, say) is not unique across teams, and any
// token holder can register any label for its own base. Scoping the label to
// a base means only sessions authenticated with that base's token can ever
// receive the mail.
//
// When an active slot of <base> carries the label, the mail goes straight to
// that slot's mailbox (the smallest such number). Otherwise it is stored with
// recipient "label:<base>/<label>" and status "held": no inbox shows it, the
// sender's outbox does. When a session of <base> registers, heartbeats or
// takes over with that label, every held message for it moves, in the same
// transaction, to that session's mailbox. Slot changes and label sends run in
// one transaction each on the single database connection, so exactly one
// session receives a held message.
//
// A moved message gets a new ID, larger than every existing one, so event and
// watch cursors (which only move forward) cannot skip it. held_id keeps the ID
// the sender was given; addressed_to keeps the label address.

const (
	labelPrefix = "label:"
	maxLabel    = 128
	statusHeld  = "held"
)

var errBadLabel = errors.New(`label address must be label:<base>/<label>, with a label of 1-128 characters from A-Z a-z 0-9 . _ -`)

// labelAddr is a parsed "label:<base>/<label>" address.
type labelAddr struct{ Base, Label string }

func (a labelAddr) String() string { return labelPrefix + a.Base + "/" + a.Label }

// isLabelAddress reports whether a recipient uses the label: form at all.
func isLabelAddress(s string) bool { return strings.HasPrefix(s, labelPrefix) }

// parseLabelAddress parses "label:<base>/<label>". The label is the part after
// the last slash (labels have no slashes; bases may).
func parseLabelAddress(s string) (labelAddr, error) {
	rest := strings.TrimPrefix(s, labelPrefix)
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return labelAddr{}, errBadLabel
	}
	a := labelAddr{Base: rest[:i], Label: rest[i+1:]}
	if !wire.ValidRole(a.Base) || !validLabel(a.Label) {
		return labelAddr{}, errBadLabel
	}
	return a, nil
}

// validLabel reports whether a slot label can be addressed. Slots may store
// any label up to 512 bytes; only these can receive label mail.
func validLabel(l string) bool {
	if len(l) < 1 || len(l) > maxLabel {
		return false
	}
	for _, c := range l {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}

// labelTarget returns the mailbox of the active slot of a.Base that carries
// a.Label (the smallest number when several do), or "" when none does.
func labelTarget(tx *sql.Tx, a labelAddr, now time.Time) (string, error) {
	var n int
	err := tx.QueryRow(`SELECT number FROM slots WHERE base=? AND label=? AND lease_until>? ORDER BY number LIMIT 1`, a.Base, a.Label, ms(now)).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	box, _ := identity.Mailbox(tx, identity.SlotRole(a.Base, n))
	return box, nil
}

// DeliverHeld moves the mail held for label:<base>/<label> to the mailbox of
// slot number under base. It returns that mailbox and how many messages moved.
func DeliverHeld(tx *sql.Tx, base string, number int, label string, now time.Time) (string, int, error) {
	if !validLabel(label) {
		return "", 0, nil
	}
	addr := labelAddr{Base: base, Label: label}.String()
	rows, err := tx.Query(`SELECT id FROM messages WHERE recipient=? AND status=? ORDER BY id`, addr, statusHeld)
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
	var next int64
	if err = tx.QueryRow(`SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='messages'),0),COALESCE((SELECT MAX(id) FROM messages),0))`).Scan(&next); err != nil {
		return "", 0, err
	}
	at := now.UTC().Format(time.RFC3339Nano)
	for _, old := range ids {
		next++
		if _, err = tx.Exec(`UPDATE messages SET id=?,held_id=id,recipient=?,status='pending',delivered_at=? WHERE id=?`, next, box, at, old); err != nil {
			return "", 0, err
		}
		for _, q := range []string{
			`UPDATE messages SET thread_id=? WHERE thread_id=?`,
			`UPDATE messages SET reply_to=? WHERE reply_to=?`,
			`UPDATE role_states SET message_id=? WHERE message_id=?`,
		} {
			if _, err = tx.Exec(q, next, old); err != nil {
				return "", 0, err
			}
		}
	}
	if _, err = tx.Exec(`UPDATE sqlite_sequence SET seq=? WHERE name='messages'`, next); err != nil {
		return "", 0, err
	}
	return box, len(ids), nil
}

// messageCols is the column list scanMessage reads, for a query with the
// messages table aliased m. status is the expression for the status column.
func messageCols(status string) string {
	return `m.id,m.sender,m.recipient,m.subject,m.body,m.created_at,COALESCE(m.task_id,''),COALESCE(m.thread_id,0),COALESCE(m.reply_to,0),` + status + `,COALESCE(m.priority,'normal'),COALESCE(m.addressed_to,''),COALESCE(m.held_id,0),COALESCE(m.held_at,''),COALESCE(m.delivered_at,'')`
}

func scanMessage(sc interface{ Scan(...any) error }) (wire.Message, error) {
	var m wire.Message
	var created, held, delivered string
	err := sc.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &created, &m.TaskID, &m.ThreadID, &m.ReplyTo, &m.Status, &m.Priority, &m.AddressedTo, &m.HeldID, &held, &delivered)
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	m.HeldAt, m.DeliveredAt = optTime(held), optTime(delivered)
	return m, err
}

func optTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}
