package mail

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/store"
)

// Label stall notices.
//
// When mail to label:<base>/<label> stays held for SWITCHBOARD_LABEL_STALL
// (default 10m) because no session with that label connected, its sender
// gets one notice per sweep and label covering every message that crossed the
// limit since the last notice. The notice is stored in the same transaction
// that marks the messages handled, so running the sweep any number of times
// (or concurrently) sends it once.

const noticeListMax = 5 // held message IDs quoted in a label notice

// SweepLabels sends the due label stall notices for mail held longer than
// after and returns how many it sent.
func (s *Service) SweepLabels(ctx context.Context, after time.Duration) (int, error) {
	now := s.now()
	limit := now.Add(-after)
	rows, err := s.db.QueryContext(ctx, `SELECT id,sender,recipient,COALESCE(held_at,'') FROM messages WHERE status=? AND held_notice_at IS NULL ORDER BY id`, statusHeld)
	if err != nil {
		return 0, err
	}
	type key struct{ sender, recipient string }
	groups := map[key][]int64{}
	var order []key
	for rows.Next() {
		var id int64
		var sender, recipient, at string
		if err = rows.Scan(&id, &sender, &recipient, &at); err != nil {
			rows.Close()
			return 0, err
		}
		// held_at is RFC 3339 with trimmed fractions, which does not sort as
		// text, so the limit is applied after parsing.
		if t, err := time.Parse(time.RFC3339Nano, at); err != nil || t.After(limit) {
			continue
		}
		k := key{sender, recipient}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	sent := 0
	for _, k := range order {
		var to string
		err := store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			stamp := now.UTC().Format(time.RFC3339Nano)
			var marked []int64
			for _, id := range groups[k] {
				res, err := tx.Exec(`UPDATE messages SET held_notice_at=? WHERE id=? AND status=? AND held_notice_at IS NULL`, stamp, id, statusHeld)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 1 {
					marked = append(marked, id)
				}
			}
			if len(marked) == 0 {
				return nil
			}
			box, ok := identity.Mailbox(tx, k.sender)
			if !ok || identity.Reserved(k.sender) {
				return nil
			}
			to = box
			return InsertSystemMail(tx, box, "label not connected: "+k.recipient, labelBody(k.recipient, marked, after), "", "normal", "", now)
		})
		if err != nil {
			return sent, err
		}
		if to != "" {
			sent++
			s.hub.Publish(to)
		}
	}
	return sent, nil
}

func labelBody(addr string, ids []int64, after time.Duration) string {
	base, label := addr, ""
	if a, err := parseLabelAddress(addr); err == nil {
		base, label = a.Base, a.Label
	}
	shown := make([]string, 0, noticeListMax)
	for i, id := range ids {
		if i == noticeListMax {
			shown = append(shown, "…")
			break
		}
		shown = append(shown, "#"+strconv.FormatInt(id, 10))
	}
	return fmt.Sprintf("No session of %s with label %s connected within %s. %d held message(s): %s. They are delivered when such a session registers or heartbeats with that label.",
		base, label, after, len(ids), strings.Join(shown, ", "))
}

// InsertSystemMail stores mail from identity.SystemSender inside tx. idem, when not
// empty, makes a second insert with the same key a no-op.
func InsertSystemMail(tx *sql.Tx, to, subject, body, task, priority, idem string, now time.Time) error {
	_, err := InsertSystemMailOnce(tx, to, subject, body, task, priority, idem, now)
	return err
}

// InsertSystemMailOnce is InsertSystemMail that also reports whether a row
// was stored (false: the idem key was already used).
func InsertSystemMailOnce(tx *sql.Tx, to, subject, body, task, priority, idem string, now time.Time) (bool, error) {
	res, err := tx.Exec(`INSERT OR IGNORE INTO messages(sender,recipient,subject,body,created_at,task_id,priority,status,idem) VALUES(?,?,?,?,?,?,?,'pending',?)`,
		identity.SystemSender, to, subject, body, now.UTC().Format(time.RFC3339Nano), nullable(task), priority, nullable(idem))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
