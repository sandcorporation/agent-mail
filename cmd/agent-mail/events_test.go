package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type sseFrame struct {
	ID, Event, Data, Comment string
}

type sseStream struct {
	resp   *http.Response
	frames chan sseFrame
	cancel context.CancelFunc
}

// openEvents connects to /api/events as role and parses frames in the background.
func openEvents(t *testing.T, base, token, role string, header map[string]string, query string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Agent-Mail-Role", role)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	st := &sseStream{resp: resp, frames: make(chan sseFrame, 100), cancel: cancel}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	go func() {
		defer close(st.frames)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f != (sseFrame{}) {
					st.frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				f.Comment = strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "id: "):
				f.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				f.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				f.Data = line[6:]
			}
		}
	}()
	return st
}

// next returns the next frame that is not a retry-only or ping frame unless wantPing.
func (st *sseStream) next(t *testing.T, wantPing bool) sseFrame {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				t.Fatal("stream closed")
			}
			if f.Comment == "ping" && !wantPing {
				continue
			}
			if f.Event == "" && f.Comment == "" {
				continue // retry hint
			}
			return f
		case <-timeout:
			t.Fatal("no event within 3s")
		}
	}
}

// messageIDs reads frames until the ready event and returns the message IDs before it.
func (st *sseStream) untilReady(t *testing.T) ([]int64, int64) {
	t.Helper()
	var ids []int64
	for {
		f := st.next(t, false)
		if f.Event == "ready" {
			var r struct{ Cursor int64 }
			_ = json.Unmarshal([]byte(f.Data), &r)
			return ids, r.Cursor
		}
		ids = append(ids, int64(decodeEvent(t, f)["id"].(float64)))
	}
}

func decodeEvent(t *testing.T, f sseFrame) map[string]any {
	t.Helper()
	if f.Event != "message" {
		t.Fatalf("event=%q data=%s", f.Event, f.Data)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
		t.Fatal(err)
	}
	if f.ID != itoa(int64(m["id"].(float64))) {
		t.Fatalf("id field %q != data id %v", f.ID, m["id"])
	}
	return m
}

func (st *sseStream) nextMessage(t *testing.T) map[string]any {
	t.Helper()
	return decodeEvent(t, st.next(t, false))
}

func sendMail(t *testing.T, s *Server, token, to, subject string) int64 {
	t.Helper()
	w := call(s, "POST", "/api/messages", token, map[string]any{"recipient": to, "subject": subject, "body": "body-" + subject, "task_id": "T-1"}, "")
	if w.Code != 201 {
		t.Fatalf("send=%d %s", w.Code, w.Body)
	}
	var m Message
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m.ID
}

func eventServer(t *testing.T) (*Server, string) {
	s := testServer(t)
	s.eventKeepAlive = time.Hour
	if _, err := seedRoles(s.db, `INSERT INTO roles(role,token,created_at) VALUES('c','secret-c','now')`); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(secureHeaders(http.HandlerFunc(s.api)))
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func TestEventsStreamNewMailForOwnRoleOnly(t *testing.T) {
	s, base := eventServer(t)
	st := openEvents(t, base, "secret-b", "b", nil, "")
	h := st.resp.Header
	if st.resp.StatusCode != 200 || h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-cache" || h.Get("X-Accel-Buffering") != "no" || h.Get("Content-Security-Policy") == "" {
		t.Fatalf("status=%d headers=%v", st.resp.StatusCode, h)
	}
	if ids, cursor := st.untilReady(t); len(ids) != 0 || cursor != 0 {
		t.Fatalf("empty backlog ids=%v cursor=%d", ids, cursor)
	}
	sendMail(t, s, "secret-a", "c", "not-for-b")
	id := sendMail(t, s, "secret-a", "b", "hello")
	m := st.nextMessage(t)
	if int64(m["id"].(float64)) != id || m["sender"] != "a" || m["recipient"] != "b" || m["subject"] != "hello" || m["priority"] != "normal" || m["task_id"] != "T-1" || m["created_at"] == nil {
		t.Fatalf("event=%v", m)
	}
	if _, has := m["body"]; has {
		t.Fatalf("normal mail leaked body: %v", m)
	}
	// Streaming has no state side effect.
	if w := call(s, "GET", "/api/messages?box=inbox&status=pending", "secret-b", nil, ""); !strings.Contains(w.Body.String(), `"subject":"hello"`) {
		t.Fatalf("stream consumed mail: %s", w.Body)
	}
}

func TestEventsBacklogOnConnectAndResumeByLastEventID(t *testing.T) {
	s, base := eventServer(t)
	first := sendMail(t, s, "secret-a", "b", "one")
	read := sendMail(t, s, "secret-a", "b", "already-read")
	third := sendMail(t, s, "secret-a", "b", "three")
	call(s, "GET", "/api/messages/"+itoa(read), "secret-b", nil, "")

	// No cursor: every message still pending for the role, oldest first.
	st := openEvents(t, base, "secret-b", "b", nil, "")
	ids, cursor := st.untilReady(t)
	if len(ids) != 2 || ids[0] != first || ids[1] != third || cursor != third {
		t.Fatalf("backlog=%v cursor=%d", ids, cursor)
	}
	st.cancel()

	// Mail arrives while disconnected; resume from the last seen ID without repeats.
	fourth := sendMail(t, s, "secret-a", "b", "four")
	call(s, "POST", "/api/messages/"+itoa(fourth)+"/ack", "secret-b", nil, "")
	fifth := sendMail(t, s, "secret-a", "b", "five")
	st = openEvents(t, base, "secret-b", "b", map[string]string{"Last-Event-ID": itoa(third)}, "")
	if ids, cursor = st.untilReady(t); len(ids) != 1 || ids[0] != fifth || cursor != fifth {
		t.Fatalf("resume=%v cursor=%d", ids, cursor)
	}
	st.cancel()

	// ?after= is the same cursor for clients that cannot set headers.
	st = openEvents(t, base, "secret-b", "b", nil, "?after="+itoa(first))
	if ids, _ = st.untilReady(t); len(ids) != 2 || ids[0] != third || ids[1] != fifth {
		t.Fatalf("after=%v", ids)
	}
	// Live mail after the backlog is delivered once.
	sixth := sendMail(t, s, "secret-a", "b", "six")
	if m := st.nextMessage(t); int64(m["id"].(float64)) != sixth {
		t.Fatalf("live=%v", m)
	}

}

func TestEventsRejectsBadCredentialsAndCursor(t *testing.T) {
	_, base := eventServer(t)
	for _, c := range []struct{ token, role string }{{"wrong", "b"}, {"secret-a", "b"}, {"", "b"}} {
		req, _ := http.NewRequest("GET", base+"/api/events", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("X-Agent-Mail-Role", c.role)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("token=%q role=%q status=%d", c.token, c.role, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", base+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer secret-b")
	req.Header.Set("X-Agent-Mail-Role", "b")
	req.Header.Set("Last-Event-ID", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad cursor status=%d", resp.StatusCode)
	}
}

func TestEventsKeepAliveAndShutdown(t *testing.T) {
	s, base := eventServer(t)
	s.eventKeepAlive = 30 * time.Millisecond
	s.done = make(chan struct{})
	st := openEvents(t, base, "secret-b", "b", nil, "")
	st.untilReady(t)
	if f := st.next(t, true); f.Comment != "ping" {
		t.Fatalf("keep-alive=%+v", f)
	}
	close(s.done)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-st.frames:
			if !ok {
				s.hub.mu.Lock()
				left := len(s.hub.subs)
				s.hub.mu.Unlock()
				if left != 0 {
					t.Fatalf("subscribers left after shutdown: %d", left)
				}
				return
			}
		case <-deadline:
			t.Fatal("stream not closed on shutdown")
		}
	}
}

func TestEventsSlowSubscriberDoesNotBlockSendOrLoseMail(t *testing.T) {
	s, base := eventServer(t)
	st := openEvents(t, base, "secret-b", "b", nil, "")
	st.untilReady(t)
	// The stream is not read while mail is sent; sends must still complete.
	var want []int64
	start := time.Now()
	for i := 0; i < 30; i++ {
		want = append(want, sendMail(t, s, "secret-a", "b", "burst"))
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("send blocked by subscriber")
	}
	for _, id := range want {
		if m := st.nextMessage(t); int64(m["id"].(float64)) != id {
			t.Fatalf("got %v want %d", m["id"], id)
		}
	}
}

func TestEventsStreamEndsAfterRevoke(t *testing.T) {
	s, base := eventServer(t)
	s.eventKeepAlive = 30 * time.Millisecond
	st := openEvents(t, base, "secret-b", "b", nil, "")
	st.untilReady(t)
	if _, err := s.setRevoked(context.Background(), "b", true); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				return
			}
			if f.Comment == "ping" {
				t.Fatal("ping after revoke")
			}
		case <-deadline:
			t.Fatal("stream not closed after revoke")
		}
	}
}
