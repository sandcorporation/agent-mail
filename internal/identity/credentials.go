package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
)

// Role credentials (#343) are extra tokens of a role, in role_credentials.
// The host issues one for each hosted record on a node that takes them and
// sends it over the node channel only; the node writes it as a profile for
// the record's session. Like roles.token, only the hash is stored. A
// credential authorises exactly what the role's own token does (the role and
// its -N slots) and stops working when it is revoked or its role is.

// ErrRoleUnusable means a credential was asked for a role that is not
// registered or is revoked.
var ErrRoleUnusable = errors.New("role is not registered or is revoked")

// Credential is one live role credential, without its token.
type Credential struct {
	ID     string
	Role   string
	Record string
	Node   string
}

// Credentials issues, lists and revokes role credentials.
type Credentials struct {
	db    *sql.DB
	clock func() time.Time
}

// NewCredentials returns the credential store over db.
func NewCredentials(db *sql.DB, clock func() time.Time) *Credentials {
	if clock == nil {
		clock = time.Now
	}
	return &Credentials{db: db, clock: clock}
}

func randomToken(prefix string, n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// Issue makes a new credential of role for record on node and revokes the
// record's earlier ones, in one transaction. The token is returned once.
func (c *Credentials) Issue(role, record, node string) (id, token string, err error) {
	if id, err = randomToken("rc_", 16); err != nil {
		return "", "", err
	}
	if token, err = randomToken("sbr_", 32); err != nil {
		return "", "", err
	}
	now := c.clock().UnixMilli()
	err = store.InTx(context.Background(), c.db, func(tx *sql.Tx) error {
		if _, revoked, ok := roleRecord(tx, role); !ok || revoked {
			return ErrRoleUnusable
		}
		if _, err := tx.Exec(`UPDATE role_credentials SET revoked_at=? WHERE record=? AND revoked_at IS NULL`, now, record); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO role_credentials(id,role,token_hash,record,node,created_at) VALUES(?,?,?,?,?,?)`, id, role, store.TokenHash(token), record, node, now)
		return err
	})
	if err != nil {
		return "", "", err
	}
	return id, token, nil
}

// Live lists the credentials not revoked, by id.
func (c *Credentials) Live() ([]Credential, error) {
	rows, err := c.db.Query(`SELECT id,role,record,node FROM role_credentials WHERE revoked_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var v Credential
		if err := rows.Scan(&v.ID, &v.Role, &v.Record, &v.Node); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Revoke revokes the credentials ids; revoked ones keep their first time.
func (c *Credentials) Revoke(ids ...string) error {
	now := c.clock().UnixMilli()
	for _, id := range ids {
		if _, err := c.db.Exec(`UPDATE role_credentials SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, now, id); err != nil {
			return err
		}
	}
	return nil
}

// credentialMatches reports whether t is a live credential of role.
func credentialMatches(q Queryer, role, t string) bool {
	if t == "" {
		return false
	}
	var one int
	return q.QueryRow(`SELECT 1 FROM role_credentials WHERE token_hash=? AND role=? AND revoked_at IS NULL`, store.TokenHash(t), role).Scan(&one) == nil
}

// revokeRoleCredentials revokes every credential of role (the role itself
// was revoked), so un-revoking the role does not bring them back.
func revokeRoleCredentials(tx *sql.Tx, role string, at time.Time) error {
	_, err := tx.Exec(`UPDATE role_credentials SET revoked_at=? WHERE role=? AND revoked_at IS NULL`, at.UnixMilli(), role)
	return err
}
