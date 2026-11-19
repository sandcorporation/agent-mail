package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// termRig is a hosting server over real HTTP with a signed-in admin, one
// record of size 24x6 (the full-redraw scene) and a node over a fake backend
// running it.
type termRig struct {
	s      *Server
	base   string
	cookie *http.Cookie
	now    *time.Time
	secret string
	fake   *termtest.Fake
	id     string
}

func newTermRig(t *testing.T, limits hosting.Admin) *termRig {
	t.Helper()
	s, secret, now := builtinServer(t)
	s.hosting = true
	s.termLimits = limits
	r := &termRig{s: s, now: now, secret: secret}
	r.cookie = r.login(t, 0)
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	r.base = srv.URL
	token := nodeToken(t, s, "pc")
	got := adminCall(t, r.base, "POST", "/api/admin/terminals", r.cookie, true, newRecord)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	r.id = rec.ID
	if _, err := s.db.Exec(`UPDATE hosted_sessions SET cols=24, rows=6 WHERE id=?`, r.id); err != nil {
		t.Fatal(err)
	}
	r.fake = termtest.New()
	attachNode(t, r.base, token, r.fake)
	eventually(t, "the record running", func() bool { return terminals(t, r.base, r.cookie)[r.id].Runtime.State == "running" })
	return r
}

// login signs alice in with the TOTP code of step offset (a code is good once).
func (r *termRig) login(t *testing.T, offset int64) *http.Cookie {
	t.Helper()
	return sessionFrom(t, doAuth(r.s, authReq{method: "POST", path: "/auth/login", body: loginBody("alice", "correct horse battery", codeAt(t, r.secret, *r.now, offset))}))
}

// secondLogin signs alice in again, a TOTP step later (a code is good once).
func (r *termRig) secondLogin(t *testing.T) *http.Cookie {
	t.Helper()
	*r.now = r.now.Add(time.Duration(adminauth.TOTPStep) * time.Second)
	return r.login(t, 0)
}

func (r *termRig) name() string { return node.TerminalName(r.id) }

// ticket asks for a view ticket as cookie.
func (r *termRig) ticket(t *testing.T, cookie *http.Cookie, id string) string {
	t.Helper()
	got := adminCall(t, r.base, "POST", "/api/admin/terminals/"+id+"/ticket", cookie, true, `{"mode":"view"}`)
	var tk wire.TerminalTicket
	if err := json.Unmarshal([]byte(got.body), &tk); err != nil || tk.Ticket == "" {
		t.Fatalf("ticket = %d %s", got.code, got.body)
	}
	return tk.Ticket
}

// dial opens the terminal socket; code is the HTTP status of a refusal.
func (r *termRig) dial(t *testing.T, id, ticket string, cookie *http.Cookie, origin string) (*websocket.Conn, int) {
	t.Helper()
	return dialTerminal(t, r.base, id, ticket, cookie, origin)
}

func dialTerminal(t *testing.T, base, id, ticket string, cookie *http.Cookie, origin string) (*websocket.Conn, int) {
	t.Helper()
	h := http.Header{}
	if cookie != nil {
		h.Set("Cookie", cookie.Name+"="+cookie.Value)
	}
	if origin != "" {
		h.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(base, "http") + "/api/admin/terminals/" + id + "/socket?ticket=" + url.QueryEscape(ticket)
	c, res, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if res == nil {
			t.Fatalf("dial: %v", err)
		}
		return nil, res.StatusCode
	}
	t.Cleanup(func() { c.CloseNow() })
	c.SetReadLimit(16 << 20)
	return c, http.StatusSwitchingProtocols
}

// open dials with a fresh ticket and the right origin and reads the hello.
func (r *termRig) open(t *testing.T) (*websocket.Conn, wire.TerminalMessage) {
	t.Helper()
	c, code := r.dial(t, r.id, r.ticket(t, r.cookie, r.id), r.cookie, r.base)
	if c == nil {
		t.Fatalf("socket refused: %d", code)
	}
	var hello wire.TerminalMessage
	if err := json.Unmarshal(readFrame(t, c, websocket.MessageText), &hello); err != nil || hello.Type != "hello" {
		t.Fatalf("hello %+v: %v", hello, err)
	}
	return c, hello
}

func readFrame(t *testing.T, c *websocket.Conn, want websocket.MessageType) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != want {
		t.Fatalf("frame type %v %q, want %v", typ, b, want)
	}
	return b
}

// readUntil feeds binary frames to emu and collects text frames until done
// is true of the texts so far.
func readUntil(t *testing.T, c *websocket.Conn, emu *term.Emulator, done func(texts []wire.TerminalMessage) bool) (texts []wire.TerminalMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !done(texts) {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageText {
			var m wire.TerminalMessage
			_ = json.Unmarshal(b, &m)
			texts = append(texts, m)
			continue
		}
		_, _ = emu.Write(b)
	}
	return texts
}

func scene(t *testing.T, name string) (term.Scene, string) {
	t.Helper()
	raw, err := os.ReadFile("../term/testdata/scenes/" + name + ".vt")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := term.ParseScene(raw)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("../term/testdata/scenes/" + name + ".golden")
	if err != nil {
		t.Fatal(err)
	}
	return sc, string(golden)
}

// screenDump is g's dump without its history: what a golden scene drawn
// after earlier output must match.
func screenDump(g term.Grid) string {
	g.History = nil
	return g.Dump()
}

func TestTerminalTicketAndSocketRefusals(t *testing.T) {
	r := newTermRig(t, hosting.Admin{})
	path := "/api/admin/terminals/" + r.id + "/ticket"
	if got := adminCall(t, r.base, "POST", path, nil, true, `{"mode":"view"}`); got.code != http.StatusUnauthorized {
		t.Errorf("ticket anonymous = %d", got.code)
	}
	if got := adminCall(t, r.base, "POST", path, r.cookie, false, `{"mode":"view"}`); got.code != http.StatusForbidden {
		t.Errorf("ticket without the write guard = %d", got.code)
	}
	if got := adminCall(t, r.base, "POST", path, r.cookie, true, `{"mode":"type"}`); got.code != http.StatusBadRequest {
		t.Errorf("ticket bad mode = %d", got.code)
	}
	if got := adminCall(t, r.base, "GET", path, r.cookie, false, ""); got.code != http.StatusMethodNotAllowed {
		t.Errorf("ticket GET = %d", got.code)
	}
	if got := adminCall(t, r.base, "POST", "/api/admin/terminals/hs_00000000000000000000000000000000/ticket", r.cookie, true, `{"mode":"view"}`); got.code != http.StatusNotFound {
		t.Errorf("ticket for an unknown record = %d", got.code)
	}

	for _, c := range []struct {
		name   string
		ticket func() string
		cookie *http.Cookie
		origin string
		want   int
	}{
		{"anonymous", func() string { return r.ticket(t, r.cookie, r.id) }, nil, r.base, http.StatusUnauthorized},
		{"bad origin", func() string { return r.ticket(t, r.cookie, r.id) }, r.cookie, "http://elsewhere.example", http.StatusForbidden},
		{"no origin", func() string { return r.ticket(t, r.cookie, r.id) }, r.cookie, "", http.StatusForbidden},
		{"missing ticket", func() string { return "" }, r.cookie, r.base, http.StatusForbidden},
		{"made-up ticket", func() string { return "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }, r.cookie, r.base, http.StatusForbidden},
		{"other sign-in's ticket", func() string { return r.ticket(t, r.secondLogin(t), r.id) }, r.cookie, r.base, http.StatusForbidden},
	} {
		if conn, code := r.dial(t, r.id, c.ticket(), c.cookie, c.origin); conn != nil || code != c.want {
			t.Errorf("%s: socket = %d, want %d", c.name, code, c.want)
		}
	}

	// A ticket for another record is refused there.
	other, err := r.s.hostingService().Create(wire.HostedSessionCreate{Node: "pc", Base: "b", Profile: "team-b", Directory: "/work/b"})
	if err != nil {
		t.Fatal(err)
	}
	if conn, code := r.dial(t, other.ID, r.ticket(t, r.cookie, r.id), r.cookie, r.base); conn != nil || code != http.StatusForbidden {
		t.Errorf("other record's ticket = %d", code)
	}

	// Single use.
	tk := r.ticket(t, r.cookie, r.id)
	conn, code := r.dial(t, r.id, tk, r.cookie, r.base)
	if conn == nil {
		t.Fatalf("good ticket refused: %d", code)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	if conn, code := r.dial(t, r.id, tk, r.cookie, r.base); conn != nil || code != http.StatusForbidden {
		t.Errorf("reused ticket = %d", code)
	}

	// Valid 30 s.
	tk = r.ticket(t, r.cookie, r.id)
	*r.now = r.now.Add(hosting.TicketTTL)
	if conn, code := r.dial(t, r.id, tk, r.cookie, r.base); conn != nil || code != http.StatusForbidden {
		t.Errorf("expired ticket = %d", code)
	}
	tk = r.ticket(t, r.cookie, r.id)
	*r.now = r.now.Add(hosting.TicketTTL - time.Second)
	if conn, _ := r.dial(t, r.id, tk, r.cookie, r.base); conn == nil {
		t.Errorf("ticket refused a second before it expires")
	}
}

func TestTerminalSocketSnapshotThenStream(t *testing.T) {
	r := newTermRig(t, hosting.Admin{})
	sc, golden := scene(t, "full-redraw")
	// Printed while nobody watched: no stream is open, so nothing crosses
	// the node channel.
	if err := r.fake.Output(r.name(), []byte("seen by nobody\r\n")); err != nil {
		t.Fatal(err)
	}
	for _, st := range sc.Steps {
		if err := r.fake.Output(r.name(), st.Data); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.fake.Streams(r.name()); n != 0 {
		t.Fatalf("%d streams open without a viewer", n)
	}

	c, hello := r.open(t)
	if hello.State != "running" || hello.Cols != 24 || hello.Rows != 6 {
		t.Fatalf("hello %+v", hello)
	}
	snap := readFrame(t, c, websocket.MessageBinary)
	emu := term.NewEmulator(hello.Cols, hello.Rows, 100)
	_, _ = emu.Write(snap)
	g := emu.Grid(-1)
	if got := screenDump(g); got != golden {
		t.Errorf("snapshot screen\n%s--- golden\n%s", got, golden)
	}
	if !strings.Contains(g.Dump(), "seen by nobody") {
		t.Errorf("history lost:\n%s", g.Dump())
	}

	// Then the stream, from where the snapshot left off.
	eventually(t, "the node streaming", func() bool { return r.fake.Streams(r.name()) == 1 })
	if err := r.fake.Output(r.name(), []byte("\x1b[5;1Hlive line")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, emu, func([]wire.TerminalMessage) bool { return strings.Contains(emu.Grid(0).Dump(), "|live line") })
	if want, _ := r.fake.Screen(context.Background(), r.name(), 0); screenDump(emu.Grid(0)) != screenDump(want) {
		t.Errorf("viewer\n%s--- backend\n%s", screenDump(emu.Grid(0)), screenDump(want))
	}

	// A second viewer gets its own snapshot; both keep streaming.
	c2, _ := r.open(t)
	emu2 := term.NewEmulator(24, 6, 100)
	_, _ = emu2.Write(readFrame(t, c2, websocket.MessageBinary))
	if !strings.Contains(emu2.Grid(0).Dump(), "|live line") {
		t.Errorf("second viewer's snapshot:\n%s", emu2.Grid(0).Dump())
	}
	if n := r.fake.Streams(r.name()); n != 1 {
		t.Errorf("two viewers opened %d streams", n)
	}

	// The session exits: viewers see the state, and the last screen.
	if err := r.fake.Output(r.name(), []byte("\x1b[5;1Hbye")); err != nil {
		t.Fatal(err)
	}
	if err := r.fake.Exit(r.name(), 3); err != nil {
		t.Fatal(err)
	}
	readUntil(t, c, emu, func(texts []wire.TerminalMessage) bool {
		left := false
		for _, m := range texts {
			left = left || (m.Type == "state" && m.State != "running")
		}
		return strings.Contains(emu.Grid(0).Dump(), "|bye") && left
	})

	// The last viewer leaving stops the stream on the node.
	c.Close(websocket.StatusNormalClosure, "")
	c2.Close(websocket.StatusNormalClosure, "")
	eventually(t, "no stream after the viewers left", func() bool { return r.fake.Streams(r.name()) == 0 })
}

func TestTerminalSocketClosesOnSignOut(t *testing.T) {
	r := newTermRig(t, hosting.Admin{Recheck: 50 * time.Millisecond})
	c, _ := r.open(t)
	if w := doAuth(r.s, authReq{method: "POST", path: "/auth/logout", cookie: r.cookie}); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", w.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue
		}
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("closed with %v, want signed out", err)
		}
		return
	}
}

func TestTerminalSlowViewerIsDroppedAndReconnects(t *testing.T) {
	r := newTermRig(t, hosting.Admin{Queue: 4})
	slow, _ := r.open(t)
	eventually(t, "the node streaming", func() bool { return r.fake.Streams(r.name()) == 1 })
	// The slow viewer reads nothing while far more than its queue and the
	// socket buffers is printed.
	line := []byte(strings.Repeat("x", 23) + "\r\n")
	chunk := []byte(strings.Repeat(string(line), 4096/len(line)))
	for i := 0; i < 2048; i++ {
		if err := r.fake.Output(r.name(), chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.fake.Output(r.name(), []byte("\x1b[2J\x1b[Hlatest")); err != nil {
		t.Fatal(err)
	}
	// It has been dropped: its socket ends, and with no viewer left the node
	// stops streaming.
	eventually(t, "the stream stopped", func() bool { return r.fake.Streams(r.name()) == 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = slow.Read(ctx)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow viewer still open")
	}
	// Reconnecting gets a fresh snapshot of the latest screen.
	c, _ := r.open(t)
	emu := term.NewEmulator(24, 6, 10)
	_, _ = emu.Write(readFrame(t, c, websocket.MessageBinary))
	if !strings.Contains(emu.Grid(0).Dump(), "|latest") {
		t.Errorf("snapshot after reconnect:\n%s", emu.Grid(0).Dump())
	}
}

func TestTerminalAssetsServedOnlyWithHosting(t *testing.T) {
	for _, on := range []bool{false, true} {
		base, cookie := signedIn(t, on)
		me := send(t, base, "GET", "/api/admin/me", cookie, nil)
		page := send(t, base, "GET", "/", cookie, nil)
		if strings.Contains(page.body, "/assets/") || strings.Contains(strings.ToLower(page.body), "xterm") {
			t.Errorf("hosting %v: the page references the terminal assets", on)
		}
		for _, p := range []string{"/assets/xterm/xterm.js", "/assets/xterm/xterm.css", "/assets/xterm/addon-fit.js", "/assets/xterm/LICENSE.xterm"} {
			got := send(t, base, "GET", p, cookie, nil)
			if !on {
				if want := send(t, base, "GET", "/nowhere/x.js", cookie, nil); got != want {
					t.Errorf("hosting off: %s = %d %q, unknown path %d %q", p, got.code, got.body, want.code, want.body)
				}
				continue
			}
			if got.code != http.StatusOK || len(got.body) < 500 {
				t.Errorf("hosting on: %s = %d (%d bytes)", p, got.code, len(got.body))
			}
		}
		if on != strings.Contains(me.body, `"terminal_assets":["/assets/xterm/xterm.css","/assets/xterm/xterm.js","/assets/xterm/addon-fit.js"]`) {
			t.Errorf("hosting %v: me = %s", on, me.body)
		}
	}
	base, cookie := signedIn(t, true)
	for _, p := range []string{"/assets/", "/assets/xterm", "/assets/xterm/", "/assets/../server.go", "/assets/xterm/nope.js"} {
		if got := send(t, base, "GET", p, cookie, nil); got.code != http.StatusNotFound {
			t.Errorf("%s = %d", p, got.code)
		}
	}
	// The CSP is the same on the page and the assets: nothing from elsewhere.
	for _, p := range []string{"/", "/assets/xterm/xterm.js"} {
		req, _ := http.NewRequest("GET", base+p, nil)
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if csp := res.Header.Get("Content-Security-Policy"); csp != "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'" {
			t.Errorf("%s CSP %q", p, csp)
		}
		if p != "/" && res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Errorf("%s type %q", p, res.Header.Get("Content-Type"))
		}
	}
}
