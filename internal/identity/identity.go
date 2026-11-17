// Package identity answers "who is calling and what may they do": roles and
// their hashed tokens, revoke, the -N slot names a base token authorises,
// registration and resume, collaborator groups and boss rules, and whoami.
package identity

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Service is the identity rules over the roles, slots and groups tables. It
// holds only what those rules need, so callers build one per use.
type Service struct {
	db         *sql.DB
	clock      func() time.Time
	bossRoles  map[string]bool
	groups     *Groups
	logOffline func(tx *sql.Tx, base string, number int, at time.Time) error
}

// New returns the identity rules over db. clock is the current time,
// bossRoles is AGENT_MAIL_BOSS_ROLES, groups is the server's group cache and
// logOffline records that a slot went offline (presence log) inside a revoke.
func New(db *sql.DB, clock func() time.Time, bossRoles map[string]bool, groups *Groups, logOffline func(tx *sql.Tx, base string, number int, at time.Time) error) *Service {
	return &Service{db: db, clock: clock, bossRoles: bossRoles, groups: groups, logOffline: logOffline}
}

func (s *Service) now() time.Time { return s.clock() }

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func ms(t time.Time) int64 { return t.UnixMilli() }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// principal is an authenticated caller. Role is the mailbox name used for
// storage (slot 1 is stored as its base); Base is the role whose token
// authorised the request; Number is the slot number in the header (0 when
// the header named a role row exactly).
type Principal struct {
	Role   string
	Base   string
	Number int
}

// Queryer is the part of *sql.DB and *sql.Tx the slot code reads through, so
// the same lookups work inside a transaction.
type Queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func SlotRole(base string, n int) string { return base + "-" + strconv.Itoa(n) }

func RoleExists(q Queryer, role string) bool { _, _, ok := roleRecord(q, role); return ok }

func sameSecret(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Authenticate checks the bearer token t against the role header h
// (X-Switchboard-Role, or the old X-Agent-Mail-Role). A role row
// with exactly that name always wins, so a pre-existing role ending in -N
// keeps its own token. Otherwise "<base>-N" is accepted with the token of
// <base>. The slot need not be currently leased: the base token can claim any
// slot anyway, and mail queued for an expired slot must stay readable. A
// revoked role is refused under every name it covers.
func (s *Service) Authenticate(t, h string) (Principal, bool) {
	if t == "" || h == "" || !wire.ValidRole(h) || Reserved(h) {
		return Principal{}, false
	}
	if hash, revoked, ok := roleRecord(s.db, h); ok {
		if !tokenMatches(hash, t) || revoked {
			return Principal{}, false
		}
		return Principal{Role: h, Base: h}, true
	}
	base, n, ok := wire.SplitSlot(h)
	if !ok {
		return Principal{}, false
	}
	hash, revoked, ok := roleRecord(s.db, base)
	if !ok || !tokenMatches(hash, t) || revoked {
		return Principal{}, false
	}
	p := Principal{Role: h, Base: base, Number: n}
	if n == 1 {
		p.Role = base
	}
	return p, true
}

// mailbox returns the stored name for an address: an exact role row as is,
// "<base>-1" as <base>, "<base>-N" as itself. ok is false when no role row
// backs the address.
func Mailbox(q Queryer, name string) (string, bool) {
	if Reserved(name) {
		return "", false
	}
	if RoleExists(q, name) {
		return name, true
	}
	base, n, ok := wire.SplitSlot(name)
	if !ok || !RoleExists(q, base) {
		return "", false
	}
	if n == 1 {
		return base, true
	}
	return name, true
}

// otherSlotMailbox returns the mailbox message id belongs to when that is
// another mailbox of the caller's base (for example mail of <base>-2 while
// the header names <base>). Mailboxes of one base are kept apart: such

// BaseOf is the base whose token authorises name: an exact role row, else
// the base of <base>-N. It is "" for unknown or reserved names.
func BaseOf(q Queryer, name string) string {
	if name == "" || Reserved(name) {
		return ""
	}
	if RoleExists(q, name) {
		return name
	}
	if base, _, ok := wire.SplitSlot(name); ok && RoleExists(q, base) {
		return base
	}
	return ""
}

// SystemSender is the sender of notices. Nobody can Authenticate as it or as
// a numbered form of it (see Reserved), and mail to it is refused.
const SystemSender = "switchboard/system"

// LegacySystemSender is the system sender's old name. It stays reserved, and
// mail sent under it still counts as system mail.
const LegacySystemSender = "agent-mail/system"

// IsSystemSender reports whether name is the system sender under either name.
func IsSystemSender(name string) bool {
	return name == SystemSender || name == LegacySystemSender
}

// Reserved reports whether name is the system sender or a numbered form
// of it. Such names never Authenticate and never receive mail, even when an
// old database or AGENT_MAIL_TOKENS holds a role row by that name.
func Reserved(name string) bool {
	if IsSystemSender(name) {
		return true
	}
	base, _, ok := wire.SplitSlot(name)
	return ok && IsSystemSender(base)
}
