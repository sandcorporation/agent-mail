package hosting

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// newService is a service whose registry holds the nodes pc and laptop.
func newService(t *testing.T) *Service {
	t.Helper()
	s := bareService(t)
	for _, n := range []string{"pc", "laptop"} {
		if err := s.RegisterLocalNode(n, "sbn_"+n); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// bareService is a service over an empty database.
func bareService(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	return New(db, func() time.Time { return now }, func(base string) bool { return base == "team/worker" || base == "team/boss" })
}

func record() wire.HostedSessionCreate {
	return wire.HostedSessionCreate{Node: "pc", Base: "team/worker", Profile: "team-worker", Directory: "/work/app"}
}

func TestCreateFillsDefaultsAndIDs(t *testing.T) {
	s := newService(t)
	c := record()
	c.Args = []string{"--model", "opus"}
	c.Env = map[string]string{"LANG": "C.UTF-8"}
	h, err := s.Create(c)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(h.ID) {
		t.Errorf("id %q is not hs_ + 32 hex", h.ID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(h.AgentSessionID) {
		t.Errorf("agent session id %q is not a v4 UUID", h.AgentSessionID)
	}
	if h.Restart != "always" || h.Desired != "running" || h.Cols != 120 || h.Rows != 40 || h.Launched || h.Runtime.State != "unknown" {
		t.Errorf("defaults: %+v", h)
	}
	got, err := s.Get(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSessionID != h.AgentSessionID || strings.Join(got.Args, " ") != "--model opus" || got.Env["LANG"] != "C.UTF-8" || got.CreatedAt != h.CreatedAt {
		t.Errorf("stored %+v, created %+v", got, h)
	}
	other, err := s.Create(wire.HostedSessionCreate{Node: "pc", Base: "team/worker", Profile: "team-worker", Directory: "/work/other"})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == h.ID || other.AgentSessionID == h.AgentSessionID {
		t.Error("two records share an id or a session id")
	}
	list, err := s.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d records, %v", len(list), err)
	}
}

func TestDuplicateNodeBaseDirectoryIsRefused(t *testing.T) {
	s := newService(t)
	if _, err := s.Create(record()); err != nil {
		t.Fatal(err)
	}
	dup := record()
	dup.Directory = "/work/app/" // the same directory spelt with a trailing slash
	dup.Profile = "other"
	if _, err := s.Create(dup); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	for _, c := range []wire.HostedSessionCreate{
		{Node: "laptop", Base: "team/worker", Profile: "p", Directory: "/work/app"},
		{Node: "pc", Base: "team/boss", Profile: "p", Directory: "/work/app"},
		{Node: "pc", Base: "team/worker", Profile: "p", Directory: "/work/app2"},
	} {
		if _, err := s.Create(c); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

func TestUpdateChangesOnlyGivenFields(t *testing.T) {
	s := newService(t)
	h, err := s.Create(record())
	if err != nil {
		t.Fatal(err)
	}
	label, restart, args := "42", "on-failure", []string{"--model", "sonnet"}
	got, err := s.Update(h.ID, wire.HostedSessionChange{Label: &label, Restart: &restart, Args: &args})
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "42" || got.Restart != "on-failure" || strings.Join(got.Args, " ") != "--model sonnet" || got.Desired != "running" || got.Profile != "team-worker" {
		t.Errorf("updated %+v", got)
	}
	if got, err = s.SetDesired(h.ID, "stopped"); err != nil || got.Desired != "stopped" {
		t.Fatalf("stop: %+v %v", got, err)
	}
	stored, _ := s.Get(h.ID)
	if stored.Label != "42" || stored.Desired != "stopped" || stored.AgentSessionID != h.AgentSessionID {
		t.Errorf("stored %+v", stored)
	}
	bad := "sometimes"
	if _, err = s.Update(h.ID, wire.HostedSessionChange{Restart: &bad}); !errors.As(err, new(InvalidError)) {
		t.Errorf("bad restart: %v", err)
	}
	if stored, _ = s.Get(h.ID); stored.Restart != "on-failure" {
		t.Errorf("a refused change was stored: %+v", stored)
	}
	if _, err = s.Update("hs_00000000000000000000000000000000", wire.HostedSessionChange{Label: &label}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestStartRestartAndNewConversationCountRequests(t *testing.T) {
	s := newService(t)
	h, err := s.Create(record())
	if err != nil {
		t.Fatal(err)
	}
	// Start counts even when the record already runs (it clears a crash
	// loop on the node); a stop does not.
	if got, err := s.SetDesired(h.ID, DesiredRunning); err != nil || got.StartGen != 1 {
		t.Fatalf("start: %+v %v", got, err)
	}
	if got, err := s.SetDesired(h.ID, DesiredStopped); err != nil || got.StartGen != 1 {
		t.Fatalf("stop: %+v %v", got, err)
	}
	// Restart of a stopped record starts it.
	if got, err := s.Restart(h.ID); err != nil || got.Desired != DesiredRunning || got.StartGen != 2 || got.RestartGen != 0 {
		t.Fatalf("restart stopped: %+v %v", got, err)
	}
	if got, err := s.Restart(h.ID); err != nil || got.RestartGen != 1 {
		t.Fatalf("restart: %+v %v", got, err)
	}
	if _, err := s.db.Exec(`UPDATE hosted_sessions SET launched=1 WHERE id=?`, h.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.NewConversation(h.ID)
	if err != nil || got.AgentSessionID == h.AgentSessionID || got.Launched || got.Desired != DesiredRunning {
		t.Fatalf("new conversation: %+v %v", got, err)
	}
	stored, _ := s.Get(h.ID)
	if stored.AgentSessionID != got.AgentSessionID || stored.Launched || stored.RestartGen != 1 || stored.StartGen != 2 {
		t.Fatalf("stored %+v", stored)
	}
	// A launched report about the old conversation is ignored.
	if err := s.markLaunched(h.ID, h.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	if stored, _ = s.Get(h.ID); stored.Launched {
		t.Fatal("old conversation's report marked the new one launched")
	}
	if err := s.markLaunched(h.ID, got.AgentSessionID); err != nil {
		t.Fatal(err)
	}
	if stored, _ = s.Get(h.ID); !stored.Launched {
		t.Fatal("not marked launched")
	}
	// The resume-failure replacement happens once per failed conversation.
	again, changed, err := s.replaceConversation(h.ID, h.AgentSessionID)
	if err != nil || changed || again.AgentSessionID != got.AgentSessionID {
		t.Fatalf("stale replacement: %+v %v %v", again, changed, err)
	}
	fresh, changed, err := s.replaceConversation(h.ID, got.AgentSessionID)
	if err != nil || !changed || fresh.AgentSessionID == got.AgentSessionID || fresh.Launched {
		t.Fatalf("replacement: %+v %v %v", fresh, changed, err)
	}
	for _, f := range []func(string) (wire.HostedSession, error){s.Restart, s.NewConversation} {
		if _, err := f("hs_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
	}
}

func TestDeleteRemovesTheRecord(t *testing.T) {
	s := newService(t)
	h, err := s.Create(record())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Delete(h.ID); err != nil || got.ID != h.ID {
		t.Fatalf("delete: %+v %v", got, err)
	}
	if _, err = s.Get(h.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if _, err = s.Delete(h.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, err = s.Create(record()); err != nil {
		t.Errorf("re-create after delete: %v", err)
	}
}

func TestValidationRules(t *testing.T) {
	s := newService(t)
	cases := []struct {
		name string
		edit func(*wire.HostedSessionCreate)
		want string
	}{
		{"unknown base", func(c *wire.HostedSessionCreate) { c.Base = "team/nobody" }, "base: role team/nobody does not exist"},
		{"invalid base", func(c *wire.HostedSessionCreate) { c.Base = "team worker" }, "base: invalid role"},
		{"relative directory", func(c *wire.HostedSessionCreate) { c.Directory = "work/app" }, "directory: \"work/app\" is not an absolute path"},
		{"empty directory", func(c *wire.HostedSessionCreate) { c.Directory = "" }, "is not an absolute path"},
		{"drive-relative directory", func(c *wire.HostedSessionCreate) { c.Directory = `C:work` }, "is not an absolute path"},
		{"session id", func(c *wire.HostedSessionCreate) { c.Args = []string{"--session-id", "x"} }, "args: --session-id is not allowed"},
		{"session id joined", func(c *wire.HostedSessionCreate) { c.Args = []string{"--session-id=x"} }, "args: --session-id is not allowed"},
		{"resume", func(c *wire.HostedSessionCreate) { c.Args = []string{"--resume"} }, "args: --resume is not allowed"},
		{"resume short", func(c *wire.HostedSessionCreate) { c.Args = []string{"-r"} }, "args: -r is not allowed"},
		{"continue", func(c *wire.HostedSessionCreate) { c.Args = []string{"--continue"} }, "args: --continue is not allowed"},
		{"print short", func(c *wire.HostedSessionCreate) { c.Args = []string{"-p"} }, "args: -p is not allowed"},
		{"print", func(c *wire.HostedSessionCreate) { c.Args = []string{"--model", "opus", "--print"} }, "args: --print is not allowed"},
		{"env token", func(c *wire.HostedSessionCreate) { c.Env = map[string]string{"GH_TOKEN": "x"} }, "env: GH_TOKEN is not allowed"},
		{"env secret", func(c *wire.HostedSessionCreate) { c.Env = map[string]string{"client_secret": "x"} }, "env: client_secret is not allowed"},
		{"env key", func(c *wire.HostedSessionCreate) { c.Env = map[string]string{"API_KEY": "x"} }, "env: API_KEY is not allowed"},
		{"env password", func(c *wire.HostedSessionCreate) { c.Env = map[string]string{"DB_PASSWORD": "x"} }, "env: DB_PASSWORD is not allowed"},
		{"env bad name", func(c *wire.HostedSessionCreate) { c.Env = map[string]string{"A=B": "x"} }, "env: invalid name"},
		{"restart", func(c *wire.HostedSessionCreate) { c.Restart = "sometimes" }, `restart: "sometimes" is not one of always, on-failure, never`},
		{"desired", func(c *wire.HostedSessionCreate) { c.Desired = "paused" }, `desired: "paused" is not one of running, stopped`},
		{"node", func(c *wire.HostedSessionCreate) { c.Node = "" }, "node: invalid name"},
		{"profile", func(c *wire.HostedSessionCreate) { c.Profile = "../x" }, "profile: invalid name"},
		{"parent", func(c *wire.HostedSessionCreate) { c.Parent = "a b" }, "parent: invalid role"},
	}
	for _, tc := range cases {
		c := record()
		tc.edit(&c)
		_, err := s.Create(c)
		if !errors.As(err, new(InvalidError)) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Errorf("refused records were stored: %d", len(list))
	}
	// Stopped, so the node's session cap does not refuse them.
	for _, ok := range []string{"/", "/work/app", `C:\work\app`, "C:/work", `\\server\share\app`} {
		c := record()
		c.Directory, c.Desired = ok, DesiredStopped
		if _, err := s.Create(c); err != nil {
			t.Errorf("directory %q: %v", ok, err)
		}
	}
	for _, r := range []string{"always", "on-failure", "never"} {
		c := record()
		c.Directory, c.Restart, c.Desired = "/r/"+r, r, DesiredStopped
		if _, err := s.Create(c); err != nil {
			t.Errorf("restart %s: %v", r, err)
		}
	}
}

func TestNodesListsTheRegistry(t *testing.T) {
	s := bareService(t)
	if n, err := s.Nodes(); err != nil || len(n) != 0 {
		t.Fatalf("empty registry: %v %v", n, err)
	}
	if _, err := s.db.Exec(`INSERT INTO nodes(name,token_hash,machine,os,version,created_at,last_seen_at) VALUES('pc','h','PC-1','windows','dev',1,2)`); err != nil {
		t.Fatal(err)
	}
	n, err := s.Nodes()
	if err != nil || len(n) != 1 || n[0].Name != "pc" || n[0].OS != "windows" || n[0].LastSeenAt == nil || *n[0].LastSeenAt != 2 || n[0].RevokedAt != nil {
		t.Fatalf("nodes = %+v %v", n, err)
	}
}

// TestImportsStayInsideTheRule: hosting imports only config, store and wire
// of the internal packages (ARCHITECTURE.md, "Package map").
func TestImportsStayInsideTheRule(t *testing.T) {
	allowed := map[string]bool{"config": true, "store": true, "wire": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const internal = "github.com/sandcorporation/switchboard/internal/"
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if pkg, ok := strings.CutPrefix(path, internal); ok && !allowed[pkg] {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
