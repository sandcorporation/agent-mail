package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/presence"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Stall notices.
//
// A periodic sweep (SWITCHBOARD_SWEEP_INTERVAL, default 30s) sends ordinary
// mail from the system sender: offline notices to a session's presence parent
// (presence.Service.SweepOffline) and label stall notices to the sender of
// mail held longer than SWITCHBOARD_LABEL_STALL (mail.Service.SweepLabels).
// The same sweep sends due metrics alerts and directive reminders
// (presence.SweepReminders, SWITCHBOARD_REMIND, off by default) and prunes
// old presence_log, alerts and terminal_log rows.

const (
	defaultSweepInterval = 30 * time.Second
	defaultLabelStall    = 10 * time.Minute
)

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
		{"PERMISSION_TTL", &s.asksCfg.PermissionTTL, time.Second, "10m"},
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
		if !wire.ValidRole(r) || identity.Reserved(r) {
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
				res, err := s.sweep(context.Background())
				if err != nil {
					log.Printf("stall sweep: %v", err)
				}
				s.publishSweep(res)
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
	Remind  int
}

// sweep sends every due stall notice and metrics alert once and prunes old
// presence_log, alerts and terminal_log rows.
// It is safe to run repeatedly.
func (s *Server) sweep(ctx context.Context) (sweepResult, error) {
	var out sweepResult
	n, err := s.presence().SweepOffline(ctx)
	out.Offline = n
	if err != nil {
		return out, err
	}
	if out.Label, err = s.mail().SweepLabels(ctx, s.labelStallAfter()); err != nil {
		return out, err
	}
	if out.Alerts, err = s.insight().SweepAlerts(ctx); err != nil {
		return out, err
	}
	if out.Remind, err = s.presence().SweepReminders(ctx, insight.NewBaseResolver(s.db).Of); err != nil {
		return out, err
	}
	if _, err = presence.PruneLog(s.db, s.now()); err != nil {
		return out, err
	}
	_, err = hosting.PruneLog(s.db, s.now())
	return out, err
}

// publishSweep tells live dashboards what one sweep's notices changed.
func (s *Server) publishSweep(res sweepResult) {
	var kinds []string
	if res.Offline > 0 {
		kinds = append(kinds, "sessions", "progress", "mail")
	}
	if res.Label > 0 {
		kinds = append(kinds, "mail", "progress")
	}
	if res.Alerts > 0 {
		kinds = append(kinds, "mail", "metrics")
	}
	if res.Remind > 0 {
		kinds = append(kinds, "mail")
	}
	s.live.Publish(kinds...)
}
