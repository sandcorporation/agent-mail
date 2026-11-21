package insight

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
)

// Metrics alerts.
//
// The sweep checks the alert thresholds (ParseThresholds) and mails every
// resolvable boss role from the system sender, normal priority:
//
//   - pending: a recipient role has pending non-system mail older than
//     SWITCHBOARD_ALERT_PENDING; one alert per recipient role.
//   - error / blocked: an online session has been in error:* / blocked:*
//     longer than SWITCHBOARD_ALERT_ERROR / SWITCHBOARD_ALERT_BLOCKED; one alert
//     per session role.
//
// A zero threshold disables that kind. Each (kind, subject role) alerts at
// most once per UTC day: the alerts row and the mail are written in one
// transaction, and a second insert of the same (kind, key, day) is ignored,
// so repeated or concurrent sweeps send it once. Rows older than 30 days are
// pruned. A boss is never told about pending mail of its own base.

const alertRetainDays = 30

func alertDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// alertDue is one alert to send: kind, the role it is about, and its text.
type alertDue struct {
	Kind, Role, Subject, Body string
}

// shortAge renders a duration as 2h40m, 45m, 1h or 30s.
func shortAge(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	h, m := int64(d/time.Hour), int64(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

const alertHint = "자세히: `switchboard metrics` 또는 대시보드 #metrics 탭."

// pendingListMax is how many pending messages a pending alert names (#354).
const pendingListMax = 5

// pendingList names the oldest pending non-system mail of box, at most max,
// one line each: "#<id> <sender> — <subject>".
func (s *Service) pendingList(box string, max int) (string, error) {
	rows, err := s.db.Query(`SELECT m.id,m.sender,m.subject FROM messages m `+mail.StatusJoin+` WHERE m.recipient=? AND m.sender NOT IN (?,?) AND `+mail.StatusExpr+`='pending' ORDER BY m.id LIMIT ?`,
		box, identity.SystemSender, identity.LegacySystemSender, max)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int64
		var sender, subject string
		if err = rows.Scan(&id, &sender, &subject); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "- #%d %s — %s\n", id, sender, subject)
	}
	return b.String(), rows.Err()
}

// dueAlerts lists the alerts whose threshold is crossed at now, before
// dedupe.
func (s *Service) dueAlerts(now time.Time) ([]alertDue, error) {
	lim := s.Limits()
	var out []alertDue
	if lim.Pending > 0 {
		ages, err := s.pendingAges(now, lim.Pending)
		if err != nil {
			return nil, err
		}
		for _, p := range ages {
			if !p.OverThreshold {
				continue
			}
			age := shortAge(time.Duration(p.OldestS) * time.Second)
			list, err := s.pendingList(p.Role, pendingListMax)
			if err != nil {
				return nil, err
			}
			out = append(out, alertDue{
				Kind:    "pending",
				Role:    p.Role,
				Subject: fmt.Sprintf("[지표] pending %s %d통, 가장 오래 %s", p.Role, p.Pending, age),
				Body: fmt.Sprintf("%s 앞으로 아직 읽지 않은(pending) 메일이 %d통 있습니다(시스템 메일 제외). 가장 오래된 것은 %s 전에 왔습니다. 문턱은 %s(SWITCHBOARD_ALERT_PENDING)입니다.\n\n%s\n%s",
					p.Role, p.Pending, age, shortAge(lim.Pending), list, alertHint),
			})
		}
	}
	if lim.Error > 0 || lim.Blocked > 0 {
		cur, err := s.currentSessionStates(now)
		if err != nil {
			return nil, err
		}
		for _, c := range cur {
			kind, limit, env := "error", lim.Error, "SWITCHBOARD_ALERT_ERROR"
			if isBlockedState(c.State) {
				kind, limit, env = "blocked", lim.Blocked, "SWITCHBOARD_ALERT_BLOCKED"
			}
			if limit <= 0 || time.Duration(c.Seconds)*time.Second < limit {
				continue
			}
			age := shortAge(time.Duration(c.Seconds) * time.Second)
			out = append(out, alertDue{
				Kind:    kind,
				Role:    c.Role,
				Subject: fmt.Sprintf("[지표] %s %s %s, %s째", kind, c.Role, c.State, age),
				Body: fmt.Sprintf("세션 %s가 %s부터 %s 상태입니다(%s째). 문턱은 %s(%s)입니다.\n\n%s",
					c.Role, c.Since.UTC().Format(time.RFC3339), c.State, age, shortAge(limit), env, alertHint),
			})
		}
	}
	return out, nil
}

// bossMailboxes are the mailboxes of the configured boss roles that have a
// role row, deduplicated, with each one's base.
func (s *Service) bossMailboxes(q identity.Queryer) (boxes, bases []string) {
	seen := map[string]bool{}
	for _, boss := range sortedKeys(s.bossRoles) {
		box, ok := identity.Mailbox(q, boss)
		if !ok || seen[box] {
			continue
		}
		seen[box] = true
		boxes = append(boxes, box)
		bases = append(bases, identity.BaseOf(q, box))
	}
	return boxes, bases
}

// SweepAlerts sends the due alerts not yet sent today and prunes old alert
// rows. It returns the number of alerts sent (not mails).
func (s *Service) SweepAlerts(ctx context.Context) (int, error) {
	now := s.now()
	day := alertDay(now)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE day<?`, alertDay(now.AddDate(0, 0, -alertRetainDays))); err != nil {
		return 0, err
	}
	due, err := s.dueAlerts(now)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	sent := 0
	for _, a := range due {
		var told []string
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			boxes, bases := s.bossMailboxes(tx)
			var to []string
			subjectBase := identity.BaseOf(tx, a.Role)
			for i, box := range boxes {
				// A boss already sees its own pending mail.
				if a.Kind == "pending" && (box == a.Role || (subjectBase != "" && bases[i] == subjectBase)) {
					continue
				}
				to = append(to, box)
			}
			if len(to) == 0 {
				return nil
			}
			res, err := tx.Exec(`INSERT OR IGNORE INTO alerts(kind,key,day,sent_at) VALUES(?,?,?,?)`, a.Kind, a.Role, day, ms(now))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			task := "alert:" + a.Kind + ":" + a.Role + ":" + day
			for _, box := range to {
				if err = mail.InsertSystemMail(tx, box, a.Subject, a.Body, task, "normal", task+":"+box, now); err != nil {
					return err
				}
			}
			told = to
			return nil
		})
		if err != nil {
			return sent, err
		}
		if len(told) > 0 {
			sent++
			for _, box := range told {
				s.wake(box)
			}
		}
	}
	return sent, nil
}
