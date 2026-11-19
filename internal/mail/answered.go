package mail

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/identity"
)

// FlowMail is the part of a message the flow metrics read.
type FlowMail struct {
	ID        int64
	Sender    string
	Recipient string
	Subject   string
	Body      string
	Created   time.Time
	TaskID    string
	ThreadID  int64
	ReplyTo   int64
	Directive bool // stored directive flag (reply --directive)
}

// Thread is the thread a message belongs to: its root's ID.
func (m FlowMail) Thread() int64 {
	if m.ThreadID != 0 {
		return m.ThreadID
	}
	return m.ID
}

// IsDirective: a send (no reply_to), or a reply sent with --directive. A
// plain reply (an acknowledgement, a follow-up) carries no new work.
func (m FlowMail) IsDirective() bool { return m.ReplyTo == 0 || m.Directive }

// mentionPattern finds #<id> with a word boundary after the digits, so #499
// does not match inside #4990 or #499a.
var mentionPattern = regexp.MustCompile(`#([0-9]+)\b`)

// Mentions lists the message ids written as #<id> in text.
func Mentions(text string) []int64 {
	var out []int64
	for _, sm := range mentionPattern.FindAllStringSubmatch(text, -1) {
		if id, err := strconv.ParseInt(sm[1], 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// Mentioned lists the ids m mentions in its subject or body (rule c).
func (m FlowMail) Mentioned() []int64 { return Mentions(m.Subject + "\n" + m.Body) }

func (m FlowMail) mentions(id int64) bool {
	for _, x := range m.Mentioned() {
		if x == id {
			return true
		}
	}
	return false
}

// Answers is the loose rule (#63, #138) for whether m answers the directive
// d. The metrics first_reply uses it. Whether a directive is open is the
// AnsweredRule's (Replies and Open).
//
// m answers d when it is sent no earlier than d, not by the system, by a
// role of d's recipient base, and either
//   - (a) in d's thread (same thread root, or a reply to d itself) back to
//     d's sender base; or
//   - (b) carries d's non-empty task_id and goes to d's sender base or to
//     any boss role; or
//   - (c) mentions #<d's id> in its subject or body and goes to d's sender
//     base or to any boss role (a report sent as new mail, "Re #499, #500").
//
// For a boss directive (b) and (c) are "to any boss role"; for other mail
// they also keep the "back to the sender's base" match.
func Answers(d, m FlowMail, base func(string) string, isBoss func(string) bool) bool {
	if m.ID == d.ID || identity.IsSystemSender(m.Sender) || m.Created.Before(d.Created) || base(m.Sender) != base(d.Recipient) {
		return false
	}
	toSender := base(m.Recipient) == base(d.Sender)
	if toSender && (m.Thread() == d.Thread() || m.ThreadID == d.ID || m.ReplyTo == d.ID) {
		return true
	}
	if !toSender && !isBoss(m.Recipient) {
		return false
	}
	return (d.TaskID != "" && m.TaskID == d.TaskID) || m.mentions(d.ID)
}

// AnsweredRuleVersion is the version of the answered rule: what counts as a
// reply (Answers, Replies) and which directives are open
// (AnsweredRule.Open). Bump it with every change to how any of them decides,
// together with the golden table (answered_golden_test.go), an entry in
// AnsweredRules and a line in the release notes: at start-up
// presence.MigrateAnsweredRule marks the directives the new rule would reopen
// as needing no reply.
//
//   - 1 (#214): a reply (Replies) closes a directive.
//   - 2 (#269): replies never close a directive; only its end does (reply
//     --done or complete by its recipient, close by its sender).
const AnsweredRuleVersion = 2

// AnsweredRule is a version of the rule open directives follow.
type AnsweredRule struct {
	Version int
	// Replies decides whether m is a reply to the directive d.
	Replies func(d, m FlowMail, base func(string) string) bool
	// Open decides whether a directive in state st is still open.
	Open func(st DirectiveState) bool
}

// CurrentAnsweredRule is the rule this code runs.
var CurrentAnsweredRule = AnsweredRules[AnsweredRuleVersion]

// AnsweredRules is every version of the rule this build knows, by number, so
// the start-up migration can tell which directives a new version opens that
// the stored one did not (#268). Keep past versions when bumping.
var AnsweredRules = map[int]AnsweredRule{
	1: {Version: 1, Replies: Replies, Open: func(st DirectiveState) bool { return !st.Ended() && !st.Replied }},
	2: {Version: 2, Replies: Replies, Open: OpenUntilDone},
}

// OpenUntilDone is the open decision since version 2 (#269), the one place
// that decides whether a directive is still open: the progress view and 대기,
// the reminders, the idle check and the report status all go through it. A
// directive is open until it ends; replies, however many, leave it open.
func OpenUntilDone(st DirectiveState) bool { return !st.Ended() }

// Replies is the strict rule (#214) for whether m replies to the directive d.
// Since version 2 a reply no longer closes the directive; the progress view,
// the reminders, the idle check and the report show when it last got one.
//
// m replies to d when it is sent no earlier than d, not by the system, by a
// role of d's recipient base to a role of d's sender base, and either has
// reply_to = d or is in d's thread (same thread root, or a thread rooted at
// d). A mention of #<d's id> or a shared task id in other mail does not
// count: that is Answers' rule (b) and (c), which only the metrics keep.
func Replies(d, m FlowMail, base func(string) string) bool {
	if m.ID == d.ID || identity.IsSystemSender(m.Sender) || m.Created.Before(d.Created) || base(m.Sender) != base(d.Recipient) || base(m.Recipient) != base(d.Sender) {
		return false
	}
	return m.ReplyTo == d.ID || m.ThreadID == d.ID || m.Thread() == d.Thread()
}

// DirectiveState is what an AnsweredRule's Open reads.
type DirectiveState struct {
	Replied   bool // a reply under Replies
	Completed bool // its recipient marked it done (reply --done, complete)
	Closed    bool // no_reply: sent with --no-reply, or closed by its sender or an admin (close)
}

// Ended: done by its recipient or closed. Nothing watches it any more.
func (st DirectiveState) Ended() bool { return st.Completed || st.Closed }

// IsCompleteStatus reports whether a recipient status is complete.
func IsCompleteStatus(status string) bool { return status == "complete" || status == "completed" }

// ReplyCandidates loads the mail that could reply to any of ds under Replies:
// mail in their threads or threads rooted at them (idx_thread). A reply
// always carries a thread_id (Send sets it from reply_to), so this covers
// reply_to too. Ids are chunked to stay under SQLite's variable limit.
func ReplyCandidates(db *sql.DB, ds []FlowMail) ([]FlowMail, error) {
	threadSet := map[int64]bool{}
	for _, d := range ds {
		threadSet[d.Thread()], threadSet[d.ID] = true, true
	}
	var threads []any
	for id := range threadSet {
		threads = append(threads, id)
	}
	var out []FlowMail
	const chunk = 500
	for i := 0; i < len(threads); i += chunk {
		part := append([]any{}, threads[i:min(i+chunk, len(threads))]...)
		rows, err := db.Query(`SELECT `+FlowCols+` FROM messages WHERE thread_id IN (`+placeholders(len(part))+`) AND sender NOT IN (?,?)`, append(part, identity.SystemSender, identity.LegacySystemSender)...)
		if err != nil {
			return nil, err
		}
		ms, err := ScanFlowMail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// FlowCols is the column list ScanFlowMail reads, for queries on messages
// (unaliased).
const FlowCols = `id,sender,recipient,subject,body,created_at,COALESCE(task_id,''),COALESCE(thread_id,0),COALESCE(reply_to,0),directive`

// AnswerCandidates loads the mail that could answer any of ds under Answers:
// mail in their threads (or replying to them), mail with their task ids
// (idx_thread, idx_task), and mail from each recipient base to a sender base
// or a boss role since the oldest of its directives that mentions a #id
// (idx_sender_recipient). Ids are chunked to stay under SQLite's variable
// limit.
func AnswerCandidates(db *sql.DB, ds []FlowMail, base func(string) string, isBoss func(string) bool) ([]FlowMail, error) {
	threadSet, taskSet := map[int64]bool{}, map[string]bool{}
	for _, d := range ds {
		threadSet[d.Thread()], threadSet[d.ID] = true, true
		if d.TaskID != "" {
			taskSet[d.TaskID] = true
		}
	}
	var threads []any
	for id := range threadSet {
		threads = append(threads, id)
	}
	var tasks []any
	for t := range taskSet {
		tasks = append(tasks, t)
	}
	seen := map[int64]bool{}
	var out []FlowMail
	query := func(where string, args ...any) error {
		rows, err := db.Query(`SELECT `+FlowCols+` FROM messages WHERE `+where+` AND sender NOT IN (?,?)`, append(args, identity.SystemSender, identity.LegacySystemSender)...)
		if err != nil {
			return err
		}
		ms, err := ScanFlowMail(rows)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
		return nil
	}
	const chunk = 500
	load := func(col string, vals []any) error {
		for i := 0; i < len(vals); i += chunk {
			part := append([]any{}, vals[i:min(i+chunk, len(vals))]...)
			if err := query(col+` IN (`+placeholders(len(part))+`)`, part...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := load("thread_id", threads); err != nil {
		return nil, err
	}
	if err := load("task_id", tasks); err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		return out, nil
	}
	return out, loadMentions(db, ds, base, isBoss, query)
}

// loadMentions queries, per recipient base of ds, the mail its roles sent to
// a sender base of its directives or to a boss role since its oldest
// directive, keeping only mail with a '#' in the subject or body (rule c).
func loadMentions(db *sql.DB, ds []FlowMail, base func(string) string, isBoss func(string) bool, query func(string, ...any) error) error {
	senders, err := distinct(db, "sender")
	if err != nil {
		return err
	}
	recipients, err := distinct(db, "recipient")
	if err != nil {
		return err
	}
	type group struct {
		since time.Time
		to    map[string]bool // sender bases of the directives
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range ds {
		rb := base(d.Recipient)
		g := groups[rb]
		if g == nil {
			g = &group{since: d.Created, to: map[string]bool{}}
			groups[rb] = g
			order = append(order, rb)
		}
		if d.Created.Before(g.since) {
			g.since = d.Created
		}
		g.to[base(d.Sender)] = true
	}
	for _, rb := range order {
		g := groups[rb]
		var from, to []any
		for _, n := range senders {
			if !identity.IsSystemSender(n) && base(n) == rb {
				from = append(from, n)
			}
		}
		for _, n := range recipients {
			if g.to[base(n)] || isBoss(n) {
				to = append(to, n)
			}
		}
		if len(from) == 0 || len(to) == 0 {
			continue
		}
		since := g.since.Add(-time.Second).UTC().Format("2006-01-02T15:04:05")
		for i := 0; i < len(from); i += chunk2 {
			fp := from[i:min(i+chunk2, len(from))]
			for j := 0; j < len(to); j += chunk2 {
				tp := to[j:min(j+chunk2, len(to))]
				args := append(append(append([]any{}, fp...), tp...), since)
				if err := query(`sender IN (`+placeholders(len(fp))+`) AND recipient IN (`+placeholders(len(tp))+`) AND created_at>=? AND (instr(subject,'#')>0 OR instr(body,'#')>0)`, args...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// chunk2 bounds each side of a sender x recipient lookup: two lists of it
// stay under SQLite's variable limit.
const chunk2 = 400

// distinct lists the distinct values of a messages column (sender or
// recipient), read from its index.
func distinct(db *sql.DB, col string) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT ` + col + ` FROM messages`)
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

// ScanFlowMail reads rows of FlowCols and closes them. Rows whose created_at
// does not parse are skipped.
func ScanFlowMail(rows *sql.Rows) ([]FlowMail, error) {
	defer rows.Close()
	var out []FlowMail
	for rows.Next() {
		var m FlowMail
		var created string
		if err := rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &m.Body, &created, &m.TaskID, &m.ThreadID, &m.ReplyTo, &m.Directive); err != nil {
			return nil, err
		}
		var err error
		if m.Created, err = time.Parse(time.RFC3339Nano, created); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func placeholders(n int) string { return "?" + strings.Repeat(",?", n-1) }
