package server

import (
	"os"
	"testing"

	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
)

// TestMain lets the test binary stand in for the agent CLI (fakeagent) when
// a hosted session launches it.
func TestMain(m *testing.M) {
	fakeagent.MainIfRequested()
	os.Exit(m.Run())
}
