package fakeagent_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestMain(m *testing.M) {
	fakeagent.MainIfRequested()
	os.Exit(m.Run())
}

// agent re-executes the test binary as the fake agent.
func agent(t *testing.T, dir, input string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), fakeagent.Env(dir)...), extraEnv...)
	cmd.Stdin = strings.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode()
	default:
		t.Fatalf("run fake agent: %v", err)
		return "", -1
	}
}

func TestFakeAgentPrintsArgvEchoesAndExits(t *testing.T) {
	dir := t.TempDir()
	out, code := agent(t, dir, "hello there\nexit 7\nnever read\n",
		[]string{"SWITCHBOARD_PROFILE=team-worker", "SWITCHBOARD_TOKEN=role-secret-value", "API_KEY=key-secret-value"},
		"--session-id", "abc", "--model", "x")
	if code != 7 {
		t.Fatalf("exit code %d, want 7\n%s", code, out)
	}
	for _, want := range []string{
		"argv --session-id abc --model x\r\n",
		"env SWITCHBOARD_PROFILE=team-worker\r\n",
		"env SWITCHBOARD_TOKEN\r\n",
		"env API_KEY\r\n",
		"echo hello there\r\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"role-secret-value", "key-secret-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q", secret)
		}
	}
	if strings.Contains(out, "never read") {
		t.Error("input after exit was handled")
	}
}

func TestFakeAgentResumeNeedsKnownSession(t *testing.T) {
	dir := t.TempDir()
	if out, code := agent(t, dir, "", nil, "--resume", "missing"); code != fakeagent.ExitUnknownResume {
		t.Fatalf("resume of unknown id: exit %d, want %d\n%s", code, fakeagent.ExitUnknownResume, out)
	}
	if out, code := agent(t, dir, "first\n", nil, "--session-id", "s1"); code != 0 {
		t.Fatalf("first launch: exit %d\n%s", code, out)
	}
	if out, code := agent(t, dir, "", nil, "--session-id", "s1"); code != fakeagent.ExitSessionInUse {
		t.Fatalf("reused --session-id: exit %d, want %d\n%s", code, fakeagent.ExitSessionInUse, out)
	}
	out, code := agent(t, dir, "second\n", nil, "--resume", "s1")
	if code != 0 || !strings.Contains(out, "ready s1") {
		t.Fatalf("resume: exit %d\n%s", code, out)
	}
	tr, err := os.ReadFile(filepath.Join(dir, "s1.transcript"))
	if err != nil || string(tr) != "first\nsecond\n" {
		t.Fatalf("transcript %q, %v", tr, err)
	}
}

func TestFakeAgentRefusesPrintAndContinue(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "--session-id", "a"},
		{"--continue"},
		{},
		{"--session-id", "a", "--resume", "a"},
	} {
		var out bytes.Buffer
		if code := fakeagent.Run(args, fakeagent.Env(t.TempDir()), strings.NewReader(""), &out); code != fakeagent.ExitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, fakeagent.ExitUsage)
		}
	}
}

func TestFakeAgentDrawsScene(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fakeagent.ScenesDir(), "colors-bold.vt"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := term.ParseScene(raw)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, st := range sc.Steps {
		want = append(want, st.Data...)
	}
	var out bytes.Buffer
	code := fakeagent.Run([]string{"--session-id", "d"}, fakeagent.Env(t.TempDir()), strings.NewReader("draw colors-bold\n"), &out)
	if code != 0 || !bytes.Contains(out.Bytes(), want) {
		t.Fatalf("exit %d; output lacks the scene bytes:\n%q", code, out.String())
	}
	out.Reset()
	fakeagent.Run([]string{"--session-id", "e"}, fakeagent.Env(t.TempDir()), strings.NewReader("draw ../x\n"), &out)
	if !strings.Contains(out.String(), "error: draw: bad scene") {
		t.Fatalf("path in scene name accepted:\n%s", out.String())
	}
}

// With a profile the fake agent plays the plugin: it registers its slot with
// the profile's token, its machine and working directory, heartbeats it, and
// on "clear" releases it and registers the new conversation.
func TestFakeAgentRegistersItsSlotAndFollowsClear(t *testing.T) {
	type call struct {
		action, auth, role string
		req                wire.SlotRequest
	}
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wire.SlotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls = append(calls, call{strings.TrimPrefix(r.URL.Path, "/api/slots/"), r.Header.Get("Authorization"), r.Header.Get("X-Switchboard-Role"), req})
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(wire.SlotGrant{SlotView: wire.SlotView{Role: "team/worker-1"}})
	}))
	defer srv.Close()
	conf, work, dir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "team-worker.json"), []byte(`{"endpoint":"http://unused.invalid","role":"team/worker","token":"profile-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "--session-id", "11111111-1111-4111-8111-111111111111")
	cmd.Dir = work
	cmd.Env = append(append(os.Environ(), fakeagent.Env(dir)...), "SWITCHBOARD_PROFILE=team-worker", "SWITCHBOARD_CONFIG_DIR="+conf,
		"SWITCHBOARD_ENDPOINT="+srv.URL, "SWITCHBOARD_MACHINE=pc-1", "SWITCHBOARD_LABEL=42", "SWITCHBOARD_FAKE_AGENT_HEARTBEAT=20ms")
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	var out syncBuffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s:\n%s", what, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	count := func(action string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, c := range calls {
			if c.action == action {
				n++
			}
		}
		return n
	}
	waitFor("a heartbeat", func() bool { return count("heartbeat") > 0 })
	_, _ = io.WriteString(pw, "clear\n")
	waitFor("the new conversation registered", func() bool { return count("register") == 2 })
	_, _ = io.WriteString(pw, "exit 0\n")
	_ = pw.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("fake agent: %v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "slot team/worker-1\r\n") {
		t.Errorf("output lacks the slot:\n%s", text)
	}
	i := strings.Index(text, "session ")
	if i < 0 {
		t.Fatalf("no session line:\n%s", text)
	}
	next := strings.TrimSpace(strings.SplitN(text[i+len("session "):], "\r\n", 2)[0])
	if next == "11111111-1111-4111-8111-111111111111" || len(next) != 36 {
		t.Fatalf("new session id %q", next)
	}
	if _, err := os.Stat(filepath.Join(dir, next+".transcript")); err != nil {
		t.Errorf("no transcript for the new conversation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var seq []string
	for _, c := range calls {
		if c.auth != "Bearer profile-secret-value" || c.role != "team/worker" || c.req.Machine != "pc-1" || c.req.Label != "42" {
			t.Errorf("call %+v", c)
		}
		if wd, _ := filepath.EvalSymlinks(work); c.req.Directory != work && c.req.Directory != wd {
			t.Errorf("directory %q, want %q", c.req.Directory, work)
		}
		if c.action != "heartbeat" {
			seq = append(seq, c.action+" "+c.req.SessionID)
		}
	}
	want := []string{"register 11111111-1111-4111-8111-111111111111", "release 11111111-1111-4111-8111-111111111111", "register " + next}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Errorf("slot calls %q, want %q", seq, want)
	}
	if strings.Contains(text, "profile-secret-value") {
		t.Error("the profile token was printed")
	}
}

// syncBuffer is a bytes.Buffer safe to read while a process writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
