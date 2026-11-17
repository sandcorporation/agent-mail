package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/store"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// tokenMatches reports whether presented is the token behind the stored hash.
// A stored value without the hash prefix never matches: initSchema hashes
// every such row before the server accepts requests.
func tokenMatches(stored, presented string) bool {
	return presented != "" && strings.HasPrefix(stored, store.TokenHashPrefix) && sameSecret(stored, store.TokenHash(presented))
}

// roleRecord is the stored hash of role and whether it is revoked.
func roleRecord(q queryer, role string) (hash string, revoked bool, ok bool) {
	var at sql.NullString
	if q.QueryRow(`SELECT token,revoked_at FROM roles WHERE role=?`, role).Scan(&hash, &at) != nil {
		return "", false, false
	}
	return hash, at.Valid, true
}

// roleView is one row of GET /api/admin/roles. It never carries the token or
// its hash. Group is the role's group prefix, "" for an internal role.
type roleView struct {
	Role      string  `json:"role"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
	Group     string  `json:"group"`
}

func (s *Server) listRoles() ([]roleView, error) {
	rows, err := s.db.Query(`SELECT role,created_at,revoked_at FROM roles ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []roleView{}
	for rows.Next() {
		var v roleView
		var at sql.NullString
		if err = rows.Scan(&v.Role, &v.CreatedAt, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			v.RevokedAt = &at.String
		}
		if g, ok := s.groupOf(v.Role); ok {
			v.Group = g.Prefix
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

var errNoRole = errors.New("no such role")

// setRevoked revokes (or restores) role. Revoking releases every live slot of
// the role at once, as an intended stop (no offline notice), so its sessions
// drop off presence; from then on authenticate refuses the role and all its
// -N names. Revoking a revoked role keeps the first revoked_at.
func (s *Server) setRevoked(ctx context.Context, role string, revoke bool) (roleView, error) {
	now := s.now()
	var v roleView
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var at sql.NullString
		if err := tx.QueryRow(`SELECT created_at,revoked_at FROM roles WHERE role=?`, role).Scan(&v.CreatedAt, &at); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNoRole
			}
			return err
		}
		v.Role = role
		if !revoke {
			_, err := tx.Exec(`UPDATE roles SET revoked_at=NULL WHERE role=?`, role)
			return err
		}
		stamp := at.String
		if !at.Valid {
			stamp = now.UTC().Format(time.RFC3339Nano)
			if _, err := tx.Exec(`UPDATE roles SET revoked_at=? WHERE role=?`, stamp, role); err != nil {
				return err
			}
		}
		v.RevokedAt = &stamp
		rows, err := tx.Query(`SELECT number FROM slots WHERE base=? AND lease_until>?`, role, ms(now))
		if err != nil {
			return err
		}
		var live []int
		for rows.Next() {
			var n int
			if err = rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			live = append(live, n)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, n := range live {
			if _, err = tx.Exec(`UPDATE slots SET lease_until=?,offline_noticed_at=? WHERE base=? AND number=?`, ms(now), ms(now), role, n); err != nil {
				return err
			}
			if err = logPresenceOffline(tx, role, n, now); err != nil {
				return err
			}
		}
		return nil
	})
	return v, err
}

// adminRolesAPI serves the role list and revoke/restore behind the admin
// proxy secret (writes also behind adminWriteAllowed, checked by adminAPI):
//
//	GET  /api/admin/roles
//	POST /api/admin/roles/{role}/revoke
//	POST /api/admin/roles/{role}/restore
//
// Role names keep their slashes as path separators.
func (s *Server) adminRolesAPI(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out, err := s.listRoles()
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
		return
	}
	role, revoke := strings.CutSuffix(rest, "/revoke")
	if !revoke {
		var restore bool
		if role, restore = strings.CutSuffix(rest, "/restore"); !restore {
			http.NotFound(w, r)
			return
		}
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !wire.ValidRole(role) {
		http.Error(w, "invalid role", 400)
		return
	}
	v, err := s.setRevoked(r.Context(), role, revoke)
	if errors.Is(err, errNoRole) {
		http.Error(w, "no such role", 404)
		return
	}
	if err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	writeJSON(w, v)
}
