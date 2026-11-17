package identity

import "testing"

func TestSystemSendersBothReserved(t *testing.T) {
	for _, n := range []string{"switchboard/system", "agent-mail/system", "switchboard/system-2", "agent-mail/system-3"} {
		if !Reserved(n) {
			t.Errorf("%s not reserved", n)
		}
	}
	if !IsSystemSender("agent-mail/system") || !IsSystemSender(SystemSender) || IsSystemSender("team/system") {
		t.Fatal("isSystemSender")
	}
	if SystemSender != "switchboard/system" {
		t.Fatalf("new mail must be sent as switchboard/system, got %s", SystemSender)
	}
}
