package presence

import (
	"testing"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

func TestProgressBadgePriority(t *testing.T) {
	ss := func(states ...string) []wire.ProgressSession {
		var out []wire.ProgressSession
		for _, st := range states {
			out = append(out, wire.ProgressSession{State: st})
		}
		return out
	}
	for _, c := range []struct {
		states []string
		want   string
	}{
		{nil, "none"},
		{[]string{"idle", "unknown"}, "idle"},
		{[]string{"idle", "compacting"}, "busy"},
		{[]string{"busy", "idle"}, "busy"},
		{[]string{"busy", "blocked:permission_prompt", "idle"}, "blocked"},
		{[]string{"blocked:x", "error:StopFailure", "busy"}, "error"},
	} {
		if got := progressBadge(ss(c.states...)); got != c.want {
			t.Fatalf("%v: %q, want %q", c.states, got, c.want)
		}
	}
}
