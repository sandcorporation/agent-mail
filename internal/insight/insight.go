// Package insight is what the server learns from the mail flow: the metrics
// report (metrics.go), the metrics alerts the sweep sends to boss roles
// (alerts.go), and the Discord notice for new questions (discord.go).
package insight

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Service is the metrics and alert rules over the mail tables. It holds only
// what those rules need, so callers build one per use.
type Service struct {
	db        *sql.DB
	clock     func() time.Time
	ids       *identity.Service
	alerts    *Thresholds
	bossRoles map[string]bool
	wake      func(box string)
	aliasUse  func() wire.AliasUse
}

// New returns the metrics and alert rules over db. clock is the current time,
// ids the identity rules, alerts the thresholds (nil means
// DefaultThresholds), bossRoles the configured boss roles (alerts go to
// them), wake tells event subscribers of a mailbox that mail arrived (the
// mail hub's Publish), and aliasUse reports old env and header use for the
// admin metrics.
func New(db *sql.DB, clock func() time.Time, ids *identity.Service, alerts *Thresholds, bossRoles map[string]bool, wake func(box string), aliasUse func() wire.AliasUse) *Service {
	return &Service{db: db, clock: clock, ids: ids, alerts: alerts, bossRoles: bossRoles, wake: wake, aliasUse: aliasUse}
}

func (s *Service) now() time.Time { return s.clock() }

func (s *Service) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	return store.InTx(ctx, s.db, f)
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
