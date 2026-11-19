package config

import (
	"slices"
	"testing"
)

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestEnvAliasNewWinsOldFallsBack(t *testing.T) {
	v, from := EnvAliasFrom(fakeEnv(map[string]string{"SWITCHBOARD_X1": "new", "AGENT_MAIL_X1": "old"}), "X1")
	if v != "new" || from != "SWITCHBOARD_X1" {
		t.Fatalf("both set: %q %q", v, from)
	}
	if slices.Contains(OldEnvNames(), "AGENT_MAIL_X1") {
		t.Fatal("old name recorded although the new one won")
	}
	v, from = EnvAliasFrom(fakeEnv(map[string]string{"AGENT_MAIL_X2": "old"}), "X2")
	if v != "old" || from != "AGENT_MAIL_X2" || !slices.Contains(OldEnvNames(), "AGENT_MAIL_X2") {
		t.Fatalf("old only: %q %q %v", v, from, OldEnvNames())
	}
	if v, from = EnvAliasFrom(fakeEnv(nil), "X3"); v != "" || from != "SWITCHBOARD_X3" {
		t.Fatalf("neither: %q %q", v, from)
	}
	t.Setenv("SWITCHBOARD_X4", "real")
	t.Setenv("AGENT_MAIL_X4", "old")
	if EnvAlias("X4") != "real" {
		t.Fatal("envAlias must read the process environment, new name first")
	}
}

func TestHostingOffByDefaultOnlyOnEnables(t *testing.T) {
	if on, from := HostingFrom(fakeEnv(nil)); on || from != "SWITCHBOARD_HOSTING" {
		t.Fatalf("default: %v %q", on, from)
	}
	for _, v := range []string{"off", "true", "1", "yes", "ON", "enabled"} {
		if on, _ := HostingFrom(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING": v})); on {
			t.Fatalf("%q turned hosting on", v)
		}
	}
	for _, v := range []string{"on", " on "} {
		if on, _ := HostingFrom(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING": v})); !on {
			t.Fatalf("%q left hosting off", v)
		}
	}
	if on, from := HostingFrom(fakeEnv(map[string]string{"AGENT_MAIL_HOSTING": "on"})); !on || from != "AGENT_MAIL_HOSTING" {
		t.Fatalf("old name: %v %q", on, from)
	}
	if on, from := HostingFrom(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING": "off", "AGENT_MAIL_HOSTING": "on"})); on || from != "SWITCHBOARD_HOSTING" {
		t.Fatalf("new name must win: %v %q", on, from)
	}
}
