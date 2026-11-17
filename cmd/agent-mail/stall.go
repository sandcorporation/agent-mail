package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/agent-mail/internal/config"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Stall notices.
//
// A periodic sweep (AGENT_MAIL_SWEEP_INTERVAL, default 30s) sends ordinary
// mail from the system sender:
//
//   - When a session's lease ends without a release (missed heartbeats), its
//     presence parent gets one notice. Every report clears the mark, so a
//     session that comes back and goes offline again is noticed again. A
//     release marks the row noticed: an intended stop is not a stall.
//   - When mail to label:<base>/<label> stays held for AGENT_MAIL_LABEL_STALL
//     (default 10m) because no session with that label connected, its sender
//     gets one notice per sweep and label covering every message that crossed
//     the limit since the last notice.
//
// Each notice is stored in the same transaction that marks the row handled,
// so running the sweep any number of times (or concurrently) sends it once.

// systemSender is the sender of notices. Nobody can authenticate as it or as
// a numbered form of it (see reservedRole), and mail to it is refused.
const systemSender = "switchboard/system"

const (
	defaultSweepInterval = 30 * time.Second
	defaultLabelStall    = 10 * time.Minute
	noticeListMax        = 5 // held message IDs quoted in a label notice
)

// reservedRole reports whether name is the system sender or a numbered form
// of it. Such names never authenticate and never receive mail, even when an
// old database or AGENT_MAIL_TOKENS holds a role row by that name.
func reservedRole(name string) bool {
	if isSystemSender(name) {
		return true
	}
	base, _, ok := wire.SplitSlot(name)
	return ok && isSystemSender(base)
}

func (s *Server) sweepInterval() time.Duration {
	if s.sweepEvery > 0 {
		return s.sweepEvery
	}
	return defaultSweepInterval
}

func (s *Server) labelStallAfter() time.Duration {
	if s.labelStall > 0 {
		return s.labelStall
	}
	return defaultLabelStall
}

// configureStall reads the sweep, label stall, permission and boss settings.
func configureStall(s *Server, getenv func(string) string) error {
	for _, d := range []struct {
		name string
		dst  *time.Duration
		min  time.Duration
		hint string
	}{
		{"SWEEP_INTERVAL", &s.sweepEvery, time.Second, "30s"},
		{"LABEL_STALL", &s.labelStall, time.Second, "10m"},
		{"PERMISSION_TTL", &s.permissionTTL, time.Second, "10m"},
	} {
		v, from := config.EnvAliasFrom(getenv, d.name)
		if v == "" {
			continue
		}
		t, err := time.ParseDuration(v)
		if err != nil || t < d.min {
			return fmt.Errorf("%s must be a duration of at least %s such as %s", from, d.min, d.hint)
		}
		*d.dst = t
	}
	s.bossRoles = map[string]bool{}
	roles, from := config.EnvAliasFrom(getenv, "BOSS_ROLES")
	for _, r := range strings.Split(roles, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !wire.ValidRole(r) || reservedRole(r) {
			return fmt.Errorf("%s: invalid role %q", from, r)
		}
		s.bossRoles[r] = true
	}
	return nil
}

// startSweeper runs sweep every sweepInterval until s.done closes. The
// returned function waits for the goroutine to finish.
func (s *Server) startSweeper() func() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.sweepInterval())
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				if _, err := s.sweep(context.Background()); err != nil {
					log.Printf("stall sweep: %v", err)
				}
			}
		}
	}()
	return wg.Wait
}

// sweepResult counts the notices one sweep sent.
type sweepResult struct {
	Offline int
	Label   int
	Alerts  int
}

// sweep sends every due stall notice and metrics alert once and prunes old
// presence_log and alerts rows.
// It is safe to run repeatedly.
func (s *Server) sweep(ctx context.Context) (sweepResult, error) {
	var out sweepResult
	n, err := s.sweepOffline(ctx)
	out.Offline = n
	if err != nil {
		return out, err
	}
	if out.Label, err = s.sweepLabels(ctx); err != nil {
		return out, err
	}
	if out.Alerts, err = s.sweepAlerts(ctx); err != nil {
		return out, err
	}
	_, err = prunePresenceLog(s.db, s.now())
	return out, err
}

func (s *Server) sweepOffline(ctx context.Context) (int, error) {
	due, err := s.offlineUnnoticed()
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, v := range due {
		var to string
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			ok, err := markOffline(tx, v, s.now())
			if err != nil || !ok {
				return err
			}
			if err = logPresenceOffline(tx, v.Base, v.Number, v.LeaseExpiresAt); err != nil {
				return err
			}
			box, ok := mailbox(tx, v.Parent)
			if v.Parent == "" || !ok || reservedRole(v.Parent) {
				if v.Parent != "" {
					log.Printf("offline notice for %s skipped: parent %q is not a registered role", v.Role, v.Parent)
				}
				return nil
			}
			if self, _ := mailbox(tx, v.Role); box == self {
				return nil
			}
			to = box
			return insertSystemMail(tx, box, "offline: "+v.Role, offlineBody(v), "", "normal", "offline:"+v.Role+":"+strconv.FormatInt(ms(v.LeaseExpiresAt), 10), s.now())
		})
		if err != nil {
			return sent, err
		}
		if to != "" {
			sent++
			s.hub.publish(to)
		}
	}
	return sent, nil
}

func offlineBody(v presenceView) string {
	var b strings.Builder
	b.WriteString("Session " + v.Role)
	var parts []string
	if v.Label != "" {
		parts = append(parts, "label "+v.Label)
	}
	parts = append(parts, "machine "+v.Machine, "directory "+v.Directory)
	b.WriteString(" (" + strings.Join(parts, ", ") + ")")
	b.WriteString(" went offline at " + v.LeaseExpiresAt.UTC().Format(time.RFC3339))
	if v.ReportedAt != nil {
		b.WriteString("; last report " + v.ReportedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString(".")
	return b.String()
}

// markOffline is markOfflineNoticed inside a transaction.
func markOffline(tx *sql.Tx, v presenceView, now time.Time) (bool, error) {
	res, err := tx.Exec(`UPDATE slots SET offline_noticed_at=? WHERE base=? AND number=? AND lease_until=? AND lease_until<=? AND offline_noticed_at=0`,
		ms(now), v.Base, v.Number, ms(v.LeaseExpiresAt), ms(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Server) sweepLabels(ctx context.Context) (int, error) {
	now := s.now()
	limit := now.Add(-s.labelStallAfter())
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
		err := s.inTx(ctx, func(tx *sql.Tx) error {
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
			box, ok := mailbox(tx, k.sender)
			if !ok || reservedRole(k.sender) {
				return nil
			}
			to = box
			return insertSystemMail(tx, box, "label not connected: "+k.recipient, labelBody(k.recipient, marked, s.labelStallAfter()), "", "normal", "", now)
		})
		if err != nil {
			return sent, err
		}
		if to != "" {
			sent++
			s.hub.publish(to)
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

// insertSystemMail stores mail from systemSender inside tx. idem, when not
// empty, makes a second insert with the same key a no-op.
func insertSystemMail(tx *sql.Tx, to, subject, body, task, priority, idem string, now time.Time) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO messages(sender,recipient,subject,body,created_at,task_id,priority,status,idem) VALUES(?,?,?,?,?,?,?,'pending',?)`,
		systemSender, to, subject, body, now.UTC().Format(time.RFC3339Nano), nullable(task), priority, nullable(idem))
	return err
}
