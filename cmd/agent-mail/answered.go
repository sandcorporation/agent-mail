package main

import (
	"database/sql"
	"time"
)

// answers is the one rule (#63) for whether m answers the directive d. The
// progress view's open_directives and the metrics first_reply both use it.
//
// m answers d when it is sent no earlier than d, not by the system, by a
// role of d's recipient base, and either
//   - (a) in d's thread (same thread root, or a reply to d itself) back to
//     d's sender base; or
//   - (b) carries d's non-empty task_id and goes to d's sender base or to
//     any boss role.
//
// For a boss directive (b) is "to any boss role"; for other mail it also
// keeps the older "back to the sender's base" match.
func answers(d, m flowMail, base func(string) string, isBoss func(string) bool) bool {
	if m.ID == d.ID || m.Sender == systemSender || m.Created.Before(d.Created) || base(m.Sender) != base(d.Recipient) {
		return false
	}
	toSender := base(m.Recipient) == base(d.Sender)
	if toSender && (m.thread() == d.thread() || m.ThreadID == d.ID || m.ReplyTo == d.ID) {
		return true
	}
	return d.TaskID != "" && m.TaskID == d.TaskID && (toSender || isBoss(m.Recipient))
}

// answerCandidates loads the mail that could answer any of ds under answers:
// mail in their threads (or replying to them) and mail with their task ids.
// Both lookups use an index (idx_thread, idx_task); ids are chunked to stay
// under SQLite's variable limit.
func (s *Server) answerCandidates(ds []flowMail) ([]flowMail, error) {
	threadSet, taskSet := map[int64]bool{}, map[string]bool{}
	for _, d := range ds {
		threadSet[d.thread()], threadSet[d.ID] = true, true
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
	var out []flowMail
	load := func(col string, vals []any) error {
		const chunk = 500
		for i := 0; i < len(vals); i += chunk {
			part := append([]any{}, vals[i:min(i+chunk, len(vals))]...)
			rows, err := s.db.Query(`SELECT id,sender,recipient,subject,created_at,COALESCE(task_id,''),COALESCE(thread_id,0),COALESCE(reply_to,0) FROM messages WHERE `+col+` IN (`+placeholders(len(part))+`) AND sender<>?`, append(part, systemSender)...)
			if err != nil {
				return err
			}
			ms, err := scanFlowMail(rows)
			if err != nil {
				return err
			}
			for _, m := range ms {
				if !seen[m.ID] {
					seen[m.ID] = true
					out = append(out, m)
				}
			}
		}
		return nil
	}
	if err := load("thread_id", threads); err != nil {
		return nil, err
	}
	return out, load("task_id", tasks)
}

// isBossName reports whether a mailbox name is a boss role or a slot of one.
func (s *Server) isBossName(base func(string) string) func(string) bool {
	return func(name string) bool {
		return s.bossRoles[name] || s.bossRoles[base(name)]
	}
}

// scanFlowMail reads id,sender,recipient,subject,created_at,task_id,thread_id,
// reply_to rows and closes them. Rows whose created_at does not parse are
// skipped.
func scanFlowMail(rows *sql.Rows) ([]flowMail, error) {
	defer rows.Close()
	var out []flowMail
	for rows.Next() {
		var m flowMail
		var created string
		if err := rows.Scan(&m.ID, &m.Sender, &m.Recipient, &m.Subject, &created, &m.TaskID, &m.ThreadID, &m.ReplyTo); err != nil {
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
