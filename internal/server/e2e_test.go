package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// End to end (#194): the built switchboard binary runs `switchboard up` on a
// temporary data directory and a free loopback port, with this OS's real
// terminal backend (tmux on Linux, the ConPTY keeper on Windows) and this
// test binary as the agent CLI (fakeagent). Every step goes through public
// surfaces: the admin API, the browser terminal socket, the mail API and the
// process itself. scripts/ci/e2e.sh runs it on both CI runners and fails when
// fewer steps than expected ran.
//
// SWITCHBOARD_TEST_E2E=require turns a skip (no tmux, no ConPTY, no Go
// toolchain) into a failure. SWITCHBOARD_TEST_E2E_BIN names a prebuilt
// binary instead of building one.

// e2eWait bounds every wait for an event; the keeper on a CI runner can be
// slow to start.
const e2eWait = 60 * time.Second

// e2eRequired reports whether a skip must fail instead.
func e2eRequired() bool { return os.Getenv("SWITCHBOARD_TEST_E2E") == "require" }

func e2eSkip(t *testing.T, format string, args ...any) {
	t.Helper()
	if e2eRequired() {
		t.Fatalf("SWITCHBOARD_TEST_E2E=require: "+format, args...)
	}
	t.Skipf(format, args...)
}

// e2eBinary builds cmd/switchboard into dir (or takes SWITCHBOARD_TEST_E2E_BIN).
func e2eBinary(t *testing.T, dir string) string {
	t.Helper()
	if bin := os.Getenv("SWITCHBOARD_TEST_E2E_BIN"); bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	return e2eBuild(t, dir, "e2e")
}

// e2eBuild builds switchboard into dir with version as its version string.
func e2eBuild(t *testing.T, dir, version string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, err := os.Stat(goTool); err != nil {
			e2eSkip(t, "no Go toolchain to build switchboard: %v", err)
		}
	}
	bin := filepath.Join(dir, "switchboard")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command(goTool, "build", "-o", bin, "-ldflags", "-X main.version="+version, "github.com/sandcorporation/switchboard/cmd/switchboard")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// e2eUp is one installation: its data directory, port and up process.
type e2eUp struct {
	t       *testing.T
	bin     string
	dir     string
	port    string
	env     []string
	cmd     *exec.Cmd
	done    chan error
	logf    *os.File
	cookie  *http.Cookie
	backend e2eBackend
}

// e2eBackend is the per-OS part: the environment the backend needs, how up
// is stopped, and how the backend's server (tmux server, keeper) is ended.
type e2eBackend struct {
	env  []string
	stop func(*exec.Cmd)
	end  func(instance string)
}

func newE2EUp(t *testing.T) *e2eUp {
	t.Helper()
	b := e2eBackendFor(t)
	// A directory outside t.TempDir: on Windows the keeper and its children
	// hold files in it until the cleanup below has ended them.
	dir, err := os.MkdirTemp("", "sbe2e")
	if err != nil {
		t.Fatal(err)
	}
	r := &e2eUp{t: t, dir: dir, backend: b}
	instance := instanceFor(filepath.Join(dir, upNodeToken))
	t.Cleanup(func() {
		r.stop()
		b.end(instance)
		// Windows releases files a moment after their process ends.
		deadline := time.Now().Add(15 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil || time.Now().After(deadline) {
				if err != nil {
					t.Logf("leaving %s: %v", dir, err)
				}
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	r.bin = e2eBinary(t, filepath.Join(dir, "bin"))

	// The agent CLI is this test binary; its transcripts stay in the data
	// directory. The variables pass through up to every child it launches.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "config")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "team-worker.json"), []byte(`{"role":"team/worker","token":"worker-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(dir, upDatabase))
	if err != nil {
		t.Fatal(err)
	}
	secret := totpEncoding.EncodeToString([]byte("0123456789abcdefghij"))
	hash, err := adminauth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO roles(role,token,created_at) VALUES('team/worker',?,'now')`, []any{store.TokenHash("worker-secret")}},
		{`INSERT INTO roles(role,token,created_at) VALUES('team/lead',?,'now')`, []any{store.TokenHash("lead-secret")}},
		{`INSERT INTO admin_users(name,password_hash,totp_secret,created_at) VALUES('alice',?,?,'now')`, []any{hash, secret}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.port = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	r.env = append(cliEnv(),
		"SWITCHBOARD_ADMIN_AUTH=builtin",
		"SWITCHBOARD_HOSTING_AGENT="+exe,
		"SWITCHBOARD_HOSTING_MACHINE=pc-1",
		"SWITCHBOARD_HOSTING_MAX=1",
		"SWITCHBOARD_CONFIG_DIR="+conf,
		"SWITCHBOARD_FAKE_AGENT_HEARTBEAT=200ms",
		"SWITCHBOARD_TOKEN=role-secret-of-the-operator")
	// The scenes from this source tree (tests run in the package directory).
	scenes, err := filepath.Abs(filepath.Join("..", "term", "testdata", "scenes"))
	if err != nil {
		t.Fatal(err)
	}
	r.env = append(append(r.env, fakeagent.Env(filepath.Join(dir, "transcripts"))...), b.env...)
	r.env = append(r.env, "SWITCHBOARD_FAKE_AGENT_SCENES="+scenes)

	logf, err := os.OpenFile(filepath.Join(dir, "up.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	r.logf = logf
	t.Cleanup(func() { logf.Close() })
	r.start()
	r.login(secret)
	return r
}

func (r *e2eUp) base() string { return "http://127.0.0.1:" + r.port }

func (r *e2eUp) log() string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "up.log"))
	return string(b)
}

// launch starts `switchboard up` without waiting for it.
func (r *e2eUp) launch() {
	r.t.Helper()
	r.cmd = exec.Command(r.bin, "up", "--data", r.dir, "--port", r.port)
	r.cmd.Env = r.env
	r.cmd.Stdout, r.cmd.Stderr = r.logf, r.logf
	if err := r.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	done := make(chan error, 1)
	r.done = done
	cmd := r.cmd
	go func() { done <- cmd.Wait() }()
}

// start runs up and waits until it answers.
func (r *e2eUp) start() {
	r.t.Helper()
	r.launch()
	r.wait("up to answer", func() bool {
		select {
		case err := <-r.done:
			r.done <- err
			r.t.Fatalf("up exited: %v\n%s", err, r.log())
		default:
		}
		res, err := http.Get(r.base() + "/health")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
}

// stop ends up (SIGTERM on Linux, as Ctrl-C; terminated on Windows).
func (r *e2eUp) stop() {
	if r.cmd == nil {
		return
	}
	select {
	case <-r.done:
	default:
		r.backend.stop(r.cmd)
		select {
		case <-r.done:
		case <-time.After(30 * time.Second):
			_ = r.cmd.Process.Kill()
			<-r.done
		}
	}
	r.cmd = nil
}

func (r *e2eUp) login(secret string) {
	r.t.Helper()
	code, err := adminauth.TOTPCode(secret, time.Now().Unix()/adminauth.TOTPStep)
	if err != nil {
		r.t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", r.base()+"/auth/login", strings.NewReader(loginBody("alice", "correct horse battery", code)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Switchboard-Dashboard", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			r.cookie = c
		}
	}
	if r.cookie == nil {
		r.t.Fatalf("login = %d", res.StatusCode)
	}
}

func (r *e2eUp) wait(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(e2eWait)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s; up log:\n%s", what, r.log())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *e2eUp) session(id string) wire.HostedSession {
	r.t.Helper()
	return terminals(r.t, r.base(), r.cookie)[id]
}

func (r *e2eUp) add(directory, desired string) string {
	r.t.Helper()
	body, _ := json.Marshal(wire.HostedSessionCreate{Node: "local", Base: "team/worker", Profile: "team-worker", Directory: directory, Desired: desired})
	got := adminCall(r.t, r.base(), "POST", "/api/admin/terminals", r.cookie, true, string(body))
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil || got.code != http.StatusCreated {
		r.t.Fatalf("create = %d %s", got.code, got.body)
	}
	return rec.ID
}

func (r *e2eUp) setDesired(id, desired string) {
	r.t.Helper()
	if got := adminCall(r.t, r.base(), "PUT", "/api/admin/terminals/"+id, r.cookie, true, `{"desired":"`+desired+`"}`); got.code != http.StatusOK {
		r.t.Fatalf("desired %s = %d %s", desired, got.code, got.body)
	}
}

// e2eTerm is one browser terminal socket and the screen it has drawn.
type e2eTerm struct {
	t     *testing.T
	c     *websocket.Conn
	hello wire.TerminalMessage
	emu   *term.Emulator
}

// open dials the terminal socket of id with a fresh ticket of mode and
// reads its hello and snapshot.
func (r *e2eUp) open(id, mode string) *e2eTerm {
	r.t.Helper()
	tk := adminCall(r.t, r.base(), "POST", "/api/admin/terminals/"+id+"/ticket", r.cookie, true, `{"mode":"`+mode+`"}`)
	var ticket wire.TerminalTicket
	if err := json.Unmarshal([]byte(tk.body), &ticket); err != nil || ticket.Ticket == "" {
		r.t.Fatalf("ticket = %d %s", tk.code, tk.body)
	}
	c, code := dialTerminal(r.t, r.base(), id, ticket.Ticket, r.cookie, r.base())
	if c == nil {
		r.t.Fatalf("socket = %d", code)
	}
	tm := &e2eTerm{t: r.t, c: c}
	if err := json.Unmarshal(readFrame(r.t, c, websocket.MessageText), &tm.hello); err != nil || tm.hello.Type != "hello" {
		r.t.Fatalf("hello %+v %v", tm.hello, err)
	}
	tm.emu = term.NewEmulator(tm.hello.Cols, tm.hello.Rows, 2000)
	_, _ = tm.emu.Write(readFrame(r.t, c, websocket.MessageBinary))
	return tm
}

func (tm *e2eTerm) close() { _ = tm.c.Close(websocket.StatusNormalClosure, "") }

// screen is everything drawn so far, history included, with the dump's row
// borders removed so a line wrapped at a narrow width reads whole.
func (tm *e2eTerm) screen() string {
	return strings.ReplaceAll(tm.emu.Grid(-1).Dump(), "|\n|", "")
}

// until reads the stream until the drawn screen contains want.
func (tm *e2eTerm) until(want string) {
	tm.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eWait)
	defer cancel()
	for !strings.Contains(tm.screen(), want) {
		typ, b, err := tm.c.Read(ctx)
		if err != nil {
			tm.t.Fatalf("waiting for %q: %v; screen:\n%s", want, err, tm.screen())
		}
		if typ == websocket.MessageBinary {
			_, _ = tm.emu.Write(b)
		}
	}
}

// line types a line as a browser would (Enter is CR).
func (tm *e2eTerm) line(s string) {
	tm.t.Helper()
	typeInto(tm.t, tm.c, s+"\r")
}

// TestE2E_HostingUp walks one installation through hosting on this OS. The
// steps share one up process and run in order; the first failure stops the
// rest.
func TestE2E_HostingUp(t *testing.T) {
	if testing.Short() {
		t.Skip("end to end: not in -short")
	}
	r := newE2EUp(t)
	work := filepath.Join(r.dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	var id, sessionID string
	var pid, runNo int
	online := func(sid string) func() bool {
		return func() bool {
			h := r.session(id)
			return h.Slot != nil && h.Slot.Role == "team/worker-1" && h.Presence == wire.PresenceOnline && h.Slot.SessionID == sid
		}
	}
	steps := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"UpRegistersTheLocalNode", func(t *testing.T) {
			nodes := adminCall(t, r.base(), "GET", "/api/admin/nodes", r.cookie, false, "")
			if !strings.Contains(nodes.body, `"name":"local"`) || !strings.Contains(nodes.body, `"machine":"pc-1"`) || !strings.Contains(nodes.body, `"os":"`+runtime.GOOS+`"`) {
				t.Fatalf("nodes %s", nodes.body)
			}
		}},
		{"AddedSessionRuns", func(t *testing.T) {
			id = r.add(work, "")
			r.wait("the session running", func() bool { h := r.session(id); return h.Runtime.State == "running" && h.Runtime.PID != 0 })
			h := r.session(id)
			sessionID, pid, runNo = h.AgentSessionID, h.Runtime.PID, h.Runtime.Run
			if sessionID == "" || runNo != 1 {
				t.Fatalf("session %+v", h)
			}
		}},
		{"AgentHoldsSlotOne", func(t *testing.T) {
			r.wait("team/worker-1 online", online(sessionID))
		}},
		{"ViewerSeesTheLaunch", func(t *testing.T) {
			tm := r.open(id, "view")
			defer tm.close()
			if tm.hello.State != "running" {
				t.Fatalf("hello %+v", tm.hello)
			}
			tm.until("argv --session-id " + sessionID)
			if s := tm.screen(); strings.Contains(s, "role-secret-of-the-operator") || !strings.Contains(s, "env SWITCHBOARD_PROFILE=team-worker") {
				t.Fatalf("child environment:\n%s", s)
			}
		}},
		{"InputEchoes", func(t *testing.T) {
			tm := r.open(id, "input")
			defer tm.close()
			tm.line("typed-in-the-browser")
			tm.until("echo typed-in-the-browser")
		}},
		{"ResizeReachesTheTerminal", func(t *testing.T) {
			// The golden scene's size, so the snapshot step compares as is.
			tm := r.open(id, "input")
			defer tm.close()
			clientSend(t, tm.c, wire.TerminalInput{Type: "resize", Cols: 24, Rows: 6})
			r.wait("the size stored", func() bool { h := r.session(id); return h.Cols == 24 && h.Rows == 6 })
			tm.line("size")
			tm.until("size 24x6")
		}},
		{"MailReachesTheSession", func(t *testing.T) {
			req, _ := http.NewRequest("POST", r.base()+"/api/messages", strings.NewReader(`{"recipient":"team/worker","subject":"e2e-mail","body":"hello"}`))
			req.Header.Set("Authorization", "Bearer lead-secret")
			req.Header.Set("X-Switchboard-Role", "team/lead")
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
				t.Fatalf("send = %d", res.StatusCode)
			}
			tm := r.open(id, "input")
			defer tm.close()
			tm.line("inbox")
			tm.until("mail team/lead e2e-mail")
		}},
		{"UpRestartKeepsTheSession", func(t *testing.T) {
			r.stop()
			r.start()
			r.wait("the session adopted", online(sessionID))
			r.wait("the runtime reported", func() bool { return r.session(id).Runtime.State == "running" })
			if h := r.session(id); h.Runtime.PID != pid || h.Runtime.Run != runNo || h.AgentSessionID != sessionID {
				t.Fatalf("after up restart %+v, was pid %d run %d", h.Runtime, pid, runNo)
			}
		}},
		{"CrashRelaunchesWithResume", func(t *testing.T) {
			tm := r.open(id, "input")
			tm.line("exit 1")
			tm.close()
			r.wait("relaunched", func() bool { h := r.session(id); return h.Runtime.Run > runNo && h.Runtime.State == "running" })
			r.wait("team/worker-1 online again", online(sessionID))
			v := r.open(id, "view")
			defer v.close()
			v.until("argv --resume " + sessionID)
			if h := r.session(id); h.AgentSessionID != sessionID || h.Runtime.PID == pid {
				t.Fatalf("relaunch %+v", h)
			}
		}},
		{"ReconnectingViewerGetsTheGoldenScreen", func(t *testing.T) {
			tm := r.open(id, "input")
			tm.line("draw full-redraw")
			tm.until("list the files ok")
			tm.close()
			v := r.open(id, "view")
			defer v.close()
			if v.hello.Cols != 24 || v.hello.Rows != 6 {
				t.Fatalf("hello %+v", v.hello)
			}
			sc, golden := scene(t, "full-redraw")
			got := screenDump(v.emu.Grid(-1))
			if runtime.GOOS == "windows" {
				// ConPTY repaints with its own cursor and blank-cell
				// styles; compare the way the ConPTY golden test does.
				got, golden = conptyGoldenView(v.emu.Grid(-1)), conptyGoldenView(playScene(t, sc))
			}
			if got != golden {
				t.Fatalf("snapshot screen\n%s--- golden\n%s", got, golden)
			}
		}},
		{"CapRefusesASecondSession", func(t *testing.T) {
			// SWITCHBOARD_HOSTING_MAX=1: while one session is set to run, a
			// second is refused at add and at start; it starts once the
			// first is stopped.
			other := filepath.Join(r.dir, "work2")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(wire.HostedSessionCreate{Node: "local", Base: "team/worker", Profile: "team-worker", Directory: other})
			if got := adminCall(t, r.base(), "POST", "/api/admin/terminals", r.cookie, true, string(body)); got.code != http.StatusConflict || !strings.Contains(got.body, "SWITCHBOARD_HOSTING_MAX") {
				t.Fatalf("add over the cap = %d %s", got.code, got.body)
			}
			second := r.add(other, "stopped")
			if got := adminCall(t, r.base(), "PUT", "/api/admin/terminals/"+second, r.cookie, true, `{"desired":"running"}`); got.code != http.StatusConflict {
				t.Fatalf("start over the cap = %d %s", got.code, got.body)
			}
			r.setDesired(id, "stopped")
			r.wait("the first stopped", func() bool { return r.session(id).Runtime.State == "stopped" })
			r.setDesired(second, "running")
			r.wait("the second running", func() bool { h := r.session(second); return h.Runtime.State == "running" && h.Runtime.PID != 0 })
			if got := adminCall(t, r.base(), "PUT", "/api/admin/terminals/"+id, r.cookie, true, `{"desired":"running"}`); got.code != http.StatusConflict {
				t.Fatalf("restart the first over the cap = %d %s", got.code, got.body)
			}
		}},
		{"RevokedNodeIsShutOut", func(t *testing.T) {
			got := adminCall(t, r.base(), "DELETE", "/api/admin/nodes/local", r.cookie, true, "")
			var n wire.NodeView
			if err := json.Unmarshal([]byte(got.body), &n); err != nil || got.code != http.StatusOK || n.RevokedAt == nil {
				t.Fatalf("revoke = %d %s", got.code, got.body)
			}
			r.wait("the session's node offline", func() bool { return r.session(id).Runtime.State == "node offline" })
			// up refuses to start with its node revoked.
			r.stop()
			r.launch()
			select {
			case err := <-r.done:
				r.done <- err
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() == 0 || !strings.Contains(r.log(), "revoked") {
					t.Fatalf("up with a revoked node: %v\n%s", err, r.log())
				}
			case <-time.After(e2eWait):
				t.Fatalf("up kept running with its node revoked:\n%s", r.log())
			}
		}},
	}
	for _, s := range steps {
		ok := t.Run(s.name, func(st *testing.T) {
			// The rig's helpers fail the step, not the parent test.
			r.t = st
			defer func() { r.t = t }()
			s.fn(st)
		})
		if !ok {
			t.Fatalf("step %s failed; up log:\n%s", s.name, r.log())
		}
	}
}

// playScene is the emulator's grid of sc, which term's own scene test holds
// equal to the scene's golden file.
func playScene(t *testing.T, sc term.Scene) term.Grid {
	t.Helper()
	e := term.NewEmulator(sc.Cols, sc.Rows, 0)
	defer e.Close()
	for _, s := range sc.Steps {
		if s.Data == nil {
			e.Resize(s.Cols, s.Rows)
			continue
		}
		if _, err := e.Write(s.Data); err != nil {
			t.Fatal(err)
		}
	}
	return e.Grid(-1)
}

// conptyGoldenView is g's screen as the ConPTY golden test compares it
// (internal/term/conpty conptyView): a hidden cursor's position and the
// styles of blank cells are not compared, since ConPTY's repaint sets both
// its own way.
func conptyGoldenView(g term.Grid) string {
	g.History = nil
	if g.Cursor.Hidden {
		g.Cursor.X, g.Cursor.Y = 0, 0
	}
	screen := make([][]term.Cell, len(g.Screen))
	for i, row := range g.Screen {
		screen[i] = append([]term.Cell(nil), row...)
		for j, c := range screen[i] {
			a := c.Attr
			if strings.TrimSpace(c.Text) != "" || a.Reverse || a.Underline || a.Strike {
				continue
			}
			screen[i][j].Attr = term.Attr{Bg: a.Bg}
		}
	}
	g.Screen = screen
	return g.Dump()
}
