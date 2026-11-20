package adminauth

import (
	"encoding/base32"
	"io"
	"strings"
	"testing"
)

func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := totpEncoding.EncodeToString([]byte("12345678901234567890"))
	for at, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got, _ := TOTPCode(secret, at/TOTPStep); got != want {
			t.Errorf("T=%d: %s, want %s", at, got, want)
		}
	}
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordHashIsArgon2id(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=2,p=2$") {
		t.Fatalf("hash %q %v", h, err)
	}
	if !verifyPassword(h, "correct horse battery") || verifyPassword(h, "correct horse batterY") || verifyPassword("garbage", "x") {
		t.Fatal("verify")
	}
}

// A password piped from Windows PowerShell 5.1 may start with a UTF-8 byte
// order mark and end with CRLF; neither is part of the password (#242).
func TestPipedPasswordDropsBOMAndLineEnd(t *testing.T) {
	for _, in := range []string{"correct horse battery\n", "correct horse battery\r\n", "\ufeffcorrect horse battery\r\n", "correct horse battery"} {
		got, err := readNewPassword(strings.NewReader(in), io.Discard)
		if err != nil || got != "correct horse battery" {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
}
