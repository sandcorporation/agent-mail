package server

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/agent-mail/internal/cli"
	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/insight"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// flowRow is a message for mailAt.
type flowRow struct {
	sender, recipient, subject, task string
	at                               time.Time
	thread, reply                    int64
	status                           string
}

func mailAt(t *testing.T, s *Server, m flowRow) int64 {
	t.Helper()
	if m.status == "" {
		m.status = "read"
	}
	if m.subject == "" {
		m.subject = "s"
	}
	res, err := s.db.Exec(`INSERT INTO messages(sender,recipient,subject,body,created_at,task_id,thread_id,reply_to,status) VALUES(?,?,?,'b',?,?,?,?,?)`,
		m.sender, m.recipient, m.subject, m.at.UTC().Format(time.RFC3339Nano), nullable(m.task), nullInt(m.thread), nullInt(m.reply), m.status)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func mustMetrics(t *testing.T, s *Server, days int) wire.MetricsReport {
	t.Helper()
	out, err := s.insight().Metrics(days)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func i64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestMetricsFirstReplyMatchesThreadAndTask(t *testing.T) {
	s, clock := slotServer(t)
	now := clock.now()
	t0 := now.Add(-48 * time.Hour)
	// Thread match: answered after 5m.
	r1 := mailAt(t, s, flowRow{sender: "a", recipient: "b", at: t0})
	mailAt(t, s, flowRow{sender: "b", recipient: "a", at: t0.Add(5 * time.Minute), thread: r1, reply: r1})
	// task_id match: a new message (no thread) with the same task, after 2h.
	mailAt(t, s, flowRow{sender: "a", recipient: "b", at: t0.Add(time.Hour), task: "T-1"})
	mailAt(t, s, flowRow{sender: "b", recipient: "a", at: t0.Add(3 * time.Hour), task: "T-1"})
	// A slot of the recipient's base answers from another number: 30s.
	r3 := mailAt(t, s, flowRow{sender: "a", recipient: "team/worker-1", at: t0.Add(2 * time.Hour)})
	mailAt(t, s, flowRow{sender: "team/worker-2", recipient: "a", at: t0.Add(2*time.Hour + 30*time.Second), thread: r3})
	// Unanswered: only the sender follows up, and a third party replies.
	r4 := mailAt(t, s, flowRow{sender: "b", recipient: "a", at: t0.Add(4 * time.Hour)})
	mailAt(t, s, flowRow{sender: "b", recipient: "a", at: t0.Add(5 * time.Hour), thread: r4})
	mailAt(t, s, flowRow{sender: "team/worker", recipient: "b", at: t0.Add(5 * time.Hour), thread: r4})
	// Not measured: system mail, and a thread started before the window.
	mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "a", at: t0})
	old := mailAt(t, s, flowRow{sender: "a", recipient: "b", at: now.Add(-8 * 24 * time.Hour)})
	mailAt(t, s, flowRow{sender: "b", recipient: "a", at: t0, thread: old})

	fr := mustMetrics(t, s, 7).FirstReply
	if fr.Count != 4 || fr.Answered != 3 || fr.Unanswered != 1 {
		t.Fatalf("counts: %+v", fr.LatencyStats)
	}
	if i64(fr.P50) != int64(300) || i64(fr.P90) != int64(7200) || i64(fr.Max) != int64(7200) {
		t.Fatalf("percentiles: p50=%v p90=%v max=%v (unanswered must not be a latency)", i64(fr.P50), i64(fr.P90), i64(fr.Max))
	}
	buckets := map[string]int{}
	for _, b := range fr.Buckets {
		buckets[b.Label] = b.Count
	}
	if !reflect.DeepEqual(buckets, map[string]int{"<10m": 2, "<1h": 0, "<4h": 1, "<1d": 0, ">=1d": 0}) {
		t.Fatalf("buckets: %v", buckets)
	}
	got := map[string]string{}
	for _, r := range fr.ByRecipient {
		got[r.Base] = strings.Join([]string{anyStr(r.Count), anyStr(r.Answered), anyStr(r.Unanswered), anyStr(i64(r.P50))}, "/")
	}
	want := map[string]string{"a": "1/0/1/<nil>", "b": "2/2/0/300", "team/worker": "1/1/0/30"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("by recipient: %v", got)
	}
}

func anyStr(v any) string {
	b, _ := json.Marshal(v)
	if s := string(b); s != "null" {
		return s
	}
	return "<nil>"
}

func TestMetricsPendingAgeExcludesSystemMail(t *testing.T) {
	s, clock := slotServer(t)
	now := clock.now()
	mailAt(t, s, flowRow{sender: "a", recipient: "b", at: now.Add(-3 * time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: "a", recipient: "b", at: now.Add(-time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "b", at: now.Add(-5 * time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: identity.LegacySystemSender, recipient: "b", at: now.Add(-6 * time.Hour), status: "pending"})
	mailAt(t, s, flowRow{sender: "b", recipient: "a", at: now.Add(-30 * time.Minute), status: "pending"})
	mailAt(t, s, flowRow{sender: "b", recipient: "a", at: now.Add(-9 * time.Hour), status: "read"})
	mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "team/worker-1", at: now.Add(-9 * time.Hour), status: "pending"})

	got := mustMetrics(t, s, 7).Pending
	want := []wire.PendingAge{
		{Role: "b", Pending: 2, OldestS: 3 * 3600, OverThreshold: true},
		{Role: "a", Pending: 1, OldestS: 1800},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending: %+v", got)
	}
	s.alerts = &insight.Thresholds{Pending: 0}
	if got = mustMetrics(t, s, 7).Pending; got[0].OverThreshold {
		t.Fatalf("threshold 0 must disable: %+v", got)
	}
}

func TestMetricsRoundTrips(t *testing.T) {
	s, clock := slotServer(t)
	t0 := clock.now().Add(-time.Hour)
	thread := func(subject string, senders ...string) int64 {
		var root int64
		for i, from := range senders {
			to := "b"
			if from == "b" {
				to = "a"
			}
			id := mailAt(t, s, flowRow{sender: from, recipient: to, subject: subject, at: t0.Add(time.Duration(i) * time.Minute), thread: root})
			if root == 0 {
				root = id
			}
			if i == 1 {
				// System mail inside the thread changes nothing.
				mailAt(t, s, flowRow{sender: identity.SystemSender, recipient: "a", at: t0.Add(time.Duration(i) * time.Minute), thread: root})
			}
		}
		return root
	}
	thread("one", "a", "b", "a", "b")                                // 3 changes: 1 round trip
	two := thread("two", "a", "b", "a", "b", "a")                    // 4 changes: 2
	three := thread("three", "a", "a", "b", "a", "b", "a", "b", "a") // 6 changes: 3

	got := mustMetrics(t, s, 7).Threads
	want := wire.ThreadMetrics{MultiRound: 2, Top: []wire.ThreadRounds{
		{ThreadID: three, Subject: "three", Parties: []string{"a", "b"}, RoundTrips: 3},
		{ThreadID: two, Subject: "two", Parties: []string{"a", "b"}, RoundTrips: 2},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("threads: %+v", got)
	}
}

type logRow struct {
	Role, State string
	Start, End  time.Time // End zero: open
}

func presenceLogRows(t *testing.T, s *Server) []logRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT role,state,started_at,COALESCE(ended_at,0) FROM presence_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var r logRow
		var start, end int64
		if err = rows.Scan(&r.Role, &r.State, &start, &end); err != nil {
			t.Fatal(err)
		}
		r.Start = fromMS(start)
		if end > 0 {
			r.End = fromMS(end)
		}
		out = append(out, r)
	}
	return out
}

func TestPresenceLogOpensAndClosesRows(t *testing.T) {
	s, clock := presenceServer(t) // lease 30s
	t0 := clock.now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	slotDo(t, s, "register", report("m", "/a", "busy", nil, ""))
	clock.add(10 * time.Second)
	slotDo(t, s, "heartbeat", report("m", "/a", "busy", nil, "")) // same state: no row
	clock.add(10 * time.Second)
	slotDo(t, s, "heartbeat", report("m", "/a", "error:StopFailure", nil, ""))
	clock.add(10 * time.Second)
	slotDo(t, s, "heartbeat", report("m", "/a", "", nil, "")) // state kept
	clock.add(31 * time.Second)                               // lease lapsed at +60s
	sweepOnce(t, s)
	sweepOnce(t, s)
	clock.add(9 * time.Second)
	slotDo(t, s, "heartbeat", report("m", "/a", "idle", nil, ""))
	clock.add(5 * time.Second)
	slotDo(t, s, "release", ident("m", "/a"))
	sweepOnce(t, s)
	want := []logRow{
		{"team/worker-1", "busy", at(0), at(20 * time.Second)},
		{"team/worker-1", "error:StopFailure", at(20 * time.Second), at(60 * time.Second)},
		{"team/worker-1", "offline", at(60 * time.Second), at(70 * time.Second)},
		{"team/worker-1", "idle", at(70 * time.Second), at(75 * time.Second)},
		{"team/worker-1", "offline", at(75 * time.Second), time.Time{}},
	}
	if got := presenceLogRows(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("presence_log:\n got %+v\nwant %+v", got, want)
	}

	// A register without a state logs unknown; a takeover moves the displaced
	// holder's open row to its new number.
	clock.add(time.Second)
	slotDo(t, s, "register", report("m", "/a", "", nil, ""))
	slotDo(t, s, "takeover", report("m", "/b", "blocked:permission_prompt", nil, ""))
	var open []string
	for _, r := range presenceLogRows(t, s) {
		if r.End.IsZero() {
			open = append(open, r.Role+"="+r.State)
		}
	}
	if want := []string{"team/worker-2=unknown", "team/worker-1=blocked:permission_prompt"}; !reflect.DeepEqual(open, want) {
		t.Fatalf("open rows after takeover: %v", open)
	}
}

func TestPresenceLogPrunedAfter60Days(t *testing.T) {
	s, clock := presenceServer(t)
	slotDo(t, s, "register", report("m", "/a", "busy", nil, ""))
	slotDo(t, s, "heartbeat", report("m", "/a", "idle", nil, ""))
	clock.add(31 * time.Second)
	sweepOnce(t, s) // idle closes, offline opens
	states := func() []string {
		var out []string
		for _, r := range presenceLogRows(t, s) {
			out = append(out, r.State)
		}
		return out
	}
	clock.add(59 * 24 * time.Hour)
	sweepOnce(t, s)
	if got := states(); !reflect.DeepEqual(got, []string{"busy", "idle", "offline"}) {
		t.Fatalf("before 60 days: %v", got)
	}
	clock.add(2 * 24 * time.Hour)
	sweepOnce(t, s) // closed rows go; the open offline row stays
	if got := states(); !reflect.DeepEqual(got, []string{"offline"}) {
		t.Fatalf("after 60 days: %v", got)
	}
}

func TestMetricsSessionStatesClippedToWindow(t *testing.T) {
	s, clock := presenceServer(t)
	now := clock.now()
	day := 24 * time.Hour
	slotDo(t, s, "register", report("m", "/a", "blocked:permission_prompt", nil, ""))
	clock.add(2 * time.Hour)
	slotDo(t, s, "heartbeat", report("m", "/a", "", nil, ""))
	now = clock.now()
	ins := func(role, state string, start, end time.Time) {
		if _, err := s.db.Exec(`INSERT INTO presence_log(base,number,role,state,started_at,ended_at) VALUES('x',9,?,?,?,?)`, role, state, ms(start), ms(end)); err != nil {
			t.Fatal(err)
		}
	}
	ins("team/worker-1", "error:StopFailure", now.Add(-8*day), now.Add(-7*day+time.Hour)) // 1h inside 7 days
	ins("team/worker-1", "error:Other", now.Add(-20*day), now.Add(-19*day))               // only in 30 days
	ins("team/worker-1", "busy", now.Add(-time.Hour), now)                                // not counted
	ins("team/worker-3", "error:StopFailure", now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))

	m := mustMetrics(t, s, 7)
	want := []wire.RoleStateTime{
		{Role: "team/worker-1", ErrorS: 3600, BlockedS: 7200, TopKinds: []wire.KindTime{{State: "blocked:permission_prompt", Seconds: 7200}, {State: "error:StopFailure", Seconds: 3600}}},
		{Role: "team/worker-3", ErrorS: 60, TopKinds: []wire.KindTime{{State: "error:StopFailure", Seconds: 60}}},
	}
	if !reflect.DeepEqual(m.Sessions.ByRole, want) {
		t.Fatalf("by role (7d): %+v", m.Sessions.ByRole)
	}
	cur := []wire.CurrentState{{Role: "team/worker-1", State: "blocked:permission_prompt", Since: now.Add(-2 * time.Hour), Seconds: 7200}}
	if !reflect.DeepEqual(m.Sessions.Current, cur) {
		t.Fatalf("current: %+v", m.Sessions.Current)
	}
	if got := mustMetrics(t, s, 30).Sessions.ByRole[0]; got.ErrorS != 25*3600+24*3600 {
		t.Fatalf("by role (30d): %+v", got)
	}
	// Offline sessions are not current, even before the sweep logs them.
	clock.add(time.Minute)
	if got := mustMetrics(t, s, 7).Sessions.Current; len(got) != 0 {
		t.Fatalf("offline session listed as current: %+v", got)
	}
}

func TestMetricsAPI(t *testing.T) {
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	s.alerts = &insight.Thresholds{Pending: time.Hour, Error: 0, Blocked: 90 * time.Second}
	if w := call(s, "GET", "/api/metrics", "secret-a", nil, ""); w.Code != 403 {
		t.Fatalf("non-boss: %d %s", w.Code, w.Body)
	}
	s.bossRoles = map[string]bool{"a": true}
	w := call(s, "GET", "/api/metrics?days=30", "secret-a", nil, "")
	if w.Code != 200 {
		t.Fatalf("boss: %d %s", w.Code, w.Body)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"window", "first_reply", "pending", "threads", "sessions", "thresholds"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("missing %q: %s", k, w.Body)
		}
	}
	for _, want := range []string{`"days":30`, `"pending_s":3600`, `"error_s":0`, `"blocked_s":90`, `"by_recipient":[]`, `"multi_round":0`, `"top":[]`, `"by_role":[]`, `"current":[]`, `"p50_s":null`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s: %s", want, w.Body)
		}
	}
	for _, bad := range []string{"0", "91", "x", "7.5"} {
		if w := call(s, "GET", "/api/metrics?days="+bad, "secret-a", nil, ""); w.Code != 400 {
			t.Fatalf("days=%s: %d", bad, w.Code)
		}
	}
	if w := call(s, "POST", "/api/metrics", "secret-a", nil, ""); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	admin := func(secret string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/admin/metrics", nil)
		if secret != "" {
			r.Header.Set("X-Agent-Mail-Admin", secret)
		}
		w := httptest.NewRecorder()
		s.api(w, r)
		return w
	}
	if w := admin(""); w.Code != 401 {
		t.Fatalf("admin without secret: %d", w.Code)
	}
	if w := admin("proxy-secret"); w.Code != 200 || !strings.Contains(w.Body.String(), `"days":7`) {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
}

func TestMetricsCommand(t *testing.T) {
	h := newCLIHarnessWith(t, func(s *Server) { s.bossRoles = map[string]bool{"b": true} })
	now := time.Now()
	r := mailAt(t, h.s, flowRow{sender: "a", recipient: "b", at: now.Add(-time.Hour)})
	mailAt(t, h.s, flowRow{sender: "b", recipient: "a", at: now.Add(-50 * time.Minute), thread: r})
	mailAt(t, h.s, flowRow{sender: "a", recipient: "b", at: now.Add(-3 * time.Hour), status: "pending"})
	h.configure(nil, "a")
	if res := h.run(nil, "metrics"); res.code == 0 || !strings.Contains(res.stderr, "403") && !strings.Contains(res.stderr, "boss") {
		t.Fatalf("non-boss metrics: %d %s", res.code, res.stderr)
	}
	h.configure(nil, "b")
	out := h.mustRun(nil, "metrics")
	for _, want := range []string{"(7 days)", "2 sent, 1 answered, 1 unanswered; p50 10m", "  b", "pending", "oldest 3h", "over 2h", "0 threads with 2+ round trips"} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, out)
		}
	}
	if out = h.mustRun(nil, "metrics", "--days", "30", "--json"); !strings.Contains(out, `"days":30`) {
		t.Fatalf("--json: %s", out)
	}
	for _, bad := range []string{"0", "91", "x"} {
		if res := h.run(nil, "metrics", "--days", bad); res.code != cli.ExitUsage {
			t.Fatalf("--days %s: %d %s", bad, res.code, res.stderr)
		}
	}
	if out = h.mustRun(nil, "help", "metrics"); !strings.Contains(out, "--days N") {
		t.Fatalf("help: %s", out)
	}
}
