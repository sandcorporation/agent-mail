package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// hostedRig is one hosted session on a node over the fake backend (machine
// pc-1), with the server's clock in the test's hands. plugin plays the
// switchboard plugin inside the session: it registers, heartbeats and
// releases the session's slot over HTTP, as the real one does.
type hostedRig struct {
	t      *testing.T
	s      *Server
	url    string
	clock  *fakeClock
	fake   *termtest.Fake
	id     string
	token  string // the base role's token
	base   string
	folder string
}

const hostedMachine = "pc-1" // attachNode's hello

// newHostedRig turns hosting on for s (served at url, s.api), adds a hosted
// session of base in folder and waits until it runs with its conversation
// marked launched.
func newHostedRig(t *testing.T, s *Server, url string, clock *fakeClock, base, token, folder string) *hostedRig {
	t.Helper()
	s.hosting = true
	if s.admin.ProxyToken == "" {
		s.admin.ProxyToken = "proxy-secret"
	}
	nt := nodeToken(t, s, "pc")
	rec, err := s.hostingService().Create(wire.HostedSessionCreate{Node: "pc", Base: base, Profile: "p", Directory: folder})
	if err != nil {
		t.Fatal(err)
	}
	r := &hostedRig{t: t, s: s, url: url, clock: clock, fake: termtest.New(), id: rec.ID, token: token, base: base, folder: folder}
	attachNode(t, url, nt, r.fake)
	r.sync()
	eventually(t, "the hosted session running", func() bool { return r.runtime().State == "running" })
	eventually(t, "launched persisted", func() bool { return r.record().Launched })
	return r
}

func (r *hostedRig) sync() {
	r.t.Helper()
	if err := r.s.hostingHub().Sync(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *hostedRig) record() wire.HostedSession {
	r.t.Helper()
	h, err := r.s.hostingService().Get(r.id)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *hostedRig) call(method, path string, headers map[string]string, body any) (int, []byte) {
	r.t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req, err := http.NewRequest(method, r.url+path, &b)
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

// listed is the session as GET /api/admin/terminals shows it.
func (r *hostedRig) listed() wire.HostedSession {
	r.t.Helper()
	headers := map[string]string{"X-Switchboard-Admin": "proxy-secret"}
	for k, v := range dashboardHeaders {
		headers[k] = v
	}
	code, body := r.call("GET", "/api/admin/terminals", headers, nil)
	var list []wire.HostedSession
	if err := json.Unmarshal(body, &list); err != nil || code != 200 {
		r.t.Fatalf("terminals = %d %s", code, body)
	}
	for _, h := range list {
		if h.ID == r.id {
			return h
		}
	}
	r.t.Fatalf("terminals lack %s: %s", r.id, body)
	return wire.HostedSession{}
}

func (r *hostedRig) runtime() wire.HostedRuntime {
	r.t.Helper()
	list, err := r.s.hostingHub().List()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, h := range list {
		if h.ID == r.id {
			return h.Runtime
		}
	}
	return wire.HostedRuntime{}
}

// plugin calls /api/slots/<action> as the session's plugin would, at dir
// (the folder or a session-marked form of it) with conversation sid.
func (r *hostedRig) plugin(action, role, dir, sid string) wire.SlotGrant {
	r.t.Helper()
	code, body := r.call("POST", "/api/slots/"+action, map[string]string{"Authorization": "Bearer " + r.token, "X-Switchboard-Role": role},
		wire.SlotRequest{Machine: hostedMachine, Directory: dir, SessionID: sid})
	if code != 200 {
		r.t.Fatalf("%s %s %s: %d %s", action, role, dir, code, body)
	}
	var g wire.SlotGrant
	_ = json.Unmarshal(body, &g)
	return g
}

// clear is /clear inside the session: the plugin ends the old lease and
// registers the new conversation at dir.
func (r *hostedRig) clear(g wire.SlotGrant, dir, sid string) wire.SlotGrant {
	r.t.Helper()
	r.plugin("release", g.Role, g.Directory, "")
	return r.plugin("register", r.base, dir, sid)
}

func (r *hostedRig) launchArgs() []string { return launchArgs(r.fake) }

// restart relaunches the session with resume (the Restart button) and
// waits for the new launch.
func (r *hostedRig) restart() {
	r.t.Helper()
	before := len(r.fake.Launches())
	if _, err := r.s.hostingHub().Restart(r.id); err != nil {
		r.t.Fatal(err)
	}
	eventually(r.t, "the relaunch", func() bool { return len(r.fake.Launches()) == before+1 })
	eventually(r.t, "running again", func() bool { return r.runtime().State == "running" })
}

func hostedSlotServer(t *testing.T) (*Server, string, *fakeClock) {
	t.Helper()
	s, clock := slotServer(t)
	ts := httptest.NewServer(secureHeaders(http.HandlerFunc(s.api)))
	t.Cleanup(ts.Close)
	return s, ts.URL, clock
}

// The hosted session's plugin registers team/worker-1; the list shows the
// numbered role and presence, a relaunch resumes and gets the same number,
// and a lapsed lease with the process alive shows "offline (process alive)"
// without the node touching the process.
func TestHostedSessionShowsItsSlotAndKeepsItsNumber(t *testing.T) {
	t.Parallel()
	s, url, clock := hostedSlotServer(t)
	r := newHostedRig(t, s, url, clock, "team/worker", workerToken, "/work/app")
	if h := r.listed(); h.Slot != nil || h.Presence != wire.PresenceProcessAlive {
		t.Fatalf("before the plugin registers: slot %+v presence %q", h.Slot, h.Presence)
	}
	sid := r.record().AgentSessionID
	g := r.plugin("register", "team/worker", "/work/app", sid)
	if g.Role != "team/worker-1" {
		t.Fatalf("registered %s", g.Role)
	}
	h := r.listed()
	if h.Slot == nil || h.Slot.Role != "team/worker-1" || h.Slot.SessionID != sid || !h.Slot.Online || h.Presence != wire.PresenceOnline {
		t.Fatalf("listed slot %+v presence %q", h.Slot, h.Presence)
	}

	// A relaunch resumes the same conversation and gets the same number.
	clock.add(time.Second)
	r.restart()
	if l := r.launchArgs(); l[len(l)-1] != "sb-"+r.id+" --resume "+sid {
		t.Fatalf("launches %q", l)
	}
	if again := r.plugin("register", "team/worker", "/work/app", sid); again.Role != "team/worker-1" {
		t.Fatalf("after relaunch registered %s", again.Role)
	}
	r.sync()
	if got := r.record().AgentSessionID; got != sid {
		t.Fatalf("relaunch changed the conversation to %s", got)
	}

	// The lease lapses while the process lives: offline (process alive),
	// left running.
	launches, stops := len(r.fake.Launches()), len(r.fake.Stops())
	clock.add(s.presenceCfg.SlotLease + time.Second)
	r.sync()
	h = r.listed()
	if h.Presence != wire.PresenceProcessAlive || h.Slot == nil || h.Slot.Online || h.Slot.Role != "team/worker-1" || h.Runtime.State != "running" {
		t.Fatalf("lapsed lease: slot %+v presence %q runtime %+v", h.Slot, h.Presence, h.Runtime)
	}
	time.Sleep(100 * time.Millisecond) // a few node reconciles
	if len(r.fake.Launches()) != launches || len(r.fake.Stops()) != stops {
		t.Fatalf("the node acted on a lapsed lease: launches %q stops %+v", r.launchArgs(), r.fake.Stops())
	}

	// Stopped: offline.
	if _, err := s.hostingService().SetDesired(r.id, "stopped"); err != nil {
		t.Fatal(err)
	}
	r.sync()
	eventually(t, "stopped", func() bool { return r.runtime().State == "stopped" })
	if h := r.listed(); h.Presence != wire.PresenceOffline {
		t.Fatalf("stopped presence %q", h.Presence)
	}
}

// /clear inside the session: the plugin reports a new conversation, the host
// adopts it without relaunching, and the next relaunch resumes it. The
// plugin's session-marked directory "<dir> #<8 hex>" is the same folder.
// A slot the previous process left behind is never followed.
func TestHostedSessionFollowsTheConversation(t *testing.T) {
	t.Parallel()
	s, url, clock := hostedSlotServer(t)
	r := newHostedRig(t, s, url, clock, "team/worker", workerToken, "/work/app")
	first := r.record().AgentSessionID
	g := r.plugin("register", "team/worker", "/work/app", first)
	launches, stops := len(r.fake.Launches()), len(r.fake.Stops())

	// Not a conversation id: ignored.
	g = r.clear(g, "/work/app", "not-a-uuid")
	r.sync()
	if got := r.record().AgentSessionID; got != first {
		t.Fatalf("followed an invalid id: %s", got)
	}

	second := "22222222-2222-4222-8222-222222222222"
	g = r.clear(g, "/work/app", second)
	r.sync()
	if h := r.record(); h.AgentSessionID != second || !h.Launched {
		t.Fatalf("after clear: %+v", h)
	}
	time.Sleep(100 * time.Millisecond) // a few node reconciles
	if len(r.fake.Launches()) != launches || len(r.fake.Stops()) != stops {
		t.Fatalf("following relaunched: launches %q stops %+v", r.launchArgs(), r.fake.Stops())
	}
	if h := r.listed(); h.Slot == nil || h.Slot.SessionID != second || h.Slot.Role != "team/worker-1" {
		t.Fatalf("listed slot %+v", h.Slot)
	}
	clock.add(time.Second)
	r.restart()
	if l := r.launchArgs(); l[len(l)-1] != "sb-"+r.id+" --resume "+second {
		t.Fatalf("relaunch after clear: %q", l)
	}
	g = r.plugin("register", "team/worker", "/work/app", second)

	// The plugin's session-marked directory matches the folder.
	third := "33333333-3333-4333-8333-333333333333"
	g = r.clear(g, "/work/app #33333333", third)
	r.sync()
	if got := r.record().AgentSessionID; got != third {
		t.Fatalf("session-marked directory not followed: %s", got)
	}
	if h := r.listed(); h.Slot == nil || h.Slot.Directory != "/work/app #33333333" || h.Slot.SessionID != third {
		t.Fatalf("listed slot %+v", h.Slot)
	}
	// Another folder that only starts with the same path never matches.
	r.plugin("register", "team/worker", "/work/apple", "44444444-4444-4444-8444-444444444444")
	r.sync()
	if got := r.record().AgentSessionID; got != third {
		t.Fatalf("followed another folder: %s", got)
	}

	// New conversation: the old process's slot (still leased, its last
	// heartbeat before the new run) is never followed back.
	clock.add(time.Second)
	renewed, err := r.s.hostingHub().NewConversation(r.id)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new conversation launched", func() bool { return r.record().Launched && r.runtime().State == "running" })
	eventually(t, "the new run reported", func() bool {
		l := r.launchArgs()
		return l[len(l)-1] == "sb-"+r.id+" --session-id "+renewed.AgentSessionID
	})
	r.sync()
	if got := r.record().AgentSessionID; got != renewed.AgentSessionID {
		t.Fatalf("followed the previous process's slot back to %s", got)
	}
	if sl := r.listed().Slot; sl == nil || sl.SessionID != third {
		t.Fatalf("slot %+v", sl)
	}
}
