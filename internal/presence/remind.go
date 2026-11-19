package presence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
)

// Directive reminders (#214).
//
// A directive that its recipient has read but neither replied to nor
// completed (an open directive, see openDirectives, with status read and a
// non-boss recipient) gets reminders from the system sender: the first when
// it is SWITCHBOARD_REMIND_AFTER old, each next one twice the previous gap
// later (T, 3T, 7T for the defaults), at most SWITCHBOARD_REMIND_MAX. When
// the directive is still open one more gap after the last reminder, its
// sender gets one notice.
//
// SWITCHBOARD_REMIND=on turns the sweep on; off (the default) it reads and
// writes nothing. The first sweep with it on records the time in
// remind_state; directives created before that time are never reminded, and
// switching it off and on again keeps the first time.
//
// Every reminder and notice carries an idempotency key (remind:<id>:<n>,
// remind-sender:<id>) on the messages table's UNIQUE(sender,idem), so
// repeated or concurrent sweeps and restarts send each exactly once. The
// sent mail is also the schedule's state: the next one is due a gap after
// the last one stored.

const (
	defaultRemindAfter = 2 * time.Hour
	defaultRemindMax   = 3
)

// RemindConfig is the directive reminder settings.
type RemindConfig struct {
	On    bool          // SWITCHBOARD_REMIND=on
	After time.Duration // SWITCHBOARD_REMIND_AFTER; 0 means defaultRemindAfter
	Max   int           // SWITCHBOARD_REMIND_MAX; 0 means defaultRemindMax
}

func (c RemindConfig) after() time.Duration {
	if c.After > 0 {
		return c.After
	}
	return defaultRemindAfter
}

func (c RemindConfig) max() int {
	if c.Max > 0 {
		return c.Max
	}
	return defaultRemindMax
}

func configureRemind(c *RemindConfig, getenv func(string) string) error {
	switch v, from := config.EnvAliasFrom(getenv, "REMIND"); v {
	case "", "off":
		c.On = false
	case "on":
		c.On = true
	default:
		return errors.New(from + " must be on or off")
	}
	if v, from := config.EnvAliasFrom(getenv, "REMIND_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return errors.New(from + " must be a duration of at least 1s such as 2h")
		}
		c.After = d
	}
	if v, from := config.EnvAliasFrom(getenv, "REMIND_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New(from + " must be a whole number of at least 1 such as 3")
		}
		c.Max = n
	}
	return nil
}

// RemindSubject and RemindBody are a reminder's text.
func RemindSubject(id int64, subject string) string {
	return fmt.Sprintf("[다시 알림] #%d %s", id, subject)
}

func RemindBody(id int64) string {
	return fmt.Sprintf("#%d 에 답하거나(reply) complete 하라", id)
}

func remindKey(id int64, n int) string {
	return "remind:" + strconv.FormatInt(id, 10) + ":" + strconv.Itoa(n)
}

func remindSenderKey(id int64) string { return "remind-sender:" + strconv.FormatInt(id, 10) }

// SweepReminders sends the due directive reminders and sender notices and
// returns how many it sent. base maps a mailbox name to its base role. With
// reminders off it returns at once without touching the database.
func (s *Service) SweepReminders(ctx context.Context, base func(string) string) (int, error) {
	c := s.cfg.Remind
	if !c.On {
		return 0, nil
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO remind_state(id,enabled_at) VALUES(1,?)`, ms(now)); err != nil {
		return 0, err
	}
	var enabledMS int64
	if err := s.db.QueryRowContext(ctx, `SELECT enabled_at FROM remind_state WHERE id=1`).Scan(&enabledMS); err != nil {
		return 0, err
	}
	enabled := fromMS(enabledMS)
	isBoss := s.ids.IsBossName(base)
	open, err := s.openDirectives(base, isBoss)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, d := range open {
		if d.Status != "read" || isBoss(d.Recipient) || d.At.Before(enabled) {
			continue
		}
		n, last, noticed, err := s.remindersSent(ctx, d.ID)
		if err != nil {
			return sent, err
		}
		if noticed {
			continue
		}
		if last.IsZero() {
			last = d.At
		}
		gap := c.after() << n // T, 2T, 4T, ... after the previous one
		if now.Before(last.Add(gap)) {
			continue
		}
		to, subject, body, key := d.Recipient, RemindSubject(d.ID, d.Subject), RemindBody(d.ID), remindKey(d.ID, n+1)
		if n >= c.max() {
			to, key = d.Sender, remindSenderKey(d.ID)
			subject = fmt.Sprintf("[답 없음] #%d %s", d.ID, d.Subject)
			body = fmt.Sprintf("#%d (%s) 는 다시 알림 %d번 뒤에도 답(reply)도 complete 도 없다", d.ID, d.Recipient, n)
		}
		var stored bool
		err = store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			var e error
			stored, e = mail.InsertSystemMailOnce(tx, to, subject, body, d.TaskID, "normal", key, now)
			return e
		})
		if err != nil {
			return sent, err
		}
		if stored {
			sent++
			s.wake(to)
		}
	}
	return sent, nil
}

// remindersSent reads the reminder mail already stored for directive id: how
// many reminders, when the last one (zero when none) was sent, and whether
// the sender notice went out.
func (s *Service) remindersSent(ctx context.Context, id int64) (n int, last time.Time, noticed bool, err error) {
	// A range on idem (':' + 1 is ';') reads the UNIQUE(sender,idem) index.
	prefix := "remind:" + strconv.FormatInt(id, 10) + ":"
	rows, err := s.db.QueryContext(ctx, `SELECT idem,created_at FROM messages WHERE sender=? AND (idem=? OR (idem>=? AND idem<?))`,
		identity.SystemSender, remindSenderKey(id), prefix, prefix[:len(prefix)-1]+";")
	if err != nil {
		return 0, time.Time{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var idem, created string
		if err = rows.Scan(&idem, &created); err != nil {
			return 0, time.Time{}, false, err
		}
		if idem == remindSenderKey(id) {
			noticed = true
			continue
		}
		k, e := strconv.Atoi(idem[len(prefix):])
		if e != nil {
			continue
		}
		if k > n {
			n = k
			last, _ = time.Parse(time.RFC3339Nano, created)
		}
	}
	return n, last, noticed, rows.Err()
}
