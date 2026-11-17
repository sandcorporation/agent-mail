package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mail-flow metrics.
//
// GET /api/admin/metrics?days=N (proxy secret) and GET /api/metrics?days=N
// (boss roles only) answer the same metricsReport for the window of the last
// N days (1-90, default 7) ending now:
//
//   - first_reply: how long a thread-starting message from a non-system sender
//     waits for the first message that answers it (answers in answered.go:
//     in its thread back to the sender's base, or with its non-empty task_id
//     to the sender's base or any boss role, sent by the recipient's base).
//   - pending: per recipient role, the pending non-system mail right now.
//   - threads: direction changes between a thread's two main bases.
//   - sessions: error:* and blocked:* time from presence_log, clipped to the
//     window, and the sessions in such a state now.
//
// The alert thresholds (AGENT_MAIL_ALERT_PENDING / _ERROR / _BLOCKED, Go
// durations, 0 disables) are read by configureAlerts and reported alongside.

const (
	defaultMetricsDays = 7
	maxMetricsDays     = 90
	metricsTopThreads  = 10
	metricsTopKinds    = 3
)

// alertThresholds are the alert limits; a zero limit disables that kind.
type alertThresholds struct {
	Pending time.Duration
	Error   time.Duration
	Blocked time.Duration
}

var defaultAlertThresholds = alertThresholds{Pending: 2 * time.Hour, Error: 10 * time.Minute, Blocked: 30 * time.Minute}

// parseAlertThresholds reads AGENT_MAIL_ALERT_PENDING, AGENT_MAIL_ALERT_ERROR
// and AGENT_MAIL_ALERT_BLOCKED. Unset keeps the default; "0" disables.
func parseAlertThresholds(getenv func(string) string) (alertThresholds, error) {
	t := defaultAlertThresholds
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"ALERT_PENDING", &t.Pending},
		{"ALERT_ERROR", &t.Error},
		{"ALERT_BLOCKED", &t.Blocked},
	} {
		v, from := envAliasFrom(getenv, d.name)
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		p, err := time.ParseDuration(v)
		if err != nil || p < 0 {
			return t, fmt.Errorf("%s must be a duration such as 2h, or 0 to disable", from)
		}
		*d.dst = p
	}
	return t, nil
}

func configureAlerts(s *Server, getenv func(string) string) error {
	t, err := parseAlertThresholds(getenv)
	if err != nil {
		return err
	}
	s.alerts = &t
	return nil
}

// alertLimits are the configured thresholds, or the defaults.
func (s *Server) alertLimits() alertThresholds {
	if s.alerts == nil {
		return defaultAlertThresholds
	}
	return *s.alerts
}

// parseMetricsDays reads ?days=: empty means 7, otherwise 1-90.
func parseMetricsDays(v string) (int, error) {
	if v == "" {
		return defaultMetricsDays, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxMetricsDays {
		return 0, fmt.Errorf("days must be a whole number from 1 to %d", maxMetricsDays)
	}
	return n, nil
}

type metricsReport struct {
	Window     metricsWindow     `json:"window"`
	FirstReply firstReplyMetrics `json:"first_reply"`
	Pending    []pendingAge      `json:"pending"`
	Threads    threadMetrics     `json:"threads"`
	Sessions   sessionMetrics    `json:"sessions"`
	Thresholds thresholdSeconds  `json:"thresholds"`
	Aliases    *aliasUse         `json:"aliases,omitempty"` // admin view only
}

type metricsWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days int       `json:"days"`
}

// latencyStats summarises first-reply latencies in whole seconds. The
// percentiles are null when nothing was answered.
type latencyStats struct {
	Count      int    `json:"count"`
	Answered   int    `json:"answered"`
	Unanswered int    `json:"unanswered"`
	P50        *int64 `json:"p50_s"`
	P90        *int64 `json:"p90_s"`
	Max        *int64 `json:"max_s"`
}

type firstReplyMetrics struct {
	latencyStats
	Buckets     []latencyBucket    `json:"buckets"`
	ByRecipient []recipientLatency `json:"by_recipient"`
}

type recipientLatency struct {
	Base string `json:"base"`
	latencyStats
}

// latencyBucket counts answered latencies below UpToS (null: no upper bound)
// and at or above the previous bucket's bound.
type latencyBucket struct {
	Label string `json:"label"`
	UpToS *int64 `json:"up_to_s"`
	Count int    `json:"count"`
}

type pendingAge struct {
	Role          string `json:"role"`
	Pending       int    `json:"pending"`
	OldestS       int64  `json:"oldest_s"`
	OverThreshold bool   `json:"over_threshold"`
}

type threadMetrics struct {
	MultiRound int            `json:"multi_round"`
	Top        []threadRounds `json:"top"`
}

type threadRounds struct {
	ThreadID   int64    `json:"thread_id"`
	Subject    string   `json:"subject"`
	Parties    []string `json:"parties"`
	RoundTrips int      `json:"round_trips"`
}

type sessionMetrics struct {
	ByRole  []roleStateTime `json:"by_role"`
	Current []currentState  `json:"current"`
}

type roleStateTime struct {
	Role     string     `json:"role"`
	ErrorS   int64      `json:"error_s"`
	BlockedS int64      `json:"blocked_s"`
	TopKinds []kindTime `json:"top_kinds"`
}

type kindTime struct {
	State   string `json:"state"`
	Seconds int64  `json:"seconds"`
}

// currentState is a session that is online and in error:* or blocked:* now.
type currentState struct {
	Role    string    `json:"role"`
	State   string    `json:"state"`
	Since   time.Time `json:"since"`
	Seconds int64     `json:"seconds"`
}

type thresholdSeconds struct {
	PendingS int64 `json:"pending_s"`
	ErrorS   int64 `json:"error_s"`
	BlockedS int64 `json:"blocked_s"`
}

// metricsAPI serves GET /api/metrics (boss roles) and, with boss checked by
// the caller as true, GET /api/admin/metrics. admin adds the alias-use block.
func (s *Server) metricsAPI(w http.ResponseWriter, r *http.Request, allowed, admin bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !allowed {
		http.Error(w, "metrics are for boss roles (SWITCHBOARD_BOSS_ROLES)", http.StatusForbidden)
		return
	}
	days, err := parseMetricsDays(r.URL.Query().Get("days"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.metrics(days)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	if admin {
		a := s.aliasUse()
		out.Aliases = &a
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, out)
}

// metrics computes the report for the days ending now.
func (s *Server) metrics(days int) (metricsReport, error) {
	to := s.now().UTC()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	lim := s.alertLimits()
	out := metricsReport{
		Window: metricsWindow{From: from, To: to, Days: days},
		Thresholds: thresholdSeconds{
			PendingS: int64(lim.Pending / time.Second),
			ErrorS:   int64(lim.Error / time.Second),
			BlockedS: int64(lim.Blocked / time.Second),
		},
	}
	msgs, err := s.windowMail(from)
	if err != nil {
		return out, err
	}
	bases := newBaseResolver(s.db)
	out.FirstReply = firstReplies(msgs, bases.of, s.isBossName(bases.of))
	out.Threads = threadRoundTrips(msgs, bases.of)
	if out.Pending, err = s.pendingAges(to, lim.Pending); err != nil {
		return out, err
	}
	if out.Sessions.ByRole, err = s.sessionStateTime(from, to); err != nil {
		return out, err
	}
	out.Sessions.Current, err = s.currentSessionStates(to)
	return out, err
}

// flowMail is the part of a message the flow metrics read.
type flowMail struct {
	ID        int64
	Sender    string
	Recipient string
	Subject   string
	Created   time.Time
	TaskID    string
	ThreadID  int64
	ReplyTo   int64
}

// thread is the thread a message belongs to: its root's ID.
func (m flowMail) thread() int64 {
	if m.ThreadID != 0 {
		return m.ThreadID
	}
	return m.ID
}

// windowMail loads the mail created at or after from, oldest first.
// created_at is RFC 3339 text whose fraction length varies, so the SQL bound
// is one second early and the exact cut is made on the parsed time.
func (s *Server) windowMail(from time.Time) ([]flowMail, error) {
	rows, err := s.db.Query(`SELECT id,sender,recipient,subject,created_at,COALESCE(task_id,''),COALESCE(thread_id,0),COALESCE(reply_to,0) FROM messages WHERE created_at>=? ORDER BY id`,
		from.Add(-time.Second).UTC().Format("2006-01-02T15:04:05"))
	if err != nil {
		return nil, err
	}
	all, err := scanFlowMail(rows)
	if err != nil {
		return nil, err
	}
	var out []flowMail
	for _, m := range all {
		if !m.Created.Before(from) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, rows.Err()
}

// baseResolver maps a mailbox name to its base, caching lookups. Names that
// no longer resolve fall back to the part before -N, or the name itself.
type baseResolver struct {
	q     queryer
	cache map[string]string
}

func newBaseResolver(q queryer) *baseResolver {
	return &baseResolver{q: q, cache: map[string]string{}}
}

func (b *baseResolver) of(name string) string {
	if v, ok := b.cache[name]; ok {
		return v
	}
	v := baseOf(b.q, name)
	if v == "" {
		v = name
		if base, _, ok := splitSlot(name); ok {
			v = base
		}
	}
	b.cache[name] = v
	return v
}

var latencyBucketBounds = []struct {
	label string
	upTo  time.Duration // 0: no upper bound
}{
	{"<10m", 10 * time.Minute},
	{"<1h", time.Hour},
	{"<4h", 4 * time.Hour},
	{"<1d", 24 * time.Hour},
	{">=1d", 0},
}

// firstReplies measures thread-starting mail in msgs (sorted oldest first):
// no thread_id or reply_to, not from the system, and not carrying a task_id
// an earlier message in the window already carried.
func firstReplies(msgs []flowMail, base func(string) string, isBoss func(string) bool) firstReplyMetrics {
	byThread, byTask := map[int64][]flowMail{}, map[string][]flowMail{}
	for _, m := range msgs {
		byThread[m.thread()] = append(byThread[m.thread()], m)
		if m.TaskID != "" {
			byTask[m.TaskID] = append(byTask[m.TaskID], m)
		}
	}
	var all []int64
	answered := map[string][]int64{}
	counts := map[string]*latencyStats{}
	taskSeen := map[string]bool{}
	for _, root := range msgs {
		// A message with a task already under way continues that task.
		continued := root.TaskID != "" && taskSeen[root.TaskID]
		if root.TaskID != "" {
			taskSeen[root.TaskID] = true
		}
		if isSystemSender(root.Sender) || root.ThreadID != 0 || root.ReplyTo != 0 || continued {
			continue
		}
		to := base(root.Recipient)
		var first *flowMail
		candidates := byThread[root.ID]
		if root.TaskID != "" {
			candidates = append(append([]flowMail{}, candidates...), byTask[root.TaskID]...)
		}
		for i := range candidates {
			if m := candidates[i]; answers(root, m, base, isBoss) && (first == nil || m.Created.Before(first.Created) || (m.Created.Equal(first.Created) && m.ID < first.ID)) {
				first = &candidates[i]
			}
		}
		c := counts[to]
		if c == nil {
			c = &latencyStats{}
			counts[to] = c
		}
		c.Count++
		if first == nil {
			c.Unanswered++
			continue
		}
		c.Answered++
		d := int64(first.Created.Sub(root.Created) / time.Second)
		answered[to] = append(answered[to], d)
		all = append(all, d)
	}
	out := firstReplyMetrics{latencyStats: latencySummary(all), ByRecipient: []recipientLatency{}}
	for _, c := range counts {
		out.Unanswered += c.Unanswered
	}
	out.Count = out.Answered + out.Unanswered
	for _, b := range latencyBucketBounds {
		lb := latencyBucket{Label: b.label}
		if b.upTo > 0 {
			v := int64(b.upTo / time.Second)
			lb.UpToS = &v
		}
		out.Buckets = append(out.Buckets, lb)
	}
	for _, d := range all {
		for i, b := range latencyBucketBounds {
			if b.upTo == 0 || d < int64(b.upTo/time.Second) {
				out.Buckets[i].Count++
				break
			}
		}
	}
	for to, c := range counts {
		st := latencySummary(answered[to])
		st.Count, st.Unanswered = c.Count, c.Unanswered
		out.ByRecipient = append(out.ByRecipient, recipientLatency{Base: to, latencyStats: st})
	}
	sort.Slice(out.ByRecipient, func(i, j int) bool { return out.ByRecipient[i].Base < out.ByRecipient[j].Base })
	return out
}

// latencySummary fills Answered and the nearest-rank percentiles of ds.
func latencySummary(ds []int64) latencyStats {
	st := latencyStats{Count: len(ds), Answered: len(ds)}
	if len(ds) == 0 {
		return st
	}
	s := append([]int64{}, ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := func(p float64) *int64 {
		v := s[int(math.Ceil(p*float64(len(s))))-1]
		return &v
	}
	st.P50, st.P90, st.Max = rank(0.5), rank(0.9), rank(1)
	return st
}

// threadRoundTrips counts direction changes between each thread's two main
// bases (the two that appear most, by sender and recipient) over msgs.
func threadRoundTrips(msgs []flowMail, base func(string) string) threadMetrics {
	byThread := map[int64][]flowMail{}
	var order []int64
	for _, m := range msgs {
		if isSystemSender(m.Sender) || isSystemSender(m.Recipient) {
			continue
		}
		if _, ok := byThread[m.thread()]; !ok {
			order = append(order, m.thread())
		}
		byThread[m.thread()] = append(byThread[m.thread()], m)
	}
	out := threadMetrics{Top: []threadRounds{}}
	var multi []threadRounds
	for _, id := range order {
		ms := byThread[id]
		seen := map[string]int{}
		for _, m := range ms {
			seen[base(m.Sender)]++
			seen[base(m.Recipient)]++
		}
		parties := make([]string, 0, len(seen))
		for b := range seen {
			parties = append(parties, b)
		}
		if len(parties) < 2 {
			continue
		}
		sort.Slice(parties, func(i, j int) bool {
			if seen[parties[i]] != seen[parties[j]] {
				return seen[parties[i]] > seen[parties[j]]
			}
			return parties[i] < parties[j]
		})
		a, b := parties[0], parties[1]
		changes, last := 0, ""
		for _, m := range ms {
			from, to := base(m.Sender), base(m.Recipient)
			if !(from == a && to == b) && !(from == b && to == a) {
				continue
			}
			if last != "" && from != last {
				changes++
			}
			last = from
		}
		if rt := changes / 2; rt >= 2 {
			multi = append(multi, threadRounds{ThreadID: id, Subject: ms[0].Subject, Parties: []string{a, b}, RoundTrips: rt})
		}
	}
	out.MultiRound = len(multi)
	sort.SliceStable(multi, func(i, j int) bool {
		if multi[i].RoundTrips != multi[j].RoundTrips {
			return multi[i].RoundTrips > multi[j].RoundTrips
		}
		return multi[i].ThreadID > multi[j].ThreadID
	})
	if len(multi) > metricsTopThreads {
		multi = multi[:metricsTopThreads]
	}
	out.Top = append(out.Top, multi...)
	return out
}

// pendingAges lists, per recipient role, the pending mail not sent by the
// system as of now, oldest first. over is the pending threshold (0: never).
func (s *Server) pendingAges(now time.Time, over time.Duration) ([]pendingAge, error) {
	rows, err := s.db.Query(`SELECT m.recipient,m.created_at FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient WHERE m.sender NOT IN (?,?) AND COALESCE(rs.status,m.status,'pending')='pending'`, systemSender, legacySystemSender)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRole := map[string]*pendingAge{}
	for rows.Next() {
		var role, created string
		if err = rows.Scan(&role, &created); err != nil {
			return nil, err
		}
		p := byRole[role]
		if p == nil {
			p = &pendingAge{Role: role}
			byRole[role] = p
		}
		p.Pending++
		if t, e := time.Parse(time.RFC3339Nano, created); e == nil {
			if age := int64(now.Sub(t) / time.Second); age > p.OldestS {
				p.OldestS = age
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []pendingAge{}
	for _, p := range byRole {
		p.OverThreshold = over > 0 && p.OldestS >= int64(over/time.Second)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OldestS != out[j].OldestS {
			return out[i].OldestS > out[j].OldestS
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

func isErrorState(state string) bool   { return strings.HasPrefix(state, "error:") }
func isBlockedState(state string) bool { return strings.HasPrefix(state, "blocked:") }

// sessionStateTime sums each role's error:* and blocked:* time within
// [from, to); a row still open counts up to to.
func (s *Server) sessionStateTime(from, to time.Time) ([]roleStateTime, error) {
	rows, err := s.db.Query(`SELECT role,state,started_at,COALESCE(ended_at,0) FROM presence_log WHERE (state LIKE 'error:%' OR state LIKE 'blocked:%') AND started_at<? AND (ended_at IS NULL OR ended_at>?)`, ms(to), ms(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		roleStateTime
		kinds map[string]int64
	}
	byRole := map[string]*acc{}
	for rows.Next() {
		var role, state string
		var start, end int64
		if err = rows.Scan(&role, &state, &start, &end); err != nil {
			return nil, err
		}
		if end == 0 || end > ms(to) {
			end = ms(to)
		}
		start = max(start, ms(from))
		if end <= start {
			continue
		}
		a := byRole[role]
		if a == nil {
			a = &acc{roleStateTime: roleStateTime{Role: role}, kinds: map[string]int64{}}
			byRole[role] = a
		}
		a.kinds[state] += end - start
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []roleStateTime{}
	for _, a := range byRole {
		a.TopKinds = []kindTime{}
		var errMS, blockMS int64
		for state, d := range a.kinds {
			if isErrorState(state) {
				errMS += d
			} else {
				blockMS += d
			}
			a.TopKinds = append(a.TopKinds, kindTime{State: state, Seconds: d / 1000})
		}
		a.ErrorS, a.BlockedS = errMS/1000, blockMS/1000
		sort.Slice(a.TopKinds, func(i, j int) bool {
			if a.TopKinds[i].Seconds != a.TopKinds[j].Seconds {
				return a.TopKinds[i].Seconds > a.TopKinds[j].Seconds
			}
			return a.TopKinds[i].State < a.TopKinds[j].State
		})
		if len(a.TopKinds) > metricsTopKinds {
			a.TopKinds = a.TopKinds[:metricsTopKinds]
		}
		out = append(out, a.roleStateTime)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].ErrorS+out[i].BlockedS, out[j].ErrorS+out[j].BlockedS
		if ti != tj {
			return ti > tj
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

// currentSessionStates lists the online sessions whose current state is
// error:* or blocked:*, longest first.
func (s *Server) currentSessionStates(now time.Time) ([]currentState, error) {
	rows, err := s.db.Query(`SELECT p.role,p.state,p.started_at FROM presence_log p JOIN slots s ON s.base=p.base AND s.number=p.number WHERE p.ended_at IS NULL AND (p.state LIKE 'error:%' OR p.state LIKE 'blocked:%') AND s.lease_until>? ORDER BY p.started_at,p.role`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []currentState{}
	for rows.Next() {
		var c currentState
		var start int64
		if err = rows.Scan(&c.Role, &c.State, &start); err != nil {
			return nil, err
		}
		c.Since = fromMS(start)
		c.Seconds = max(0, int64(now.Sub(c.Since)/time.Second))
		out = append(out, c)
	}
	return out, rows.Err()
}
