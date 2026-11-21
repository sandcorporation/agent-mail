package presence

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Mail to a role with no attached session (#300).
//
// The pending alert (insight) fires only past its age threshold and does not
// look at presence. This sweep tells each sender sooner: a mailbox has
// pending non-system mail, no live session reads it (the slot of that number,
// #354; before, any slot of its base), its session did not go offline in the
// last NoSessionGrace (a session restart), and the sender's oldest pending
// mail to it is NoSessionGrace old. A mailbox that never had a session gets
// the same notice. The notice is one system mail per sender and stretch
// (idem no-session:<mailbox>:<oldest pending id>), so a second sweep sends
// nothing; once that mail is read or the mailbox is attached, the stretch
// ends. Mail held for an ended session (mail.HandOffUnread) is not pending:
// its sender was told when it was held.

// NoSessionGrace is how long a role must stay without a session (and its
// mail wait) before the sender is told.
const NoSessionGrace = 5 * time.Minute

type noSessionMail struct {
	id      int64
	task    string
	created time.Time
}

// SweepNoSession sends the due [미수신] notices and returns how many it sent.
// base maps a mailbox name to its base role.
func (s *Service) SweepNoSession(ctx context.Context, base func(string) string) (int, error) {
	now := s.now().UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,m.sender,m.recipient,COALESCE(m.task_id,''),m.created_at FROM messages m `+mail.StatusJoin+
		` WHERE m.sender NOT IN (?,?) AND `+mail.StatusExpr+`='pending' AND `+mail.LiveRecipient+` ORDER BY m.id`, identity.SystemSender, identity.LegacySystemSender)
	if err != nil {
		return 0, err
	}
	type key struct{ box, sender string }
	type group struct {
		oldest noSessionMail
		count  int
	}
	type row struct {
		m                 noSessionMail
		sender, recipient string
	}
	var all []row
	for rows.Next() {
		var x row
		var created string
		if err = rows.Scan(&x.m.id, &x.sender, &x.recipient, &x.m.task, &created); err != nil {
			rows.Close()
			return 0, err
		}
		x.m.created, _ = time.Parse(time.RFC3339Nano, created)
		all = append(all, x)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	// base queries the database: only after the rows are closed.
	groups := map[key]*group{}
	var order []key
	for _, x := range all {
		k := key{x.recipient, x.sender}
		g := groups[k]
		if g == nil {
			g = &group{oldest: x.m}
			groups[k] = g
			order = append(order, k)
		}
		g.count++
	}
	attached := map[string]bool{}
	sent := 0
	for _, k := range order {
		g := groups[k]
		if now.Sub(g.oldest.created) < NoSessionGrace {
			continue
		}
		busy, seen := attached[k.box]
		if !seen {
			b := base(k.box)
			number := 1
			if b != k.box {
				if _, n, ok := wire.SplitSlot(k.box); ok {
					number = n
				}
			}
			if busy, err = s.attachedRecently(ctx, b, number, now); err != nil {
				return sent, err
			}
			attached[k.box] = busy
		}
		if busy {
			continue
		}
		subject := fmt.Sprintf("[미수신] %s 붙은 세션 없음 — #%d", k.box, g.oldest.id)
		if g.count > 1 {
			subject += fmt.Sprintf(" 외 %d통", g.count-1)
		}
		body := fmt.Sprintf("%s 앞으로 보낸 메일 %d통이 아직 읽히지 않았고, 그 메일함을 읽는 세션이 없습니다(가장 오래된 것 #%d, %s 전). 세션이 프로필과 함께 시작됐는지 `switchboard status` 로 확인하세요. 메일은 받는 쪽 편지함에 남아 있습니다.",
			k.box, g.count, g.oldest.id, shortDuration(now.Sub(g.oldest.created).Truncate(time.Minute)))
		var stored bool
		err = store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			var e error
			stored, e = mail.InsertSystemMailOnce(tx, k.sender, subject, body, g.oldest.task, "normal", fmt.Sprintf("no-session:%s:%d", k.box, g.oldest.id), now)
			return e
		})
		if err != nil {
			return sent, err
		}
		if stored {
			sent++
			s.wake(k.sender)
		}
	}
	return sent, nil
}

// attachedRecently reports whether slot number of base is live, or its
// session went offline within NoSessionGrace of now (the log's offline rows
// are the gaps, not sessions).
func (s *Service) attachedRecently(ctx context.Context, base string, number int, now time.Time) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM slots WHERE base=? AND number=? AND lease_until>?`, base, number, ms(now)).Scan(&n); err != nil || n > 0 {
		return n > 0, err
	}
	var last sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(COALESCE(ended_at,?)) FROM presence_log WHERE base=? AND number=? AND state<>'offline'`, ms(now), base, number).Scan(&last); err != nil {
		return false, err
	}
	return last.Valid && now.Sub(fromMS(last.Int64)) < NoSessionGrace, nil
}

// Attached reports whether a session of base is attached now (a live slot).
func (s *Service) Attached(ctx context.Context, base string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM slots WHERE base=? AND lease_until>?`, base, ms(s.now().UTC())).Scan(&n)
	return n > 0, err
}
