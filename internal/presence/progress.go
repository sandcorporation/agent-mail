package presence

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/mail"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Progress view (the dashboard's 진행 tab, GET /api/progress for boss roles).
//
// It answers, per service, "what is it doing now, and when did we last hear
// from it?" from data Switchboard already holds: live sessions and mail. A service is the part of a role before the first "/" (the whole name
// when there is none): switchboard/coordinator-1 belongs to switchboard.
//
// Mail from the system sender is not activity of its service. A role is a
// boss role when its name or its base is in SWITCHBOARD_BOSS_ROLES.

// serviceOf is the part of name before the first "/".
func serviceOf(name string) string {
	svc, _, _ := strings.Cut(name, "/")
	return svc
}

// badgeRank orders sessions for the card badge: error > blocked > busy
// (compacting, and idle or unknown with running sub-agents, count as busy) >
// idle (unknown counts as idle).
func badgeRank(v wire.ProgressSession) (int, string) {
	state := v.State
	switch {
	case isErrorState(state):
		return 4, "error"
	case isBlockedState(state):
		return 3, "blocked"
	case state == "busy" || state == "compacting" || v.Subagents > 0:
		return 2, "busy"
	default:
		return 1, "idle"
	}
}

func progressBadge(sessions []wire.ProgressSession) string {
	best, badge := 0, "none"
	for _, v := range sessions {
		if r, b := badgeRank(v); r > best {
			best, badge = r, b
		}
	}
	return badge
}

func isErrorState(state string) bool   { return strings.HasPrefix(state, "error:") }
func isBlockedState(state string) bool { return strings.HasPrefix(state, "blocked:") }

// ProgressAPI serves GET /api/progress (boss roles) and, with allowed true,
// GET /api/admin/progress. pending and base are as for Progress.
func (s *Service) ProgressAPI(w http.ResponseWriter, r *http.Request, allowed bool, pending time.Duration, base func(string) string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !allowed {
		http.Error(w, "progress is for boss roles (SWITCHBOARD_BOSS_ROLES)", http.StatusForbidden)
		return
	}
	out, err := s.Progress(pending, base)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, out)
}

// progressMailRow is a message as the progress view reads it; Status is the
// recipient's status (role_states first).
type progressMailRow struct {
	ID                         int64
	Sender, Recipient, Subject string
	TaskID, Status             string
	At                         time.Time
	ThreadID, ReplyTo          int64
	NoReply                    bool
	// Filled by settle: whether the directive is open, and its newest
	// answer, zero when none.
	State      mail.DirectiveState
	LastAnswer time.Time
}

// openDirectives lists the open directives, oldest first: mail from a boss
// role that is a directive (a send, or a reply sent with --directive) whose
// mail.DirectiveState is Open: not sent with --no-reply or closed, not
// completed by its recipient and not replied to (the answered rule,
// mail.Replies unless Config.Rule says otherwise: reply_to or the
// directive's thread, back to the sender's base; a mention of #id or a
// shared task id elsewhere does not count). The progress view lists them whatever their
// status; the reminders (SweepReminders) take only the read ones to
// non-boss roles.
func (s *Service) openDirectives(base func(string) string, isBossName func(string) bool) ([]progressMailRow, error) {
	return s.openDirectivesUnder(s.rule(), false, base, isBossName)
}

// openDirectivesUnder is openDirectives under the answered rule given. wide
// loads every mail the loose rule (mail.Answers) could count, for a past rule
// that read more than the thread.
func (s *Service) openDirectivesUnder(rule mail.AnsweredRule, wide bool, base func(string) string, isBossName func(string) bool) ([]progressMailRow, error) {
	bossSenders, err := s.bossSenders(isBossName)
	if err != nil || len(bossSenders) == 0 {
		return nil, err
	}
	// Completed and closed directives are never open (mail.DirectiveState),
	// so the query leaves them out before the answered rule runs.
	directives, err := s.progressMailQuery(`m.sender IN (`+placeholders(len(bossSenders))+`) AND (COALESCE(m.reply_to,0)=0 OR m.directive=1) AND m.no_reply=0 AND `+mail.StatusExpr+` NOT IN ('complete','completed')`, anySlice(bossSenders)...)
	if err != nil {
		return nil, err
	}
	if err = s.settle(rule, wide, directives, base, isBossName); err != nil {
		return nil, err
	}
	var out []progressMailRow
	for _, m := range directives {
		if m.State.Open() {
			out = append(out, m)
		}
	}
	return out, nil
}

// bossSenders lists the boss roles that ever sent mail.
func (s *Service) bossSenders(isBossName func(string) bool) ([]string, error) {
	senders, err := s.distinctNames("sender")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range senders {
		if !identity.IsSystemSender(r) && isBossName(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// settle fills State and LastAnswer of directives (rows of
// progressMailQuery with NoReply) under the answered rule given.
func (s *Service) settle(rule mail.AnsweredRule, wide bool, directives []progressMailRow, base func(string) string, isBossName func(string) bool) error {
	if len(directives) == 0 {
		return nil
	}
	ds := make([]mail.FlowMail, len(directives))
	for i, m := range directives {
		ds[i] = mail.FlowMail{ID: m.ID, Sender: m.Sender, Recipient: m.Recipient, Created: m.At, TaskID: m.TaskID, ThreadID: m.ThreadID, ReplyTo: m.ReplyTo}
	}
	var candidates []mail.FlowMail
	var err error
	if wide {
		candidates, err = mail.AnswerCandidates(s.db, ds, base, isBossName)
	} else {
		candidates, err = mail.ReplyCandidates(s.db, ds)
	}
	if err != nil {
		return err
	}
	byThread := map[int64][]mail.FlowMail{}
	for _, c := range candidates {
		byThread[c.ThreadID] = append(byThread[c.ThreadID], c)
	}
	for i := range directives {
		m, d := &directives[i], ds[i]
		groups := [][]mail.FlowMail{byThread[d.Thread()], byThread[d.ID]}
		if wide {
			groups = [][]mail.FlowMail{candidates} // start-up only: every candidate against every directive
		}
		seen := map[int64]bool{}
		for _, group := range groups {
			for _, c := range group {
				if seen[c.ID] {
					continue
				}
				seen[c.ID] = true
				if rule.Replies(d, c, base) {
					m.State.Answered = true
					if c.Created.After(m.LastAnswer) {
						m.LastAnswer = c.Created
					}
				}
			}
		}
		m.State.Completed = mail.IsCompleteStatus(m.Status)
		m.State.Closed = m.NoReply
	}
	return nil
}

func (s *Service) progressMailQuery(where string, args ...any) ([]progressMailRow, error) {
	rows, err := s.db.Query(`SELECT m.id,m.sender,m.recipient,m.subject,COALESCE(m.task_id,''),`+mail.StatusExpr+`,m.created_at,COALESCE(m.thread_id,0),COALESCE(m.reply_to,0),m.no_reply FROM messages m `+mail.StatusJoin+` WHERE `+where+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []progressMailRow
	for rows.Next() {
		var m progressMailRow
		var created string
		if err = rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.TaskID, &m.Status, &created, &m.ThreadID, &m.ReplyTo, &m.NoReply); err != nil {
			return nil, err
		}
		m.At, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// distinctNames lists the distinct values of a messages column (sender or
// recipient), read from its index.
func (s *Service) distinctNames(col string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT ` + col + ` FROM messages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func placeholders(n int) string { return "?" + strings.Repeat(",?", n-1) }

func anySlice(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// Progress builds the report as of now. pending is the pending-mail alert
// limit (0 means none) and base maps a mailbox name to its base role.
func (s *Service) Progress(pending time.Duration, base func(string) string) (wire.ProgressReport, error) {
	now := s.now().UTC()
	out := wire.ProgressReport{GeneratedAt: now, PendingAlertS: int64(pending / time.Second), IdleAfterS: int64(max(0, s.cfg.Remind.idle()) / time.Second), Services: []wire.ServiceProgress{}}
	isBossName := s.ids.IsBossName(base)
	svcs := map[string]*wire.ServiceProgress{}
	get := func(name string) *wire.ServiceProgress {
		svc := serviceOf(name)
		p := svcs[svc]
		if p == nil {
			p = &wire.ServiceProgress{Service: svc, Sessions: []wire.ProgressSession{}, OpenDirectives: []wire.ProgressDirective{}, Stalled: []wire.ProgressStall{}}
			svcs[svc] = p
		}
		return p
	}

	// Live sessions, with the start of their current presence_log stretch.
	live, err := s.liveSessions(now)
	if err != nil {
		return out, err
	}
	for _, v := range live {
		p := get(v.Base)
		p.Sessions = append(p.Sessions, wire.ProgressSession{Role: v.Role(), Directory: v.Directory, State: v.State, Subagents: v.Subagents, Since: v.Since, LastSeen: v.LastSeen})
	}

	// Last activity: the newest mail of every sender.
	last, err := s.progressMailQuery(`m.id IN (SELECT MAX(id) FROM messages WHERE sender NOT IN (?,?) GROUP BY sender)`, identity.SystemSender, identity.LegacySystemSender)
	if err != nil {
		return out, err
	}
	for _, m := range last {
		p := get(m.Sender)
		if p.LastActivityAt == nil || m.At.After(*p.LastActivityAt) {
			t := m.At
			p.LastActivityAt = &t
		}
	}

	// Last report: the newest mail of every sender to a boss role.
	recipients, err := s.distinctNames("recipient")
	if err != nil {
		return out, err
	}
	var bossBoxes []string
	for _, r := range recipients {
		if isBossName(r) {
			bossBoxes = append(bossBoxes, r)
		}
	}
	if len(bossBoxes) > 0 {
		reports, err := s.progressMailQuery(`m.id IN (SELECT MAX(id) FROM messages WHERE sender NOT IN (?,?) AND recipient IN (`+placeholders(len(bossBoxes))+`) GROUP BY sender)`, append([]any{identity.SystemSender, identity.LegacySystemSender}, anySlice(bossBoxes)...)...)
		if err != nil {
			return out, err
		}
		for _, m := range reports {
			p := get(m.Sender)
			if p.LastReport == nil || m.ID > p.LastReport.ID {
				p.LastReport = &wire.ProgressMail{ID: m.ID, From: m.Sender, To: m.Recipient, Subject: m.Subject, At: m.At}
			}
		}
	}

	// Open directives (openDirectives), oldest first.
	directives, err := s.openDirectives(base, isBossName)
	if err != nil {
		return out, err
	}
	for _, m := range directives {
		age := max(0, int64(now.Sub(m.At)/time.Second))
		p := get(m.Recipient)
		p.OpenDirectives = append(p.OpenDirectives, wire.ProgressDirective{ID: m.ID, To: m.Recipient, Subject: m.Subject, Status: m.Status, AgeS: age, OverThreshold: pending > 0 && age >= int64(pending/time.Second)})
	}

	// Directives idle after their answer (idle.go): the 대기 멈춤 rows.
	stalls, err := s.idleAfterAnswer(now, live, base, isBossName)
	if err != nil {
		return out, err
	}
	for _, st := range stalls {
		p := get(st.Recipient)
		p.Stalled = append(p.Stalled, wire.ProgressStall{ID: st.ID, From: st.Sender, To: st.Recipient, Subject: st.Subject, AnsweredAt: st.LastAnswer, IdleSince: st.Since, IdleS: max(0, int64(now.Sub(st.Since)/time.Second)), Sessions: st.Sessions})
	}

	for _, p := range svcs {
		p.Badge = progressBadge(p.Sessions)
		sort.SliceStable(p.OpenDirectives, func(i, j int) bool { return p.OpenDirectives[i].AgeS > p.OpenDirectives[j].AgeS })
		out.Services = append(out.Services, *p)
	}
	sort.Slice(out.Services, func(i, j int) bool {
		a, b := out.Services[i].LastActivityAt, out.Services[j].LastActivityAt
		switch {
		case a != nil && b != nil && !a.Equal(*b):
			return a.After(*b)
		case a != nil && b == nil:
			return true
		case a == nil && b != nil:
			return false
		}
		return out.Services[i].Service < out.Services[j].Service
	})
	return out, nil
}
