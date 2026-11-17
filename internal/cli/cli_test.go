package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHelpDoesNotReadConfigOrConnect(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"agent-mail", "send", "--help"}
	if !hasHelp(os.Args[2:]) {
		t.Fatal("subcommand help was not recognized")
	}
	os.Args = []string{"agent-mail", "--help"}
	if profileName() != "default" {
		t.Fatal("help profile setup unexpectedly required config")
	}
}

func TestDefaultProfileKeepsExistingConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_MAIL_CONFIG_DIR", dir)
	t.Setenv("AGENT_MAIL_PROFILE", "")
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"agent-mail"}
	if got, want := configPath(), filepath.Join(dir, "config.json"); got != want {
		t.Fatalf("configPath()=%q, want %q", got, want)
	}
	profileFlag = "worker-a"
	defer func() { profileFlag = "" }()
	if got, want := configPath(), filepath.Join(dir, "worker-a.json"); got != want {
		t.Fatalf("profile configPath()=%q, want %q", got, want)
	}
}

func TestProfileDirFallsBackToOldName(t *testing.T) {
	cfg := t.TempDir()
	newDir, oldDir := filepath.Join(cfg, "switchboard"), filepath.Join(cfg, "agent-mail")
	write := func(dir, name string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got := profilePath(cfg, "w.json"); got != filepath.Join(newDir, "w.json") {
		t.Fatalf("none: %s", got)
	}
	write(oldDir, "w.json")
	if got := profilePath(cfg, "w.json"); got != filepath.Join(oldDir, "w.json") {
		t.Fatalf("old only: %s", got)
	}
	write(newDir, "w.json")
	if got := profilePath(cfg, "w.json"); got != filepath.Join(newDir, "w.json") {
		t.Fatalf("both: %s", got)
	}
	if got := profilePath(cfg, "other.json"); got != filepath.Join(newDir, "other.json") {
		t.Fatalf("new profile: %s", got)
	}
}

func TestProgNameFollowsInvocation(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	for arg0, want := range map[string]string{"/usr/local/bin/agent-mail": "agent-mail", "agent-mail.exe": "agent-mail", "/usr/local/bin/switchboard": "switchboard", "switchboard.exe": "switchboard", "agent-mail.test": "switchboard"} {
		os.Args = []string{filepath.FromSlash(arg0)}
		if got := progName(); got != want {
			t.Errorf("%s: %s, want %s", arg0, got, want)
		}
	}
}
