package identity

// Role aliases and renames. A rename moves a role to a new name in place:
// the role row keeps its token, every stored reference to the old name (and
// its numbered forms <old>-N) is rewritten, and the old name is kept in
// role_aliases. From then on the old name, and <old>-N, authenticate with the
// same token as the new role, and mail sent to them is delivered to the new
// role, so sessions and senders that still use the old name keep working.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

var (
	errRenameConflict = errors.New("new name is taken")
	errRenameSame     = errors.New("old and new name are the same")
)

// Resolve maps an old role name to the role it was renamed to: an alias as
// its role, <alias>-N as <role>-N. Any other name is returned unchanged. A
// name that is itself a role row is never mapped.
func Resolve(q Queryer, name string) string {
	if name == "" || RoleExists(q, name) {
		return name
	}
	if to, ok := aliasTarget(q, name); ok {
		return to
	}
	if base, n, ok := wire.SplitSlot(name); ok && !RoleExists(q, base) {
		if to, ok := aliasTarget(q, base); ok {
			return SlotRole(to, n)
		}
	}
	return name
}

func aliasTarget(q Queryer, alias string) (string, bool) {
	var to string
	if q.QueryRow(`SELECT role FROM role_aliases WHERE alias=?`, alias).Scan(&to) != nil {
		return "", false
	}
	return to, true
}

// isAlias reports whether name, or the base of a numbered name, is an alias.
func isAlias(q Queryer, name string) bool {
	if _, ok := aliasTarget(q, name); ok {
		return true
	}
	base, _, ok := wire.SplitSlot(name)
	if !ok {
		return false
	}
	_, ok = aliasTarget(q, base)
	return ok
}

// RenameView is the result of a rename.
type RenameView struct {
	Role    string   `json:"role"`
	Aliases []string `json:"aliases"`
}

// fullNameColumns hold a whole role name: <old> or <old>-N is rewritten.
var fullNameColumns = [][2]string{
	{"messages", "sender"}, {"messages", "recipient"}, {"role_states", "role"},
	{"presence_log", "role"}, {"slots", "parent"}, {"groups", "boss_role"}, {"invites", "role"},
	{"questions", "asker"}, {"questions", "answered_by"}, {"questions", "answerer"},
	{"permissions", "requester"}, {"permissions", "declared_parent"}, {"permissions", "parent"}, {"permissions", "answered_by"},
}

// baseColumns hold a base: only <old> itself is rewritten.
var baseColumns = [][2]string{
	{"slots", "base"}, {"presence_log", "base"}, {"questions", "asker_base"},
	{"permissions", "requester_base"}, {"permissions", "parent_base"},
	{"handoffs", "base"},
}

// labelColumns hold label addresses label:<base>/<label>.
var labelColumns = [][2]string{{"messages", "recipient"}, {"messages", "addressed_to"}}

// slotSuffix matches c = <?1>-N with N a number without a leading zero.
const slotSuffix = `(substr(%[1]s,1,length(?1)+1)=?1||'-' AND substr(%[1]s,length(?1)+2) GLOB '[1-9]*' AND substr(%[1]s,length(?1)+2) NOT GLOB '*[^0-9]*')`

// RenameRole moves role old to new (see the package comment above). It fails
// with errNoRole when old is not a role, and errRenameConflict when new (or
// new-N) is a role or an alias, or when a separate role named <old>-N exists
// (its rows could not be told apart from old's).
func (s *Service) RenameRole(ctx context.Context, old, new string) (RenameView, error) {
	if old == new {
		return RenameView{}, errRenameSame
	}
	stamp := s.now().UTC().Format(time.RFC3339Nano)
	var v RenameView
	err := store.InTx(ctx, s.db, func(tx *sql.Tx) error {
		if !RoleExists(tx, old) {
			return errNoRole
		}
		if RoleExists(tx, new) || isAlias(tx, new) {
			return errRenameConflict
		}
		if base, _, ok := wire.SplitSlot(new); ok && (RoleExists(tx, base) || isAlias(tx, base)) {
			return errRenameConflict
		}
		var n int
		for _, name := range []string{old, new} {
			if err := tx.QueryRow(`SELECT count(*) FROM roles WHERE `+fmt.Sprintf(slotSuffix, "role"), name).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return errRenameConflict
			}
		}
		exec := func(q string, args ...any) error { _, err := tx.Exec(q, args...); return err }
		if err := exec(`UPDATE roles SET role=?2 WHERE role=?1`, old, new); err != nil {
			return err
		}
		for _, c := range fullNameColumns {
			q := `UPDATE OR IGNORE ` + c[0] + ` SET ` + c[1] + `=?2||substr(` + c[1] + `,length(?1)+1) WHERE ` + c[1] + `=?1 OR ` + fmt.Sprintf(slotSuffix, c[1])
			if err := exec(q, old, new); err != nil {
				return err
			}
		}
		for _, c := range baseColumns {
			if err := exec(`UPDATE OR IGNORE `+c[0]+` SET `+c[1]+`=?2 WHERE `+c[1]+`=?1`, old, new); err != nil {
				return err
			}
		}
		for _, c := range labelColumns {
			q := `UPDATE ` + c[0] + ` SET ` + c[1] + `='label:'||?2||substr(` + c[1] + `,length(?1)+7) WHERE substr(` + c[1] + `,1,length(?1)+7)='label:'||?1||'/'`
			if err := exec(q, old, new); err != nil {
				return err
			}
		}
		if err := exec(`UPDATE OR IGNORE question_settings SET target=?2 WHERE target=?1`, old, new); err != nil {
			return err
		}
		// Earlier names of old now point to new, so no alias chains form.
		if err := exec(`UPDATE role_aliases SET role=?2 WHERE role=?1`, old, new); err != nil {
			return err
		}
		if err := exec(`INSERT INTO role_aliases(alias,role,created_at) VALUES(?,?,?)`, old, new, stamp); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT alias FROM role_aliases WHERE role=? ORDER BY alias`, new)
		if err != nil {
			return err
		}
		defer rows.Close()
		v = RenameView{Role: new, Aliases: []string{}}
		for rows.Next() {
			var a string
			if err = rows.Scan(&a); err != nil {
				return err
			}
			v.Aliases = append(v.Aliases, a)
		}
		return rows.Err()
	})
	if err == nil {
		// groups.boss_role may have changed; refresh the cache.
		err = s.LoadGroups()
	}
	return v, err
}

// adminRename serves POST /api/admin/roles/{role}/rename with {"to": NEW}.
func (s *Service) adminRename(w http.ResponseWriter, r *http.Request, old string) {
	var req struct {
		To string `json:"to"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || !wire.ValidRole(req.To) {
		http.Error(w, "to: a valid role name is required", 400)
		return
	}
	if Reserved(old) || Reserved(req.To) {
		http.Error(w, "role name is reserved", 403)
		return
	}
	v, err := s.RenameRole(r.Context(), old, req.To)
	switch {
	case errors.Is(err, errNoRole):
		http.Error(w, "no such role", 404)
	case errors.Is(err, errRenameConflict):
		http.Error(w, "new name is taken (role, alias, or a numbered form of one)", 409)
	case errors.Is(err, errRenameSame):
		http.Error(w, "old and new name are the same", 400)
	case err != nil:
		http.Error(w, "rename failed", 500)
	default:
		writeJSON(w, v)
	}
}
