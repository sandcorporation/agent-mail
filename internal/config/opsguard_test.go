package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hostingNameRE matches SWITCHBOARD_HOSTING, any SWITCHBOARD_HOSTING_* name
// and their old-prefix forms.
var hostingNameRE = regexp.MustCompile(`(` + EnvPrefix + `|` + legacyEnvPrefix + `)` + HostingName + `\w*`)

// repoRoot is the directory holding go.mod above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// isDeployEnvFile: .env, .env.example, prod.env, deploy.env.sample, env.example ...
func isDeployEnvFile(name string) bool {
	return strings.HasPrefix(name, ".env") || strings.HasSuffix(name, ".env") ||
		strings.Contains(name, ".env.") || strings.HasPrefix(name, "env.")
}

// operationsFiles lists the files that configure the operations instance:
// the compose file(s), Jenkinsfile, Dockerfile, every workflow and every
// deploy env file or example in the repository.
func operationsFiles(t *testing.T, root string) []string {
	t.Helper()
	files := []string{filepath.Join(root, "compose.yaml"), filepath.Join(root, "Jenkinsfile"), filepath.Join(root, "Dockerfile")}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("operations file missing, the guard would pass vacuously: %v", err)
		}
	}
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*"))
	if err != nil || len(workflows) == 0 {
		t.Fatalf("no workflows found under .github/workflows: %v", err)
	}
	files = append(files, workflows...)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if isDeployEnvFile(name) || (strings.HasPrefix(name, "compose") && p != filepath.Join(root, "compose.yaml") && (strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml"))) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestOperationsConfigNeverEnablesHosting keeps terminal hosting (#173) out
// of the operations instance: no deployment file may name its settings.
func TestOperationsConfigNeverEnablesHosting(t *testing.T) {
	root := repoRoot(t)
	for _, f := range operationsFiles(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if names := hostingNameRE.FindAllString(string(b), -1); len(names) > 0 {
			rel, _ := filepath.Rel(root, f)
			t.Errorf("%s names %v: hosting must never be configured on the operations instance", rel, names)
		}
	}
}

// remindNameRE matches SWITCHBOARD_REMIND, SWITCHBOARD_REMIND_* and their
// old-prefix forms.
var remindNameRE = regexp.MustCompile(`(` + EnvPrefix + `|` + legacyEnvPrefix + `)REMIND\w*`)

// TestOperationsConfigNeverEnablesRemind keeps directive reminders (#214)
// off on the operations instance until they are switched on on purpose:
// compose.yaml passes only the variables it names, so naming
// SWITCHBOARD_REMIND there is what would let the deploy .env turn it on.
// Switching it on removes this test in the same change.
func TestOperationsConfigNeverEnablesRemind(t *testing.T) {
	root := repoRoot(t)
	for _, f := range operationsFiles(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if names := remindNameRE.FindAllString(string(b), -1); len(names) > 0 {
			rel, _ := filepath.Rel(root, f)
			t.Errorf("%s names %v: directive reminders stay off on the operations instance until switched on on purpose", rel, names)
		}
	}
	for _, s := range []string{"SWITCHBOARD_REMIND: ${SWITCHBOARD_REMIND:-}", "- SWITCHBOARD_REMIND_AFTER=2h", "AGENT_MAIL_REMIND=on"} {
		if !remindNameRE.MatchString(s) {
			t.Errorf("guard misses %q", s)
		}
	}
}

func TestHostingNameGuardMatches(t *testing.T) {
	for _, s := range []string{"SWITCHBOARD_HOSTING: on", "- SWITCHBOARD_HOSTING_MAX=4", "AGENT_MAIL_HOSTING=on", "env.SWITCHBOARD_HOSTING_AGENT"} {
		if !hostingNameRE.MatchString(s) {
			t.Errorf("guard misses %q", s)
		}
	}
	for _, s := range []string{"SWITCHBOARD_HOST", "SWITCHBOARD_ADMIN_AUTH: builtin"} {
		if hostingNameRE.MatchString(s) {
			t.Errorf("guard flags %q", s)
		}
	}
	for name, want := range map[string]bool{".env": true, ".env.example": true, "prod.env": true, "deploy.env.sample": true, "env.example": true, "envelope.go": false, "compose.yaml": false} {
		if isDeployEnvFile(name) != want {
			t.Errorf("isDeployEnvFile(%q) = %v", name, !want)
		}
	}
}
