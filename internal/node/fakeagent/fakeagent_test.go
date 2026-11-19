package fakeagent_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
	"github.com/sandcorporation/switchboard/internal/term"
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
