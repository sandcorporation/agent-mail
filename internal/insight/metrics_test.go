package insight

import (
	"strings"
	"testing"
	"time"
)

func TestAlertThresholdSettings(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	got, err := ParseThresholds(env(nil))
	if err != nil || got != DefaultThresholds || got.Pending != 2*time.Hour {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	got, err = ParseThresholds(env(map[string]string{"AGENT_MAIL_ALERT_PENDING": "45m", "AGENT_MAIL_ALERT_ERROR": "0", "AGENT_MAIL_ALERT_BLOCKED": "1h"}))
	if err != nil || got != (Thresholds{45 * time.Minute, 0, time.Hour}) {
		t.Fatalf("set: %+v %v", got, err)
	}
	for _, bad := range []string{"soon", "-1m", "10"} {
		if _, err = ParseThresholds(env(map[string]string{"AGENT_MAIL_ALERT_ERROR": bad})); err == nil || !strings.Contains(err.Error(), "AGENT_MAIL_ALERT_ERROR") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
