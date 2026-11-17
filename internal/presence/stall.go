package presence

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/mail"
	"github.com/sandcorporation/agent-mail/internal/store"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Offline notices.
//
// When a session's lease ends without a release (missed heartbeats), its
// presence parent gets one notice from the system sender. Every report clears
// the mark, so a session that comes back and goes offline again is noticed
// again. A release marks the row noticed: an intended stop is not a stall.
//
// The notice is stored in the same transaction that marks the row handled,
// so running the sweep any number of times (or concurrently) sends it once.

// SweepOffline sends the due offline notices and returns how many it sent.
func (s *Service) SweepOffline(ctx context.Context) (int, error) {
	due, err := s.OfflineUnnoticed()
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, v := range due {
		var to string
		err := store.InTx(ctx, s.db, func(tx *sql.Tx) error {
			ok, err := markOffline(tx, v, s.now())
			if err != nil || !ok {
				return err
			}
			if err = LogOffline(tx, v.Base, v.Number, v.LeaseExpiresAt); err != nil {
				return err
			}
			box, ok := identity.Mailbox(tx, v.Parent)
			if v.Parent == "" || !ok || identity.Reserved(v.Parent) {
				if v.Parent != "" {
					log.Printf("offline notice for %s skipped: parent %q is not a registered role", v.Role, v.Parent)
				}
				return nil
			}
			if self, _ := identity.Mailbox(tx, v.Role); box == self {
				return nil
			}
			to = box
			return mail.InsertSystemMail(tx, box, "offline: "+v.Role, offlineBody(v), "", "normal", "offline:"+v.Role+":"+strconv.FormatInt(ms(v.LeaseExpiresAt), 10), s.now())
		})
		if err != nil {
			return sent, err
		}
		if to != "" {
			sent++
			s.wake(to)
		}
	}
	return sent, nil
}

func offlineBody(v wire.PresenceView) string {
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

// markOffline is MarkOfflineNoticed inside a transaction.
func markOffline(tx *sql.Tx, v wire.PresenceView, now time.Time) (bool, error) {
	res, err := tx.Exec(`UPDATE slots SET offline_noticed_at=? WHERE base=? AND number=? AND lease_until=? AND lease_until<=? AND offline_noticed_at=0`,
		ms(now), v.Base, v.Number, ms(v.LeaseExpiresAt), ms(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
