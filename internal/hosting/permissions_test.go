package hosting

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestNewRecordsSkipPermissionsUnlessAsked(t *testing.T) {
	s := newService(t)
	h, err := s.Create(record())
	if err != nil {
		t.Fatal(err)
	}
	ask := false
	c := record()
	c.Directory = "/work/other"
	c.SkipPermissions = &ask
	h2, err := s.Create(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		id   string
		skip bool
	}{{h.ID, true}, {h2.ID, false}} {
		got, err := s.Get(want.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.SkipPermissions != want.skip {
			t.Errorf("%s skip_permissions %v, want %v", want.id, got.SkipPermissions, want.skip)
		}
	}
}

// An installation made before #220 keeps its records' behaviour: the
// migration sets them to ask, and host set turns skipping on.
func TestMigratedRecordAsksUntilSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE hosted_sessions(id TEXT PRIMARY KEY, node TEXT NOT NULL, base TEXT NOT NULL, label TEXT NOT NULL DEFAULT '', parent TEXT NOT NULL DEFAULT '', profile TEXT NOT NULL, directory TEXT NOT NULL, agent_session_id TEXT NOT NULL, launched INTEGER NOT NULL DEFAULT 0, args TEXT NOT NULL DEFAULT '[]', env TEXT NOT NULL DEFAULT '{}', restart TEXT NOT NULL DEFAULT 'always', desired TEXT NOT NULL DEFAULT 'running', cols INTEGER NOT NULL DEFAULT 120, rows INTEGER NOT NULL DEFAULT 40, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(node, base, directory));
INSERT INTO hosted_sessions(id,node,base,profile,directory,agent_session_id,args,created_at,updated_at) VALUES('hs_00000000000000000000000000000001','local','team/worker','pc-worker','/work/app','11111111-1111-4111-8111-111111111111','["--model","opus"]',1,1);`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, time.Now, func(string) bool { return true })
	id := "hs_00000000000000000000000000000001"
	h, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if h.SkipPermissions {
		t.Fatal("migrated record skips permissions")
	}
	var out bytes.Buffer
	if err := s.RunCLI("list", "", wire.HostedSessionCreate{}, false, &out); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(out.String(), "\n"); !strings.Contains(lines[0], "PERMS") || !strings.Contains(lines[1], " ask ") {
		t.Fatalf("host list:\n%s", out.String())
	}
	on := true
	out.Reset()
	if err := s.RunCLI("set", id, wire.HostedSessionCreate{SkipPermissions: &on}, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "permissions skip") {
		t.Fatalf("host set: %q", out.String())
	}
	if h, err = s.Get(id); err != nil || !h.SkipPermissions || h.Args[1] != "opus" {
		t.Fatalf("after set: %+v %v", h, err)
	}
	out.Reset()
	if err := s.RunCLI("list", "", wire.HostedSessionCreate{}, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"skip_permissions":true`) {
		t.Fatalf("host list --json: %s", out.String())
	}
	off := false
	if h, err = s.Update(id, wire.HostedSessionChange{SkipPermissions: &off}); err != nil || h.SkipPermissions {
		t.Fatalf("update off: %+v %v", h, err)
	}
}
