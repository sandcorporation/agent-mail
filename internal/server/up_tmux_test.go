//go:build linux

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/tmux"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// upRig is a `switchboard up` process over a data directory, with real tmux
// and the test binary as the agent.
type upRig struct {
	t       *testing.T
	dir     string
	port    string
	env     []string
	cmd     *exec.Cmd
	cookie  *http.Cookie
	socket  string
	tmuxDir string
}

// newUpRig starts the rig; extraEnv is added to up's environment (and so to
// every agent it launches).
func newUpRig(t *testing.T, extraEnv ...string) *upRig {
	t.Helper()
	if _, err := tmux.Check(context.Background(), ""); err != nil {
		if os.Getenv("SWITCHBOARD_TEST_TMUX") == "require" {
			t.Fatalf("SWITCHBOARD_TEST_TMUX=require: %v", err)
		}
		t.Skipf("tmux unavailable: %v", err)
	}
	r := &upRig{t: t, dir: t.TempDir()}
	// A short socket directory keeps the unix socket path under its limit.
	d, err := os.MkdirTemp("", "sbup")
	if err != nil {
		t.Fatal(err)
	}
	r.tmuxDir = d
	r.socket = "switchboard-" + instanceFor(filepath.Join(r.dir, upNodeToken))
	t.Cleanup(func() {
		_ = r.tmux("kill-server").Run()
		_ = os.RemoveAll(d)
	})

	// The agent: this test binary as the fake agent, with its transcripts
	// in the data directory.
	agent := filepath.Join(r.dir, "agent.sh")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nSWITCHBOARD_FAKE_AGENT=1 SWITCHBOARD_FAKE_AGENT_DIR=%s SWITCHBOARD_FAKE_AGENT_SCENES=%s exec %s \"$@\"\n",
		filepath.Join(r.dir, "transcripts"), fakeagent.ScenesDir(), exe)
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	// The database up will open: a role to host and an admin to sign in.
	db, err := store.Open(filepath.Join(r.dir, upDatabase))
	if err != nil {
		t.Fatal(err)
	}
	secret := totpEncoding.EncodeToString([]byte("0123456789abcdefghij"))
	hash, err := adminauth.HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO roles(role,token,created_at) VALUES('team/worker',?,'now')`, store.TokenHash("worker-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO admin_users(name,password_hash,totp_secret,created_at) VALUES('alice',?,?,'now')`, hash, secret); err != nil {
		t.Fatal(err)
	}
	db.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.port = strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	args, _ := json.Marshal([]string{"up", "--data", r.dir, "--port", r.port})
	r.env = append(cliEnv(), "SWITCHBOARD_TEST_CLI=1", "SWITCHBOARD_TEST_ARGS="+string(args),
		"SWITCHBOARD_ADMIN_AUTH=builtin", "SWITCHBOARD_HOSTING_AGENT="+agent, "SWITCHBOARD_HOSTING_MACHINE=pc-1", "TMUX_TMPDIR="+d,
		"SWITCHBOARD_TOKEN=role-secret-of-the-operator")
	r.env = append(r.env, extraEnv...)
	r.start()

	code, err := adminauth.TOTPCode(secret, time.Now().Unix()/adminauth.TOTPStep)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", r.base()+"/auth/login", strings.NewReader(loginBody("alice", "correct horse battery", code)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Switchboard-Dashboard", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			r.cookie = c
		}
	}
	if r.cookie == nil {
		t.Fatalf("login = %d", res.StatusCode)
	}
	return r
}

// tmux runs tmux on the rig's server: the socket directory goes in the
// command's environment, not the process's, so rigs can run in parallel.
func (r *upRig) tmux(args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", append([]string{"-L", r.socket}, args...)...)
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+r.tmuxDir)
	return cmd
}

func (r *upRig) base() string { return "http://127.0.0.1:" + r.port }

// start runs up and waits for it to answer.
func (r *upRig) start() {
	r.t.Helper()
	r.cmd = exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	r.cmd.Env = r.env
	logf, err := os.OpenFile(filepath.Join(r.dir, "up.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	r.cmd.Stdout, r.cmd.Stderr = logf, logf
	if err := r.cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	cmd := r.cmd
	r.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); logf.Close() })
	r.wait("up to answer", func() bool {
		res, err := http.Get(r.base() + "/health")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
}

// stop ends up with SIGTERM, as an operator's Ctrl-C would.
func (r *upRig) stop() {
	r.t.Helper()
	_ = r.cmd.Process.Signal(syscall.SIGTERM)
	_ = r.cmd.Wait()
}

func (r *upRig) wait(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(r.dir, "up.log"))
			r.t.Fatalf("timed out waiting for %s; up log:\n%s", what, log)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *upRig) session(id string) wire.HostedSession {
	r.t.Helper()
	return terminals(r.t, r.base(), r.cookie)[id]
}

// panePID is the agent's pid as tmux sees it.
func (r *upRig) panePID(id string) string {
	out, _ := r.tmux("list-panes", "-t", "=sb-"+id+":", "-F", "#{pane_pid} #{pane_dead}").Output()
	return strings.TrimSpace(string(out))
}

func TestUpRunsAHostedSessionInTmuxAcrossRestarts(t *testing.T) {
	t.Parallel()
	r := newUpRig(t)
	work := t.TempDir()
	code, out, errOut := hostCLI(t, filepath.Join(r.dir, upDatabase), "add", "--node", "local", "--base", "team/worker", "--profile", "team-worker", "--dir", work, "--label", "e2e")
	if code != 0 {
		t.Fatalf("host add = %d %s %s", code, out, errOut)
	}
	id := strings.TrimSpace(out)
	r.wait("the session running", func() bool { return r.session(id).Runtime.State == "running" })
	ls, err := r.tmux("ls").CombinedOutput()
	if err != nil || !strings.Contains(string(ls), "sb-"+id) {
		t.Fatalf("tmux -L %s ls: %v %s", r.socket, err, ls)
	}
	pid := r.panePID(id)
	if !strings.HasSuffix(pid, " 0") || r.session(id).Runtime.PID == 0 {
		screen, _ := r.tmux("capture-pane", "-p", "-t", "=sb-"+id+":").Output()
		t.Fatalf("pane %q, runtime %+v, screen:\n%s", pid, r.session(id).Runtime, screen)
	}
	nodes := adminCall(t, r.base(), "GET", "/api/admin/nodes", r.cookie, false, "")
	if !strings.Contains(nodes.body, `"name":"local"`) || !strings.Contains(nodes.body, `"machine":"pc-1"`) {
		t.Errorf("nodes %s", nodes.body)
	}

	// Restarting up leaves the session running; the new node adopts it.
	r.stop()
	if got := r.panePID(id); got != pid {
		t.Fatalf("pane after up stopped %q, was %q", got, pid)
	}
	r.start()
	r.wait("the session adopted", func() bool { return r.session(id).Runtime.State == "running" })
	if got := r.panePID(id); got != pid || r.session(id).Runtime.Run != 1 {
		t.Fatalf("after restart pane %q (was %q), runtime %+v", got, pid, r.session(id).Runtime)
	}

	// Stop from the dashboard survives an up restart; Start resumes.
	if got := adminCall(t, r.base(), "PUT", "/api/admin/terminals/"+id, r.cookie, true, `{"desired":"stopped"}`); got.code != http.StatusOK {
		t.Fatalf("stop = %d %s", got.code, got.body)
	}
	r.wait("stopped", func() bool { return r.session(id).Runtime.State == "stopped" })
	r.stop()
	r.start()
	r.wait("still stopped after restart", func() bool { return r.session(id).Runtime.State == "stopped" })
	time.Sleep(1500 * time.Millisecond) // a reconcile pass or more
	if st := r.session(id).Runtime.State; st != "stopped" {
		t.Fatalf("after restart a stopped session is %s", st)
	}
	if got := adminCall(t, r.base(), "PUT", "/api/admin/terminals/"+id, r.cookie, true, `{"desired":"running"}`); got.code != http.StatusOK {
		t.Fatalf("start = %d %s", got.code, got.body)
	}
	r.wait("running again", func() bool { return r.session(id).Runtime.State == "running" })
	screen, _ := r.tmux("capture-pane", "-p", "-S", "-", "-t", "=sb-"+id+":").Output()
	if !strings.Contains(string(screen), "argv --resume") {
		t.Errorf("relaunch did not resume:\n%s", screen)
	}
	if strings.Contains(string(screen), "SWITCHBOARD_TOKEN") || !strings.Contains(string(screen), "env SWITCHBOARD_PROFILE=team-worker") {
		t.Errorf("child environment:\n%s", screen)
	}
}

// tmuxScreen is the session's screen and history as tmux renders it.
func (r *upRig) tmuxScreen(id string) string {
	out, _ := r.tmux("capture-pane", "-p", "-S", "-", "-t", "=sb-"+id+":").Output()
	return string(out)
}

// TestUpTerminalViewShowsTheTmuxScreen opens the browser terminal socket of a
// fake agent in tmux: the snapshot carries what it printed before anyone
// looked and the golden screen of its last draw, then the stream follows.
func TestUpTerminalViewShowsTheTmuxScreen(t *testing.T) {
	t.Parallel()
	r := newUpRig(t)
	work := t.TempDir()
	body, _ := json.Marshal(wire.HostedSessionCreate{Node: "local", Base: "team/worker", Profile: "team-worker", Directory: work, Desired: "stopped"})
	got := adminCall(t, r.base(), "POST", "/api/admin/terminals", r.cookie, true, string(body))
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	id := rec.ID
	// The size of the golden scene, so its screen compares as is.
	db, err := store.Open(filepath.Join(r.dir, upDatabase))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE hosted_sessions SET cols=24, rows=6 WHERE id=?`, id)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := adminCall(t, r.base(), "PUT", "/api/admin/terminals/"+id, r.cookie, true, `{"desired":"running"}`); got.code != http.StatusOK {
		t.Fatalf("start = %d %s", got.code, got.body)
	}
	r.wait("the agent started", func() bool {
		return r.session(id).Runtime.State == "running" && strings.Contains(r.tmuxScreen(id), "argv")
	})
	if err := r.tmux("send-keys", "-t", "=sb-"+id+":", "draw full-redraw", "Enter").Run(); err != nil {
		t.Fatal(err)
	}
	r.wait("the scene drawn", func() bool { return strings.Contains(r.tmuxScreen(id), "list the files ok") })

	tk := adminCall(t, r.base(), "POST", "/api/admin/terminals/"+id+"/ticket", r.cookie, true, `{"mode":"view"}`)
	var ticket wire.TerminalTicket
	if err := json.Unmarshal([]byte(tk.body), &ticket); err != nil {
		t.Fatalf("ticket = %d %s", tk.code, tk.body)
	}
	c, code := dialTerminal(t, r.base(), id, ticket.Ticket, r.cookie, r.base())
	if c == nil {
		t.Fatalf("socket = %d", code)
	}
	var hello wire.TerminalMessage
	if err := json.Unmarshal(readFrame(t, c, websocket.MessageText), &hello); err != nil || hello.State != "running" || hello.Cols != 24 || hello.Rows != 6 {
		t.Fatalf("hello %+v %v", hello, err)
	}
	emu := term.NewEmulator(hello.Cols, hello.Rows, 1000)
	_, _ = emu.Write(readFrame(t, c, websocket.MessageBinary))
	_, golden := scene(t, "full-redraw")
	if got := screenDump(emu.Grid(-1)); got != golden {
		t.Errorf("snapshot screen\n%s--- golden\n%s", got, golden)
	}
	if !strings.Contains(emu.Grid(-1).Dump(), "argv") {
		t.Errorf("history before the viewer attached is missing:\n%s", emu.Grid(-1).Dump())
	}

	// The stream follows: a line typed into tmux comes back as output.
	if err := r.tmux("send-keys", "-t", "=sb-"+id+":", "hi there", "Enter").Run(); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, emu, func([]wire.TerminalMessage) bool { return strings.Contains(emu.Grid(0).Dump(), "echo hi there") })
}

// TestUpTerminalInputTypesIntoTmux types into a fake agent in tmux through an
// input socket, resizes its window and finds the attach/input log without
// the typed text.
func TestUpTerminalInputTypesIntoTmux(t *testing.T) {
	t.Parallel()
	r := newUpRig(t)
	work := t.TempDir()
	body, _ := json.Marshal(wire.HostedSessionCreate{Node: "local", Base: "team/worker", Profile: "team-worker", Directory: work})
	got := adminCall(t, r.base(), "POST", "/api/admin/terminals", r.cookie, true, string(body))
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	id := rec.ID
	r.wait("the agent started", func() bool {
		return r.session(id).Runtime.State == "running" && strings.Contains(r.tmuxScreen(id), "argv")
	})

	tk := adminCall(t, r.base(), "POST", "/api/admin/terminals/"+id+"/ticket", r.cookie, true, `{"mode":"input"}`)
	var ticket wire.TerminalTicket
	if err := json.Unmarshal([]byte(tk.body), &ticket); err != nil {
		t.Fatalf("ticket = %d %s", tk.code, tk.body)
	}
	c, code := dialTerminal(t, r.base(), id, ticket.Ticket, r.cookie, r.base())
	if c == nil {
		t.Fatalf("socket = %d", code)
	}
	var hello wire.TerminalMessage
	if err := json.Unmarshal(readFrame(t, c, websocket.MessageText), &hello); err != nil || hello.Type != "hello" {
		t.Fatalf("hello %+v %v", hello, err)
	}
	emu := term.NewEmulator(hello.Cols, hello.Rows, 1000)

	const line = "typed-in-the-browser"
	typeInto(t, c, line+"\r")
	readUntil(t, c, emu, func([]wire.TerminalMessage) bool { return strings.Contains(emu.Grid(-1).Dump(), "echo "+line) })

	clientSend(t, c, wire.TerminalInput{Type: "resize", Cols: 50, Rows: 12})
	r.wait("the window resized", func() bool {
		out, _ := r.tmux("display-message", "-p", "-t", "=sb-"+id+":", "#{window_width}x#{window_height}").Output()
		return strings.TrimSpace(string(out)) == "50x12"
	})
	r.wait("the size stored", func() bool { s := r.session(id); return s.Cols == 50 && s.Rows == 12 })
	c.Close(websocket.StatusNormalClosure, "")

	db, err := store.Open(filepath.Join(r.dir, upDatabase))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r.wait("the log written", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM terminal_log WHERE session=? AND kind='detach'`, id).Scan(&n)
		return n == 1
	})
	var input int
	if err := db.QueryRow(`SELECT bytes FROM terminal_log WHERE session=? AND kind='input'`, id).Scan(&input); err != nil || input != len(line)+1 {
		t.Errorf("input row %d %v, want %d bytes", input, err, len(line)+1)
	}
	var hits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM terminal_log WHERE session LIKE ? OR admin_user LIKE ? OR kind LIKE ?`, "%"+line+"%", "%"+line+"%", "%"+line+"%").Scan(&hits); err != nil || hits != 0 {
		t.Errorf("typed text in terminal_log: %d %v", hits, err)
	}
}

// The fake agent plays the plugin with its profile: the hosted session is
// team/worker-1 in the list, /clear moves the record to the new
// conversation without a relaunch, a crash relaunches it with --resume of
// the new id under the same number, and an up restart keeps all of it.
func TestUpHostedSessionHoldsItsSlotAndFollowsClear(t *testing.T) {
	t.Parallel()
	conf := t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "team-worker.json"), []byte(`{"role":"team/worker","token":"worker-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newUpRig(t, "SWITCHBOARD_CONFIG_DIR="+conf, "SWITCHBOARD_FAKE_AGENT_HEARTBEAT=200ms")
	work := t.TempDir()
	code, out, errOut := hostCLI(t, filepath.Join(r.dir, upDatabase), "add", "--node", "local", "--base", "team/worker", "--profile", "team-worker", "--dir", work)
	if code != 0 {
		t.Fatalf("host add = %d %s %s", code, out, errOut)
	}
	id := strings.TrimSpace(out)
	online := func(sid string) func() bool {
		return func() bool {
			h := r.session(id)
			return h.Slot != nil && h.Slot.Role == "team/worker-1" && h.Presence == wire.PresenceOnline && (sid == "" || h.Slot.SessionID == sid)
		}
	}
	first := r.session(id).AgentSessionID
	r.wait("team/worker-1 online", online(first))
	pid := r.panePID(id)

	if err := r.tmux("send-keys", "-t", "=sb-"+id+":", "clear", "Enter").Run(); err != nil {
		t.Fatal(err)
	}
	r.wait("the cleared conversation followed", func() bool {
		h := r.session(id)
		return h.AgentSessionID != first && h.Slot != nil && h.Slot.SessionID == h.AgentSessionID
	})
	next := r.session(id).AgentSessionID
	if !r.session(id).Launched || r.panePID(id) != pid {
		t.Fatalf("following relaunched: pane %q (was %q), %+v", r.panePID(id), pid, r.session(id))
	}

	// A crash relaunches into the followed conversation, same number.
	if err := r.tmux("send-keys", "-t", "=sb-"+id+":", "exit 1", "Enter").Run(); err != nil {
		t.Fatal(err)
	}
	r.wait("relaunched", func() bool { return r.session(id).Runtime.Run >= 2 && r.session(id).Runtime.State == "running" })
	r.wait("team/worker-1 online again", online(next))
	if screen := r.tmuxScreen(id); !strings.Contains(screen, "argv --resume "+next) {
		t.Fatalf("relaunch did not resume %s:\n%s", next, screen)
	}

	// An up restart keeps the process, its number and its conversation.
	pid = r.panePID(id)
	r.stop()
	r.start()
	r.wait("adopted with its slot", online(next))
	if h := r.session(id); h.AgentSessionID != next || r.panePID(id) != pid {
		t.Fatalf("after up restart pane %q (was %q), %+v", r.panePID(id), pid, h)
	}
}
