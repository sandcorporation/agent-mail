package hosting

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
)

// clockService is a service over an empty database whose clock the test moves.
func clockService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	return New(db, func() time.Time { return now }, func(base string) bool { return base == "team/worker" }), &now
}

func TestInviteCodeWorksOnceAndIsStoredHashed(t *testing.T) {
	s, now := clockService(t)
	code, expires, err := s.InviteNode("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "sbj_") || !expires.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("code %q expires %v", code, expires)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT code_hash FROM node_invites WHERE name='laptop'`).Scan(&stored); err != nil || strings.Contains(stored, code) {
		t.Fatalf("stored %q (%v)", stored, err)
	}
	j, err := s.JoinNode(code)
	if err != nil || j.Name != "laptop" || !strings.HasPrefix(j.Token, "sbn_") {
		t.Fatalf("join = %+v %v", j, err)
	}
	if name, err := s.NodeByToken(j.Token); err != nil || name != "laptop" {
		t.Fatalf("token names %q %v", name, err)
	}
	if err := s.db.QueryRow(`SELECT token_hash FROM nodes WHERE name='laptop'`).Scan(&stored); err != nil || strings.Contains(stored, j.Token) {
		t.Fatalf("token stored as %q (%v)", stored, err)
	}
	if _, err := s.JoinNode(code); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("second join: %v", err)
	}
	if _, _, err := s.InviteNode("laptop"); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("invite of a registered name: %v", err)
	}
	for _, bad := range []string{"", "sbj_nope"} {
		if _, err := s.JoinNode(bad); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("join %q: %v", bad, err)
		}
	}
	if _, _, err := s.InviteNode("../x"); err == nil {
		t.Error("an invalid name was invited")
	}
}

func TestInviteCodeExpiresAfterFifteenMinutes(t *testing.T) {
	s, now := clockService(t)
	code, _, err := s.InviteNode("laptop")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(15 * time.Minute)
	if _, err := s.JoinNode(code); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("join at 15 min: %v", err)
	}
	if s.nodeKnown("laptop") {
		t.Fatal("an expired code registered the node")
	}
	again, _, err := s.InviteNode("laptop")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(15*time.Minute - time.Millisecond)
	if _, err := s.JoinNode(again); err != nil {
		t.Fatalf("join just before expiry: %v", err)
	}
}

func TestANewInviteReplacesTheUnusedOne(t *testing.T) {
	s, _ := clockService(t)
	first, _, _ := s.InviteNode("laptop")
	second, _, err := s.InviteNode("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinNode(first); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("replaced code: %v", err)
	}
	if _, err := s.JoinNode(second); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeRefusesTheTokenAndNewRecords(t *testing.T) {
	s, _ := clockService(t)
	code, _, _ := s.InviteNode("laptop")
	j, err := s.JoinNode(code)
	if err != nil {
		t.Fatal(err)
	}
	c := record()
	c.Node = "laptop"
	if _, err := s.Create(c); err != nil {
		t.Fatal(err)
	}
	n, err := s.RevokeNode("laptop")
	if err != nil || n.RevokedAt == nil {
		t.Fatalf("revoke = %+v %v", n, err)
	}
	if again, err := s.RevokeNode("laptop"); err != nil || *again.RevokedAt != *n.RevokedAt {
		t.Fatalf("second revoke = %+v %v", again, err)
	}
	if _, err := s.NodeByToken(j.Token); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("revoked token: %v", err)
	}
	c.Directory = "/work/other"
	if _, err := s.Create(c); err == nil || !strings.Contains(err.Error(), "node: laptop is revoked") {
		t.Fatalf("record on a revoked node: %v", err)
	}
	if _, _, err := s.InviteNode("laptop"); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("invite of a revoked name: %v", err)
	}
	if _, err := s.RevokeNode("nobody"); !errors.Is(err, ErrNoSuchNode) {
		t.Fatalf("revoke of an unknown node: %v", err)
	}
	c.Node = "nobody"
	if _, err := s.Create(c); err == nil || !strings.Contains(err.Error(), "node: no node named nobody") {
		t.Fatalf("record on an unknown node: %v", err)
	}
}
