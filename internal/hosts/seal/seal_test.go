package seal

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mustHex32(t *testing.T, s string) [KeySize]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != KeySize {
		t.Fatalf("bad hex key: %v", err)
	}
	var k [KeySize]byte
	copy(k[:], b)
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("role secret value")
	ct, err := Seal(pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) != len(msg)+Overhead {
		t.Fatalf("len(ct) = %d, want %d", len(ct), len(msg)+Overhead)
	}
	if bytes.Contains(ct, msg) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := Open(pub, priv, ct)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	pub, _, _ := GenerateKey()
	otherPub, otherPriv, _ := GenerateKey()
	ct, _ := Seal(pub, []byte("for pub only"))
	if _, err := Open(otherPub, otherPriv, ct); !errors.Is(err, ErrOpen) {
		t.Fatalf("Open with another key pair: %v, want ErrOpen", err)
	}
}

func TestOpenTamperedFails(t *testing.T) {
	pub, priv, _ := GenerateKey()
	ct, _ := Seal(pub, []byte("do not touch"))
	for i := range ct {
		bad := bytes.Clone(ct)
		bad[i] ^= 0x01
		if _, err := Open(pub, priv, bad); !errors.Is(err, ErrOpen) {
			t.Fatalf("flipping byte %d: %v, want ErrOpen", i, err)
		}
	}
	if _, err := Open(pub, priv, ct[:len(ct)-1]); !errors.Is(err, ErrOpen) {
		t.Fatal("truncated box opened")
	}
	if _, err := Open(pub, priv, nil); !errors.Is(err, ErrOpen) {
		t.Fatal("empty box opened")
	}
}

// TestOpenLibsodiumVector opens a box made by libsodium 1.0.22
// crypto_box_seal, with the key pair from crypto_box_seed_keypair on the
// seed 00 01 .. 1f. Test-only key, never used anywhere else.
func TestOpenLibsodiumVector(t *testing.T) {
	pub := mustHex32(t, "4701d08488451f545a409fb58ae3e58581ca40ac3f7f114698cd71deac73ca01")
	priv := mustHex32(t, "3d94eea49c580aef816935762be049559d6d1440dede12e6a125f1841fff8e6f")
	ct, _ := hex.DecodeString("6f6833d5f7bacd08e4b6a841f30b3e6ffc7f3a4617a9a967897ffd679e74ce5d" +
		"18ca81f96366326f1bed516e510b244bfeee23e14ab12045a03d9a8a107259c5" +
		"4e23eae01c5d7b83a16f1aa8")
	got, err := Open(pub, priv, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "peal sealed box known answer" {
		t.Fatalf("plaintext = %q", got)
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	_, priv, _ := GenerateKey()
	path := filepath.Join(t.TempDir(), "host.key")
	if err := SaveKey(path, priv); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %04o, want 0600", st.Mode().Perm())
		}
	}
	got, err := LoadKey(path)
	if err != nil || got != priv {
		t.Fatalf("LoadKey = %v (equal %v)", err, got == priv)
	}
	// Saving again over an existing file still leaves 0600.
	if err := SaveKey(path, priv); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeyRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	_, priv, _ := GenerateKey()
	path := filepath.Join(t.TempDir(), "host.key")
	if err := SaveKey(path, priv); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("mode %04o: LoadKey err = %v, want refusal", mode, err)
		}
	}
}

func TestLoadKeyBadContentLeaksNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.key")
	const marker = "TOPSECRETMARKER"
	if err := os.WriteFile(path, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadKey(path)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("LoadKey err = %v", err)
	}
}

func TestSetEnvMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.env")
	orig := "# role env\nKEEP=one\nAPI_TOKEN=old\n\nOTHER=two=three\nAPI_TOKEN=dupe\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetEnv(path, "API_TOKEN", "new value"); err != nil {
		t.Fatal(err)
	}
	if err := SetEnv(path, "ADDED", "x"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "# role env\nKEEP=one\nAPI_TOKEN=new value\n\nOTHER=two=three\nADDED=x\n"
	if string(b) != want {
		t.Fatalf("file =\n%s\nwant\n%s", b, want)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %04o, want 0600", st.Mode().Perm())
		}
	}
	env, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := map[string]string{"KEEP": "one", "API_TOKEN": "new value", "OTHER": "two=three", "ADDED": "x"}
	if len(env) != len(wantEnv) {
		t.Fatalf("LoadEnv = %v", env)
	}
	for k, v := range wantEnv {
		if env[k] != v {
			t.Fatalf("LoadEnv[%s] = %q, want %q", k, env[k], v)
		}
	}
}

func TestSetEnvCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.env")
	if err := SetEnv(path, "_A1", ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "_A1=\n" {
		t.Fatalf("file = %q", b)
	}
}

func TestSetEnvRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.env")
	const marker = "TOPSECRETMARKER"
	for _, name := range []string{"", "lower", "1ABC", "A-B", "A B"} {
		if err := SetEnv(path, name, "v"); err == nil {
			t.Errorf("SetEnv accepted name %q", name)
		}
	}
	for _, v := range []string{marker + "\nX=1", marker + "\r", marker + "\x00"} {
		err := SetEnv(path, "OK_NAME", v)
		if err == nil {
			t.Fatalf("SetEnv accepted value %q", v)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error leaks the value: %v", err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused SetEnv created the file")
	}
}

func TestSetEnvFailureLeavesFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "role.env")
	orig := []byte("KEEP=one\nAPI_TOKEN=old\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	renameFile = func(string, string) error { return errors.New("disk on fire") }
	t.Cleanup(func() { renameFile = os.Rename })

	const marker = "TOPSECRETMARKER"
	err := SetEnv(path, "API_TOKEN", marker)
	if err == nil {
		t.Fatal("SetEnv succeeded despite the failed rename")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error leaks the value: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !bytes.Equal(b, orig) {
		t.Fatalf("file changed to %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir holds %v, want only role.env", names)
	}
}

func TestLoadEnvBadLineLeaksNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.env")
	const marker = "TOPSECRETMARKER"
	if err := os.WriteFile(path, []byte("OK=1\n"+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEnv(path)
	if err == nil || strings.Contains(err.Error(), marker) || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("LoadEnv err = %v", err)
	}
}
