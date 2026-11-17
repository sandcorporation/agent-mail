package insight

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/config"
	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/mail"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Mail-flow metrics.
//
// GET /api/admin/metrics?days=N (proxy secret) and GET /api/metrics?days=N
// (boss roles only) answer the same wire.MetricsReport for the window of the last
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
// durations, 0 disables) are read by ParseThresholds and reported alongside.

const (
	metricsTopThreads = 10
	metricsTopKinds   = 3
)

// Thresholds are the alert limits; a zero limit disables that kind.
type Thresholds struct {
	Pending time.Duration
	Error   time.Duration
	Blocked time.Duration
}

var DefaultThresholds = Thresholds{Pending: 2 * time.Hour, Error: 10 * time.Minute, Blocked: 30 * time.Minute}

// ParseThresholds reads AGENT_MAIL_ALERT_PENDING, AGENT_MAIL_ALERT_ERROR
// and AGENT_MAIL_ALERT_BLOCKED. Unset keeps the default; "0" disables.
func ParseThresholds(getenv func(string) string) (Thresholds, error) {
	t := DefaultThresholds
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{"ALERT_PENDING", &t.Pending},
		{"ALERT_ERROR", &t.Error},
		{"ALERT_BLOCKED", &t.Blocked},
	} {
		v, from := config.EnvAliasFrom(getenv, d.name)
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

// Limits are the configured thresholds, or the defaults.
func (s *Service) Limits() Thresholds {
	if s.alerts == nil {
		return DefaultThresholds
	}
	return *s.alerts
}

// MetricsAPI serves GET /api/metrics (boss roles) and, with boss checked by
// the caller as true, GET /api/admin/metrics. admin adds the alias-use block.
func (s *Service) MetricsAPI(w http.ResponseWriter, r *http.Request, allowed, admin bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !allowed {
		http.Error(w, "metrics are for boss roles (SWITCHBOARD_BOSS_ROLES)", http.StatusForbidden)
		return
	}
	days, err := wire.ParseMetricsDays(r.URL.Query().Get("days"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.Metrics(days)
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

// Metrics computes the report for the days ending now.
func (s *Service) Metrics(days int) (wire.MetricsReport, error) {
	to := s.now().UTC()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	lim := s.Limits()
	out := wire.MetricsReport{
		Window: wire.MetricsWindow{From: from, To: to, Days: days},
		Thresholds: wire.ThresholdSeconds{
			PendingS: int64(lim.Pending / time.Second),
			ErrorS:   int64(lim.Error / time.Second),
			BlockedS: int64(lim.Blocked / time.Second),
		},
	}
	msgs, err := s.windowMail(from)
	if err != nil {
		return out, err
	}
	bases := NewBaseResolver(s.db)
	out.FirstReply = firstReplies(msgs, bases.Of, s.ids.IsBossName(bases.Of))
	out.Threads = threadRoundTrips(msgs, bases.Of)
	if out.Pending, err = s.pendingAges(to, lim.Pending); err != nil {
		return out, err
	}
	if out.Sessions.ByRole, err = s.sessionStateTime(from, to); err != nil {
		return out, err
	}
	out.Sessions.Current, err = s.currentSessionStates(to)
	return out, err
}

// windowMail loads the mail created at or after from, oldest first.
// created_at is RFC 3339 text whose fraction length varies, so the SQL bound
// is one second early and the exact cut is made on the parsed time.
func (s *Service) windowMail(from time.Time) ([]mail.FlowMail, error) {
	rows, err := s.db.Query(`SELECT id,sender,recipient,subject,created_at,COALESCE(task_id,''),COALESCE(thread_id,0),COALESCE(reply_to,0) FROM messages WHERE created_at>=? ORDER BY id`,
		from.Add(-time.Second).UTC().Format("2006-01-02T15:04:05"))
	if err != nil {
		return nil, err
	}
	all, err := mail.ScanFlowMail(rows)
	if err != nil {
		return nil, err
	}
	var out []mail.FlowMail
	for _, m := range all {
		if !m.Created.Before(from) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, rows.Err()
}

// BaseResolver maps a mailbox name to its base, caching lookups. Names that
// no longer resolve fall back to the part before -N, or the name itself.
type BaseResolver struct {
	q     identity.Queryer
	cache map[string]string
}

func NewBaseResolver(q identity.Queryer) *BaseResolver {
	return &BaseResolver{q: q, cache: map[string]string{}}
}

func (b *BaseResolver) Of(name string) string {
	if v, ok := b.cache[name]; ok {
		return v
	}
	v := identity.BaseOf(b.q, name)
	if v == "" {
		v = name
		if base, _, ok := wire.SplitSlot(name); ok {
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
func firstReplies(msgs []mail.FlowMail, base func(string) string, isBoss func(string) bool) wire.FirstReplyMetrics {
	byThread, byTask := map[int64][]mail.FlowMail{}, map[string][]mail.FlowMail{}
	for _, m := range msgs {
		byThread[m.Thread()] = append(byThread[m.Thread()], m)
		if m.TaskID != "" {
			byTask[m.TaskID] = append(byTask[m.TaskID], m)
		}
	}
	var all []int64
	answered := map[string][]int64{}
	counts := map[string]*wire.LatencyStats{}
	taskSeen := map[string]bool{}
	for _, root := range msgs {
		// A message with a task already under way continues that task.
		continued := root.TaskID != "" && taskSeen[root.TaskID]
		if root.TaskID != "" {
			taskSeen[root.TaskID] = true
		}
		if identity.IsSystemSender(root.Sender) || root.ThreadID != 0 || root.ReplyTo != 0 || continued {
			continue
		}
		to := base(root.Recipient)
		var first *mail.FlowMail
		candidates := byThread[root.ID]
		if root.TaskID != "" {
			candidates = append(append([]mail.FlowMail{}, candidates...), byTask[root.TaskID]...)
		}
		for i := range candidates {
			if m := candidates[i]; mail.Answers(root, m, base, isBoss) && (first == nil || m.Created.Before(first.Created) || (m.Created.Equal(first.Created) && m.ID < first.ID)) {
				first = &candidates[i]
			}
		}
		c := counts[to]
		if c == nil {
			c = &wire.LatencyStats{}
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
	out := wire.FirstReplyMetrics{LatencyStats: latencySummary(all), ByRecipient: []wire.RecipientLatency{}}
	for _, c := range counts {
		out.Unanswered += c.Unanswered
	}
	out.Count = out.Answered + out.Unanswered
	for _, b := range latencyBucketBounds {
		lb := wire.LatencyBucket{Label: b.label}
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
		out.ByRecipient = append(out.ByRecipient, wire.RecipientLatency{Base: to, LatencyStats: st})
	}
	sort.Slice(out.ByRecipient, func(i, j int) bool { return out.ByRecipient[i].Base < out.ByRecipient[j].Base })
	return out
}

// latencySummary fills Answered and the nearest-rank percentiles of ds.
func latencySummary(ds []int64) wire.LatencyStats {
	st := wire.LatencyStats{Count: len(ds), Answered: len(ds)}
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
func threadRoundTrips(msgs []mail.FlowMail, base func(string) string) wire.ThreadMetrics {
	byThread := map[int64][]mail.FlowMail{}
	var order []int64
	for _, m := range msgs {
		if identity.IsSystemSender(m.Sender) || identity.IsSystemSender(m.Recipient) {
			continue
		}
		if _, ok := byThread[m.Thread()]; !ok {
			order = append(order, m.Thread())
		}
		byThread[m.Thread()] = append(byThread[m.Thread()], m)
	}
	out := wire.ThreadMetrics{Top: []wire.ThreadRounds{}}
	var multi []wire.ThreadRounds
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
			multi = append(multi, wire.ThreadRounds{ThreadID: id, Subject: ms[0].Subject, Parties: []string{a, b}, RoundTrips: rt})
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
func (s *Service) pendingAges(now time.Time, over time.Duration) ([]wire.PendingAge, error) {
	rows, err := s.db.Query(`SELECT m.recipient,m.created_at FROM messages m LEFT JOIN role_states rs ON rs.message_id=m.id AND rs.role=m.recipient WHERE m.sender NOT IN (?,?) AND COALESCE(rs.status,m.status,'pending')='pending'`, identity.SystemSender, identity.LegacySystemSender)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRole := map[string]*wire.PendingAge{}
	for rows.Next() {
		var role, created string
		if err = rows.Scan(&role, &created); err != nil {
			return nil, err
		}
		p := byRole[role]
		if p == nil {
			p = &wire.PendingAge{Role: role}
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
	out := []wire.PendingAge{}
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
func (s *Service) sessionStateTime(from, to time.Time) ([]wire.RoleStateTime, error) {
	rows, err := s.db.Query(`SELECT role,state,started_at,COALESCE(ended_at,0) FROM presence_log WHERE (state LIKE 'error:%' OR state LIKE 'blocked:%') AND started_at<? AND (ended_at IS NULL OR ended_at>?)`, ms(to), ms(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		wire.RoleStateTime
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
			a = &acc{RoleStateTime: wire.RoleStateTime{Role: role}, kinds: map[string]int64{}}
			byRole[role] = a
		}
		a.kinds[state] += end - start
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []wire.RoleStateTime{}
	for _, a := range byRole {
		a.TopKinds = []wire.KindTime{}
		var errMS, blockMS int64
		for state, d := range a.kinds {
			if isErrorState(state) {
				errMS += d
			} else {
				blockMS += d
			}
			a.TopKinds = append(a.TopKinds, wire.KindTime{State: state, Seconds: d / 1000})
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
		out = append(out, a.RoleStateTime)
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
func (s *Service) currentSessionStates(now time.Time) ([]wire.CurrentState, error) {
	rows, err := s.db.Query(`SELECT p.role,p.state,p.started_at FROM presence_log p JOIN slots s ON s.base=p.base AND s.number=p.number WHERE p.ended_at IS NULL AND (p.state LIKE 'error:%' OR p.state LIKE 'blocked:%') AND s.lease_until>? ORDER BY p.started_at,p.role`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wire.CurrentState{}
	for rows.Next() {
		var c wire.CurrentState
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
