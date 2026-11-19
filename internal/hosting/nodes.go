package hosting

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Node credentials (#173). A node token authorises only /api/node/*; the
// host stores its hash in nodes.token_hash and never the token.

var (
	// ErrNodeRevoked means the node's credential was revoked.
	ErrNodeRevoked = errors.New("node is revoked")
	// ErrBadNodeToken means a bearer token matched no live node.
	ErrBadNodeToken = errors.New("unknown node token")
)

// NodeInviteTTL is how long a code from `switchboard node invite` stays
// valid; each code is used once.
const NodeInviteTTL = 15 * time.Minute

var (
	// ErrInviteExpired means the invite code was valid but is older than
	// NodeInviteTTL.
	ErrInviteExpired = errors.New("invite code expired; run switchboard node invite again")
	// ErrInviteInvalid means no unused invite has this code.
	ErrInviteInvalid = errors.New("invite code is unknown or already used")
	// ErrNodeExists means a node of that name is already registered.
	ErrNodeExists = errors.New("a node of that name already exists")
	// ErrNoSuchNode means no node of that name is registered.
	ErrNoSuchNode = errors.New("no such node")
)

func checkNodeName(name string) error {
	if !nodeNamePattern.MatchString(name) {
		return invalidf("node: invalid name %q (1-64 of A-Z a-z 0-9 . _ -, starting with a letter or digit)", name)
	}
	return nil
}

func newNodeToken() (string, error) {
	suffix, err := randomHex(32)
	return "sbn_" + suffix, err
}

// RegisterLocalNode is what `switchboard up` does on start for its loopback
// node, whose token it keeps in its data directory: it makes the nodes row
// name carry the token's hash, creating the row when missing. A revoked node
// is refused.
func (s *Service) RegisterLocalNode(name, token string) error {
	if err := checkNodeName(name); err != nil {
		return err
	}
	var revoked sql.NullInt64
	switch err := s.db.QueryRow(`SELECT revoked_at FROM nodes WHERE name=?`, name).Scan(&revoked); {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO nodes(name,token_hash,created_at) VALUES(?,?,?)`, name, store.TokenHash(token), s.clock().UnixMilli())
		return err
	case err != nil:
		return err
	case revoked.Valid:
		return fmt.Errorf("node %s: %w", name, ErrNodeRevoked)
	default:
		_, err = s.db.Exec(`UPDATE nodes SET token_hash=? WHERE name=?`, store.TokenHash(token), name)
		return err
	}
}

// InviteNode makes a one-time code for a new node name, valid for
// NodeInviteTTL; only its hash is stored. A new code replaces an unused one
// for the same name. A registered name, live or revoked, is refused.
func (s *Service) InviteNode(name string) (code string, expiresAt time.Time, err error) {
	if err = checkNodeName(name); err != nil {
		return "", time.Time{}, err
	}
	if s.nodeKnown(name) {
		return "", time.Time{}, fmt.Errorf("node %s: %w", name, ErrNodeExists)
	}
	suffix, err := randomHex(16)
	if err != nil {
		return "", time.Time{}, err
	}
	code = "sbj_" + suffix
	now := s.clock()
	expiresAt = now.Add(NodeInviteTTL)
	tx, err := s.db.Begin()
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM node_invites WHERE (name=? AND used_at IS NULL) OR expires_at<=?`, name, now.UnixMilli()); err != nil {
		return "", time.Time{}, err
	}
	if _, err = tx.Exec(`INSERT INTO node_invites(code_hash,name,created_at,expires_at) VALUES(?,?,?,?)`, store.TokenHash(code), name, now.UnixMilli(), expiresAt.UnixMilli()); err != nil {
		return "", time.Time{}, err
	}
	return code, expiresAt, tx.Commit()
}

// JoinNode spends an invite code: it registers the invited node with a new
// token and returns the name and the token, which the host keeps only as a
// hash. A code works once and only before it expires.
func (s *Service) JoinNode(code string) (wire.NodeJoined, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return wire.NodeJoined{}, ErrInviteInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return wire.NodeJoined{}, err
	}
	defer tx.Rollback()
	var name string
	var expires int64
	hash := store.TokenHash(code)
	switch err = tx.QueryRow(`SELECT name,expires_at FROM node_invites WHERE code_hash=? AND used_at IS NULL`, hash).Scan(&name, &expires); {
	case errors.Is(err, sql.ErrNoRows):
		return wire.NodeJoined{}, ErrInviteInvalid
	case err != nil:
		return wire.NodeJoined{}, err
	}
	now := s.clock().UnixMilli()
	if now >= expires {
		return wire.NodeJoined{}, ErrInviteExpired
	}
	res, err := tx.Exec(`UPDATE node_invites SET used_at=? WHERE code_hash=? AND used_at IS NULL`, now, hash)
	if err != nil {
		return wire.NodeJoined{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return wire.NodeJoined{}, ErrInviteInvalid
	}
	token, err := newNodeToken()
	if err != nil {
		return wire.NodeJoined{}, err
	}
	res, err = tx.Exec(`INSERT INTO nodes(name,token_hash,created_at) VALUES(?,?,?) ON CONFLICT(name) DO NOTHING`, name, store.TokenHash(token), now)
	if err != nil {
		return wire.NodeJoined{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return wire.NodeJoined{}, fmt.Errorf("node %s: %w", name, ErrNodeExists)
	}
	if err = tx.Commit(); err != nil {
		return wire.NodeJoined{}, err
	}
	return wire.NodeJoined{Name: name, Token: token}, nil
}

// RevokeNode revokes a node's credential: from now on its token is refused.
// Revoking a revoked node changes nothing. The hub closes its socket.
func (s *Service) RevokeNode(name string) (wire.NodeView, error) {
	if _, err := s.db.Exec(`UPDATE nodes SET revoked_at=? WHERE name=? AND revoked_at IS NULL`, s.clock().UnixMilli(), name); err != nil {
		return wire.NodeView{}, err
	}
	nodes, err := s.Nodes()
	if err != nil {
		return wire.NodeView{}, err
	}
	for _, n := range nodes {
		if n.Name == name {
			return n, nil
		}
	}
	return wire.NodeView{}, ErrNoSuchNode
}

// revokedNodes is the set of revoked node names.
func (s *Service) revokedNodes() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM nodes WHERE revoked_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// NodeByToken returns the name of the live node whose token this is.
func (s *Service) NodeByToken(token string) (string, error) {
	if token == "" {
		return "", ErrBadNodeToken
	}
	var name string
	var revoked sql.NullInt64
	err := s.db.QueryRow(`SELECT name,revoked_at FROM nodes WHERE token_hash=?`, store.TokenHash(token)).Scan(&name, &revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrBadNodeToken
	case err != nil:
		return "", err
	case revoked.Valid:
		return "", ErrNodeRevoked
	}
	return name, nil
}

// nodeSeen records a node's hello (machine, OS, version) and when it was
// last seen.
func (s *Service) nodeSeen(name string, h wire.NodeHello) error {
	_, err := s.db.Exec(`UPDATE nodes SET machine=?,os=?,version=?,max_sessions=?,last_seen_at=? WHERE name=?`, h.Machine, h.OS, h.Version, max(h.Max, 0), s.clock().UnixMilli(), name)
	return err
}

// checkNodeUsable refuses a record for a node that is not registered or is
// revoked.
func (s *Service) checkNodeUsable(name string) error {
	var revoked sql.NullInt64
	switch err := s.db.QueryRow(`SELECT revoked_at FROM nodes WHERE name=?`, name).Scan(&revoked); {
	case errors.Is(err, sql.ErrNoRows):
		return invalidf("node: no node named %s (switchboard up registers its own; switchboard node invite adds one)", name)
	case err != nil:
		return err
	case revoked.Valid:
		return invalidf("node: %s is revoked", name)
	}
	return nil
}

// nodeLeft records when a node's socket closed.
func (s *Service) nodeLeft(name string) error {
	_, err := s.db.Exec(`UPDATE nodes SET last_seen_at=? WHERE name=?`, s.clock().UnixMilli(), name)
	return err
}

// nodeKnown reports whether name is in the node registry.
func (s *Service) nodeKnown(name string) bool {
	var n int
	return s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name=?`, name).Scan(&n) == nil && n > 0
}

// markLaunched records that a session's conversation sid exists, so its
// next launch resumes it; a report about a conversation the record no longer
// holds (empty sid: any) changes nothing.
func (s *Service) markLaunched(id, sid string) error {
	_, err := s.db.Exec(`UPDATE hosted_sessions SET launched=1 WHERE id=? AND launched=0 AND (?='' OR agent_session_id=?)`, id, sid, sid)
	return err
}
