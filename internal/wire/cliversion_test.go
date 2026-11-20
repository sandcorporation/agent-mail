package wire

import "testing"

func TestCLIState(t *testing.T) {
	min := MinorVersion{0, 26}
	for v, want := range map[string]string{
		"v0.26.0": CLICurrent, "v0.26.9": CLICurrent, "v1.0.0": CLICurrent, "v0.27.0": CLICurrent,
		"v0.25.9": CLIOld, "v0.16.0": CLIOld,
		"dev": CLINoInfo, CLIUnknown: CLINoInfo, "v0.26.0-3-gabc123": CLINoInfo, "": CLINoInfo,
	} {
		if got := CLIState(v, min, true); got != want {
			t.Errorf("CLIState(%q) = %q, want %q", v, got, want)
		}
	}
	if got := CLIState("v0.1.0", min, false); got != CLINoInfo {
		t.Errorf("no minimum: %q", got)
	}
}

func TestReleaseAsset(t *testing.T) {
	if got := ReleaseAsset("v0.26.0", "windows", "amd64"); got != "switchboard_v0.26.0_windows_amd64.zip" {
		t.Fatal(got)
	}
	if got := ReleaseAsset("v0.26.0", "darwin", "arm64"); got != "switchboard_v0.26.0_darwin_arm64.tar.gz" {
		t.Fatal(got)
	}
}

func TestCLIMinimumFromDescribe(t *testing.T) {
	for _, c := range []struct {
		server string
		want   MinorVersion
		ok     bool
	}{
		{"v0.31.0", MinorVersion{0, 31}, true},
		{"v0.31.0-2-g1a2b3c4", MinorVersion{0, 31}, true},
		{"9a6b72f", MinorVersion{}, false},
		{"dev", MinorVersion{}, false},
	} {
		got, ok, _ := CLIMinimum("", c.server)
		if ok != c.ok || got != c.want {
			t.Errorf("CLIMinimum(%q) = %v, %v", c.server, got, ok)
		}
	}
	// a describe-form client stays 판 모름
	if got := CLIState("v0.31.0-2-g1a2b3c4", MinorVersion{0, 31}, true); got != CLINoInfo {
		t.Errorf("describe client = %s", got)
	}
}
