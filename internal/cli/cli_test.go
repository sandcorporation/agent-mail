package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sandcorporation/switchboard/internal/config"
)

func TestHelpDoesNotReadConfigOrConnect(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"switchboard", "send", "--help"}
	if !hasHelp(os.Args[2:]) {
		t.Fatal("subcommand help was not recognized")
	}
	os.Args = []string{"switchboard", "--help"}
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
	os.Args = []string{"switchboard"}
	if got, want := configPath(), filepath.Join(dir, "config.json"); got != want {
		t.Fatalf("configPath()=%q, want %q", got, want)
	}
	profileFlag = "worker-a"
	defer func() { profileFlag = "" }()
	if got, want := configPath(), filepath.Join(dir, "worker-a.json"); got != want {
		t.Fatalf("profile configPath()=%q, want %q", got, want)
	}
}

func TestProfileDirCopiesOldProfileOnce(t *testing.T) {
	cfg := t.TempDir()
	newDir, oldDir := filepath.Join(cfg, "switchboard"), filepath.Join(cfg, config.LegacyConfigDir)
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := profilePath(cfg, "w.json"); got != filepath.Join(newDir, "w.json") {
		t.Fatalf("none: %s", got)
	}
	if _, err := os.Stat(newDir); err == nil {
		t.Fatal("no profile anywhere must not create the new directory")
	}

	// Only the old directory has it: copied once, owner-only, old file kept.
	write(oldDir, "w.json", `{"role":"old"}`)
	if got := profilePath(cfg, "w.json"); got != filepath.Join(newDir, "w.json") {
		t.Fatalf("old only: %s", got)
	}
	if got := read(filepath.Join(newDir, "w.json")); got != `{"role":"old"}` {
		t.Fatalf("copy: %s", got)
	}
	if read(filepath.Join(oldDir, "w.json")) != `{"role":"old"}` {
		t.Fatal("old file changed")
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{newDir: 0700, filepath.Join(newDir, "w.json"): 0600} {
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
				t.Fatalf("%s mode %v, want %v (%v)", path, fi.Mode().Perm(), want, err)
			}
		}
	}

	// Second run: the new file wins, the old one is not copied again.
	write(oldDir, "w.json", `{"role":"changed-old"}`)
	if got := profilePath(cfg, "w.json"); got != filepath.Join(newDir, "w.json") {
		t.Fatalf("second: %s", got)
	}
	if got := read(filepath.Join(newDir, "w.json")); got != `{"role":"old"}` {
		t.Fatalf("new file overwritten: %s", got)
	}

	// The new directory exists without this profile: that profile is copied.
	write(oldDir, "x.json", `{"role":"x"}`)
	if got := profilePath(cfg, "x.json"); got != filepath.Join(newDir, "x.json") || read(got) != `{"role":"x"}` {
		t.Fatalf("other profile: %s", got)
	}
	if got := profilePath(cfg, "other.json"); got != filepath.Join(newDir, "other.json") {
		t.Fatalf("new profile: %s", got)
	}
}

func TestProgNameFollowsInvocation(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	for arg0, want := range map[string]string{"/usr/local/bin/" + config.LegacyConfigDir: config.LegacyConfigDir, config.LegacyConfigDir + ".exe": config.LegacyConfigDir, "/usr/local/bin/switchboard": "switchboard", "switchboard.exe": "switchboard", config.LegacyConfigDir + ".test": "switchboard"} {
		os.Args = []string{filepath.FromSlash(arg0)}
		if got := progName(); got != want {
			t.Errorf("%s: %s, want %s", arg0, got, want)
		}
	}
}
