package identity

import (
	"testing"
	"time"
)

func TestParseInviteTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 72 * time.Hour, "1h": time.Hour, "30d": 30 * 24 * time.Hour, "720h": 720 * time.Hour} {
		if got, err := parseInviteTTL(in); err != nil || got != want {
			t.Errorf("%q = %v %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "31d", "721h", "x", "1.5d"} {
		if _, err := parseInviteTTL(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
