package cli

import (
	"testing"
	"time"
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
		"/home/u/workspace/agent-mail": "…/workspace/agent-mail", "/home/u/workspace/agent-mail/": "…/workspace/agent-mail",
		`C:\Users\u\workspace\web`: "…/workspace/web", "/srv/x": "/srv/x", "": "-",
	} {
		if got := shortDir(dir); got != want {
			t.Errorf("shortDir(%q) = %q, want %q", dir, got, want)
		}
	}
}
