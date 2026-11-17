package store

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var dropAt = time.Date(2026, 11, 18, 9, 30, 5, 0, time.UTC)

const dropCopySuffix = ".pre-drop-plain-backup-20261118T093005Z"

// fixedClock pins the time used in copy names and captures log output.
func fixedClock(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := now
	now = func() time.Time { return dropAt.In(time.FixedZone("KST", 9*3600)) }
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		now = old
		log.SetOutput(os.Stderr)
	})
	return &buf
}

// openWithBackup opens a database whose roles are hashed and which still
// holds roles_plain_backup from an earlier hash migration.
func openWithBackup(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`CREATE TABLE roles(role TEXT PRIMARY KEY, token TEXT NOT NULL, created_at TEXT NOT NULL); CREATE TABLE roles_plain_backup(role TEXT PRIMARY KEY, token TEXT NOT NULL, created_at TEXT NOT NULL, backed_up_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"a", "b"} {
		if _, err = db.Exec(`INSERT INTO roles VALUES(?,?,'then')`, r, TokenHash("plain-secret-"+r)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO roles_plain_backup VALUES(?,?,'then','then')`, r, "plain-secret-"+r); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func copies(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.pre-drop-plain-backup-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStartDropsPlainBackupAfterCopy(t *testing.T) {
	logs := fixedClock(t)
	db, path := openWithBackup(t)
	if err := Init(db); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, db, "roles_plain_backup") {
		t.Fatal("roles_plain_backup still there")
	}
	copyPath := path + dropCopySuffix
	if got := copies(t, filepath.Dir(path)); len(got) != 1 || got[0] != copyPath {
		t.Fatalf("copies=%v want %s", got, copyPath)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("copy mode %v", st.Mode().Perm())
		}
	}
	cp, err := sql.Open("sqlite", copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var a, b string
	if err = cp.QueryRow(`SELECT (SELECT token FROM roles_plain_backup WHERE role='a'),(SELECT token FROM roles_plain_backup WHERE role='b')`).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != "plain-secret-a" || b != "plain-secret-b" {
		t.Fatalf("copy rows a=%q b=%q", a, b)
	}
	want := "role tokens: dropped roles_plain_backup (2 rows); copy before drop at " + copyPath + "\n"
	if !strings.HasSuffix(logs.String(), want) {
		t.Fatalf("log=%q want suffix %q", logs.String(), want)
	}
	if strings.Contains(logs.String(), "plain-secret") {
		t.Fatalf("log carries a token: %q", logs.String())
	}
	// The next start finds no table and changes nothing.
	logs.Reset()
	if err = Init(db); err != nil {
		t.Fatal(err)
	}
	if got := copies(t, filepath.Dir(path)); len(got) != 1 || logs.Len() != 0 {
		t.Fatalf("second start: copies=%v log=%q", got, logs.String())
	}
}

func TestStartWithoutPlainBackupMakesNoCopy(t *testing.T) {
	logs := fixedClock(t)
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = Init(db); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, db, "roles_plain_backup") || len(copies(t, filepath.Dir(path))) != 0 || strings.Contains(logs.String(), "role tokens") {
		t.Fatalf("copies=%v log=%q", copies(t, filepath.Dir(path)), logs.String())
	}
}

func TestFailedCopyKeepsPlainBackup(t *testing.T) {
	logs := fixedClock(t)
	db, path := openWithBackup(t)
	// A file already has the copy's name: it is kept, and so is the table.
	if err := os.WriteFile(path+dropCopySuffix, []byte("someone else's"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Init(db); err != nil {
		t.Fatalf("start should go on: %v", err)
	}
	if !hasTable(t, db, "roles_plain_backup") {
		t.Fatal("table dropped without a copy")
	}
	if got, _ := os.ReadFile(path + dropCopySuffix); string(got) != "someone else's" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	if !strings.Contains(logs.String(), "role tokens: kept roles_plain_backup; copy before drop failed: ") || strings.Contains(logs.String(), "plain-secret") {
		t.Fatalf("log=%q", logs.String())
	}
	// The next start, a second later, copies and drops.
	now = func() time.Time { return dropAt.Add(time.Second) }
	if err := Init(db); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, db, "roles_plain_backup") {
		t.Fatal("retry did not drop")
	}
}

func TestUnwritableDirectoryKeepsPlainBackup(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test user cannot write")
	}
	fixedClock(t)
	db, path := openWithBackup(t)
	dir := filepath.Dir(path)
	// WAL files exist before the directory turns read only, so writes still work.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE warm(x)`); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if err := Init(db); err != nil {
		t.Fatalf("start should go on: %v", err)
	}
	if !hasTable(t, db, "roles_plain_backup") || len(copies(t, dir)) != 0 {
		t.Fatalf("table=%v copies=%v", hasTable(t, db, "roles_plain_backup"), copies(t, dir))
	}
}

// After a rollback to a binary from before hashing, the database has plain
// rows again and an old backup. The drop runs first, so the backup the hash
// migration writes on this start is still there afterwards.
func TestDropRunsBeforeHashMigration(t *testing.T) {
	fixedClock(t)
	db, _ := openWithBackup(t)
	if _, err := db.Exec(`UPDATE roles SET token='plain-secret-new' WHERE role='a'`); err != nil {
		t.Fatal(err)
	}
	if err := Init(db); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := db.QueryRow(`SELECT token FROM roles_plain_backup WHERE role='a'`).Scan(&token); err != nil {
		t.Fatalf("new backup missing: %v", err)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM roles_plain_backup`).Scan(&n)
	if token != "plain-secret-new" || n != 1 {
		t.Fatalf("backup token=%q rows=%d", token, n)
	}
}
