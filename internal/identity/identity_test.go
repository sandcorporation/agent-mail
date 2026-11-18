package identity

import "testing"

func TestSystemSendersBothReserved(t *testing.T) {
	for _, n := range []string{"switchboard/system", LegacySystemSender, "switchboard/system-2", LegacySystemSender + "-3"} {
		if !Reserved(n) {
			t.Errorf("%s not reserved", n)
		}
	}
	if !IsSystemSender(LegacySystemSender) || !IsSystemSender(SystemSender) || IsSystemSender("team/system") {
		t.Fatal("isSystemSender")
	}
	if SystemSender != "switchboard/system" {
		t.Fatalf("new mail must be sent as switchboard/system, got %s", SystemSender)
	}
}
