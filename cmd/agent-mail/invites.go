package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/store"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Invites and join (#71 §3).
//
// A global boss (role API) or a signed-in admin (dashboard) creates a one-time
// code for a group prefix, optionally pinned to one role. The collaborator
// runs `switchboard join`, which makes the role token on their own machine and
// sends it once to POST /api/join with the code; the server keeps only the
// hash of the code and of the token. Group scope and boss decisions stay in
// groups.go: this file only creates groups and roles.

const (
	defaultInviteTTL = 72 * time.Hour
	maxInviteTTL     = 30 * 24 * time.Hour
	minJoinSecret    = 24
	inviteGoneText   = "invite code is used, cancelled or expired"
)

// apiError carries the HTTP status for a refused request.
type apiError struct {
	status int
	msg    string
}

func (e apiError) Error() string { return e.msg }

func writeAPIError(w http.ResponseWriter, err error, fallback string) {
	var ae apiError
	if errors.As(err, &ae) {
		http.Error(w, ae.msg, ae.status)
		return
	}
	http.Error(w, fallback, 500)
}

var inviteEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newInviteCode is 128 random bits in base32, dashed every four characters.
func newInviteCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	raw := inviteEncoding.EncodeToString(b)
	var out strings.Builder
	for i, c := range raw {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(c)
	}
	return out.String(), nil
}

// inviteCodeHash hashes a code as typed: case, dashes and spaces do not matter.
func inviteCodeHash(code string) string {
	norm := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	return store.TokenHash(norm)
}

// parseInviteTTL reads a Go duration or a whole number of days ("7d").
// Empty is the default; it must be positive and at most maxInviteTTL.
func parseInviteTTL(v string) (time.Duration, error) {
	if v == "" {
		return defaultInviteTTL, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, errors.New("invalid ttl")
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil {
			return 0, errors.New("invalid ttl")
		}
	}
	if d <= 0 || d > maxInviteTTL {
		return 0, errors.New("ttl must be positive and at most 30d")
	}
	return d, nil
}

// createInvite stores a new invite for req.Prefix, creating the group when
// it does not exist. Creating a group is refused (409) when roles already sit
// under the prefix: they would become group roles and lose access.
func (s *Server) createInvite(ctx context.Context, req inviteRequest, createdBy string) (inviteCreated, error) {
	var out inviteCreated
	if !validGroupPrefix(req.Prefix) {
		return out, apiError{400, "invalid prefix"}
	}
	if req.Role != "" && (!wire.ValidRole(req.Role) || !strings.HasPrefix(req.Role, req.Prefix+"/") || reservedRole(req.Role) || s.bossRoles[req.Role]) {
		return out, apiError{400, "role must be a role name under " + req.Prefix + "/"}
	}
	ttl, err := parseInviteTTL(req.TTL)
	if err != nil {
		return out, apiError{400, err.Error()}
	}
	code, err := newInviteCode()
	if err != nil {
		return out, err
	}
	now := s.now().UTC()
	out.Code = code
	out.Prefix = req.Prefix
	out.ExpiresAt = now.Add(ttl).Format(time.RFC3339Nano)
	out.CreatedBy = createdBy
	out.CreatedAt = now.Format(time.RFC3339Nano)
	if req.Role != "" {
		out.Role = &req.Role
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var revoked sql.NullString
		err := tx.QueryRow(`SELECT revoked_at FROM groups WHERE prefix=?`, req.Prefix).Scan(&revoked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var taken []string
			rows, err := tx.Query(`SELECT role FROM roles WHERE substr(role,1,?)=? ORDER BY role LIMIT 5`, len(req.Prefix)+1, req.Prefix+"/")
			if err != nil {
				return err
			}
			for rows.Next() {
				var r string
				if err = rows.Scan(&r); err != nil {
					rows.Close()
					return err
				}
				taken = append(taken, r)
			}
			rows.Close()
			if err = rows.Err(); err != nil {
				return err
			}
			if len(taken) > 0 {
				return apiError{409, "roles already exist under " + req.Prefix + "/ (" + strings.Join(taken, ", ") + "); a group there would restrict them"}
			}
			if _, err = tx.Exec(`INSERT INTO groups(prefix,boss_role,created_at) VALUES(?,?,?)`, req.Prefix, req.Prefix+"/boss", out.CreatedAt); err != nil {
				return err
			}
			out.GroupCreated = true
		case err != nil:
			return err
		case revoked.Valid:
			return apiError{409, "group " + req.Prefix + " is revoked"}
		}
		if req.Role != "" && roleExists(tx, req.Role) {
			return apiError{409, "role already registered"}
		}
		res, err := tx.Exec(`INSERT INTO invites(code_hash,prefix,role,expires_at,created_by,created_at) VALUES(?,?,?,?,?,?)`,
			inviteCodeHash(code), req.Prefix, nullable(req.Role), out.ExpiresAt, createdBy, out.CreatedAt)
		if err != nil {
			return err
		}
		out.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return inviteCreated{}, err
	}
	if out.GroupCreated {
		if err = s.loadGroups(); err != nil {
			return inviteCreated{}, err
		}
	}
	if req.Role != "" {
		if g, _ := s.groupOf(req.Role); g.Prefix != req.Prefix {
			// A deeper group owns the role; the invite could never be used.
			_, _ = s.db.Exec(`UPDATE invites SET cancelled_at=? WHERE id=?`, out.CreatedAt, out.ID)
			return inviteCreated{}, apiError{409, "role belongs to group " + g.Prefix}
		}
	}
	return out, nil
}

func decodeInviteRequest(w http.ResponseWriter, r *http.Request) (inviteRequest, bool) {
	var req inviteRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", 400)
		return req, false
	}
	return req, true
}

// invitesAPI is POST /api/invites {prefix, role?, ttl?}, for global bosses.
func (s *Server) invitesAPI(w http.ResponseWriter, r *http.Request, p principal) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.isGlobalBoss(p) {
		http.Error(w, "only a global boss may invite", 403)
		return
	}
	req, ok := decodeInviteRequest(w, r)
	if !ok {
		return
	}
	out, err := s.createInvite(r.Context(), req, p.Base)
	if err != nil {
		writeAPIError(w, err, "invite failed")
		return
	}
	w.WriteHeader(201)
	writeJSON(w, out)
}

// adminInvitesAPI serves invites to a signed-in admin (writes behind the
// admin write guard, checked by adminAPI):
//
//	GET    /api/admin/invites        open invites (unused, not cancelled, not expired)
//	POST   /api/admin/invites        {prefix, role?, ttl?}
//	DELETE /api/admin/invites/{id}   cancels an open invite
func (s *Server) adminInvitesAPI(w http.ResponseWriter, r *http.Request, who adminIdentity, rest string) {
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			out, err := s.openInvites()
			if err != nil {
				http.Error(w, "query failed", 500)
				return
			}
			writeJSON(w, out)
		case http.MethodPost:
			req, ok := decodeInviteRequest(w, r)
			if !ok {
				return
			}
			out, err := s.createInvite(r.Context(), req, "admin:"+who.User)
			if err != nil {
				writeAPIError(w, err, "invite failed")
				return
			}
			w.WriteHeader(201)
			writeJSON(w, out)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id, err := wire.ParseID(rest)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE invites SET cancelled_at=? WHERE id=? AND used_at IS NULL AND cancelled_at IS NULL`, s.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		http.Error(w, "update failed", 500)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "no open invite", 404)
		return
	}
	writeJSON(w, map[string]any{"id": id, "status": "cancelled"})
}

// openInvites lists invites that can still be used, oldest first.
func (s *Server) openInvites() ([]invite, error) {
	rows, err := s.db.Query(`SELECT id,prefix,role,expires_at,used_at,used_by,created_by,created_at,cancelled_at FROM invites WHERE used_at IS NULL AND cancelled_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now()
	out := []invite{}
	for rows.Next() {
		var v invite
		var role, usedAt, usedBy, cancelled sql.NullString
		if err = rows.Scan(&v.ID, &v.Prefix, &role, &v.ExpiresAt, &usedAt, &usedBy, &v.CreatedBy, &v.CreatedAt, &cancelled); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, v.ExpiresAt); err != nil || !now.Before(t) {
			continue
		}
		if role.Valid {
			v.Role = &role.String
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// join is POST /api/join {code, role?, credential}: no auth beyond the code.
// The role is the invite's role when set (role, if given, must equal it),
// else role, which must be under the invite's prefix. A code works once; an
// unknown, used, cancelled or expired code all get 410; those count against
// the caller's IP like failed logins.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := s.now()
	ipKey := "join:" + s.clientIP(r)
	if s.joinLimits.blocked(now, ipKey) {
		http.Error(w, "too many failed joins; try later", http.StatusTooManyRequests)
		return
	}
	var req joinRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Code == "" {
		http.Error(w, "code and credential required", 400)
		return
	}
	if len(req.Credential) < minJoinSecret {
		http.Error(w, "credential must be at least 24 characters", 400)
		return
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	var out struct {
		Role     string `json:"role"`
		Group    string `json:"group"`
		BossRole string `json:"boss_role"`
		Status   string `json:"status"`
	}
	err := s.inTx(r.Context(), func(tx *sql.Tx) error {
		var id int64
		var prefix, expires string
		var role, used, cancelled sql.NullString
		err := tx.QueryRow(`SELECT id,prefix,role,expires_at,used_at,cancelled_at FROM invites WHERE code_hash=?`, inviteCodeHash(req.Code)).Scan(&id, &prefix, &role, &expires, &used, &cancelled)
		if errors.Is(err, sql.ErrNoRows) {
			return apiError{410, inviteGoneText}
		}
		if err != nil {
			return err
		}
		if t, perr := time.Parse(time.RFC3339Nano, expires); used.Valid || cancelled.Valid || perr != nil || !now.Before(t) {
			return apiError{410, inviteGoneText}
		}
		var boss string
		var revoked sql.NullString
		if err = tx.QueryRow(`SELECT boss_role,revoked_at FROM groups WHERE prefix=?`, prefix).Scan(&boss, &revoked); err != nil || revoked.Valid {
			return apiError{410, inviteGoneText}
		}
		want := req.Role
		if role.Valid {
			if want != "" && want != role.String {
				return apiError{403, "this invite is for role " + role.String}
			}
			want = role.String
		}
		if want == "" {
			return apiError{400, "role required (a name under " + prefix + "/)"}
		}
		if !wire.ValidRole(want) || !strings.HasPrefix(want, prefix+"/") || reservedRole(want) || s.bossRoles[want] {
			return apiError{403, "role must be a role name under " + prefix + "/"}
		}
		if g, _ := s.groupOf(want); g.Prefix != prefix {
			return apiError{403, "role must be a role name under " + prefix + "/"}
		}
		if roleExists(tx, want) {
			return apiError{409, "role already registered"}
		}
		if _, err = tx.Exec(`INSERT INTO roles(role,token,created_at) VALUES(?,?,?)`, want, store.TokenHash(req.Credential), stamp); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE invites SET used_at=?,used_by=? WHERE id=? AND used_at IS NULL`, stamp, want, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return apiError{410, inviteGoneText}
		}
		out.Role, out.Group, out.BossRole, out.Status = want, prefix, boss, "joined"
		return nil
	})
	if err != nil {
		var ae apiError
		if errors.As(err, &ae) && ae.status == 410 {
			s.joinLimits.fail(now, ipKey)
		}
		writeAPIError(w, err, "join failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	writeJSON(w, out)
}

// collaboratorGroup is one group in GET /api/admin/collaborators.
type collaboratorGroup struct {
	group
	Roles   []collaboratorRole `json:"roles"`
	Invites []invite           `json:"invites"`
}

type collaboratorRole struct {
	Role      string     `json:"role"`
	CreatedAt string     `json:"created_at"`
	RevokedAt *string    `json:"revoked_at"`
	Online    bool       `json:"online"`
	LastSeen  *time.Time `json:"last_seen"` // last heartbeat of any of its sessions; null if never
}

// collaborators lists every group with its roles (last seen from presence)
// and open invites.
func (s *Server) collaborators() ([]collaboratorGroup, error) {
	groups, err := s.listGroups()
	if err != nil {
		return nil, err
	}
	roles, err := s.listRoles()
	if err != nil {
		return nil, err
	}
	invites, err := s.openInvites()
	if err != nil {
		return nil, err
	}
	seen := map[string]int64{}
	rows, err := s.db.Query(`SELECT base,MAX(lease_until) FROM slots GROUP BY base`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var base string
		var until int64
		if err = rows.Scan(&base, &until); err != nil {
			rows.Close()
			return nil, err
		}
		seen[base] = until
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]collaboratorGroup, 0, len(groups))
	at := map[string]int{}
	for _, g := range groups {
		at[g.Prefix] = len(out)
		out = append(out, collaboratorGroup{group: g, Roles: []collaboratorRole{}, Invites: []invite{}})
	}
	for _, v := range roles {
		i, ok := at[v.Group]
		if !ok {
			continue
		}
		c := collaboratorRole{Role: v.Role, CreatedAt: v.CreatedAt, RevokedAt: v.RevokedAt}
		if until, ok := seen[v.Role]; ok {
			c.Online = until > ms(now) && v.RevokedAt == nil
			last := fromMS(until).Add(-s.leaseLength())
			if last.After(now) {
				last = now.UTC()
			}
			c.LastSeen = &last
		}
		out[i].Roles = append(out[i].Roles, c)
	}
	for _, v := range invites {
		if i, ok := at[v.Prefix]; ok {
			out[i].Invites = append(out[i].Invites, v)
		}
	}
	return out, nil
}
