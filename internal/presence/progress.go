package presence

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/identity"
	"github.com/sandcorporation/agent-mail/internal/mail"
	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Progress view (the dashboard's 진행 tab, GET /api/progress for boss roles).
//
// It answers, per service, "what is it doing now, and when did we last hear
// from it?" from data Agent Mail already holds: live sessions and mail. A service is the part of a role before the first "/" (the whole name
// when there is none): agent-mail/coordinator-1 belongs to agent-mail.
//
// Mail from the system sender is not activity of its service. A role is a
// boss role when its name or its base is in SWITCHBOARD_BOSS_ROLES.

// serviceOf is the part of name before the first "/".
func serviceOf(name string) string {
	svc, _, _ := strings.Cut(name, "/")
	return svc
}

// badgeRank orders session states for the card badge: error > blocked >
// busy (compacting counts as busy) > idle (unknown counts as idle).
func badgeRank(state string) (int, string) {
	switch {
	case isErrorState(state):
		return 4, "error"
	case isBlockedState(state):
		return 3, "blocked"
	case state == "busy" || state == "compacting":
		return 2, "busy"
	default:
		return 1, "idle"
	}
}

func progressBadge(sessions []wire.ProgressSession) string {
	best, badge := 0, "none"
	for _, v := range sessions {
		if r, b := badgeRank(v.State); r > best {
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
}

func (s *Service) progressMailQuery(where string, args ...any) ([]progressMailRow, error) {
	rows, err := s.db.Query(`SELECT m.id,m.sender,m.recipient,m.subject,COALESCE(m.task_id,''),`+mail.StatusExpr+`,m.created_at,COALESCE(m.thread_id,0),COALESCE(m.reply_to,0) FROM messages m `+mail.StatusJoin+` WHERE `+where+` ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []progressMailRow
	for rows.Next() {
		var m progressMailRow
		var created string
		if err = rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.TaskID, &m.Status, &created, &m.ThreadID, &m.ReplyTo); err != nil {
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
	out := wire.ProgressReport{GeneratedAt: now, PendingAlertS: int64(pending / time.Second), Services: []wire.ServiceProgress{}}
	isBossName := s.ids.IsBossName(base)
	svcs := map[string]*wire.ServiceProgress{}
	get := func(name string) *wire.ServiceProgress {
		svc := serviceOf(name)
		p := svcs[svc]
		if p == nil {
			p = &wire.ServiceProgress{Service: svc, Sessions: []wire.ProgressSession{}, OpenDirectives: []wire.ProgressDirective{}}
			svcs[svc] = p
		}
		return p
	}

	// Live sessions, with the start of their current presence_log stretch.
	rows, err := s.db.Query(`SELECT s.base,s.number,s.directory,s.state,s.registered_at,s.reported_at,COALESCE((SELECT MAX(p.started_at) FROM presence_log p WHERE p.base=s.base AND p.number=s.number AND p.ended_at IS NULL),0) FROM slots s WHERE s.lease_until>? ORDER BY s.base,s.number`, ms(now))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var base string
		var number int
		var v wire.ProgressSession
		var reg, reported, since int64
		if err = rows.Scan(&base, &number, &v.Directory, &v.State, &reg, &reported, &since); err != nil {
			rows.Close()
			return out, err
		}
		v.Role = identity.SlotRole(base, number)
		if v.State == "" {
			v.State = presenceUnknown
		}
		if since == 0 {
			since = reg
		}
		v.Since, v.LastSeen = fromMS(since), fromMS(max(reported, reg))
		p := get(base)
		p.Sessions = append(p.Sessions, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
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

	// Open directives: mail from a boss role that is a directive (a send, or
	// a reply sent with --directive), neither completed by its recipient nor
	// answered (mail.Answers), oldest first.
	senders, err := s.distinctNames("sender")
	if err != nil {
		return out, err
	}
	var bossSenders []string
	for _, r := range senders {
		if !identity.IsSystemSender(r) && isBossName(r) {
			bossSenders = append(bossSenders, r)
		}
	}
	if len(bossSenders) > 0 {
		directives, err := s.progressMailQuery(`m.sender IN (`+placeholders(len(bossSenders))+`) AND (COALESCE(m.reply_to,0)=0 OR m.directive=1) AND `+mail.StatusExpr+` NOT IN ('complete','completed')`, anySlice(bossSenders)...)
		if err != nil {
			return out, err
		}
		ds := make([]mail.FlowMail, len(directives))
		for i, m := range directives {
			ds[i] = mail.FlowMail{ID: m.ID, Sender: m.Sender, Recipient: m.Recipient, Created: m.At, TaskID: m.TaskID, ThreadID: m.ThreadID, ReplyTo: m.ReplyTo}
		}
		candidates, err := mail.AnswerCandidates(s.db, ds, base, isBossName)
		if err != nil {
			return out, err
		}
		byThread, byTask, byMention := map[int64][]mail.FlowMail{}, map[string][]mail.FlowMail{}, map[int64][]mail.FlowMail{}
		for _, c := range candidates {
			byThread[c.ThreadID] = append(byThread[c.ThreadID], c)
			if c.TaskID != "" {
				byTask[c.TaskID] = append(byTask[c.TaskID], c)
			}
			for _, id := range c.Mentioned() {
				byMention[id] = append(byMention[id], c)
			}
		}
	directive:
		for i, m := range directives {
			d := ds[i]
			var near []mail.FlowMail
			if d.TaskID != "" {
				near = byTask[d.TaskID]
			}
			for _, group := range [][]mail.FlowMail{byThread[d.Thread()], byThread[d.ID], near, byMention[d.ID]} {
				for _, c := range group {
					if mail.Answers(d, c, base, isBossName) {
						continue directive
					}
				}
			}
			age := max(0, int64(now.Sub(m.At)/time.Second))
			p := get(m.Recipient)
			p.OpenDirectives = append(p.OpenDirectives, wire.ProgressDirective{ID: m.ID, To: m.Recipient, Subject: m.Subject, Status: m.Status, AgeS: age, OverThreshold: pending > 0 && age >= int64(pending/time.Second)})
		}
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
