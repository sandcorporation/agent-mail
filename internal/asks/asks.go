// Package asks is everything a boss answers: questions and their per-role
// settings (questions*.go), the decisions made from answered questions
// (decisions.go), and tool permission requests (permissions.go).
package asks

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/store"
)

// Config holds the ask settings.
type Config struct {
	PermissionTTL time.Duration // SWITCHBOARD_PERMISSION_TTL; 0 means 10m
}

// Service is the question, decision and permission rules over their tables.
// It holds only what those rules need, so callers build one per use.
type Service struct {
	db     *sql.DB
	clock  func() time.Time
	cfg    *Config
	ids    *identity.Service
	mail   *mail.Service
	hub    *mail.Hub
	done   <-chan struct{}
	notify func(id, asker, header string)
}

// New returns the ask rules over db. clock is the current time, cfg the ask
// settings, ids the identity rules, ml the mailbox (its long-poll wait
// rules), hub the server's event hub (asks and answers wake it), done is
// closed on shutdown (it ends question long polls), and notify sends the
// outside notice for a new question whose settings ask for one (Discord).
func New(db *sql.DB, clock func() time.Time, cfg *Config, ids *identity.Service, ml *mail.Service, hub *mail.Hub, done <-chan struct{}, notify func(id, asker, header string)) *Service {
	return &Service{db: db, clock: clock, cfg: cfg, ids: ids, mail: ml, hub: hub, done: done, notify: notify}
}

func (s *Service) now() time.Time { return s.clock() }

func (s *Service) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	return store.InTx(ctx, s.db, f)
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
