package ownerfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A credential file is closed to everyone but its owner on every OS: a
// protected one-entry DACL on Windows (#206), mode 0600 elsewhere.
func TestWriteIsOwnerOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if err := Write(p, []byte("secret\n")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "secret\n" {
		t.Fatalf("read back %q, %v", b, err)
	}
	ok, err := OnlyUser(p)
	if err != nil || !ok {
		t.Fatalf("OnlyUser = %v, %v", ok, err)
	}
}

// A file made with the default access of its folder is not owner-only on
// Windows (it inherits), which is what RestrictToUser changes.
func TestRestrictToUserClosesAFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := OnlyUser(p); ok && runtime.GOOS == "windows" {
		t.Skip("the temp folder grants no one else: nothing to close")
	}
	if err := RestrictToUser(p); err != nil {
		t.Fatal(err)
	}
	if ok, err := OnlyUser(p); err != nil || !ok {
		t.Fatalf("OnlyUser = %v, %v", ok, err)
	}
}
