package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/config"
)

// TestMain drops every variable internal/config reads (both prefixes), so a
// machine's own setup, such as an exported SWITCHBOARD_PROFILE, never
// changes what these in-process tests see (#246). Tests set what they mean
// with t.Setenv.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); config.IsOwnEnv(name) {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
