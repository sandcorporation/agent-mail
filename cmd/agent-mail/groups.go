package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Collaborator groups and per-group bosses (#71 §2, §4).
//
// A group is a role prefix: a role belongs to the group whose prefix + "/"
// starts its name, the longest prefix winning. A role in no group is internal
// and keeps full access. Every scope and boss decision goes through this file:
//
//   - groupOf(role): the role's group, if any.
//   - unrestricted(p): global bosses and internal roles see and send anywhere.
//   - maySee(p, other): whether p may see another role's sessions, mail
//     targets, questions and permission requests.
//   - bossesFor(asker): the roles that answer asker's questions and
//     permission requests: the group's boss_role for a group role, the global
//     bosses (AGENT_MAIL_BOSS_ROLES) otherwise.
//   - bossOf(p): the groups p is boss of; whoami reports it.
//
// With no groups every role is internal, bossesFor is the global list, and
// all of this reduces to the behaviour from before groups.

// migrateGroups adds the groups table (additive). revoked_at is set when the
// group is revoked; its roles are revoked at the same time.
func migrateGroups(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS groups(prefix TEXT PRIMARY KEY, boss_role TEXT NOT NULL, created_at TEXT NOT NULL, revoked_at TEXT)`)
	return err
}

// group is one row of the groups table.
type group struct {
	Prefix    string  `json:"prefix"`
	BossRole  string  `json:"boss_role"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at"`
}

// groupMatchSQL is the condition "the role in col is under g.prefix".
func groupMatchSQL(col string) string {
	return `substr(` + col + `,1,length(g.prefix)+1)=g.prefix||'/'`
}

// groupSQL is the SQL expression for the prefix of the group that the role in
// col belongs to (longest match), NULL for an internal role.
func groupSQL(col string) string {
	return `(SELECT g.prefix FROM groups g WHERE ` + groupMatchSQL(col) + ` ORDER BY length(g.prefix) DESC LIMIT 1)`
}

// groupCache is the groups table held in memory, so scope checks run without
// a query (also while a result set holds the only connection). loadGroups
// fills it at start and after every admin write; nil means no groups.
type groupCache struct {
	mu   sync.RWMutex
	list []group
}

// loadGroups reads the groups table into the cache.
func (s *Server) loadGroups() error {
	list, err := s.listGroups()
	if err != nil {
		return err
	}
	s.groups.mu.Lock()
	s.groups.list = list
	s.groups.mu.Unlock()
	return nil
}

// groupOf returns the group role belongs to: the longest prefix whose
// prefix + "/" starts the name. Revoked groups still scope their roles:
// revoking never widens access.
func (s *Server) groupOf(role string) (group, bool) {
	s.groups.mu.RLock()
	defer s.groups.mu.RUnlock()
	var best group
	found := false
	for _, g := range s.groups.list {
		if strings.HasPrefix(role, g.Prefix+"/") && len(g.Prefix) > len(best.Prefix) {
			best, found = g, true
		}
	}
	return best, found
}

// isGlobalBoss: p's role or base is in AGENT_MAIL_BOSS_ROLES. Metrics,
// progress and alerts are for global bosses only.
func (s *Server) isGlobalBoss(p principal) bool { return s.bossRoles[p.Base] || s.bossRoles[p.Role] }

// isBossName reports whether a mailbox name is a global boss role or a slot
// of one (metrics and progress).
func (s *Server) isBossName(base func(string) string) func(string) bool {
	return func(name string) bool {
		return s.bossRoles[name] || s.bossRoles[base(name)]
	}
}

// unrestricted: global bosses and internal roles keep full access.
func (s *Server) unrestricted(p principal) bool {
	if s.isGlobalBoss(p) {
		return true
	}
	_, in := s.groupOf(p.Base)
	return !in
}

// scopeGroup is the group prefix p's view is limited to, "" when unrestricted.
func (s *Server) scopeGroup(p principal) string {
	if s.unrestricted(p) {
		return ""
	}
	g, _ := s.groupOf(p.Base)
	return g.Prefix
}

// maySee reports whether p may see other (a role or base): its sessions,
// and mail, questions and permission requests addressed from it. A group
// role sees only roles of its own group.
func (s *Server) maySee(p principal, other string) bool {
	if s.unrestricted(p) {
		return true
	}
	mine, _ := s.groupOf(p.Base)
	theirs, in := s.groupOf(other)
	return in && theirs.Prefix == mine.Prefix
}

// maySend reports whether p may mail the role base to: anyone for an
// unrestricted role; for a group role, the global bosses, its group's boss
// role and roles of its own group.
func (s *Server) maySend(p principal, to string) bool {
	if s.bossRoles[to] || s.maySee(p, to) {
		return true
	}
	g, _ := s.groupOf(p.Base)
	return to == g.BossRole
}

// bossesFor lists the roles that answer asker's questions and permission
// requests, sorted: its group's boss role, or the global bosses for an
// internal role.
func (s *Server) bossesFor(asker string) []string {
	if g, ok := s.groupOf(asker); ok {
		return []string{g.BossRole}
	}
	return sortedKeys(s.bossRoles)
}

// isBossFor reports whether p answers for asker (never checked against the
// asker's own base here; callers add that).
func (s *Server) isBossFor(p principal, asker string) bool {
	for _, b := range s.bossesFor(asker) {
		if b == p.Base || b == p.Role {
			return true
		}
	}
	return false
}

// bossOf lists the prefixes of the live groups whose boss role is p.
func (s *Server) bossOf(p principal) []string {
	s.groups.mu.RLock()
	defer s.groups.mu.RUnlock()
	out := []string{}
	for _, g := range s.groups.list {
		if g.RevokedAt == nil && (g.BossRole == p.Base || g.BossRole == p.Role) {
			out = append(out, g.Prefix)
		}
	}
	return out
}

// answerableSQL is the SQL condition over the role column col for the rows p
// answers as a boss: askers in no group for a global boss, askers in the
// groups p is boss of. It is "" when p answers for nobody.
func (s *Server) answerableSQL(p principal, col string) (string, []any) {
	var conds []string
	var args []any
	if s.isGlobalBoss(p) {
		conds = append(conds, groupSQL(col)+` IS NULL`)
	}
	if of := s.bossOf(p); len(of) > 0 {
		conds = append(conds, groupSQL(col)+` IN (?`+strings.Repeat(",?", len(of)-1)+`)`)
		for _, prefix := range of {
			args = append(args, prefix)
		}
	}
	return strings.Join(conds, ` OR `), args
}

// whoami is GET /api/whoami: who the caller is and whether it is a boss.
// The switchboard plugin shows its boss panel when is_boss is true.
type whoami struct {
	Role       string   `json:"role"`
	Base       string   `json:"base"`
	Group      string   `json:"group"`       // "" for an internal role
	GlobalBoss bool     `json:"global_boss"` // in AGENT_MAIL_BOSS_ROLES
	BossOf     []string `json:"boss_of"`     // group prefixes whose boss_role is this role
	IsBoss     bool     `json:"is_boss"`     // global_boss or boss_of non-empty
}

func (s *Server) whoamiAPI(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	of := s.bossOf(p)
	out := whoami{Role: p.Role, Base: p.Base, GlobalBoss: s.isGlobalBoss(p), BossOf: of}
	if g, ok := s.groupOf(p.Base); ok {
		out.Group = g.Prefix
	}
	out.IsBoss = out.GlobalBoss || len(of) > 0
	writeJSON(w, out)
}

// listGroups returns every group, prefix order.
func (s *Server) listGroups() ([]group, error) {
	rows, err := s.db.Query(`SELECT prefix,boss_role,created_at,revoked_at FROM groups ORDER BY prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []group{}
	for rows.Next() {
		var g group
		var at sql.NullString
		if err = rows.Scan(&g.Prefix, &g.BossRole, &g.CreatedAt, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			g.RevokedAt = &at.String
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func validGroupPrefix(prefix string) bool {
	return validRole(prefix) && !strings.HasPrefix(prefix, "/") && !strings.HasSuffix(prefix, "/") && !strings.Contains(prefix, "//")
}

var errNoGroup = errors.New("no such group")

// adminGroupsAPI serves groups behind the admin proxy secret (writes also
// behind adminWriteAllowed, checked by adminAPI):
//
//	GET    /api/admin/groups
//	GET    /api/admin/groups/{prefix}
//	PUT    /api/admin/groups/{prefix}  {"boss_role": "..."}  (default <prefix>/boss)
//	DELETE /api/admin/groups/{prefix}  revokes the group and all its roles
//
// PUT on a revoked group restores the group, not its roles.
func (s *Server) adminGroupsAPI(w http.ResponseWriter, r *http.Request, prefix string) {
	if prefix == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		out, err := s.listGroups()
		if err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		writeJSON(w, out)
		return
	}
	if !validGroupPrefix(prefix) {
		http.Error(w, "invalid group prefix", 400)
		return
	}
	var (
		g   group
		err error
	)
	switch r.Method {
	case http.MethodGet:
		g, err = s.loadGroup(prefix)
	case http.MethodPut:
		var req struct {
			BossRole string `json:"boss_role"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if req.BossRole == "" {
			req.BossRole = prefix + "/boss"
		}
		if !validRole(req.BossRole) || reservedRole(req.BossRole) {
			http.Error(w, "invalid boss_role", 400)
			return
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		if _, err = s.db.ExecContext(r.Context(), `INSERT INTO groups(prefix,boss_role,created_at) VALUES(?,?,?) ON CONFLICT(prefix) DO UPDATE SET boss_role=excluded.boss_role, revoked_at=NULL`, prefix, req.BossRole, now); err == nil {
			if err = s.loadGroups(); err == nil {
				g, err = s.loadGroup(prefix)
			}
		}
	case http.MethodDelete:
		g, err = s.revokeGroup(r, prefix)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if errors.Is(err, errNoGroup) {
		http.Error(w, "no such group", 404)
		return
	}
	if err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	writeJSON(w, g)
}

func (s *Server) loadGroup(prefix string) (group, error) {
	var g group
	var at sql.NullString
	err := s.db.QueryRow(`SELECT prefix,boss_role,created_at,revoked_at FROM groups WHERE prefix=?`, prefix).Scan(&g.Prefix, &g.BossRole, &g.CreatedAt, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return g, errNoGroup
	}
	if at.Valid {
		g.RevokedAt = &at.String
	}
	return g, err
}

// revokeGroup marks the group revoked and revokes every role in it (the
// group's own roles, by longest match).
func (s *Server) revokeGroup(r *http.Request, prefix string) (group, error) {
	if _, err := s.loadGroup(prefix); err != nil {
		return group{}, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(r.Context(), `UPDATE groups SET revoked_at=? WHERE prefix=? AND revoked_at IS NULL`, now, prefix); err != nil {
		return group{}, err
	}
	if err := s.loadGroups(); err != nil {
		return group{}, err
	}
	roles, err := s.listRoles()
	if err != nil {
		return group{}, err
	}
	for _, v := range roles {
		if v.Group == prefix && v.RevokedAt == nil {
			if _, err = s.setRevoked(r.Context(), v.Role, true); err != nil {
				return group{}, err
			}
		}
	}
	return s.loadGroup(prefix)
}

// visibleSessions keeps the presence rows p may see.
func (s *Server) visibleSessions(p principal, in []presenceView) []presenceView {
	if s.unrestricted(p) {
		return in
	}
	return slices.DeleteFunc(in, func(v presenceView) bool { return !s.maySee(p, v.Base) })
}
