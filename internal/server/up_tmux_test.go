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

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/store"
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

func newUpRig(t *testing.T) *upRig {
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
		_ = exec.Command("tmux", "-L", r.socket, "kill-server").Run()
		_ = os.RemoveAll(d)
	})
	t.Setenv("TMUX_TMPDIR", d)

	// The agent: this test binary as the fake agent, with its transcripts
	// in the data directory.
	agent := filepath.Join(r.dir, "agent.sh")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nSWITCHBOARD_FAKE_AGENT=1 SWITCHBOARD_FAKE_AGENT_DIR=%s exec %s \"$@\"\n", filepath.Join(r.dir, "transcripts"), exe)
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
	r.env = append(append(os.Environ(), clearNewEnv...), "SWITCHBOARD_TEST_CLI=1", "SWITCHBOARD_TEST_ARGS="+string(args),
		"SWITCHBOARD_ADMIN_AUTH=builtin", "SWITCHBOARD_HOSTING_AGENT="+agent, "SWITCHBOARD_HOSTING_MACHINE=pc-1", "TMUX_TMPDIR="+d,
		"SWITCHBOARD_TOKEN=role-secret-of-the-operator")
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
	out, _ := exec.Command("tmux", "-L", r.socket, "list-panes", "-t", "=sb-"+id+":", "-F", "#{pane_pid} #{pane_dead}").Output()
	return strings.TrimSpace(string(out))
}

func TestUpRunsAHostedSessionInTmuxAcrossRestarts(t *testing.T) {
	r := newUpRig(t)
	work := t.TempDir()
	code, out, errOut := hostCLI(t, filepath.Join(r.dir, upDatabase), "add", "--node", "local", "--base", "team/worker", "--profile", "team-worker", "--dir", work, "--label", "e2e")
	if code != 0 {
		t.Fatalf("host add = %d %s %s", code, out, errOut)
	}
	id := strings.TrimSpace(out)
	r.wait("the session running", func() bool { return r.session(id).Runtime.State == "running" })
	ls, err := exec.Command("tmux", "-L", r.socket, "ls").CombinedOutput()
	if err != nil || !strings.Contains(string(ls), "sb-"+id) {
		t.Fatalf("tmux -L %s ls: %v %s", r.socket, err, ls)
	}
	pid := r.panePID(id)
	if !strings.HasSuffix(pid, " 0") || r.session(id).Runtime.PID == 0 {
		screen, _ := exec.Command("tmux", "-L", r.socket, "capture-pane", "-p", "-t", "=sb-"+id+":").Output()
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
	screen, _ := exec.Command("tmux", "-L", r.socket, "capture-pane", "-p", "-S", "-", "-t", "=sb-"+id+":").Output()
	if !strings.Contains(string(screen), "argv --resume") {
		t.Errorf("relaunch did not resume:\n%s", screen)
	}
	if strings.Contains(string(screen), "SWITCHBOARD_TOKEN") || !strings.Contains(string(screen), "env SWITCHBOARD_PROFILE=team-worker") {
		t.Errorf("child environment:\n%s", screen)
	}
}
