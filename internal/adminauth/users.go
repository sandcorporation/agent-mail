package adminauth

// Built-in admin users: argon2id password hashes and RFC 6238 TOTP
// (HMAC-SHA1, 30 s steps, 6 digits). The admin CLI (switchboard admin …)
// opens the server's database directly and runs inside the container.

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/store"
	"golang.org/x/crypto/argon2"
	"golang.org/x/term"
)

// argonParams are the argon2id costs for new hashes. Stored hashes carry
// their own costs, so raising these later keeps old hashes valid.
type argonParams struct {
	time    uint32
	memory  uint32 // KiB
	threads uint8
}

var argonCost = argonParams{time: 2, memory: 64 * 1024, threads: 2}

const (
	argonKeyLen    = 32
	argonSaltLen   = 16
	TOTPStep       = 30 // seconds per TOTP code
	totpDigits     = 6
	minPasswordLen = 12
	maxPasswordLen = 1024 // bytes; the login request refuses a longer password
)

// dummyHash is checked when the name is unknown, so a missing user costs the
// same time as a wrong password.
var dummyHash = func() string { h, _ := HashPassword("unused password for timing"); return h }()

// HashPassword returns the argon2id PHC string of pw with a random salt.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	c := argonCost
	key := argon2.IDKey([]byte(pw), salt, c.time, c.memory, c.threads, argonKeyLen)
	b := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, c.memory, c.time, c.threads, b.EncodeToString(salt), b.EncodeToString(key)), nil
}

var argonPHC = regexp.MustCompile(`^\$argon2id\$v=(\d+)\$m=(\d+),t=(\d+),p=(\d+)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$`)

func verifyPassword(encoded, pw string) bool {
	m := argonPHC.FindStringSubmatch(encoded)
	if m == nil || m[1] != strconv.Itoa(argon2.Version) {
		return false
	}
	mem, e1 := strconv.ParseUint(m[2], 10, 32)
	tm, e2 := strconv.ParseUint(m[3], 10, 32)
	par, e3 := strconv.ParseUint(m[4], 10, 8)
	salt, e4 := base64.RawStdEncoding.DecodeString(m[5])
	want, e5 := base64.RawStdEncoding.DecodeString(m[6])
	if err := errors.Join(e1, e2, e3, e4, e5); err != nil || par == 0 || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(tm), uint32(mem), uint8(par), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return totpEncoding.EncodeToString(b), nil
}

// TOTPCode is the RFC 6238 code for one time step.
func TOTPCode(secret string, step int64) (string, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000), nil
}

// matchTOTP returns the step (now ±1) whose code is code and that is newer
// than last, or 0. Taking only newer steps rejects a replayed code.
func matchTOTP(secret, code string, now time.Time, last int64) int64 {
	if len(code) != totpDigits {
		return 0
	}
	cur := now.Unix() / TOTPStep
	var found int64
	for _, st := range []int64{cur - 1, cur, cur + 1} {
		want, err := TOTPCode(secret, st)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && st > last {
			found = st
		}
	}
	return found
}

func otpauthURI(name, secret string) string {
	label := url.PathEscape(config.ProductName + ":" + name)
	q := url.Values{"secret": {secret}, "issuer": {config.ProductName}, "algorithm": {"SHA1"}, "digits": {strconv.Itoa(totpDigits)}, "period": {strconv.Itoa(TOTPStep)}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// checkBuiltinLogin verifies password and TOTP, and records the used step.
// The password is always hashed, even for an unknown or disabled name.
func (s *Service) checkBuiltinLogin(req loginRequest, now time.Time) bool {
	var hash, secret string
	var last int64
	var disabled sql.NullString
	err := s.db.QueryRow(`SELECT password_hash,totp_secret,totp_last_step,disabled_at FROM admin_users WHERE name=?`, req.Name).Scan(&hash, &secret, &last, &disabled)
	if err != nil {
		verifyPassword(dummyHash, req.Password)
		return false
	}
	if !verifyPassword(hash, req.Password) || disabled.Valid {
		return false
	}
	step := matchTOTP(secret, req.Code, now, last)
	if step == 0 {
		return false
	}
	// The conditional update makes two logins racing on one code end with one winner.
	res, err := s.db.Exec(`UPDATE admin_users SET totp_last_step=? WHERE name=? AND totp_last_step<?`, step, req.Name, step)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

var adminNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// CLI serves `admin create|passwd|totp-reset|disable NAME`. It opens
// DB_PATH (the server's database) directly.
func CLI(action, name string, jsonOut bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	switch action {
	case "create", "passwd", "totp-reset", "disable":
	default:
		return 0, cli.Usagef("admin: unknown action %q (create, passwd, totp-reset, disable)", action)
	}
	if !adminNamePattern.MatchString(name) {
		return 0, cli.Usagef("admin: invalid name %q (1-64 of A-Z a-z 0-9 . _ @ -)", name)
	}
	db, err := store.Open(config.Env("DB_PATH", "/data/mail.db"))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exists := func() (bool, error) {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM admin_users WHERE name=?`, name).Scan(&n)
		return n == 1, err
	}
	ok, err := exists()
	if err != nil {
		return 0, err
	}
	if action == "create" && ok {
		return 0, fmt.Errorf("admin user %s already exists", name)
	}
	if action != "create" && !ok {
		return 0, fmt.Errorf("no admin user %s", name)
	}
	endSessions := func() error {
		_, err := db.Exec(`DELETE FROM admin_sessions WHERE mode='builtin' AND user=?`, name)
		return err
	}
	var secret string
	switch action {
	case "create", "passwd":
		pw, err := readNewPassword(stdin, stderr)
		if err != nil {
			return 0, err
		}
		hash, err := HashPassword(pw)
		if err != nil {
			return 0, err
		}
		if action == "passwd" {
			if _, err = db.Exec(`UPDATE admin_users SET password_hash=? WHERE name=?`, hash, name); err != nil {
				return 0, err
			}
			break
		}
		if secret, err = newTOTPSecret(); err != nil {
			return 0, err
		}
		if _, err = db.Exec(`INSERT INTO admin_users(name,password_hash,totp_secret,created_at) VALUES(?,?,?,?)`, name, hash, secret, now); err != nil {
			return 0, err
		}
	case "totp-reset":
		if secret, err = newTOTPSecret(); err != nil {
			return 0, err
		}
		if _, err = db.Exec(`UPDATE admin_users SET totp_secret=?,totp_last_step=0 WHERE name=?`, secret, name); err != nil {
			return 0, err
		}
	case "disable":
		if _, err = db.Exec(`UPDATE admin_users SET disabled_at=COALESCE(disabled_at,?) WHERE name=?`, now, name); err != nil {
			return 0, err
		}
	}
	if action != "create" {
		if err = endSessions(); err != nil {
			return 0, err
		}
	}
	out := map[string]string{"name": name, "action": action}
	if secret != "" {
		out["secret"], out["otpauth_uri"] = secret, otpauthURI(name, secret)
	}
	if jsonOut {
		b, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(b))
		return 0, nil
	}
	switch action {
	case "create":
		fmt.Fprintf(stdout, "created %s\n", name)
	case "passwd":
		fmt.Fprintf(stdout, "password changed for %s; sessions ended\n", name)
	case "totp-reset":
		fmt.Fprintf(stdout, "new TOTP secret for %s; sessions ended\n", name)
	case "disable":
		fmt.Fprintf(stdout, "disabled %s; sessions ended\n", name)
	}
	if secret != "" {
		fmt.Fprintf(stdout, "otpauth URI (shown once): %s\nsecret (shown once): %s\n", out["otpauth_uri"], secret)
	}
	return 0, nil
}

// readNewPassword prompts twice without echo on a terminal; otherwise it
// reads the first line of stdin.
func readNewPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	var pw string
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Password: ")
		a, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(stderr, "Repeat password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
		pw = string(a)
	} else {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		// Windows PowerShell 5.1 can put a UTF-8 byte order mark in front
		// of what it pipes to a program (#242); it is never part of a
		// password.
		pw = strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "\ufeff")
	}
	if err := checkNewPassword(pw); err != nil {
		return "", err
	}
	return pw, nil
}

// checkNewPassword is the password policy of `admin create` and the setup
// page.
func checkNewPassword(pw string) error {
	if len([]rune(pw)) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	if len(pw) > maxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordLen)
	}
	return nil
}
