package dashboard

import (
	"database/sql"
	"time"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// CollaboratorGroup is one group in GET /api/admin/collaborators.
type CollaboratorGroup struct {
	identity.Group
	Roles   []CollaboratorRole `json:"roles"`
	Invites []wire.Invite      `json:"invites"`
}

type CollaboratorRole struct {
	Role      string     `json:"role"`
	CreatedAt string     `json:"created_at"`
	RevokedAt *string    `json:"revoked_at"`
	Online    bool       `json:"online"`
	LastSeen  *time.Time `json:"last_seen"` // last heartbeat of any of its sessions; null if never
}

// Collaborators lists every group with its roles (last seen from presence)
// and open invites, as of clock(). lease is the slot lease length: a slot's last
// heartbeat is its lease end minus lease.
func Collaborators(db *sql.DB, clock func() time.Time, ids *identity.Service, lease time.Duration) ([]CollaboratorGroup, error) {
	groups, err := ids.ListGroups()
	if err != nil {
		return nil, err
	}
	roles, err := ids.ListRoles()
	if err != nil {
		return nil, err
	}
	invites, err := ids.OpenInvites()
	if err != nil {
		return nil, err
	}
	seen := map[string]int64{}
	rows, err := db.Query(`SELECT base,MAX(lease_until) FROM slots GROUP BY base`)
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
	now := clock()
	out := make([]CollaboratorGroup, 0, len(groups))
	at := map[string]int{}
	for _, g := range groups {
		at[g.Prefix] = len(out)
		out = append(out, CollaboratorGroup{Group: g, Roles: []CollaboratorRole{}, Invites: []wire.Invite{}})
	}
	for _, v := range roles {
		i, ok := at[v.Group]
		if !ok {
			continue
		}
		c := CollaboratorRole{Role: v.Role, CreatedAt: v.CreatedAt, RevokedAt: v.RevokedAt}
		if until, ok := seen[v.Role]; ok {
			c.Online = until > now.UnixMilli() && v.RevokedAt == nil
			last := time.UnixMilli(until).UTC().Add(-lease)
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
