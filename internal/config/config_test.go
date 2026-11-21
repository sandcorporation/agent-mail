package config

import (
	"net/netip"
	"slices"
	"strings"
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

func TestWithoutOwnEnvDropsBothPrefixes(t *testing.T) {
	in := []string{"PATH=/bin", "AGENT_MAIL_ROLE=x/y", "SWITCHBOARD_TOKEN=t", "agent_mail_token=t", "Switchboard_Profile=p", "HOME=/h", "SWITCHBOARDX=keep", "DB_PATH=/d"}
	got := WithoutOwnEnv(in)
	if want := []string{"PATH=/bin", "HOME=/h", "SWITCHBOARDX=keep", "DB_PATH=/d"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHostingRemoteAllow(t *testing.T) {
	allow, from, err := HostingRemoteAllow(fakeEnv(nil))
	if err != nil || from != "SWITCHBOARD_HOSTING_REMOTE_ALLOW" || len(allow) != 2 || allow[0].String() != "100.64.0.0/10" || allow[1].String() != "fd7a:115c:a1e9::/48" { // leak-guard:allow
		t.Fatalf("default = %v %q %v", allow, from, err)
	}
	allow, _, err = HostingRemoteAllow(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING_REMOTE_ALLOW": " 192.0.2.0/24 , 2001:db8::/32 "}))
	if err != nil || len(allow) != 2 {
		t.Fatalf("list = %v %v", allow, err)
	}
	for _, c := range []struct {
		addr string
		want bool
	}{{"192.0.2.7", true}, {"::ffff:192.0.2.7", true}, {"2001:db8::1", true}, {"198.51.100.1", false}, {"100.64.0.10", false}} { // leak-guard:allow
		if got := AddrAllowed(netip.MustParseAddr(c.addr), allow); got != c.want {
			t.Errorf("AddrAllowed(%s) = %v", c.addr, got)
		}
	}
	for _, bad := range []string{"100.64.0.10", "nonsense", " , "} { // leak-guard:allow
		if _, _, err := HostingRemoteAllow(fakeEnv(map[string]string{"SWITCHBOARD_HOSTING_REMOTE_ALLOW": bad})); err == nil || !strings.Contains(err.Error(), "SWITCHBOARD_HOSTING_REMOTE_ALLOW") {
			t.Errorf("%q: err %v", bad, err)
		}
	}
}
