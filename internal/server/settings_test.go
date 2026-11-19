package server

import (
	"bytes"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/insight"
	"github.com/sandcorporation/switchboard/internal/presence"
)

// startLog runs the start-up reminder line as run does and returns what it
// logged.
func startLog(t *testing.T, s *Server) string {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prev); log.SetFlags(flags) }()
	if err := s.logRemindStart(); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(buf.String())
}

func settingsJSON(t *testing.T, s *Server) (map[string]json.RawMessage, string) {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/settings", "", nil)
	if w.Code != 200 {
		t.Fatalf("GET settings: %d %s", w.Code, w.Body)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	return top, w.Body.String()
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// #223: one start-up line reports the directive reminders, off or on with
// the values and the first-sweep time, and GET /api/admin/settings shows
// the same values read-only.
func TestRemindStartLineAndSettingsAPI(t *testing.T) {
	s, clock := slotsWithRoles(t)
	s.admin.ProxyToken = "proxy-secret"
	if got := startLog(t, s); got != "directive reminders: off (idle 1h)" {
		t.Fatalf("off: %q", got)
	}
	s.presenceCfg.Remind = presence.RemindConfig{Idle: -1}
	if got := startLog(t, s); got != "directive reminders: off (idle off)" {
		t.Fatalf("off, idle off: %q", got)
	}
	s.presenceCfg.Remind = presence.RemindConfig{On: true, After: 90 * time.Minute, Max: 4, Idle: 45 * time.Minute}
	if got, want := startLog(t, s), "directive reminders: on (after 1h30m, max 4, since first sweep, idle 45m)"; got != want {
		t.Fatalf("on before a sweep:\n got %q\nwant %q", got, want)
	}
	top, _ := settingsJSON(t, s)
	var rs map[string]any
	if err := json.Unmarshal(top["reminders"], &rs); err != nil {
		t.Fatal(err)
	}
	if rs["on"] != true || rs["after"] != "1h30m" || rs["after_seconds"] != float64(5400) || rs["max"] != float64(4) || rs["since"] != nil || rs["idle"] != "45m" || rs["idle_seconds"] != float64(2700) {
		t.Fatalf("reminders before a sweep: %v", rs)
	}
	sweepOnce(t, s)
	at := clock.now().UTC().Format(time.RFC3339)
	if got, want := startLog(t, s), "directive reminders: on (after 1h30m, max 4, since "+at+", idle 45m)"; got != want {
		t.Fatalf("on after a sweep:\n got %q\nwant %q", got, want)
	}
	top, _ = settingsJSON(t, s)
	_ = json.Unmarshal(top["reminders"], &rs)
	if since, _ := rs["since"].(string); since == "" || !strings.HasPrefix(since, strings.TrimSuffix(at, "Z")) {
		t.Fatalf("since after a sweep: %v, want %s", rs["since"], at)
	}
	// Switching off keeps the first time, and the line says only off.
	s.presenceCfg.Remind = presence.RemindConfig{}
	if got := startLog(t, s); got != "directive reminders: off (idle 1h)" {
		t.Fatalf("off again: %q", got)
	}
	// Defaults when on with no values: 2h, 3 and idle 1h.
	s.presenceCfg.Remind = presence.RemindConfig{On: true}
	if got, want := startLog(t, s), "directive reminders: on (after 2h, max 3, since "+at+", idle 1h)"; got != want {
		t.Fatalf("defaults:\n got %q\nwant %q", got, want)
	}
}

// #223: the settings API, its reminder object and the start-up line carry
// the version and settings only, never a token, webhook address or other
// secret configured on the server.
func TestSettingsShowNoSecrets(t *testing.T) {
	s, _ := slotsWithRoles(t)
	s.admin.ProxyToken = "proxy-secret"
	secrets := []string{"proxy-secret", workerToken, "secret-hq/boss", "env-token-value", "https://discord.example/api/webhooks/1/hook-secret", "hook-secret"}
	s.tokens["svc/env"] = "env-token-value"
	s.discord = insight.ConfigureDiscord(func(n string) string {
		if strings.HasSuffix(n, "DISCORD_WEBHOOK") {
			return secrets[4]
		}
		return ""
	})
	s.hosting = true
	s.presenceCfg.Remind = presence.RemindConfig{On: true}
	top, body := settingsJSON(t, s)
	if got, want := keysOf(top), []string{"hosting", "reminders", "version"}; !slices.Equal(got, want) {
		t.Fatalf("settings keys %v, want %v", got, want)
	}
	var rs map[string]json.RawMessage
	if err := json.Unmarshal(top["reminders"], &rs); err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(rs), []string{"after", "after_seconds", "idle", "idle_seconds", "max", "on", "since"}; !slices.Equal(got, want) {
		t.Fatalf("reminder keys %v, want %v", got, want)
	}
	line := startLog(t, s)
	for _, sec := range secrets {
		if strings.Contains(body, sec) || strings.Contains(line, sec) {
			t.Errorf("secret %q in settings %s or line %q", sec, body, line)
		}
	}
	// Admin only: no admin credential, no settings.
	w := adminRequest(s, "GET", "/api/admin/settings", "", map[string]string{"X-Switchboard-Admin": ""})
	if w.Code != 401 && w.Code != 403 {
		t.Fatalf("without admin: %d", w.Code)
	}
}
