package server

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestExitPathsLeaveALine runs each exit path in a child process (this test
// binary) and checks the line it leaves on stderr and its exit code (#257).
func TestExitPathsLeaveALine(t *testing.T) {
	t.Parallel()
	if kind := os.Getenv("SB_EXIT_KIND"); kind != "" {
		switch kind {
		case "fatal":
			fatal(errors.New("listen tcp 127.0.0.1:1: bind: in use"))
		case "panic":
			done := make(chan struct{})
			goSafe("probe", func() { panic("probe panic") })
			<-done
		}
		return
	}
	for _, c := range []struct {
		kind  string
		code  int
		lines []string
	}{
		{"fatal", 1, []string{"listen tcp 127.0.0.1:1: bind: in use", "exit 1: listen tcp 127.0.0.1:1: bind: in use"}},
		{"panic", 2, []string{"panic in probe: probe panic", "goroutine ", "exit 2: panic in probe"}},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExitPathsLeaveALine$")
		cmd.Env = append(os.Environ(), "SB_EXIT_KIND="+c.kind)
		out, err := cmd.CombinedOutput()
		t.Logf("%s: %v\n%s", c.kind, err, out)
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != c.code {
			t.Errorf("%s: exit %v, want code %d\n%s", c.kind, err, c.code, out)
		}
		for _, l := range c.lines {
			if !strings.Contains(string(out), l) {
				t.Errorf("%s: output lacks %q:\n%s", c.kind, l, out)
			}
		}
	}
}
