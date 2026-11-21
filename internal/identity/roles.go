package identity

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// tokenMatches reports whether presented is the token behind the stored hash.
// A stored value without the hash prefix never matches: initSchema hashes
// every such row before the server accepts requests.
func tokenMatches(stored, presented string) bool {
	return presented != "" && strings.HasPrefix(stored, store.TokenHashPrefix) && sameSecret(stored, store.TokenHash(presented))
}

// roleRecord is the stored hash of role and whether it is revoked.
func roleRecord(q Queryer, role string) (hash string, revoked bool, ok bool) {
	var at sql.NullString
	if q.QueryRow(`SELECT token,revoked_at FROM roles WHERE role=?`, role).Scan(&hash, &at) != nil {
		return "", false, false
	}
	return hash, at.Valid, true
}

// RoleView is one row of GET /api/admin/roles. It never carries the token or
// its hash. Group is the role's group prefix, "" for an internal role.
type RoleView struct {
	Role      string  `json:"role"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
	Group     string  `json:"group"`
}

func (s *Service) ListRoles() ([]RoleView, error) {
	rows, err := s.db.Query(`SELECT role,created_at,revoked_at FROM roles ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoleView{}
	for rows.Next() {
		var v RoleView
		var at sql.NullString
		if err = rows.Scan(&v.Role, &v.CreatedAt, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			v.RevokedAt = &at.String
		}
		if g, ok := s.GroupOf(v.Role); ok {
			v.Group = g.Prefix
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

var errNoRole = errors.New("no such role")

// SetRevoked revokes (or restores) role. Revoking releases every live slot of
// the role at once, as an intended stop (no offline notice), so its sessions
// drop off presence; from then on Authenticate refuses the role and all its
// -N names. Revoking a revoked role keeps the first revoked_at. A first
// revoke also runs the RevokeNotice, if set.
func (s *Service) SetRevoked(ctx context.Context, role string, revoke bool) (RoleView, error) {
	now := s.now()
	var v RoleView
	var after func()
	err := store.InTx(ctx, s.db, func(tx *sql.Tx) error {
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
			if err := revokeRoleCredentials(tx, role, now); err != nil {
				return err
			}
			if s.revoked != nil {
				var err error
				if after, err = s.revoked(tx, role, stamp, now); err != nil {
					return err
				}
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
			if err = s.logOffline(tx, role, n, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && after != nil {
		after()
	}
	return v, err
}

// AdminRolesAPI serves the role list and revoke/restore behind the admin
// proxy secret (writes also behind adminWriteAllowed, checked by adminAPI):
//
//	GET  /api/admin/roles
//	POST /api/admin/roles/{role}/revoke
//	POST /api/admin/roles/{role}/restore
//	POST /api/admin/roles/{role}/rename   {"to": NEW}
//
// Role names keep their slashes as path separators.
func (s *Service) AdminRolesAPI(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out, err := s.ListRoles()
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
		return
	}
	if old, ok := strings.CutSuffix(rest, "/rename"); ok {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !wire.ValidRole(old) {
			http.Error(w, "invalid role", 400)
			return
		}
		s.adminRename(w, r, old)
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
	v, err := s.SetRevoked(r.Context(), role, revoke)
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

func (s *Service) Register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Role       string `json:"role"`
		Credential string `json:"credential"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || !wire.ValidRole(req.Role) || req.Credential == "" {
		http.Error(w, "role and credential required", 400)
		return
	}
	if Reserved(req.Role) {
		http.Error(w, "role name is reserved", 403)
		return
	}
	if isAlias(s.db, req.Role) {
		http.Error(w, "role name was renamed; use the new name", 409)
		return
	}
	if g, ok := s.GroupOf(req.Role); ok && g.RevokedAt != nil {
		http.Error(w, "group revoked", 403)
		return
	}
	var existing string
	var revokedAt sql.NullString
	err := s.db.QueryRow(`SELECT token,revoked_at FROM roles WHERE role=?`, req.Role).Scan(&existing, &revokedAt)
	if err == nil {
		if !tokenMatches(existing, req.Credential) {
			http.Error(w, "role already registered; valid credential required to resume", 409)
			return
		}
		if revokedAt.Valid {
			http.Error(w, "role revoked", 401)
			return
		}
		writeJSON(w, map[string]string{"role": req.Role, "status": "resumed"})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "registration failed", 500)
		return
	}
	enrollment := config.EnvAlias("ENROLLMENT_TOKEN")
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(req.Credential) < 24 || enrollment == "" || len(enrollment) != len(provided) || subtle.ConstantTimeCompare([]byte(enrollment), []byte(provided)) != 1 {
		http.Error(w, "enrollment authorization required", 401)
		return
	}
	if len(enrollment) == len(req.Credential) && subtle.ConstantTimeCompare([]byte(enrollment), []byte(req.Credential)) == 1 {
		http.Error(w, "role credential must differ from enrollment credential", 400)
		return
	}
	_, err = s.db.Exec(`INSERT INTO roles(role,token,created_at) VALUES(?,?,?)`, req.Role, store.TokenHash(req.Credential), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		http.Error(w, "role registration failed", 409)
		return
	}
	w.WriteHeader(201)
	writeJSON(w, map[string]string{"role": req.Role, "status": "registered"})
}
