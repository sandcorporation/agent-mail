package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// fakeClock is a settable clock for lease tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const workerToken = "secret-worker"

// slotServer is testServer plus a base role team/worker and a fake clock.
func slotServer(t *testing.T) (*Server, *fakeClock) {
	t.Helper()
	s := testServer(t)
	clock := &fakeClock{t: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	s.clock = clock.now
	s.presenceCfg.SlotLease = time.Minute
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/worker',?,'now')`, workerToken); err != nil {
		t.Fatal(err)
	}
	return s, clock
}

// callAs calls the API with an explicit X-Switchboard-Role header.
func callAs(s *Server, method, path, token, role string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Switchboard-Role", role)
	w := httptest.NewRecorder()
	s.api(w, r)
	return w
}

func ident(machine, dir string) wire.SlotRequest {
	return wire.SlotRequest{Machine: machine, Directory: dir}
}

func slotDo(t *testing.T, s *Server, action string, req wire.SlotRequest) wire.SlotGrant {
	t.Helper()
	w := callAs(s, "POST", "/api/slots/"+action, workerToken, "team/worker", req)
	if w.Code != 200 {
		t.Fatalf("%s %+v: %d %s", action, req, w.Code, w.Body)
	}
	var g wire.SlotGrant
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func slotTable(t *testing.T, s *Server) map[string]string {
	t.Helper()
	w := callAs(s, "GET", "/api/slots", workerToken, "team/worker-7", nil)
	if w.Code != 200 {
		t.Fatalf("slots: %d %s", w.Code, w.Body)
	}
	var out []wire.SlotView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, v := range out {
		m[v.Role] = v.Machine + ":" + v.Directory
	}
	return m
}

func inboxSubjects(t *testing.T, s *Server, role string) []string {
	t.Helper()
	w := callAs(s, "GET", "/api/messages", workerToken, role, nil)
	if w.Code != 200 {
		t.Fatalf("inbox %s: %d %s", role, w.Code, w.Body)
	}
	var ms []wire.Message
	_ = json.Unmarshal(w.Body.Bytes(), &ms)
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Subject)
	}
	return out
}

func TestSlotConcurrentRegistrationNeverSharesANumber(t *testing.T) {
	t.Parallel()
	s, _ := slotServer(t)
	const n = 24
	var wg sync.WaitGroup
	got := make([]int, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := callAs(s, "POST", "/api/slots/register", workerToken, "team/worker", ident("host", "/w/"+itoa(int64(i))))
			codes[i] = w.Code
			var g wire.SlotGrant
			_ = json.Unmarshal(w.Body.Bytes(), &g)
			got[i] = g.Number
		}(i)
	}
	wg.Wait()
	seen := map[int]bool{}
	for i, num := range got {
		if codes[i] != 200 || num < 1 || num > n || seen[num] {
			t.Fatalf("registration %d: code=%d number=%d (numbers %v)", i, codes[i], num, got)
		}
		seen[num] = true
	}
	// The same identity registering concurrently always lands on one number.
	same := make([]int, 8)
	for i := range same {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := callAs(s, "POST", "/api/slots/register", workerToken, "team/worker", ident("other", "/same"))
			var g wire.SlotGrant
			_ = json.Unmarshal(w.Body.Bytes(), &g)
			same[i] = g.Number
		}(i)
	}
	wg.Wait()
	for _, num := range same {
		if num != n+1 {
			t.Fatalf("same identity numbers %v, want all %d", same, n+1)
		}
	}
}

func TestSlotSameIdentityReclaimsOthersGetNextFree(t *testing.T) {
	t.Parallel()
	s, clock := slotServer(t)
	a := slotDo(t, s, "register", ident("m1", "/a"))
	b := slotDo(t, s, "register", ident("m1", "/b"))
	if a.Role != "team/worker-1" || a.Number != 1 || a.Reclaimed || b.Role != "team/worker-2" || b.OtherActive != 1 {
		t.Fatalf("first registrations: %+v / %+v", a, b)
	}
	if want := clock.now().Add(time.Minute); !a.LeaseExpiresAt.Equal(want) || a.LeaseSeconds != 60 {
		t.Fatalf("lease: %v %d", a.LeaseExpiresAt, a.LeaseSeconds)
	}
	// Reconnect before the lease ends: same number, lease extended.
	clock.add(30 * time.Second)
	again := slotDo(t, s, "register", wire.SlotRequest{Machine: "m1", Directory: "/a", SessionID: "s-2", Label: "issue-9", Parent: "team/lead"})
	if again.Number != 1 || !again.Reclaimed || again.SessionID != "s-2" || again.Label != "issue-9" || again.Parent != "team/lead" || !again.LeaseExpiresAt.Equal(clock.now().Add(time.Minute)) {
		t.Fatalf("reclaim: %+v", again)
	}
	// Same directory on another machine is a different identity.
	if c := slotDo(t, s, "register", ident("m2", "/a")); c.Number != 3 || c.OtherActive != 2 {
		t.Fatalf("other machine: %+v", c)
	}
	// Release frees the number for the next newcomer.
	slotDo(t, s, "release", ident("m1", "/b"))
	if d := slotDo(t, s, "register", ident("m3", "/d")); d.Number != 2 {
		t.Fatalf("after release: %+v", d)
	}
	// The released identity's number was taken, so it gets a new one.
	if b2 := slotDo(t, s, "register", ident("m1", "/b")); b2.Number != 4 || b2.Reclaimed {
		t.Fatalf("released identity: %+v", b2)
	}
	// After expiry an identity whose number nobody took gets it back.
	clock.add(2 * time.Minute)
	if a3 := slotDo(t, s, "register", ident("m1", "/a")); a3.Number != 1 || !a3.Reclaimed || a3.OtherActive != 0 {
		t.Fatalf("reclaim after expiry: %+v", a3)
	}
	for _, bad := range []wire.SlotRequest{{Machine: "m"}, {Directory: "/d"}, {Machine: "m", Directory: strings.Repeat("x", 513)}} {
		if w := callAs(s, "POST", "/api/slots/register", workerToken, "team/worker", bad); w.Code != 400 {
			t.Fatalf("bad identity %+v: %d", bad, w.Code)
		}
	}
	if w := callAs(s, "POST", "/api/slots/heartbeat", workerToken, "team/worker", ident("nobody", "/x")); w.Code != 404 {
		t.Fatalf("heartbeat without slot: %d", w.Code)
	}
}

func TestSlotUnnumberedAddressIsSlotOne(t *testing.T) {
	t.Parallel()
	s, _ := slotServer(t)
	send := func(to, subject string) {
		t.Helper()
		if w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": to, "subject": subject, "body": "x"}, ""); w.Code != 201 {
			t.Fatalf("send to %s: %d %s", to, w.Code, w.Body)
		}
	}
	send("team/worker", "to-base")
	send("team/worker-1", "to-one")
	send("team/worker-2", "to-two")
	send("team/worker-5", "to-five") // nobody holds 5: the mail waits
	if w := call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "team/other-1", "subject": "x"}, ""); w.Code != 403 {
		t.Fatalf("unknown base: %d", w.Code)
	}
	// A client that never registers (header = base) and the -1 session see the same mail.
	for _, role := range []string{"team/worker", "team/worker-1"} {
		if got := strings.Join(inboxSubjects(t, s, role), ","); got != "to-one,to-base" {
			t.Fatalf("%s inbox: %s", role, got)
		}
	}
	if got := strings.Join(inboxSubjects(t, s, "team/worker-2"), ","); got != "to-two" {
		t.Fatalf("-2 inbox: %s", got)
	}
	// Read state is shared: -1 acknowledges, the base client sees it.
	if w := callAs(s, "POST", "/api/messages/1/ack", workerToken, "team/worker-1", nil); w.Code != 200 {
		t.Fatalf("ack via -1: %d %s", w.Code, w.Body)
	}
	if w := callAs(s, "GET", "/api/messages?status=acknowledged", workerToken, "team/worker", nil); !strings.Contains(w.Body.String(), "to-base") {
		t.Fatalf("base sees ack: %s", w.Body)
	}
	// Mailboxes of one base stay apart: -1 (or the base header) gets a clear
	// 403 naming the role to use, with no body and no state change.
	for _, path := range []string{"/api/messages/3", "/api/messages/3/ack", "/api/messages/3/read", "/api/messages/3/complete", "/api/messages/3/reply"} {
		for _, role := range []string{"team/worker-1", "team/worker"} {
			w := callAs(s, "POST", path, workerToken, role, map[string]any{"body": "x"})
			if strings.HasSuffix(path, "/3") {
				w = callAs(s, "GET", path, workerToken, role, nil)
			}
			if w.Code != 403 || !strings.Contains(w.Body.String(), "team/worker-2") || strings.Contains(w.Body.String(), "to-two") {
				t.Fatalf("%s as %s on -2 mail: %d %s", path, role, w.Code, w.Body)
			}
		}
	}
	if w := callAs(s, "GET", "/api/messages?status=pending", workerToken, "team/worker-2", nil); !strings.Contains(w.Body.String(), "to-two") {
		t.Fatalf("-2 mail changed state: %s", w.Body)
	}
	if w := callAs(s, "GET", "/api/messages/3", workerToken, "team/worker-2", nil); w.Code != 200 || decodeMessage(t, w.Body.Bytes()).Status != "read" {
		t.Fatalf("read as -2: %d %s", w.Code, w.Body)
	}
	// Mail of another base stays 404.
	if w := callAs(s, "GET", "/api/messages/3", "secret-b", "b", nil); w.Code != 404 {
		t.Fatalf("other base: %d", w.Code)
	}
	// A reply from -1 goes out as the base, so answers come back to slot 1.
	w := callAs(s, "POST", "/api/messages", workerToken, "team/worker-1", map[string]any{"recipient": "a", "subject": "hi"})
	if m := decodeMessage(t, w.Body.Bytes()); w.Code != 201 || m.Sender != "team/worker" {
		t.Fatalf("send from -1: %d %s", w.Code, w.Body)
	}
	if w := callAs(s, "POST", "/api/messages", workerToken, "team/worker-2", map[string]any{"recipient": "a", "subject": "hi"}); decodeMessage(t, w.Body.Bytes()).Sender != "team/worker-2" {
		t.Fatalf("send from -2: %s", w.Body)
	}
}

func TestSlotEventsWakeOnTheSharedMailbox(t *testing.T) {
	t.Parallel()
	s, _ := slotServer(t)
	ts := httptest.NewServer(http.HandlerFunc(s.api))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := make(chan pollResult, 2)
	go func() { res <- poll(t, ctx, ts.URL, workerToken, "team/worker-1", "?timeout=5", nil) }()
	go func() { res <- poll(t, ctx, ts.URL, workerToken, "team/worker-2", "?timeout=1", nil) }()
	waitSubscribers(t, s, "team/worker", 1)
	waitSubscribers(t, s, "team/worker-2", 1)
	id := sendMail(t, s, "secret-a", "team/worker", "wake")
	for i := 0; i < 2; i++ {
		r := <-res
		ids := pollIDs(r)
		switch {
		case r.status != 200:
			t.Fatalf("poll: %d %s", r.status, r.body)
		case len(ids) == 1 && ids[0] == id:
		case len(ids) == 0 && r.resp.Cursor == 0: // -2 timed out without the base mail
		default:
			t.Fatalf("poll result %v", ids)
		}
	}
	// SSE: a -1 stream gets base mail as a backlog frame.
	st := openEvents(t, ts.URL, workerToken, "team/worker-1", nil, "")
	ids, _ := st.untilReady(t)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("sse backlog for -1: %v", ids)
	}
}

func TestSlotTakeover(t *testing.T) {
	t.Parallel()
	s, clock := slotServer(t)
	slotDo(t, s, "register", ident("m", "/a")) // 1
	slotDo(t, s, "register", ident("m", "/b")) // 2
	slotDo(t, s, "register", ident("m", "/c")) // 3
	g := slotDo(t, s, "takeover", ident("m", "/c"))
	if g.Number != 1 || g.Displaced == nil || g.Displaced.Role != "team/worker-3" || g.Displaced.Directory != "/a" {
		t.Fatalf("takeover: %+v displaced=%+v", g, g.Displaced)
	}
	want := map[string]string{"team/worker-1": "m:/c", "team/worker-2": "m:/b", "team/worker-3": "m:/a"}
	if got := slotTable(t, s); len(got) != 3 || got["team/worker-1"] != want["team/worker-1"] || got["team/worker-2"] != want["team/worker-2"] || got["team/worker-3"] != want["team/worker-3"] {
		t.Fatalf("slot table after takeover: %v", got)
	}
	// The displaced session learns its new role from its next heartbeat.
	if h := slotDo(t, s, "heartbeat", ident("m", "/a")); h.Role != "team/worker-3" {
		t.Fatalf("displaced heartbeat: %+v", h)
	}
	// Taking over again as the holder is a plain renewal.
	if g := slotDo(t, s, "takeover", ident("m", "/c")); g.Number != 1 || !g.Reclaimed || g.Displaced != nil {
		t.Fatalf("repeat takeover: %+v", g)
	}
	// An expired holder of 1 is dropped rather than moved.
	clock.add(2 * time.Minute)
	slotDo(t, s, "heartbeat", ident("m", "/b"))
	if g := slotDo(t, s, "takeover", ident("m", "/b")); g.Number != 1 || g.Displaced != nil {
		t.Fatalf("takeover from expired holder: %+v", g)
	}
	if got := slotTable(t, s); len(got) != 1 || got["team/worker-1"] != "m:/b" {
		t.Fatalf("slot table: %v", got)
	}
}

func TestSlotPrefixTokenRules(t *testing.T) {
	t.Parallel()
	s, _ := slotServer(t)
	// A separately registered role whose name ends in -2 keeps its own token.
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('team/lead','secret-lead','now'),('team/lead-2','secret-lead-two','now')`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, role string
		code        int
	}{
		{workerToken, "team/worker", 200},
		{workerToken, "team/worker-1", 200},
		{workerToken, "team/worker-42", 200}, // any number under the base, leased or not
		{"secret-b", "team/worker-2", 401},   // another base's token
		{workerToken, "b-2", 401},
		{workerToken, "team/worker-0", 401},
		{workerToken, "team/worker-01", 401},
		{workerToken, "team/worker-1000", 401},
		{workerToken, "team/worker-x", 401},
		{workerToken, "team/nobody-1", 401},
		{"wrong", "team/worker-3", 401},
		{"secret-lead", "team/lead-2", 401}, // exact row wins over the prefix
		{"secret-lead-two", "team/lead-2", 200},
		{"secret-lead", "team/lead-3", 200},
	} {
		if w := callAs(s, "GET", "/api/messages", tc.token, tc.role, nil); w.Code != tc.code {
			t.Fatalf("%s as %s: %d, want %d", tc.token, tc.role, w.Code, tc.code)
		}
	}
	// Mail to team/lead-2 belongs to the separate role, and registration skips that number.
	call(s, "POST", "/api/messages", "secret-a", map[string]any{"recipient": "team/lead-2", "subject": "exact"}, "")
	if w := callAs(s, "GET", "/api/messages", "secret-lead-two", "team/lead-2", nil); !strings.Contains(w.Body.String(), "exact") {
		t.Fatalf("exact role inbox: %s", w.Body)
	}
	for _, want := range []string{"team/lead-1", "team/lead-3"} {
		w := callAs(s, "POST", "/api/slots/register", "secret-lead", "team/lead", ident("m", want))
		var g wire.SlotGrant
		_ = json.Unmarshal(w.Body.Bytes(), &g)
		if g.Role != want {
			t.Fatalf("lead registration: %d %s, want %s", w.Code, w.Body, want)
		}
	}
	// The slot table shows only the caller's base.
	if got := slotTable(t, s); len(got) != 0 {
		t.Fatalf("worker slot table leaks other bases: %v", got)
	}
}

// The number is freed, but the ended session's unread mail is held for it,
// not handed to the next holder of the number (#354).
func TestSlotLeaseExpiryFreesSlotAndHoldsMail(t *testing.T) {
	t.Parallel()
	s, clock := slotServer(t)
	slotDo(t, s, "register", ident("m", "/a"))
	slotDo(t, s, "register", ident("m", "/b"))
	sendMail(t, s, "secret-a", "team/worker-2", "queued")
	clock.add(45 * time.Second)
	slotDo(t, s, "heartbeat", ident("m", "/a"))
	clock.add(30 * time.Second) // /b's lease (60s) has run out; /a's has not
	if got := slotTable(t, s); len(got) != 1 || got["team/worker-1"] != "m:/a" {
		t.Fatalf("slot table after expiry: %v", got)
	}
	c := slotDo(t, s, "register", ident("m", "/c"))
	if c.Number != 2 || c.Reclaimed {
		t.Fatalf("expired number reused: %+v", c)
	}
	if got := strings.Join(inboxSubjects(t, s, "team/worker-2"), ","); got != "" {
		t.Fatalf("mail for the freed slot reached the next holder: %s", got)
	}
	// /b's row is gone, so its heartbeat asks it to register again.
	if w := callAs(s, "POST", "/api/slots/heartbeat", workerToken, "team/worker-2", ident("m", "/b")); w.Code != 404 {
		t.Fatalf("heartbeat of a slot taken after expiry: %d", w.Code)
	}
}

func TestSlotAdminTableIsProxyOnly(t *testing.T) {
	t.Parallel()
	s, _ := slotServer(t)
	s.admin.ProxyToken = "proxy-secret"
	slotDo(t, s, "register", ident("m", "/a"))
	for secret, code := range map[string]int{"": 401, "proxy-secret": 200} {
		r := httptest.NewRequest("GET", "/api/admin/slots", nil)
		if secret != "" {
			r.Header.Set("X-Switchboard-Admin", secret)
		}
		w := httptest.NewRecorder()
		s.api(w, r)
		if w.Code != code || code == 200 && !strings.Contains(w.Body.String(), `"role":"team/worker-1"`) {
			t.Fatalf("admin slots with %q: %d %s", secret, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), workerToken) {
			t.Fatal("token leaked")
		}
	}
}

func TestSlotSchemaOnLegacyDatabase(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", testDSN(filepath.Join(t.TempDir(), "legacy.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err = db.Exec(legacySchema + `INSERT INTO roles VALUES('team/worker','` + workerToken + `','now'); INSERT INTO messages(sender,recipient,subject,body,created_at) VALUES('a','team/worker','before slots','x','2026-01-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = store.Init(db); err != nil {
			t.Fatalf("schema run %d: %v", i, err)
		}
	}
	s := &Server{db: db, tokens: map[string]string{}}
	for _, role := range []string{"team/worker", "team/worker-1"} {
		if got := strings.Join(inboxSubjects(t, s, role), ","); got != "before slots" {
			t.Fatalf("%s sees old mail: %q", role, got)
		}
	}
	if g := slotDo(t, s, "register", ident("m", "/a")); g.Role != "team/worker-1" {
		t.Fatalf("register on migrated db: %+v", g)
	}
}

func TestSlotCommands(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	h.configure(nil, "b")
	out := h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/a", "--label", "issue-1")
	if !strings.HasPrefix(out, "b-1  m:/a  until ") || !strings.Contains(out, "label=issue-1") || !strings.Contains(out, "(0 other active)") {
		t.Fatalf("slot register: %q", out)
	}
	h.mustRun(nil, "slot", "register", "--machine", "m", "--directory", "/b")
	out = h.mustRun(nil, "slot", "takeover", "--machine", "m", "--directory", "/b")
	if !strings.HasPrefix(out, "b-1  m:/b") || !strings.Contains(out, "moved previous holder: b-2  m:/a") {
		t.Fatalf("slot takeover: %q", out)
	}
	if out = h.mustRun(nil, "slots"); !strings.Contains(out, "b-1  m:/b") || !strings.Contains(out, "b-2  m:/a") {
		t.Fatalf("slots: %q", out)
	}
	if out = h.mustRun(nil, "slots", "--json"); !strings.Contains(out, `"role":"b-2"`) {
		t.Fatalf("slots --json: %q", out)
	}
	if out = h.mustRun(nil, "slot", "heartbeat", "--machine", "m", "--directory", "/a", "--json"); !strings.Contains(out, `"number":2`) {
		t.Fatalf("slot heartbeat --json: %q", out)
	}
	h.mustRun(nil, "slot", "release", "--machine", "m", "--directory", "/a")
	if out = h.mustRun(nil, "slots"); strings.Contains(out, "b-2") {
		t.Fatalf("after release: %q", out)
	}
	if r := h.run(nil, "slot", "steal"); r.code != cli.ExitUsage {
		t.Fatalf("unknown action: %d %s", r.code, r.stderr)
	}
}
