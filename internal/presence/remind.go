package presence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
)

// Directive reminders (#214).
//
// A directive that its recipient has read and not finished (an open
// directive, see openDirectives, with status read and a non-boss recipient;
// replies leave it open since #269) gets reminders from the system sender:
// the first when it has had no reply for SWITCHBOARD_REMIND_AFTER, each next
// one twice the previous gap later (T, 3T, 7T for the defaults), at most
// SWITCHBOARD_REMIND_MAX. When the directive is still open one more gap after
// the last reminder, its sender gets one notice.
//
// The schedule starts at the directive's send time, or at the time reminders
// were first switched on when the directive is older (#268), so the first
// reminder is due at max(sent, enabled_at) + T and switching on sends no
// burst at once. A reply pushes the next reminder to one gap after it. An
// open directive whose recipient sits idle is the idle check's (idle.go).
//
// SWITCHBOARD_REMIND=on turns the sweep on; off (the default) it reads and
// writes nothing. The first sweep with it on records the time in
// remind_state; switching it off and on again keeps the first time.
//
// Every reminder and notice carries an idempotency key (remind:<id>:<n>,
// remind-sender:<id>) on the messages table's UNIQUE(sender,idem), so
// repeated or concurrent sweeps and restarts send each exactly once. The
// sent mail is also the schedule's state: the next one is due a gap after
// the last one stored.

const (
	defaultRemindAfter   = 2 * time.Hour
	defaultRemindMax     = 3
	defaultRemindIdleMax = 1
)

// RemindConfig is the directive reminder settings.
type RemindConfig struct {
	On    bool          // SWITCHBOARD_REMIND=on
	After time.Duration // SWITCHBOARD_REMIND_AFTER; 0 means defaultRemindAfter
	Max   int           // SWITCHBOARD_REMIND_MAX; 0 means defaultRemindMax
	// Idle is SWITCHBOARD_REMIND_IDLE (idle.go): 0 means defaultRemindIdle,
	// negative means off.
	Idle time.Duration
	// IdleMax is SWITCHBOARD_REMIND_IDLE_MAX, the idle notices per
	// directive; 0 means defaultRemindIdleMax.
	IdleMax int
}

func (c RemindConfig) idleMax() int {
	if c.IdleMax > 0 {
		return c.IdleMax
	}
	return defaultRemindIdleMax
}

// idle is the idle threshold; 0 or less is off.
func (c RemindConfig) idle() time.Duration {
	if c.Idle == 0 {
		return defaultRemindIdle
	}
	return max(0, c.Idle)
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
	if v, from := config.EnvAliasFrom(getenv, "REMIND_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || (d != 0 && d < time.Second) {
			return errors.New(from + " must be 0 (off) or a duration of at least 1s such as 1h")
		}
		c.Idle = d
		if d == 0 {
			c.Idle = -1
		}
	}
	if v, from := config.EnvAliasFrom(getenv, "REMIND_IDLE_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return errors.New(from + " must be a whole number of at least 1 such as 1")
		}
		c.IdleMax = n
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

// RemindStatus is the reminder settings in force, as the start-up line and
// GET /api/admin/settings report them (#223). Since is when the first sweep
// with reminders on ran (remind_state), nil before that.
type RemindStatus struct {
	On           bool       `json:"on"`
	After        string     `json:"after"`
	AfterSeconds int64      `json:"after_seconds"`
	Max          int        `json:"max"`
	Since        *time.Time `json:"since"`
	Idle         string     `json:"idle"`         // SWITCHBOARD_REMIND_IDLE, "off" when 0
	IdleSeconds  int64      `json:"idle_seconds"` // 0 when off
	IdleMax      int        `json:"idle_max"`     // SWITCHBOARD_REMIND_IDLE_MAX
}

// RemindStatus reads the reminder settings and remind_state.
func (s *Service) RemindStatus(ctx context.Context) (RemindStatus, error) {
	c := s.cfg.Remind
	st := RemindStatus{On: c.On, After: shortDuration(c.after()), AfterSeconds: int64(c.after() / time.Second), Max: c.max(), Idle: "off", IdleMax: c.idleMax()}
	if d := c.idle(); d > 0 {
		st.Idle, st.IdleSeconds = shortDuration(d), int64(d/time.Second)
	}
	var enabledMS int64
	switch err := s.db.QueryRowContext(ctx, `SELECT enabled_at FROM remind_state WHERE id=1`).Scan(&enabledMS); {
	case err == nil:
		t := fromMS(enabledMS)
		st.Since = &t
	case !errors.Is(err, sql.ErrNoRows):
		return st, err
	}
	return st, nil
}

// StartLine is the one start-up log line for directive reminders:
// "directive reminders: on (after 2h, max 3, since <RFC 3339 time or first
// sweep>, idle 1h, idle max 1)" or "directive reminders: off (idle 1h, idle
// max 1)", idle being SWITCHBOARD_REMIND_IDLE and idle max
// SWITCHBOARD_REMIND_IDLE_MAX; "idle off" when the idle check is off.
func (st RemindStatus) StartLine() string {
	idle := "idle off"
	if st.Idle != "off" {
		idle = fmt.Sprintf("idle %s, idle max %d", st.Idle, st.IdleMax)
	}
	if !st.On {
		return fmt.Sprintf("directive reminders: off (%s)", idle)
	}
	since := "first sweep"
	if st.Since != nil {
		since = st.Since.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("directive reminders: on (after %s, max %d, since %s, %s)", st.After, st.Max, since, idle)
}

// shortDuration drops the zero minutes and seconds of a duration: 2h, 1h30m,
// 45s.
func shortDuration(d time.Duration) string {
	t := d.String()
	if strings.HasSuffix(t, "m0s") {
		t = strings.TrimSuffix(t, "0s")
	}
	if strings.HasSuffix(t, "h0m") {
		t = strings.TrimSuffix(t, "0m")
	}
	return t
}

// RemindSubject and RemindBody are a reminder's text.
func RemindSubject(id int64, subject string) string {
	return fmt.Sprintf("[다시 알림] #%d %s", id, subject)
}

func RemindBody(id int64) string {
	return fmt.Sprintf("#%d 가 끝났으면 reply --done 으로 보고하고, 아직이면 reply 로 진행을 알려라", id)
}

func remindKey(id int64, n int) string {
	return "remind:" + strconv.FormatInt(id, 10) + ":" + strconv.Itoa(n)
}

func remindSenderKey(id int64) string { return "remind-sender:" + strconv.FormatInt(id, 10) }

// idleAfterKey is the key of the n-th idle notice: idle-after:<id> for the
// first (as #268 stored it), idle-after:<id>:<n> after that.
func idleAfterKey(id int64, n int) string {
	k := "idle-after:" + strconv.FormatInt(id, 10)
	if n > 1 {
		k += ":" + strconv.Itoa(n)
	}
	return k
}

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
		if d.Status != "read" || isBoss(d.Recipient) {
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
			if last.Before(enabled) {
				last = enabled
			}
		}
		if d.LastAnswer.After(last) {
			last = d.LastAnswer
		}
		gap := c.after() << n // T, 2T, 4T, ... after the previous one
		if now.Before(last.Add(gap)) {
			continue
		}
		to, subject, body, key := d.Recipient, RemindSubject(d.ID, d.Subject), RemindBody(d.ID), remindKey(d.ID, n+1)
		if n >= c.max() {
			to, key = d.Sender, remindSenderKey(d.ID)
			subject = fmt.Sprintf("[안 끝남] #%d %s", d.ID, d.Subject)
			body = fmt.Sprintf("#%d (%s) 는 다시 알림 %d번 뒤에도 끝나지 않았다(reply --done·complete 없음)", d.ID, d.Recipient, n)
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
	n, err := s.sweepIdle(ctx, now, enabled, base, isBoss)
	return sent + n, err
}

// sweepIdle tells the sender of each idle open directive (idle.go) up to
// SWITCHBOARD_REMIND_IDLE_MAX times, one threshold apart, the first not
// before SWITCHBOARD_REMIND_IDLE after the reminders were switched on, so
// switching on sends no burst.
func (s *Service) sweepIdle(ctx context.Context, now, enabled time.Time, base func(string) string, isBoss func(string) bool) (int, error) {
	idle := s.cfg.Remind.idle()
	if idle <= 0 || now.Before(enabled.Add(idle)) {
		return 0, nil
	}
	live, err := s.liveSessions(now)
	if err != nil {
		return 0, err
	}
	stalls, err := s.idleOpen(now, live, base, isBoss)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, st := range stalls {
		n, last, err := s.idleNoticesSent(ctx, st.ID)
		if err != nil {
			return sent, err
		}
		if n >= s.cfg.Remind.idleMax() || (n > 0 && now.Before(last.Add(idle))) {
			continue
		}
		subject := fmt.Sprintf("[멈춤] #%d %s", st.ID, st.Subject)
		body := fmt.Sprintf("#%d (%s) 는 끝나지 않았고(reply --done·complete 없음), 세션 %s 이(가) %s째 idle 이다. 끝났는지 확인하라", st.ID, st.Recipient, strings.Join(st.Sessions, ", "), shortDuration(now.Sub(st.Since).Truncate(time.Minute)))
		var stored bool
		err = store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			var e error
			stored, e = mail.InsertSystemMailOnce(tx, st.Sender, subject, body, st.TaskID, "normal", idleAfterKey(st.ID, n+1), now)
			return e
		})
		if err != nil {
			return sent, err
		}
		if stored {
			sent++
			s.wake(st.Sender)
		}
	}
	return sent, nil
}

// idleNoticesSent reads how many idle notices directive id got and when the
// last one was sent.
func (s *Service) idleNoticesSent(ctx context.Context, id int64) (n int, last time.Time, err error) {
	first := idleAfterKey(id, 1)
	rows, err := s.db.QueryContext(ctx, `SELECT idem,created_at FROM messages WHERE sender=? AND (idem=? OR (idem>=? AND idem<?))`,
		identity.SystemSender, first, first+":", first+";")
	if err != nil {
		return 0, time.Time{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var idem, created string
		if err = rows.Scan(&idem, &created); err != nil {
			return 0, time.Time{}, err
		}
		k := 1
		if idem != first {
			if k, err = strconv.Atoi(idem[len(first)+1:]); err != nil {
				err = nil
				continue
			}
		}
		if k > n {
			n = k
			last, _ = time.Parse(time.RFC3339Nano, created)
		}
	}
	return n, last, rows.Err()
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
