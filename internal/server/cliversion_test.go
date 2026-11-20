package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// slotDoVersion is slotDo with X-Switchboard-Version set to version (empty:
// no header, an older CLI).
func slotDoVersion(t *testing.T, s *Server, action, version string, req wire.SlotRequest) {
	t.Helper()
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(req)
	r := httptest.NewRequest("POST", "/api/slots/"+action, &b)
	r.Header.Set("Authorization", "Bearer "+workerToken)
	r.Header.Set("X-Switchboard-Role", "team/worker")
	if version != "" {
		r.Header.Set("X-Switchboard-Version", version)
	}
	w := httptest.NewRecorder()
	s.api(w, r)
	if w.Code != 200 {
		t.Fatalf("%s %+v: %d %s", action, req, w.Code, w.Body)
	}
}

// cliServer is alertServer with the minimum client version v0.26.
func cliServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s, clock := alertServer(t)
	s.admin.ProxyToken = "proxy-secret"
	configureCLIMin(s, func(string) string { return "" }, "v0.26.3", t.Logf)
	return s, clock
}

func adminPresence(t *testing.T, s *Server) map[string]wire.PresenceView {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/presence", "", nil)
	if w.Code != 200 {
		t.Fatalf("admin presence: %d %s", w.Code, w.Body)
	}
	var l wire.PresenceList
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	out := map[string]wire.PresenceView{}
	for _, v := range l.Sessions {
		out[v.Role] = v
	}
	return out
}

func adminProgress(t *testing.T, s *Server) wire.ProgressReport {
	t.Helper()
	w := adminRequest(s, "GET", "/api/admin/progress", "", nil)
	if w.Code != 200 {
		t.Fatalf("admin progress: %d %s", w.Code, w.Body)
	}
	var r wire.ProgressReport
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func cliMail(t *testing.T, s *Server) []string {
	t.Helper()
	var out []string
	for _, m := range systemMail(t, s, "secret-a", "a") {
		if strings.HasPrefix(m.Subject, "[옛 판] ") {
			out = append(out, m.Subject+"\n"+m.Body)
		}
	}
	return out
}

func TestHeartbeatRecordsCLIVersion(t *testing.T) {
	s, _ := cliServer(t)
	slotDoVersion(t, s, "register", "v0.26.0", ident("machine-a", "/w"))
	if v := adminPresence(t, s)["team/worker-1"]; v.CLIVersion != "v0.26.0" || v.CLI != wire.CLICurrent {
		t.Fatalf("after register: %+v", v)
	}
	slotDoVersion(t, s, "heartbeat", "v0.16.0", ident("machine-a", "/w"))
	if v := adminPresence(t, s)["team/worker-1"]; v.CLIVersion != "v0.16.0" || v.CLI != wire.CLIOld {
		t.Fatalf("after heartbeat: %+v", v)
	}
	// An older CLI sends no header: unknown, not old.
	slotDoVersion(t, s, "heartbeat", "", ident("machine-a", "/w"))
	if v := adminPresence(t, s)["team/worker-1"]; v.CLIVersion != wire.CLIUnknown || v.CLI != wire.CLINoInfo {
		t.Fatalf("without header: %+v", v)
	}
	// A development build is 판 모름 too.
	slotDoVersion(t, s, "heartbeat", "dev", ident("machine-a", "/w"))
	if v := adminPresence(t, s)["team/worker-1"]; v.CLIVersion != "dev" || v.CLI != wire.CLINoInfo {
		t.Fatalf("dev build: %+v", v)
	}
}

func TestCLIBehindInProgressWaitList(t *testing.T) {
	s, _ := cliServer(t)
	slotDoVersion(t, s, "register", "v0.26.1", ident("machine-a", "/a"))
	slotDoVersion(t, s, "register", "v0.16.0", ident("machine-b", "/b1"))
	slotDoVersion(t, s, "register", "v0.25.9", ident("machine-b", "/b2"))
	slotDoVersion(t, s, "register", "", ident("machine-c", "/c"))
	slotDoVersion(t, s, "register", "dev", ident("machine-d", "/d"))

	got := adminProgress(t, s).CLIBehind
	want := []wire.CLIBehind{
		{Machine: "machine-b", Version: "v0.16.0", Roles: []string{"team/worker-2", "team/worker-3"}, RoleVersions: []string{"v0.16.0", "v0.25.9"}},
		{Machine: "machine-c", Version: wire.CLIUnknown, Roles: []string{"team/worker-4"}, RoleVersions: []string{wire.CLIUnknown}},
	}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Fatalf("cli_behind:\n got %s\nwant %s", gj, wj)
	}
	if v := adminPresence(t, s)["team/worker-2"]; v.CLI != wire.CLIOld {
		t.Fatalf("admin sessions JSON does not mark the old CLI: %+v", v)
	}
}

func TestCLIBehindMailOncePerDayPerMachine(t *testing.T) {
	s, clock := cliServer(t)
	slotDoVersion(t, s, "register", "v0.16.0", ident("machine-a", "/a"))
	slotDoVersion(t, s, "register", "", ident("machine-b", "/b"))
	slotDoVersion(t, s, "register", "v0.26.0", ident("machine-c", "/c"))

	if got := sweepOnce(t, s).CLIBehind; got != 2 {
		t.Fatalf("first sweep mailed %d machines, want 2", got)
	}
	for i := 0; i < 3; i++ {
		clock.add(time.Hour)
		if got := sweepOnce(t, s).CLIBehind; got != 0 {
			t.Fatalf("same day again: %d", got)
		}
	}
	mails := cliMail(t, s)
	sort.Strings(mails)
	want := []string{
		"[옛 판] machine-a switchboard v0.16.0 (서버 dev)\nmachine-a\n- team/worker-1 v0.16.0",
		"[옛 판] machine-b switchboard unknown (서버 dev)\nmachine-b\n- team/worker-2 unknown",
	}
	if strings.Join(mails, "\n--\n") != strings.Join(want, "\n--\n") {
		t.Fatalf("mail:\n%s", strings.Join(mails, "\n--\n"))
	}
	clock.add(24 * time.Hour)
	slotDoVersion(t, s, "heartbeat", "v0.16.0", ident("machine-a", "/a"))
	slotDoVersion(t, s, "heartbeat", "v0.26.0", ident("machine-b", "/b"))
	if got := sweepOnce(t, s).CLIBehind; got != 1 {
		t.Fatalf("next day: %d machines, want 1 (machine-a)", got)
	}
}

func TestCLIMailNoneWhenCurrent(t *testing.T) {
	s, clock := cliServer(t)
	slotDoVersion(t, s, "register", "v0.26.0", ident("machine-a", "/a"))
	slotDoVersion(t, s, "register", "v1.0.0", ident("machine-b", "/b"))
	slotDoVersion(t, s, "register", "dev", ident("machine-c", "/c"))
	for i := 0; i < 2; i++ {
		if got := sweepOnce(t, s).CLIBehind; got != 0 {
			t.Fatalf("sweep mailed %d machines", got)
		}
		clock.add(25 * time.Hour)
	}
	if m := cliMail(t, s); len(m) != 0 {
		t.Fatalf("mail when current: %q", m)
	}
	if b := adminProgress(t, s).CLIBehind; len(b) != 0 {
		t.Fatalf("cli_behind when current: %+v", b)
	}
}

func TestCLIMinSetting(t *testing.T) {
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	for _, c := range []struct{ env, server, want string }{
		{"", "v0.26.0", "v0.26"},
		{"v0.20", "v0.26.0", "v0.20"},
		{"0.21.4", "v0.26.0", "v0.21"},
		{"bogus", "v0.26.0", "v0.26"},
		{"", "dev", ""},
	} {
		s := &Server{}
		configureCLIMin(s, func(k string) string {
			if k == "SWITCHBOARD_CLI_MIN" {
				return c.env
			}
			return ""
		}, c.server, logf)
		w := httptest.NewRecorder()
		s.health(w, httptest.NewRequest("GET", "/health", nil))
		var info wire.ServerVersionInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if info.CLIMin != c.want {
			t.Errorf("CLI_MIN %q on %s: cli_min %q, want %q", c.env, c.server, info.CLIMin, c.want)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("log lines: %q, want one for the invalid value", logged)
	}
}

// A development server has no minimum: nothing is behind, not even a CLI
// without the header.
func TestCLIBehindNoneWithoutMinimum(t *testing.T) {
	s, _ := alertServer(t)
	s.admin.ProxyToken = "proxy-secret"
	configureCLIMin(s, func(string) string { return "" }, "dev", t.Logf)
	slotDoVersion(t, s, "register", "", ident("machine-a", "/a"))
	slotDoVersion(t, s, "register", "v0.1.0", ident("machine-b", "/b"))
	if got := sweepOnce(t, s).CLIBehind; got != 0 {
		t.Fatalf("mailed %d machines", got)
	}
	if b := adminProgress(t, s).CLIBehind; len(b) != 0 {
		t.Fatalf("cli_behind: %+v", b)
	}
}
