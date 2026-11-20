package server

import (
	"os"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
)

// TestMain lets the test binary stand in for the agent CLI (fakeagent) when
// a hosted session launches it.
func TestMain(m *testing.M) {
	loadAgentEnvFile()
	fakeagent.MainIfRequested()
	os.Exit(m.Run())
}

// loadAgentEnvFile sets the KEY=value lines of <this executable>.env, when
// there is one. The Windows doc test copies this binary as claude.exe and
// writes that file next to it, because the up the scheduled task starts
// does not carry the test's environment to the agent it launches (#242).
func loadAgentEnvFile() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	b, err := os.ReadFile(exe + ".env")
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "="); ok && k != "" {
			_ = os.Setenv(k, v)
		}
	}
}
