package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// Production cases of #65 replayed through the send API: a boss directs
// sandcorp/worker-1 (a role name that itself ends in -1), the worker answers
// by task (b) or in the thread (a). Every combination of which of
// sandcorp/worker-1 and sandcorp/worker has a role row, and every session
// name the worker may send as, must close the directive.
func TestProgressAnsweredWithSlotLikeRoleNames(t *testing.T) {
	type combo struct {
		name      string
		roles     []string // registered besides sandcorp/boss
		worker    string   // session name the worker sends as
		bossAs    string   // session name the boss sends as
		recipient string   // address the boss sends the directive to
	}
	var combos []combo
	for _, regs := range [][]string{{"sandcorp/worker-1"}, {"sandcorp/worker"}, {"sandcorp/worker-1", "sandcorp/worker"}} {
		has := map[string]bool{}
		for _, r := range regs {
			has[r] = true
		}
		var sessions []string
		if has["sandcorp/worker-1"] {
			sessions = append(sessions, "sandcorp/worker-1", "sandcorp/worker-1-1", "sandcorp/worker-1-2")
		} else {
			sessions = append(sessions, "sandcorp/worker-1")
		}
		for _, w := range sessions {
			for _, bossAs := range []string{"sandcorp/boss", "sandcorp/boss-1", "sandcorp/boss-2"} {
				combos = append(combos, combo{name: fmt.Sprintf("%v/%s/%s", regs, w, bossAs), roles: regs, worker: w, bossAs: bossAs, recipient: "sandcorp/worker-1"})
			}
		}
	}
	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			s, clock := slotServer(t)
			s.bossRoles = map[string]bool{"sandcorp/boss": true}
			tokens := map[string]string{"sandcorp/boss": "secret-boss"}
			for _, r := range c.roles {
				tokens[r] = "secret-" + r
			}
			for r, tok := range tokens {
				if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES(?,?,'now')`, r, tok); err != nil {
					t.Fatal(err)
				}
			}
			tokenOf := func(session string) string {
				if tok, ok := tokens[session]; ok {
					return tok
				}
				base, _, _ := wire.SplitSlot(session)
				return tokens[base]
			}
			send := func(as string, body map[string]any) int64 {
				t.Helper()
				clock.add(time.Minute)
				w := callAs(s, "POST", "/api/messages", tokenOf(as), as, body)
				if w.Code != 201 && w.Code != 200 {
					t.Fatalf("send as %s %v: %d %s", as, body, w.Code, w.Body)
				}
				var m Message
				if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
					t.Fatal(err)
				}
				return m.ID
			}
			// (b) task rule.
			d1 := send(c.bossAs, map[string]any{"recipient": c.recipient, "subject": "retro", "task_id": "dev-kit-retro"})
			send(c.worker, map[string]any{"recipient": "sandcorp/boss", "subject": "report", "task_id": "dev-kit-retro"})
			// (a) thread rule: a directive inside the worker's report thread.
			r := send(c.worker, map[string]any{"recipient": "sandcorp/boss", "subject": "report 2"})
			d2 := send(c.bossAs, map[string]any{"recipient": c.recipient, "subject": "follow up", "thread_id": r})
			// Sessions of the directive's mailbox answer with the CLI's reply
			// (POST /api/messages/{id}/reply); another slot may not, so it sends
			// with thread_id and reply_to.
			if c.worker == "sandcorp/worker-1-2" {
				send(c.worker, map[string]any{"recipient": "sandcorp/boss", "subject": "re", "thread_id": r, "reply_to": d2})
			} else {
				clock.add(time.Minute)
				if w := callAs(s, "POST", fmt.Sprintf("/api/messages/%d/reply", d2), tokenOf(c.worker), c.worker, map[string]any{"subject": "re", "body": "done"}); w.Code != 201 {
					t.Fatalf("reply as %s: %d %s", c.worker, w.Code, w.Body)
				}
			}
			// Control: a directive nobody answered stays open.
			d3 := send(c.bossAs, map[string]any{"recipient": c.recipient, "subject": "unanswered", "task_id": "other"})
			var open []int64
			for _, p := range mustProgress(t, s).Services {
				for _, d := range p.OpenDirectives {
					open = append(open, d.ID)
				}
			}
			if len(open) != 1 || open[0] != d3 {
				t.Fatalf("directives %d,%d answered and %d open: open %v", d1, d2, d3, open)
			}
			if fr := mustMetrics(t, s, 7).FirstReply; fr.Count != 3 || fr.Answered != 2 {
				t.Fatalf("first_reply %+v, want 2 of 3 answered", fr.LatencyStats)
			}
		})
	}
}

// The rows of the two production cases of #65 as the server stores them
// (RFC 3339 created_at with varying fraction lengths); both are answered.
func TestProgressReplicaOfProductionCases(t *testing.T) {
	s, _ := slotServer(t)
	s.bossRoles = map[string]bool{"sandcorp/boss": true}
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('sandcorp/boss','x','now'),('sandcorp/worker-1','y','now')`); err != nil {
		t.Fatal(err)
	}
	ins := func(id int64, from, to, at, task string, thread, reply int64) {
		if _, err := s.db.Exec(`INSERT INTO messages(id,sender,recipient,subject,body,created_at,task_id,thread_id,reply_to,status) VALUES(?,?,?,'s','b',?,?,?,?,'read')`, id, from, to, at, nullable(task), nullInt(thread), nullInt(reply)); err != nil {
			t.Fatal(err)
		}
	}
	ins(355, "sandcorp/boss", "sandcorp/worker-1", "2026-11-17T08:54:00.123456789Z", "dev-kit-retro", 0, 0)
	ins(361, "sandcorp/worker-1", "sandcorp/boss", "2026-11-17T09:03:00.5Z", "dev-kit-retro", 0, 0)
	ins(362, "sandcorp/worker-1", "sandcorp/boss", "2026-11-17T09:04:00Z", "dev-kit-retro", 0, 0)
	ins(363, "sandcorp/boss", "sandcorp/worker-1", "2026-11-17T09:04:17.1Z", "", 361, 0)
	ins(364, "sandcorp/worker-1", "sandcorp/boss", "2026-11-17T09:06:00Z", "", 361, 363)
	ins(370, "sandcorp/boss", "sandcorp/worker-1", "2026-11-17T09:10:00Z", "", 0, 0) // control: unanswered
	got := serviceNamed(t, mustProgress(t, s), "sandcorp").OpenDirectives
	if len(got) != 1 || got[0].ID != 370 {
		t.Fatalf("open %+v", got)
	}
}
