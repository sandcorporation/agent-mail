// Package store opens the SQLite database and owns its schema: every table,
// the additive migrations that upgrade older databases in place, and the
// transaction helper.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultQuestionTarget is the question_settings row every database starts with.
const DefaultQuestionTarget = "default"

// Open opens and upgrades the SQLite database at path, as the server does.
func Open(path string) (*sql.DB, error) {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	if err = Init(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Init creates the tables when missing and upgrades databases created by
// older versions in place. It is safe to run on every start.
func Init(db *sql.DB) error {
	if _, e := db.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT,sender TEXT NOT NULL,recipient TEXT NOT NULL,subject TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,task_id TEXT,thread_id INTEGER,reply_to INTEGER,status TEXT NOT NULL DEFAULT 'pending',idem TEXT,priority TEXT NOT NULL DEFAULT 'normal', UNIQUE(sender,idem)); CREATE INDEX IF NOT EXISTS idx_recipient ON messages(recipient,id); CREATE INDEX IF NOT EXISTS idx_sender ON messages(sender,id); CREATE INDEX IF NOT EXISTS idx_thread ON messages(thread_id); CREATE INDEX IF NOT EXISTS idx_task ON messages(task_id); CREATE TABLE IF NOT EXISTS role_states(role TEXT NOT NULL,message_id INTEGER NOT NULL,status TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(role,message_id)); CREATE TABLE IF NOT EXISTS roles(role TEXT PRIMARY KEY, token TEXT NOT NULL, created_at TEXT NOT NULL); CREATE TABLE IF NOT EXISTS slots(base TEXT NOT NULL,number INTEGER NOT NULL,machine TEXT NOT NULL,directory TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',label TEXT NOT NULL DEFAULT '',parent TEXT NOT NULL DEFAULT '',registered_at INTEGER NOT NULL,lease_until INTEGER NOT NULL,PRIMARY KEY(base,number),UNIQUE(base,machine,directory));`); e != nil {
		return e
	}
	has, e := HasColumn(db, "messages", "priority")
	if e != nil {
		return e
	}
	if !has {
		// Databases from before message priority: existing mail becomes normal.
		if _, e = db.Exec(`ALTER TABLE messages ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal'`); e != nil {
			return e
		}
	}
	if e = migrateLabels(db); e != nil {
		return e
	}
	if e = migrateDirective(db); e != nil {
		return e
	}
	if e = migratePresence(db); e != nil {
		return e
	}
	if e = migratePresenceLog(db); e != nil {
		return e
	}
	if e = migrateAlerts(db); e != nil {
		return e
	}
	// Before the hash migration, so a backup it writes on this start (after a
	// rollback to a binary from before hashing) survives until the next one.
	if e = dropPlainBackup(db); e != nil {
		return e
	}
	if e = migrateRoleTokens(db); e != nil {
		return e
	}
	if e = migratePermissions(db); e != nil {
		return e
	}
	if e = migrateQuestionSettings(db); e != nil {
		return e
	}
	if e = migrateGroups(db); e != nil {
		return e
	}
	if e = migrateAdminAuth(db); e != nil {
		return e
	}
	if e = migrateInvites(db); e != nil {
		return e
	}
	return migrateQuestions(db)
}

func HasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, e := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if e != nil {
		return false, e
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if e = rows.Scan(&name); e != nil {
			return false, e
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Role tokens are stored as TokenHashPrefix + hex(sha256(token)), never as
// the token itself. Tokens are high-entropy (registration asks for at least
// 24 characters), so a fast hash is enough; comparisons stay constant time.
// Neither tokens nor their hashes leave the server: no API response or log
// line carries them.
const TokenHashPrefix = "sha256:"

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return TokenHashPrefix + hex.EncodeToString(sum[:])
}

// migrateRoleTokens upgrades the roles table in place: it adds revoked_at and
// hashes every token that lacks the hash prefix. Before the first hash it
// copies those plain rows to roles_plain_backup (in the same transaction), so
// a failure part way leaves the table as it was. Rows that already carry the
// prefix are left alone, so running it again changes nothing.
//
// roles_plain_backup holds plain tokens. dropPlainBackup removes it on the
// next start, after copying the database file aside.
func migrateRoleTokens(db *sql.DB) error {
	has, err := HasColumn(db, "roles", "revoked_at")
	if err != nil {
		return err
	}
	if !has {
		if _, err = db.Exec(`ALTER TABLE roles ADD COLUMN revoked_at TEXT`); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT role,token FROM roles WHERE substr(token,1,7)<>?`, TokenHashPrefix)
	if err != nil {
		return err
	}
	plain := map[string]string{}
	for rows.Next() {
		var role, token string
		if err = rows.Scan(&role, &token); err != nil {
			rows.Close()
			return err
		}
		plain[role] = token
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(plain) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS roles_plain_backup(role TEXT PRIMARY KEY, token TEXT NOT NULL, created_at TEXT NOT NULL, backed_up_at TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT OR REPLACE INTO roles_plain_backup(role,token,created_at,backed_up_at) SELECT role,token,created_at,? FROM roles WHERE substr(token,1,7)<>?`, now, TokenHashPrefix); err != nil {
		return err
	}
	for role, token := range plain {
		if _, err = tx.Exec(`UPDATE roles SET token=? WHERE role=? AND token=?`, TokenHash(token), role, token); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("role tokens: hashed %d plain rows; plain copies kept in roles_plain_backup", len(plain))
	return nil
}

// now is the clock for names of copies made before a drop; tests replace it.
var now = time.Now

// dropPlainBackup removes roles_plain_backup, the plain tokens kept by
// migrateRoleTokens. It first copies the whole database next to its file as
// <file>.pre-drop-plain-backup-<UTC time>, mode 0600, never overwriting a file
// already there. When the copy cannot be made it logs a line, keeps the table
// and lets the start go on; the next start tries again. A database without
// the table is left alone.
func dropPlainBackup(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='roles_plain_backup'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	var rowCount int
	if err := db.QueryRow(`SELECT count(*) FROM roles_plain_backup`).Scan(&rowCount); err != nil {
		return err
	}
	copyPath, err := copyBeforeDrop(db)
	if err != nil {
		log.Printf("role tokens: kept roles_plain_backup; copy before drop failed: %v", err)
		return nil
	}
	if _, err = db.Exec(`DROP TABLE roles_plain_backup`); err != nil {
		return err
	}
	log.Printf("role tokens: dropped roles_plain_backup (%d rows); copy before drop at %s", rowCount, copyPath)
	return nil
}

func copyBeforeDrop(db *sql.DB) (string, error) {
	var seq int
	var name, file string
	if err := db.QueryRow(`SELECT seq,name,file FROM pragma_database_list WHERE name='main'`).Scan(&seq, &name, &file); err != nil {
		return "", err
	}
	if file == "" {
		return "", errors.New("the database has no file")
	}
	path := file + ".pre-drop-plain-backup-" + now().UTC().Format("20060102T150405Z")
	// Create the file first, so it never exists with a wider mode and an
	// existing one is never overwritten; VACUUM INTO accepts an empty file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	f.Close()
	if _, err = db.Exec(`VACUUM INTO ?`, path); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return "", err
	}
	return path, nil
}

// LabelColumns are added to messages in place; see migrateLabels.
var LabelColumns = []struct{ Name, Def string }{
	{"addressed_to", `TEXT`},   // the label address, for mail sent to one
	{"held_id", `INTEGER`},     // the ID the message had while held
	{"held_at", `TEXT`},        // RFC 3339; when the message was held
	{"delivered_at", `TEXT`},   // RFC 3339; when held mail moved to a session
	{"held_notice_at", `TEXT`}, // RFC 3339; for telling the sender nobody connected (not set here)
}

func migrateLabels(db *sql.DB) error {
	for _, c := range LabelColumns {
		has, err := HasColumn(db, "messages", c.Name)
		if err != nil {
			return err
		}
		if !has {
			if _, err = db.Exec(`ALTER TABLE messages ADD COLUMN ` + c.Name + ` ` + c.Def); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_held ON messages(held_at) WHERE status='held'`)
	return err
}

// migrateDirective adds messages.directive (1: a reply sent with --directive,
// which the progress view counts as a directive) and the index the answer
// lookup by mention reads (mail.AnswerCandidates).
func migrateDirective(db *sql.DB) error {
	has, err := HasColumn(db, "messages", "directive")
	if err != nil {
		return err
	}
	if !has {
		if _, err = db.Exec(`ALTER TABLE messages ADD COLUMN directive INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sender_recipient ON messages(sender,recipient,created_at)`)
	return err
}

// presenceColumns are added to slots in place; see migratePresence.
var presenceColumns = []struct{ name, def string }{
	{"state", `TEXT NOT NULL DEFAULT ''`},
	{"context_tokens", `INTEGER NOT NULL DEFAULT -1`}, // -1: never reported
	{"last_turn_at", `INTEGER NOT NULL DEFAULT 0`},    // Unix ms, 0: never reported
	{"reported_at", `INTEGER NOT NULL DEFAULT 0`},     // Unix ms of the last register/heartbeat/takeover
	// offline_noticed_at is the place to remember that an offline transition
	// has been handled (for example, a parent notified). Every report resets
	// it to 0. See offlineUnnoticed and markOfflineNoticed.
	{"offline_noticed_at", `INTEGER NOT NULL DEFAULT 0`},
}

func migratePresence(db *sql.DB) error {
	for _, c := range presenceColumns {
		has, err := HasColumn(db, "slots", c.name)
		if err != nil {
			return err
		}
		if !has {
			if _, err = db.Exec(`ALTER TABLE slots ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

func migratePresenceLog(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS presence_log(id INTEGER PRIMARY KEY AUTOINCREMENT,base TEXT NOT NULL,number INTEGER NOT NULL,role TEXT NOT NULL,state TEXT NOT NULL,started_at INTEGER NOT NULL,ended_at INTEGER);
CREATE INDEX IF NOT EXISTS idx_presence_log_slot ON presence_log(base,number,ended_at);
CREATE INDEX IF NOT EXISTS idx_presence_log_started ON presence_log(started_at);`)
	return err
}

// migrateAlerts adds the alerts table (additive).
func migrateAlerts(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS alerts(kind TEXT NOT NULL,key TEXT NOT NULL,day TEXT NOT NULL,sent_at INTEGER NOT NULL,PRIMARY KEY(kind,key,day))`)
	return err
}

func migratePermissions(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS permissions(id TEXT PRIMARY KEY,requester TEXT NOT NULL,requester_base TEXT NOT NULL,tool TEXT NOT NULL,summary TEXT NOT NULL,declared_parent TEXT NOT NULL DEFAULT '',parent TEXT NOT NULL DEFAULT '',parent_base TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,decision TEXT NOT NULL DEFAULT '',note TEXT NOT NULL DEFAULT '',answered_by TEXT NOT NULL DEFAULT '',answered_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_permissions_requester ON permissions(requester_base,created_at);
CREATE INDEX IF NOT EXISTS idx_permissions_parent ON permissions(parent_base,created_at);`)
	return err
}

func migrateQuestionSettings(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS question_settings(target TEXT PRIMARY KEY,can_ask INTEGER NOT NULL DEFAULT 0,watchers TEXT NOT NULL DEFAULT '[]',discord INTEGER NOT NULL DEFAULT 0,updated_at TEXT NOT NULL)`); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO question_settings(target,can_ask,watchers,discord,updated_at) VALUES(?,0,'[]',0,?)`, DefaultQuestionTarget, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// migrateGroups adds the groups table (additive). revoked_at is set when the
// group is revoked; its roles are revoked at the same time.
func migrateGroups(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS groups(prefix TEXT PRIMARY KEY, boss_role TEXT NOT NULL, created_at TEXT NOT NULL, revoked_at TEXT)`)
	return err
}

func migrateAdminAuth(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS admin_users(name TEXT PRIMARY KEY, password_hash TEXT NOT NULL, totp_secret TEXT NOT NULL, totp_last_step INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, disabled_at TEXT);
CREATE TABLE IF NOT EXISTS admin_sessions(id_hash TEXT PRIMARY KEY, user TEXT NOT NULL, mode TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS idx_admin_sessions_user ON admin_sessions(user);`)
	return err
}

// migrateInvites adds the invites table (additive). code_hash is
// TokenHash(normalized code); the code itself is never stored.
func migrateInvites(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS invites(id INTEGER PRIMARY KEY AUTOINCREMENT, code_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL, role TEXT, expires_at TEXT NOT NULL, used_at TEXT, used_by TEXT, created_by TEXT NOT NULL, created_at TEXT NOT NULL, cancelled_at TEXT)`)
	return err
}

func migrateQuestions(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS questions(id TEXT PRIMARY KEY,asker TEXT NOT NULL,asker_base TEXT NOT NULL,questions TEXT NOT NULL,answers TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'pending',answered_by TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,answered_at INTEGER NOT NULL DEFAULT 0,withdrawn_at INTEGER NOT NULL DEFAULT 0,seq INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idx_questions_seq ON questions(seq);
CREATE INDEX IF NOT EXISTS idx_questions_asker ON questions(asker_base,created_at);
CREATE INDEX IF NOT EXISTS idx_questions_answered ON questions(status,answered_at);`)
	if err != nil {
		return err
	}
	// answerer: 'owner' when no boss role could answer at ask time.
	has, err := HasColumn(db, "questions", "answerer")
	if err != nil || has {
		return err
	}
	_, err = db.Exec(`ALTER TABLE questions ADD COLUMN answerer TEXT NOT NULL DEFAULT ''`)
	return err
}

// InTx runs f in one transaction on db: it commits when f returns nil and
// rolls back otherwise.
func InTx(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
