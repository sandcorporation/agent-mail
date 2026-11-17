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
