package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// cliEnv is os.Environ() without any variable internal/config reads (both
// prefixes), for CLI helper processes: a machine's own setup, such as an
// exported AGENT_MAIL_ROLE, never reaches a test's child (#246). Callers
// append the variables the test means.
func cliEnv() []string { return config.WithoutOwnEnv(os.Environ()) }

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestServerConfigReadsBothEnvNames(t *testing.T) {
	t.Parallel()
	s := testServer(t)
	if err := configureStall(s, fakeEnv(map[string]string{"SWITCHBOARD_BOSS_ROLES": "hq/boss"})); err != nil || !s.bossRoles["hq/boss"] {
		t.Fatalf("new name: %v %v", err, s.bossRoles)
	}
	if err := configureStall(s, fakeEnv(map[string]string{"AGENT_MAIL_BOSS_ROLES": "old/boss"})); err != nil || !s.bossRoles["old/boss"] {
		t.Fatalf("old name: %v %v", err, s.bossRoles)
	}
	if err := configureStall(s, fakeEnv(map[string]string{"SWITCHBOARD_BOSS_ROLES": "new/boss", "AGENT_MAIL_BOSS_ROLES": "old/boss"})); err != nil || !s.bossRoles["new/boss"] || s.bossRoles["old/boss"] {
		t.Fatalf("both: %v %v", err, s.bossRoles)
	}
}

func TestHeadersAcceptBothNamesAndCountOld(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	get := func(set map[string]string) int {
		r := httptest.NewRequest("GET", "/api/messages", nil)
		r.Header.Set("Authorization", "Bearer secret-a")
		for k, v := range set {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.api(w, r)
		return w.Code
	}
	if c := get(map[string]string{"X-Switchboard-Role": "a"}); c != 200 {
		t.Fatalf("new header: %d", c)
	}
	if n := s.oldHeaders.Snapshot()[config.LegacyHeaderPrefix+"Role"]; n != 0 {
		t.Fatalf("new header counted as old: %d", n)
	}
	if c := get(map[string]string{config.LegacyHeaderPrefix + "Role": "a"}); c != 200 {
		t.Fatalf("old header: %d", c)
	}
	// The new header wins: a wrong old one beside it is ignored.
	if c := get(map[string]string{"X-Switchboard-Role": "a", config.LegacyHeaderPrefix + "Role": "b"}); c != 200 {
		t.Fatalf("both headers: %d", c)
	}
	if n := s.oldHeaders.Snapshot()[config.LegacyHeaderPrefix+"Role"]; n != 1 {
		t.Fatalf("old header count=%d, want 1", n)
	}

	// Admin and dashboard headers: both names.
	newAdmin := map[string]string{"X-Switchboard-Admin": "proxy-secret"}
	if w := adminRequestWith(s, "GET", "/api/admin/metrics", "", newAdmin); w.Code != 200 {
		t.Fatalf("new admin header: %d", w.Code)
	}
	oldAdmin := map[string]string{config.LegacyHeaderPrefix + "Admin": "proxy-secret"}
	if w := adminRequestWith(s, "GET", "/api/admin/metrics", "", oldAdmin); w.Code != 200 {
		t.Fatalf("old admin header: %d", w.Code)
	}
	w := adminRequestWith(s, "GET", "/api/admin/metrics", "", oldAdmin)
	var m wire.MetricsReport
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.Aliases == nil {
		t.Fatalf("admin metrics aliases: %v %s", err, w.Body)
	}
	if m.Aliases.OldHeaders[config.LegacyHeaderPrefix+"Admin"] != 2 || m.Aliases.OldHeaders[config.LegacyHeaderPrefix+"Role"] != 1 || m.Aliases.OldEnv == nil {
		t.Fatalf("aliases: %+v", m.Aliases)
	}
	// The boss-facing /api/metrics keeps its shape.
	t.Setenv("AGENT_MAIL_BOSS_ROLES", "a")
	if err := configureStall(s, os.Getenv); err != nil {
		t.Fatal(err)
	}
	if w := call(s, "GET", "/api/metrics", "secret-a", nil, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &map[string]any{}) != nil || strings.Contains(w.Body.String(), `"aliases"`) {
		t.Fatalf("boss metrics: %d %s", w.Code, w.Body)
	}
}

func TestDashboardWriteAcceptsBothHeaders(t *testing.T) {
	t.Parallel()
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	for _, h := range []string{"X-Switchboard-Dashboard", config.LegacyHeaderPrefix + "Dashboard"} {
		hs := map[string]string{"Content-Type": "application/json", h: "1", "Sec-Fetch-Site": "same-origin"}
		if w := adminRequest(s, "PUT", "/api/admin/question-settings/a", `{}`, hs); w.Code == 403 {
			t.Fatalf("%s refused: %d %s", h, w.Code, w.Body)
		}
	}
	if s.oldHeaders.Snapshot()[config.LegacyHeaderPrefix+"Dashboard"] != 1 {
		t.Fatalf("dashboard count: %v", s.oldHeaders.Snapshot())
	}
}

func adminRequestWith(s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}

// TestCLIProfileOldAndNewEnv runs the CLI with the profile picked by the old
// and by the new variable, and with the profile saved only in the old
// <config>/<config.LegacyConfigDir>/ directory (copied once into <config>/switchboard/).
func TestCLIProfileOldAndNewEnv(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.mustRun(nil, "--profile", "pa", "config", "set", "--endpoint", h.url, "--role", "a", "--token-file", h.tokens["a"])
	h.mustRun(nil, "--profile", "pa", "send", "--to", "b", "--subject", "alias-sent", "--body", "x")
	for _, env := range [][]string{{"AGENT_MAIL_PROFILE=pa"}, {"SWITCHBOARD_PROFILE=pa"}, {"SWITCHBOARD_PROFILE=pa", "AGENT_MAIL_PROFILE=nope"}} {
		if out := h.mustRun(env, "outbox"); !strings.Contains(out, "alias-sent") {
			t.Fatalf("%v outbox: %s", env, out)
		}
	}
	if out := h.mustRun(nil, "send", "--help"); !strings.Contains(out, "Usage: switchboard send") {
		t.Fatalf("help under the new name: %s", out)
	}
	if out := h.mustRun([]string{"SWITCHBOARD_TEST_ARG0=/usr/bin/" + config.LegacyConfigDir}, "--help"); !strings.Contains(out, "Usage: "+config.LegacyConfigDir+" [--profile") {
		t.Fatalf("help under the old name: %s", out)
	}
	if r := h.run([]string{"SWITCHBOARD_PROFILE=nope", "AGENT_MAIL_PROFILE=pa"}, "outbox"); r.code == cli.ExitOK {
		t.Fatal("the new variable must win over the old one")
	}

	// No CONFIG_DIR: the profile lives in the user config dir; only the old directory has it.
	cfg := t.TempDir()
	root := userConfigDirUnder(cfg)
	oldDir := filepath.Join(root, config.LegacyConfigDir)
	if err := os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(h.dir, "pa.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(oldDir, "legacy.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"AGENT_MAIL_CONFIG_DIR=", "XDG_CONFIG_HOME=" + cfg, "HOME=" + cfg, "AppData=" + cfg, "SWITCHBOARD_PROFILE=legacy"}
	if out := h.mustRun(env, "outbox"); !strings.Contains(out, "alias-sent") {
		t.Fatalf("old dir profile: %s", out)
	}
	// The first run copied it into the new directory; the old file stays.
	newFile := filepath.Join(root, "switchboard", "legacy.json")
	copied, err := os.ReadFile(newFile)
	if err != nil || string(copied) != string(b) {
		t.Fatalf("profile not copied to the new directory: %v", err)
	}
	if _, err = os.Stat(filepath.Join(oldDir, "legacy.json")); err != nil {
		t.Fatalf("old profile removed: %v", err)
	}
	// From then on only the new file is read and written.
	if err = os.WriteFile(filepath.Join(oldDir, "legacy.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := h.mustRun(env, "outbox"); !strings.Contains(out, "alias-sent") {
		t.Fatalf("second run read the old directory: %s", out)
	}
	h.mustRun(env, "config", "set", "--endpoint", h.url)
	if old, _ := os.ReadFile(filepath.Join(oldDir, "legacy.json")); string(old) != "{}" {
		t.Fatal("config set wrote the old directory")
	}
	// A new profile goes to the new directory.
	h.mustRun([]string{"AGENT_MAIL_CONFIG_DIR=", "XDG_CONFIG_HOME=" + cfg, "HOME=" + cfg, "AppData=" + cfg}, "--profile", "fresh", "config", "set", "--endpoint", h.url, "--role", "a", "--token-file", h.tokens["a"])
	if _, err := os.Stat(filepath.Join(root, "switchboard", "fresh.json")); err != nil {
		t.Fatalf("new profile not in the new directory: %v", err)
	}
}

// userConfigDirUnder is where os.UserConfigDir points in a CLI child whose
// HOME, XDG_CONFIG_HOME and AppData are all home: home itself on Linux and
// Windows, home/Library/Application Support on macOS.
func userConfigDirUnder(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return home
}
