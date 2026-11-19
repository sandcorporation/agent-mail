package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestProgressDurations(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "방금"}, {-time.Minute, "방금"}, {90 * time.Second, "2분 전"},
		{59 * time.Minute, "59분 전"}, {90 * time.Minute, "1.5시간 전"}, {11 * time.Hour, "11시간 전"},
		{36 * time.Hour, "1.5일 전"}, {12 * 24 * time.Hour, "12일 전"},
	} {
		if got := relTime(now, now.Add(-c.ago)); got != c.want {
			t.Errorf("relTime(%v) = %q, want %q", c.ago, got, c.want)
		}
	}
	for dir, want := range map[string]string{
		"/home/u/workspace/switchboard": "…/workspace/switchboard", "/home/u/workspace/switchboard/": "…/workspace/switchboard",
		`C:\Users\u\workspace\web`: "…/workspace/web", "/srv/x": "/srv/x", "": "-",
	} {
		if got := shortDir(dir); got != want {
			t.Errorf("shortDir(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestProgressSessionShowsSubagents(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ss := []wire.ServiceProgress{{Service: "api", Badge: "busy", Sessions: []wire.ProgressSession{
		{Role: "api-1", State: "idle", Directory: "srv", Subagents: 2},
		{Role: "api-2", State: "busy", Directory: "srv"},
	}}}
	out := formatProgress(ss, now)
	if !strings.Contains(out, "    api-1 idle srv · 서브 에이전트 2개 작업 중\n") || !strings.Contains(out, "    api-2 busy srv\n") {
		t.Fatalf("progress text:\n%s", out)
	}
}
