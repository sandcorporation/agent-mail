package hosting

import (
	"log"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Role credentials for sessions on a node (#343). A node that asks for them
// (config.NodeCredentialsHeaderName on its socket) gets, for each of its
// records, a credential of the record's base role over the node channel,
// before the assign that names the record; it writes the credential as the
// profile wire.HostedProfileName(id) and the session runs with that profile.
// The host keeps only hashes (identity's role_credentials). A record's
// credential is revoked when the record is deleted, its node revoked, or
// the role revoked; the hub also revokes any live credential whose record
// is gone or moved at its next sync, so a record removed with `host remove`
// loses its credential too.

// LiveCredential is a credential that is not revoked, without its token.
type LiveCredential struct {
	ID     string
	Record string
	Node   string
}

// RoleCredentials issues, lists and revokes role credentials; the server
// passes in identity's. Issue revokes the record's earlier credentials and
// refuses a role that is unregistered or revoked.
type RoleCredentials struct {
	Issue  func(role, record, node string) (id, token string, err error)
	Live   func() ([]LiveCredential, error)
	Revoke func(ids ...string) error
}

// WithCredentials sets the role credential store; without it no node gets
// credentials and nothing is revoked.
func (s *Service) WithCredentials(c *RoleCredentials) *Service {
	s.creds = c
	return s
}

// revokeCredentials revokes the live credentials match picks.
func (s *Service) revokeCredentials(match func(LiveCredential) bool) error {
	if s.creds == nil {
		return nil
	}
	live, err := s.creds.Live()
	if err != nil {
		return err
	}
	var ids []string
	for _, c := range live {
		if match(c) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return s.creds.Revoke(ids...)
}

// liveCredentials is the record → credential id map of the live
// credentials, after revoking those whose record is gone, has moved to
// another node, or sits on a revoked node.
func (s *Service) liveCredentials(recs []wire.HostedSession, revoked map[string]bool) (map[string]string, error) {
	if s.creds == nil {
		return nil, nil
	}
	live, err := s.creds.Live()
	if err != nil {
		return nil, err
	}
	node := map[string]string{}
	for _, r := range recs {
		node[r.ID] = r.Node
	}
	out := map[string]string{}
	var stale []string
	for _, c := range live {
		if n, ok := node[c.Record]; !ok || n != c.Node || revoked[c.Node] {
			stale = append(stale, c.ID)
			continue
		}
		out[c.Record] = c.ID
	}
	if len(stale) > 0 {
		if err := s.creds.Revoke(stale...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseHeld reads the node's credentials header: ok is false when it is
// absent (the node uses its records' own profiles).
func parseHeld(values []string) (held map[string]bool, ok bool) {
	if len(values) == 0 {
		return nil, false
	}
	held = map[string]bool{}
	for _, v := range values {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" && id != "-" {
				held[id] = true
			}
		}
	}
	return held, true
}

// deliverLocked sends c a credential for each of recs it does not hold the
// live one of, issuing a new one (which revokes the record's earlier ones).
// live is updated with what it issued. A role that cannot have one (revoked)
// is skipped: the session's CLI is refused as the role itself would be.
func (h *Hub) deliverLocked(c *nodeConn, recs []wire.HostedSession, live map[string]string) {
	if !c.takes || h.svc.creds == nil || live == nil {
		return
	}
	for _, r := range recs {
		if id, ok := live[r.ID]; ok && c.held[id] {
			continue
		}
		id, token, err := h.svc.creds.Issue(r.Base, r.ID, c.name)
		if err != nil {
			continue
		}
		live[r.ID] = id
		if !h.sendLocked(c, wire.NodeFrame{Type: wire.NodeCredentialType, ID: r.ID, Credential: &wire.NodeCredential{ID: id, Role: r.Base, Token: token}}) {
			log.Printf("hosting: node %s: credential for %s not sent (node too slow)", c.name, r.ID)
			return
		}
		c.held[id] = true
	}
}
