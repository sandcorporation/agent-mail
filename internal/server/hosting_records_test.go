package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// adminCall sends body to an admin route; guard adds the dashboard write
// guard header.
func adminCall(t *testing.T, base, method, path string, cookie *http.Cookie, guard bool, body string) httpReply {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if guard {
		req.Header.Set("X-Switchboard-Dashboard", "1")
	}
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return httpReply{res.StatusCode, string(b)}
}

const newRecord = `{"node":"pc","base":"a","profile":"team-a","directory":"/work/app","label":"42","args":["--model","opus"]}`

func TestHostingRecordsThroughAdminAPI(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	if got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord); got.code != http.StatusBadRequest || !strings.Contains(got.body, "node: no node named pc") {
		t.Fatalf("create on an unregistered node = %d %s", got.code, got.body)
	}
	nodeToken(t, s, "pc")

	if got := adminCall(t, base, "POST", "/api/admin/terminals", nil, true, newRecord); got.code != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d", got.code)
	}
	if got := adminCall(t, base, "GET", "/api/admin/terminals", nil, false, ""); got.code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", got.code)
	}
	if got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, false, newRecord); got.code != http.StatusForbidden {
		t.Fatalf("create without the write guard = %d", got.code)
	}

	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord)
	if got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	var h wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &h); err != nil {
		t.Fatal(err)
	}
	if h.Node != "pc" || h.Base != "a" || h.Label != "42" || h.Restart != "always" || h.Desired != "running" || h.Runtime.State != "unknown" || h.AgentSessionID == "" {
		t.Errorf("created %+v", h)
	}
	if got = adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, newRecord); got.code != http.StatusConflict {
		t.Errorf("duplicate = %d %s", got.code, got.body)
	}
	for body, want := range map[string]string{
		`{"node":"pc","base":"nobody","profile":"p","directory":"/w"}`:                   "base: role nobody does not exist",
		`{"node":"pc","base":"a","profile":"p","directory":"w"}`:                         "not an absolute path",
		`{"node":"pc","base":"a","profile":"p","directory":"/w","args":["--resume"]}`:    "args: --resume is not allowed",
		`{"node":"pc","base":"a","profile":"p","directory":"/w","env":{"GH_TOKEN":"x"}}`: "env: GH_TOKEN is not allowed",
		`{"node":"pc","base":"a","profile":"p","directory":"/w","restart":"maybe"}`:      "restart:",
		`{"node":"pc","base":"a","profile":"p","directory":"/w","colour":"red"}`:         "unknown field",
	} {
		if got = adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, body); got.code != http.StatusBadRequest || !strings.Contains(got.body, want) {
			t.Errorf("%s = %d %q, want 400 %q", body, got.code, got.body, want)
		}
	}

	if got = adminCall(t, base, "PUT", "/api/admin/terminals/"+h.ID, cookie, false, `{"desired":"stopped"}`); got.code != http.StatusForbidden {
		t.Errorf("change without the write guard = %d", got.code)
	}
	if got = adminCall(t, base, "PUT", "/api/admin/terminals/"+h.ID, cookie, true, `{"desired":"stopped","restart":"never","env":{"LANG":"C"}}`); got.code != http.StatusOK {
		t.Fatalf("change = %d %s", got.code, got.body)
	}
	if got = adminCall(t, base, "PUT", "/api/admin/terminals/"+h.ID, cookie, true, `{"args":["-p"]}`); got.code != http.StatusBadRequest {
		t.Errorf("refused change = %d %s", got.code, got.body)
	}
	got = adminCall(t, base, "GET", "/api/admin/terminals", cookie, false, "")
	var list []wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &list); err != nil || got.code != http.StatusOK {
		t.Fatalf("list = %d %s", got.code, got.body)
	}
	if len(list) != 1 || list[0].Desired != "stopped" || list[0].Restart != "never" || list[0].Env["LANG"] != "C" || list[0].Label != "42" || strings.Join(list[0].Args, " ") != "--model opus" {
		t.Errorf("list %+v", list)
	}

	if got = adminCall(t, base, "DELETE", "/api/admin/terminals/"+h.ID, cookie, false, ""); got.code != http.StatusForbidden {
		t.Errorf("delete without the write guard = %d", got.code)
	}
	if got = adminCall(t, base, "DELETE", "/api/admin/terminals/"+h.ID, cookie, true, ""); got.code != http.StatusOK {
		t.Errorf("delete = %d %s", got.code, got.body)
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if got = adminCall(t, base, m, "/api/admin/terminals/"+h.ID, cookie, true, `{}`); got.code != http.StatusNotFound {
			t.Errorf("%s removed record = %d", m, got.code)
		}
	}
	if got = adminCall(t, base, "GET", "/api/admin/terminals", cookie, false, ""); strings.TrimSpace(got.body) != "[]" {
		t.Errorf("list after delete = %s", got.body)
	}
	if got = adminCall(t, base, "GET", "/api/admin/nodes", cookie, false, ""); got.code != http.StatusOK || !strings.HasPrefix(got.body, `[{"name":"pc",`) || strings.Contains(got.body, "token") || strings.Contains(got.body, "sha256") {
		t.Errorf("nodes = %d %s", got.code, got.body)
	}
}

// With hosting off a create is answered as an unknown route and stores
// nothing.
func TestHostingOffStoresNoRecords(t *testing.T) {
	s, secret, now := builtinServer(t)
	c := sessionFrom(t, doAuth(s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, secret, *now, 0))}))
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	if got := adminCall(t, srv.URL, "POST", "/api/admin/terminals", c, true, newRecord); got.code == http.StatusCreated {
		t.Fatalf("create with hosting off = %d", got.code)
	}
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM hosted_sessions)+(SELECT COUNT(*) FROM nodes)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("hosting tables hold %d rows (%v)", n, err)
	}
}

// hostCLI runs `switchboard host …` in a child process against dbPath.
func hostCLI(t *testing.T, dbPath string, args ...string) (int, string, string) {
	t.Helper()
	return binaryCLI(t, dbPath, append([]string{"host"}, args...)...)
}

// binaryCLI runs the binary's client with args against dbPath and returns
// its exit code, stdout and stderr.
func binaryCLI(t *testing.T, dbPath string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	argJSON, _ := json.Marshal(args)
	cmd.Env = append(cliEnv(), "SWITCHBOARD_TEST_CLI=1", "SWITCHBOARD_TEST_ARGS="+string(argJSON), "DB_PATH="+dbPath)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(out.String()), "PASS")), errOut.String()
}

func TestHostCLIManagesRecords(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pc.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO roles(role,token,created_at) VALUES('team/worker','h','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO nodes(name,token_hash,created_at) VALUES('pc','h',1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	add := []string{"add", "--node", "pc", "--base", "team/worker", "--profile", "team-worker", "--dir", `C:\work\app`, "--label", "42", "--parent", "team/boss", "--restart", "on-failure", "--", "--model", "opus"}
	code, id, stderr := hostCLI(t, dbPath, add...)
	if code != 0 || !strings.HasPrefix(id, "hs_") {
		t.Fatalf("add = %d %q %s", code, id, stderr)
	}
	if code, _, stderr = hostCLI(t, dbPath, add...); code != 1 || !strings.Contains(stderr, "already exists") {
		t.Errorf("duplicate add = %d %s", code, stderr)
	}
	bad := append(append([]string{}, add[:len(add)-3]...), "--", "--resume")
	bad[8] = `D:\other`
	if code, _, stderr = hostCLI(t, dbPath, bad...); code != 1 || !strings.Contains(stderr, "args: --resume is not allowed") {
		t.Errorf("refused add = %d %s", code, stderr)
	}
	if code, _, stderr = hostCLI(t, dbPath, "add", "--node", "pc"); code != 2 || !strings.Contains(stderr, "requires --node, --base, --profile and --dir") {
		t.Errorf("incomplete add = %d %s", code, stderr)
	}

	code, out, stderr := hostCLI(t, dbPath, "list", "--json")
	var list []wire.HostedSession
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list) != 1 {
		t.Fatalf("list = %d %q %s", code, out, stderr)
	}
	h := list[0]
	if h.ID != id || h.Directory != `C:\work\app` || h.Label != "42" || h.Parent != "team/boss" || h.Restart != "on-failure" || strings.Join(h.Args, " ") != "--model opus" || h.Profile != "team-worker" {
		t.Errorf("listed %+v", h)
	}
	if code, out, _ = hostCLI(t, dbPath, "list"); code != 0 || !strings.Contains(out, "DESIRED") || !strings.Contains(out, id) || !strings.Contains(out, "unknown") {
		t.Errorf("list table = %q", out)
	}

	// Permission mode (#220): host add skips by default, --ask-permissions
	// and host set --skip-permissions change it.
	if !h.SkipPermissions {
		t.Errorf("host add did not skip permissions: %+v", h)
	}
	if code, out, stderr = hostCLI(t, dbPath, "set", id, "--skip-permissions", "off"); code != 0 || out != id+" permissions ask (from the next launch or restart)" {
		t.Errorf("set off = %d %q %s", code, out, stderr)
	}
	if _, out, _ = hostCLI(t, dbPath, "list", "--json"); json.Unmarshal([]byte(out), &list) != nil || list[0].SkipPermissions {
		t.Errorf("after set off %+v", list[0])
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"set", id}, "requires --skip-permissions on|off"},
		{[]string{"set", id, "--skip-permissions", "yes"}, "wants on or off"},
		{[]string{"set", "--skip-permissions", "on"}, "host set requires ID"},
		{[]string{"set", id, "--skip-permissions", "on", "--ask-permissions"}, "--ask-permissions is for host add"},
		{[]string{"stop", id, "--skip-permissions", "on"}, "--skip-permissions is for host set"},
	} {
		if code, _, stderr = hostCLI(t, dbPath, c.args...); code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%q = %d %s", c.args, code, stderr)
		}
	}
	asked := append(append([]string{}, add[:len(add)-3]...), "--ask-permissions")
	asked[8] = `C:\work\asked`
	if code, out, stderr = hostCLI(t, dbPath, append(asked, "--json")...); code != 0 || !strings.Contains(out, `"skip_permissions":false`) {
		t.Errorf("add --ask-permissions = %d %q %s", code, out, stderr)
	} else if code, _, stderr = hostCLI(t, dbPath, "remove", out[strings.Index(out, "hs_"):strings.Index(out, "hs_")+35]); code != 0 {
		t.Errorf("remove asked = %d %s", code, stderr)
	}

	if code, out, stderr = hostCLI(t, dbPath, "stop", id); code != 0 || out != id+" stopped" {
		t.Errorf("stop = %d %q %s", code, out, stderr)
	}
	if code, out, _ = hostCLI(t, dbPath, "start", id); code != 0 || out != id+" running" {
		t.Errorf("start = %d %q", code, out)
	}
	if code, _, stderr = hostCLI(t, dbPath, "start"); code != 2 {
		t.Errorf("start without ID = %d %s", code, stderr)
	}
	// Restart and New conversation go through the record, so the host's
	// next sync carries them to the node.
	if code, out, _ = hostCLI(t, dbPath, "restart", id); code != 0 || out != id+" restart requested" {
		t.Errorf("restart = %d %q", code, out)
	}
	if code, out, _ = hostCLI(t, dbPath, "new-conversation", id); code != 0 || !strings.HasPrefix(out, id+" new conversation ") || strings.HasSuffix(out, h.AgentSessionID) {
		t.Errorf("new-conversation = %d %q", code, out)
	}
	if _, out, _ = hostCLI(t, dbPath, "list", "--json"); json.Unmarshal([]byte(out), &list) != nil || list[0].RestartGen != 1 || list[0].StartGen != 1 ||
		list[0].AgentSessionID == h.AgentSessionID || list[0].Launched {
		t.Errorf("after restart and new conversation %+v", list[0])
	}
	if code, _, stderr = hostCLI(t, dbPath, "restart"); code != 2 {
		t.Errorf("restart without ID = %d %s", code, stderr)
	}
	if code, out, _ = hostCLI(t, dbPath, "remove", id); code != 0 || out != "removed "+id {
		t.Errorf("remove = %d %q", code, out)
	}
	if code, _, stderr = hostCLI(t, dbPath, "remove", id); code != 1 || !strings.Contains(stderr, "no hosted session") {
		t.Errorf("second remove = %d %s", code, stderr)
	}
	if code, out, _ = hostCLI(t, dbPath, "list"); code != 0 || out != "no hosted sessions" {
		t.Errorf("empty list = %q", out)
	}
}

func TestHostCLIRefusesTheContainerDatabase(t *testing.T) {
	for _, path := range []string{"", "/data/mail.db"} {
		code, _, stderr := hostCLI(t, path, "list")
		if code != 1 || !strings.Contains(stderr, "name this instance's own database") {
			t.Errorf("DB_PATH=%q: %d %s", path, code, stderr)
		}
	}
}
