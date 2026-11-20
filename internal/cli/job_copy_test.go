package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The runner copy is named by version and content: the same binary reuses
// its copy, a new one gets its own and the old copy goes when nothing
// holds it.
func TestRunnerCopyIsPerVersionAndPrunesOldCopies(t *testing.T) {
	src, dir := t.TempDir(), filepath.Join(t.TempDir(), "runner")
	exe := filepath.Join(src, "switchboard.exe")
	if err := os.WriteFile(exe, []byte("build one"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := runnerCopy(dir, exe, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(first); string(b) != "build one" || !strings.HasPrefix(filepath.Base(first), "switchboard-v1.2.3-") || filepath.Ext(first) != ".exe" {
		t.Fatalf("copy %s: %q", first, b)
	}
	if again, err := runnerCopy(dir, exe, "v1.2.3"); err != nil || again != first {
		t.Fatalf("same binary: %s %v, want %s", again, err, first)
	}
	// An unfinished copy of a writer still at work stays; an old one goes.
	fresh := filepath.Join(dir, "switchboard-x.exe.1.tmp")
	stale := filepath.Join(dir, "switchboard-x.exe.2.tmp")
	for _, f := range []string{fresh, stale} {
		if err = os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	if err = os.WriteFile(exe, []byte("build two"), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := runnerCopy(dir, exe, "dev/1 2")
	if err != nil {
		t.Fatal(err)
	}
	if second == first || !strings.HasPrefix(filepath.Base(second), "switchboard-dev_1_2-") {
		t.Fatalf("new binary: %s", second)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !contains(names, filepath.Base(second)) || !contains(names, filepath.Base(fresh)) {
		t.Fatalf("left in the runner folder: %v", names)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
