package hosting

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// tokenFileMode keeps a node token readable by its owner only.
const tokenFileMode = 0o600

// EnsureNodeToken is what `switchboard up` does on start for its loopback
// node: it reads the token in path, creating it (mode 0600) when missing,
// and makes the nodes row name carry its hash. A revoked node is refused.
func (s *Service) EnsureNodeToken(name, path string) (string, error) {
	if !nodeNamePattern.MatchString(name) {
		return "", invalidf("node: invalid name %q (1-64 of A-Z a-z 0-9 . _ -, starting with a letter or digit)", name)
	}
	token, err := ReadTokenFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var suffix string
		if suffix, err = randomHex(32); err != nil {
			return "", err
		}
		token = "sbn_" + suffix
		if err = writeTokenFile(path, token); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	var revoked sql.NullInt64
	switch err = s.db.QueryRow(`SELECT revoked_at FROM nodes WHERE name=?`, name).Scan(&revoked); {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO nodes(name,token_hash,created_at) VALUES(?,?,?)`, name, store.TokenHash(token), s.clock().UnixMilli())
	case err != nil:
	case revoked.Valid:
		return "", fmt.Errorf("node %s: %w", name, ErrNodeRevoked)
	default:
		_, err = s.db.Exec(`UPDATE nodes SET token_hash=? WHERE name=?`, store.TokenHash(token), name)
	}
	return token, err
}

// ReadTokenFile reads a node token file (one line).
func ReadTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("node token file %s is empty", path)
	}
	return token, nil
}

func writeTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tokenFileMode)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(token + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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

// nodeSeen records a node's hello and the time it was last attached.
func (s *Service) nodeSeen(name string, h wire.NodeHello) error {
	_, err := s.db.Exec(`UPDATE nodes SET machine=?,os=?,version=?,last_seen_at=? WHERE name=?`, h.Machine, h.OS, h.Version, s.clock().UnixMilli(), name)
	return err
}

// nodeKnown reports whether name is in the node registry.
func (s *Service) nodeKnown(name string) bool {
	var n int
	return s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name=?`, name).Scan(&n) == nil && n > 0
}

// markLaunched records that a session's conversation exists, so its next
// launch resumes it.
func (s *Service) markLaunched(id string) error {
	_, err := s.db.Exec(`UPDATE hosted_sessions SET launched=1 WHERE id=? AND launched=0`, id)
	return err
}
