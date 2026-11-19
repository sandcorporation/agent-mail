package server

import (
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Running background sub-agents (#215): stored per slot, shown in presence
// and progress, and an idle session with some ranks busy in the badge.
func TestPresenceSubagents(t *testing.T) {
	s, clock := presenceServer(t)
	// An old-plugin body (no field) is accepted, and so is a field this
	// server does not know.
	slotDo(t, s, "register", report("m", "/a", "idle", nil, ""))
	if w := callAs(s, "POST", "/api/slots/heartbeat", workerToken, "team/worker", map[string]any{"machine": "m", "directory": "/a", "state": "idle", "some_future_field": 1}); w.Code != 200 {
		t.Fatalf("unknown field: %d %s", w.Code, w.Body)
	}

	req := report("m", "/a", "idle", nil, "")
	req.Subagents = int64p(2)
	slotDo(t, s, "heartbeat", req)
	out, body := presenceAs(t, s, "team/worker-1")
	if v := out.Sessions[0]; v.Subagents != 2 || v.Status != "idle" || !strings.Contains(body, `"subagents":2`) {
		t.Fatalf("subagents 2: %+v %s", v, body)
	}
	team := serviceNamed(t, mustProgress(t, s), "team")
	if len(team.Sessions) != 1 || team.Sessions[0].Subagents != 2 || team.Sessions[0].State != "idle" || team.Badge != "busy" {
		t.Fatalf("progress with subagents: %+v", team)
	}

	// The next report without the field sets 0, and the JSON leaves it out.
	slotDo(t, s, "heartbeat", report("m", "/a", "idle", nil, ""))
	out, body = presenceAs(t, s, "team/worker-1")
	if out.Sessions[0].Subagents != 0 || strings.Contains(body, "subagents") {
		t.Fatalf("subagents after plain heartbeat: %s", body)
	}
	if team = serviceNamed(t, mustProgress(t, s), "team"); team.Sessions[0].Subagents != 0 || team.Badge != "idle" {
		t.Fatalf("progress after plain heartbeat: %+v", team)
	}

	// Absurd values are clamped.
	req.Subagents = int64p(1000)
	slotDo(t, s, "heartbeat", req)
	if v := onlySession(t, s, "team/worker-1"); v.Subagents != wire.MaxSubagents {
		t.Fatalf("clamp: %d", v.Subagents)
	}

	// Malformed values are 400 and change nothing.
	for name, body := range map[string]any{
		"negative": map[string]any{"machine": "m", "directory": "/a", "subagents": -1},
		"string":   map[string]any{"machine": "m", "directory": "/a", "subagents": "2"},
		"float":    map[string]any{"machine": "m", "directory": "/a", "subagents": 1.5},
	} {
		for _, action := range []string{"register", "heartbeat", "takeover"} {
			if w := callAs(s, "POST", "/api/slots/"+action, workerToken, "team/worker", body); w.Code != 400 {
				t.Fatalf("%s with %s subagents: %d %s", action, name, w.Code, w.Body)
			}
		}
	}
	if v := onlySession(t, s, "team/worker-1"); v.Subagents != wire.MaxSubagents {
		t.Fatalf("rejected report changed subagents: %d", v.Subagents)
	}

	// Offline: missed heartbeats hide it, release clears it.
	clock.add(31 * time.Second)
	if v := onlySession(t, s, "team/worker-1"); v.Online || v.Subagents != 0 {
		t.Fatalf("offline session still shows subagents: %+v", v)
	}
	req.Subagents = int64p(3)
	slotDo(t, s, "register", req)
	if v := onlySession(t, s, "team/worker-1"); v.Subagents != 3 {
		t.Fatalf("register with subagents: %+v", v)
	}
	slotDo(t, s, "release", ident("m", "/a"))
	var n int
	if err := s.db.QueryRow(`SELECT subagents FROM slots WHERE base='team/worker' AND number=1`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("release left subagents %d %v", n, err)
	}
}

// TestCLIHarnessIgnoresMachineEnv: the machine's own client variables, under
// either prefix, never reach the CLI child; it uses the test's profile (#246).
func TestCLIHarnessIgnoresMachineEnv(t *testing.T) {
	for _, name := range []string{"ROLE", "TOKEN", "ENDPOINT", "PROFILE", "CONFIG_DIR", "MACHINE", "LABEL", "PARENT"} {
		t.Setenv(config.EnvPrefix+name, "bogus-new")
		t.Setenv(config.LegacyEnv(name), "bogus-old")
	}
	h := newCLIHarness(t)
	h.configure(nil, "b")
	h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/a", "--state", "idle")
	if out := h.mustRun(nil, "status", "b-1", "--json"); !strings.Contains(out, `"role":"b-1","base":"b"`) {
		t.Fatalf("status --json: %q", out)
	}
}

func TestSubagentsCommands(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "b")
	h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/a", "--state", "idle", "--subagents", "2")
	h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/b", "--state", "busy")
	lines := strings.Split(strings.TrimSpace(h.mustRun(nil, "status")), "\n")
	if f := strings.Fields(lines[0]); f[len(f)-1] != "SUBAGENTS" {
		t.Fatalf("header: %q", lines[0])
	}
	for i, want := range []string{"2", "-"} {
		if f := strings.Fields(lines[i+1]); f[len(f)-1] != want {
			t.Fatalf("row %d: %q, want SUBAGENTS %s", i, lines[i+1], want)
		}
	}
	if out := h.mustRun(nil, "status", "b-1", "--json"); !strings.Contains(out, `"subagents":2`) {
		t.Fatalf("status --json: %q", out)
	}
	for _, args := range [][]string{
		{"slot", "heartbeat", "--subagents", "-1"},
		{"slot", "heartbeat", "--subagents", "two"},
	} {
		if r := h.run(nil, args...); r.code != cli.ExitUsage {
			t.Fatalf("%v: %d %s", args, r.code, r.stderr)
		}
	}
}
