package server

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// hostingMail is the hosting notices in the database: "recipient idem
// subject", sorted.
func hostingMail(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT recipient, idem, subject FROM messages WHERE sender=? AND idem LIKE 'hosting:%'`, identity.SystemSender)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var to, idem, subject string
		if err := rows.Scan(&to, &idem, &subject); err != nil {
			t.Fatal(err)
		}
		out = append(out, to+" "+idem+" "+subject)
	}
	slices.Sort(out)
	return out
}

// fakeNodeConn is the node channel driven by hand, as a node would.
type fakeNodeConn struct {
	t   *testing.T
	ws  *websocket.Conn
	seq int
}

func dialFakeNode(t *testing.T, base, token string) *fakeNodeConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/node/socket", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	c := &fakeNodeConn{t: t, ws: ws}
	c.send(wire.NodeFrame{Type: wire.NodeHelloType, Hello: &wire.NodeHello{Name: "pc", Machine: "pc-1", OS: "linux", Version: "test", Backend: "fake"}})
	return c
}

func (c *fakeNodeConn) send(f wire.NodeFrame) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c.ws, f); err != nil {
		c.t.Fatal(err)
	}
}

// flush waits until the host has handled every frame sent before it: the
// host reads a connection's frames in order, so a state report it shows
// came after them.
func (c *fakeNodeConn) flush(s *Server, id string) {
	c.t.Helper()
	c.seq++
	probe := "probe-" + strconv.Itoa(c.seq)
	c.send(wire.NodeFrame{Type: wire.NodeStateType, States: []wire.HostedNodeState{{ID: id, Runtime: wire.HostedRuntime{State: probe}}}})
	eventually(c.t, probe, func() bool {
		list, _ := s.hostingHub().List()
		for _, r := range list {
			if r.ID == id && r.Runtime.State == probe {
				return true
			}
		}
		return false
	})
}

// assign reads frames until an assign arrives.
func (c *fakeNodeConn) assign() []wire.HostedSession {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var f wire.NodeFrame
		if err := wsjson.Read(ctx, c.ws, &f); err != nil {
			c.t.Fatal(err)
		}
		if f.Type == wire.NodeAssign {
			return f.Sessions
		}
	}
}

func TestHostingNoticesMailOncePerKey(t *testing.T) {
	t.Parallel()
	s, base, _ := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	svc := s.hostingService()
	withParent, err := svc.Create(wire.HostedSessionCreate{Node: "pc", Base: "a", Profile: "team-a", Directory: "/work/a", Label: "7", Parent: "b"})
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := svc.Create(wire.HostedSessionCreate{Node: "pc", Base: "a", Profile: "team-a", Directory: "/work/o"})
	if err != nil {
		t.Fatal(err)
	}
	nodeToken(t, s, "other")
	foreign, err := svc.Create(wire.HostedSessionCreate{Node: "other", Base: "a", Profile: "team-a", Directory: "/work/f"})
	if err != nil {
		t.Fatal(err)
	}
	s.hostingHub().SetNotify(s.hostingNotify, []string{"a", "b"})

	c := dialFakeNode(t, base, token)
	c.assign()
	crash := wire.NodeNotice{ID: withParent.ID, Kind: wire.NoticeCrashLoop, Run: 5, AgentSessionID: withParent.AgentSessionID, LastExit: 7, Runs: 5}
	for range 3 { // a node sends it again on every connection
		c.send(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &crash})
	}
	key := "hosting:crashloop:" + withParent.ID + ":5"
	c.flush(s, orphan.ID)
	if got := hostingMail(t, s); len(got) != 1 || got[0] != "b "+key+" crash loop: a (label 7)" {
		t.Fatalf("mail %q", got)
	}
	var body string
	if err := s.db.QueryRow(`SELECT body FROM messages WHERE idem=?`, key).Scan(&body); err != nil ||
		!strings.Contains(body, withParent.ID) || !strings.Contains(body, "/work/a") || !strings.Contains(body, "Start or Restart") {
		t.Fatalf("body %q %v", body, err)
	}

	// Without a parent every boss role gets one.
	crash.ID, crash.AgentSessionID = orphan.ID, orphan.AgentSessionID
	c.send(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &crash})
	c.send(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &crash})
	c.flush(s, withParent.ID)
	if got := hostingMail(t, s); len(got) != 3 {
		t.Fatalf("mail %q", got)
	}

	// A resume failure gets the record one new conversation and one mail
	// per recipient.
	fail := wire.NodeNotice{ID: orphan.ID, Kind: wire.NoticeResumeFailed, Run: 2, AgentSessionID: orphan.AgentSessionID, LastExit: 1, Runs: 2}
	c.send(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &fail})
	var renewed wire.HostedSession
	for renewed.ID == "" {
		for _, r := range c.assign() {
			if r.ID == orphan.ID && r.AgentSessionID != orphan.AgentSessionID {
				renewed = r
			}
		}
	}
	if renewed.Launched || renewed.AgentSessionID == "" {
		t.Fatalf("renewed %+v", renewed)
	}
	c.send(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &fail})
	eventually(t, "resume-failure mail", func() bool { return len(hostingMail(t, s)) == 5 })
	c.flush(s, withParent.ID)
	if h, _ := svc.Get(orphan.ID); h.AgentSessionID != renewed.AgentSessionID {
		t.Fatalf("conversation replaced twice: %s, then %s", renewed.AgentSessionID, h.AgentSessionID)
	}
	mail := hostingMail(t, s)
	if len(mail) != 5 {
		t.Fatalf("mail %q", mail)
	}
	resume := 0
	for _, m := range mail {
		if strings.Contains(m, "hosting:resumefail:"+orphan.ID+":2:") && strings.HasSuffix(m, "resume failed: a") {
			resume++
		}
	}
	if resume != 2 {
		t.Fatalf("mail %q", mail)
	}
	if err := s.db.QueryRow(`SELECT body FROM messages WHERE idem LIKE 'hosting:resumefail:%' LIMIT 1`).Scan(&body); err != nil ||
		!strings.Contains(body, orphan.AgentSessionID) || !strings.Contains(body, renewed.AgentSessionID) {
		t.Fatalf("body %q %v", body, err)
	}

	// The launched report of the old conversation does not mark the new one.
	c.send(wire.NodeFrame{Type: wire.NodeStateType, States: []wire.HostedNodeState{{ID: orphan.ID, Launched: true, AgentSessionID: orphan.AgentSessionID, Runtime: wire.HostedRuntime{State: "resume failed"}}}})
	c.flush(s, withParent.ID)
	if h, _ := svc.Get(orphan.ID); h.Launched {
		t.Fatal("new conversation marked launched by a report about the old one")
	}

	// Another node's record is not this node's to report.
	c.send(wire.NodeFrame{Type: wire.NodeNoticeType, Notice: &wire.NodeNotice{ID: foreign.ID, Kind: wire.NoticeCrashLoop, Run: 1, Runs: 5}})
	c.flush(s, withParent.ID)
	if got := hostingMail(t, s); len(got) != 5 {
		t.Fatalf("foreign notice mailed: %q", got)
	}
}
