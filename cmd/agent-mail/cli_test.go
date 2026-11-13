package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type cliResult struct {
	code   int
	stdout string
	stderr string
}

// cliHarness runs the real CLI in a subprocess against an in-memory test server.
type cliHarness struct {
	t      *testing.T
	s      *Server
	url    string
	dir    string
	tokens map[string]string // role -> token file
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	s := testServer(t)
	srv := httptest.NewServer(http.HandlerFunc(s.api))
	t.Cleanup(srv.Close)
	h := &cliHarness{t: t, s: s, url: srv.URL, dir: t.TempDir(), tokens: map[string]string{}}
	for role, token := range map[string]string{"a": "secret-a", "b": "secret-b"} {
		f := filepath.Join(h.dir, role+".token")
		if err := os.WriteFile(f, []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		h.tokens[role] = f
	}
	return h
}

func (h *cliHarness) run(env []string, args ...string) cliResult {
	h.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	argJSON, _ := json.Marshal(args)
	cmd.Env = append(os.Environ(), "AGENT_MAIL_TEST_CLI=1", "AGENT_MAIL_CONFIG_DIR="+h.dir, "AGENT_MAIL_PROFILE=", "AGENT_MAIL_TEST_ARGS="+string(argJSON))
	cmd.Env = append(cmd.Env, env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	// The test binary prints PASS on success; strip it.
	stdout := strings.TrimSuffix(strings.TrimSpace(out.String()), "PASS")
	return cliResult{code: code, stdout: stdout, stderr: errOut.String()}
}

func (h *cliHarness) mustRun(env []string, args ...string) string {
	h.t.Helper()
	r := h.run(env, args...)
	if r.code != 0 {
		h.t.Fatalf("CLI %v exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

func (h *cliHarness) configure(profileArgs []string, role string) {
	h.t.Helper()
	args := append(append([]string{}, profileArgs...), "config", "set", "--endpoint", h.url, "--role", role, "--token-file", h.tokens[role])
	h.mustRun(nil, args...)
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// Regression: a leading --profile used to be dropped (os.Args was rewritten in place),
// so config set wrote the default config and other commands used the default role.
func TestLeadingProfileSelectsProfile(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "role-a"}, "a")
	if !exists(filepath.Join(h.dir, "role-a.json")) || exists(filepath.Join(h.dir, "config.json")) {
		t.Fatal("leading --profile did not write the named profile")
	}
	h.configure([]string{"--profile=role-b"}, "b")
	h.configure(nil, "b") // default profile is a different role
	h.mustRun(nil, "--profile", "role-a", "send", "--to", "b", "--subject", "from-a", "--body", "x")
	out := h.mustRun(nil, "outbox", "--json", "--profile", "role-a")
	if !strings.Contains(out, "from-a") {
		t.Fatalf("trailing --profile outbox: %s", out)
	}
	if out := h.mustRun(nil, "--profile", "role-b", "inbox"); !strings.Contains(out, "a → b") {
		t.Fatalf("leading --profile inbox: %s", out)
	}
	if out := h.mustRun([]string{"AGENT_MAIL_PROFILE=role-b"}, "--profile", "role-a", "outbox"); !strings.Contains(out, "from-a") {
		t.Fatalf("--profile must override AGENT_MAIL_PROFILE: %s", out)
	}
	if out := h.mustRun([]string{"AGENT_MAIL_PROFILE=role-a"}, "outbox"); !strings.Contains(out, "from-a") {
		t.Fatalf("AGENT_MAIL_PROFILE outbox: %s", out)
	}
}

func TestMissingProfileIsAnError(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "a") // a usable default must not be used as a fallback
	for _, tc := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"--profile", "nonexist", "inbox"}},
		{nil, []string{"inbox", "--profile", "nonexist"}},
		{[]string{"AGENT_MAIL_PROFILE=nonexist"}, []string{"inbox"}},
	} {
		r := h.run(tc.env, tc.args...)
		if r.code != exitError || !strings.Contains(r.stderr, `profile "nonexist" not found`) {
			t.Fatalf("%v %v: code=%d stderr=%s", tc.env, tc.args, r.code, r.stderr)
		}
	}
	if r := h.run(nil, "--profile", "bad/name", "inbox"); r.code != exitUsage {
		t.Fatalf("invalid profile name: code=%d", r.code)
	}
}

func TestUnknownOptionsAndCommandsAreUsageErrors(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "a")
	for _, args := range [][]string{
		{"inbox", "--stauts", "pending"},
		{"inbox", "--status", "unread"},
		{"inbox", "--status"},
		{"inbox", "extra"},
		{"send", "--to", "b", "--subject", "s", "--body", "b", "--thread", "1"},
		{"read"},
		{"read", "abc"},
		{"read", "1", "--json=yes"},
		{"--verbose", "inbox"},
		{"frobnicate"},
		{"config", "get"},
		{"--profile"},
	} {
		r := h.run(nil, args...)
		if r.code != exitUsage || !strings.Contains(r.stderr, "agent-mail:") {
			t.Fatalf("%v: code=%d stderr=%s", args, r.code, r.stderr)
		}
	}
	if out := h.mustRun(nil, "outbox"); out != "" {
		t.Fatalf("rejected send must not have been sent: %s", out)
	}
}

func TestInboxStatusFilter(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "b")
	for _, subject := range []string{"first", "second"} {
		if w := call(h.s, "POST", "/api/messages", "secret-a", map[string]string{"recipient": "b", "subject": subject, "body": "x"}, ""); w.Code != 201 {
			t.Fatal(w.Body)
		}
	}
	h.mustRun(nil, "read", "1")
	pending := h.mustRun(nil, "inbox", "--status", "pending", "--json")
	if strings.Contains(pending, "first") || !strings.Contains(pending, "second") {
		t.Fatalf("pending filter: %s", pending)
	}
	read := h.mustRun(nil, "inbox", "--status=read")
	if !strings.Contains(read, "first") || strings.Contains(read, "second") {
		t.Fatalf("read filter: %s", read)
	}
}

func TestWaitCommand(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "b")
	r := h.run(nil, "wait", "--timeout", "1", "--interval", "1")
	if r.code != exitTimeout || r.stdout != "" {
		t.Fatalf("empty inbox wait: code=%d stdout=%q stderr=%s", r.code, r.stdout, r.stderr)
	}
	if r := h.run(nil, "wait", "--timeout", "1", "--json"); r.code != exitTimeout || strings.TrimSpace(r.stdout) != "[]" {
		t.Fatalf("json timeout: code=%d stdout=%q", r.code, r.stdout)
	}
	if w := call(h.s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "wake\nup", "body": "x", "task_id": "t-1"}, ""); w.Code != 201 {
		t.Fatal(w.Body)
	}
	if w := call(h.s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "b", "subject": "already read", "body": "x"}, ""); w.Code != 201 {
		t.Fatal(w.Body)
	}
	h.mustRun(nil, "read", "2")
	out := h.mustRun(nil, "wait", "--timeout", "5")
	if out != "#1 a | wake up | task=t-1" {
		t.Fatalf("wait output: %q", out)
	}
	// wait must not consume: the message stays pending and a second wait returns at once.
	if out := h.mustRun(nil, "wait", "--json"); !strings.Contains(out, `"status": "pending"`) || strings.Contains(out, "already read") {
		t.Fatalf("wait --json: %s", out)
	}
	h.configure([]string{"--profile", "wrong"}, "b")
	cfg := filepath.Join(h.dir, "wrong.json")
	b, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, []byte(strings.Replace(string(b), "secret-b", "bad-token", 1)), 0600)
	if r := h.run(nil, "--profile", "wrong", "wait", "--timeout", "30"); r.code != exitError {
		t.Fatalf("bad credential must fail fast: code=%d stderr=%s", r.code, r.stderr)
	}
}

func TestCommandHelpListsOptions(t *testing.T) {
	h := newCLIHarness(t)
	for cmd, want := range map[string][]string{
		"send":   {"--to ROLE", "--body-file PATH", "--idempotency-key KEY", "--json", "--profile NAME"},
		"inbox":  {"--status STATUS", "--q TEXT"},
		"wait":   {"--timeout SECONDS", "Exit codes"},
		"config": {"--endpoint URL", "--token-file PATH"},
		"reply":  {"Usage: agent-mail reply ID", "--body TEXT"},
	} {
		for _, form := range [][]string{{cmd, "--help"}, {"help", cmd}} {
			out := h.mustRun(nil, form...)
			for _, w := range want {
				if !strings.Contains(out, w) {
					t.Fatalf("%v help missing %q:\n%s", form, w, out)
				}
			}
		}
	}
	if out := h.mustRun(nil, "--help"); !strings.Contains(out, "wait") || !strings.Contains(out, "Exit codes") {
		t.Fatalf("root help: %s", out)
	}
}
