package server

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/asks"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/presence"
	"github.com/sandcorporation/switchboard/internal/reports"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// queryer is the part of *sql.DB and *sql.Tx the server code reads through,
// so the same lookups work inside a transaction.
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// inTx runs f in one transaction on the server's single connection.
func (s *Server) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	return store.InTx(ctx, s.db, f)
}

// authenticate is the role check every /api/ request goes through: the bearer
// token against X-Switchboard-Role (or the old-prefix role header), by the
// rules in identity.Service.Authenticate.
func (s *Server) authenticate(r *http.Request) (identity.Principal, bool) {
	return s.ids().Authenticate(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.header(r, "Role"))
}

// ids is the identity rules over this server's database, clock, boss roles
// and group cache. A revoke logs the slot offline in the presence history.
func (s *Server) ids() *identity.Service {
	return identity.New(s.db, s.now, s.bossRoles, &s.groups, presence.LogOffline)
}

// mail is the mailbox over this server's database, clock, settings and hub.
// An event stream re-checks its credentials with authenticate.
func (s *Server) mail() *mail.Service {
	return mail.New(s.db, s.now, &s.mailCfg, &s.hub, s.done, s.ids(), func(r *http.Request) bool {
		_, ok := s.authenticate(r)
		return ok
	})
}

// presence is the slot and presence rules over this server's database,
// clock and settings. Held mail a slot change delivers wakes the hub.
func (s *Server) presence() *presence.Service {
	return presence.New(s.db, s.now, &s.presenceCfg, s.ids(), s.hub.Publish)
}

// asks is the question, decision and permission rules over this server's
// database, clock and settings. A new question whose settings ask for it is
// posted to Discord; an answer goes to the asking session's live slot
// (internal/presence).
func (s *Server) asks() *asks.Service {
	return asks.New(s.db, s.now, &s.asksCfg, s.ids(), s.mail(), &s.hub, s.done, s.discord.NoticeQuestion,
		asks.SlotRules{Asking: presence.AskingSlot, Answer: presence.AnswerSlot})
}

// insight is the metrics and alert rules over this server's database, clock,
// alert thresholds and boss roles. Alert mail wakes the hub.
func (s *Server) insight() *insight.Service {
	return insight.New(s.db, s.now, s.ids(), s.alerts, s.bossRoles, s.hub.Publish, s.aliasUse)
}

// reports is the boss reports over this server's database, clock and
// identity rules, with the base resolver the answered rule compares.
func (s *Server) reports() *reports.Service {
	return reports.New(s.db, s.now, s.ids(), insight.NewBaseResolver(s.db).Of)
}

// progressAPI serves the progress view with the metrics alert limit and base
// resolver (internal/insight).
func (s *Server) progressAPI(w http.ResponseWriter, r *http.Request, allowed bool) {
	s.presence().ProgressAPI(w, r, allowed, s.insight().Limits().Pending, insight.NewBaseResolver(s.db).Of)
}

// progress is the progress report as of now.
func (s *Server) progress() (wire.ProgressReport, error) {
	return s.presence().Progress(s.insight().Limits().Pending, insight.NewBaseResolver(s.db).Of)
}

// migrateAnsweredRule runs presence.MigrateAnsweredRule at start-up and logs
// one line when it wrote the stored version.
func (s *Server) migrateAnsweredRule(logf func(string, ...any)) error {
	m, err := s.presence().MigrateAnsweredRule(insight.NewBaseResolver(s.db).Of)
	if err != nil || !m.Changed {
		return err
	}
	logf("%s", m.Line())
	return nil
}
