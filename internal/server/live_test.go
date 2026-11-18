package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func liveServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := testServer(t)
	s.admin.ProxyToken = "proxy-secret"
	srv := httptest.NewServer(http.HandlerFunc(s.api))
	t.Cleanup(srv.Close)
	return s, srv
}

func liveDial(t *testing.T, srv *httptest.Server, query string, h http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/admin/live"+query, &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

func adminHeaders(origin string) http.Header {
	h := http.Header{}
	h.Set("X-Switchboard-Admin", "proxy-secret")
	if origin != "" {
		h.Set("Origin", origin)
	}
	return h
}

func liveRead(t *testing.T, c *websocket.Conn) liveMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var m liveMessage
	if err := wsjson.Read(ctx, c, &m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func liveWrite(t *testing.T, srv *httptest.Server, method, path, body string, h map[string]string) int {
	t.Helper()
	r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

var asA = map[string]string{"Authorization": "Bearer secret-a", "X-Switchboard-Role": "a"}

func TestLiveSocketAuthAndOrigin(t *testing.T) {
	s, srv := liveServer(t)
	for _, c := range []struct {
		name string
		h    http.Header
		want int
	}{
		{"no admin auth", http.Header{"Origin": {srv.URL}}, 401},
		{"no origin", adminHeaders(""), 403},
		{"other origin", adminHeaders("https://evil.example"), 403},
		{"public url not set", adminHeaders("https://mail.example"), 403},
	} {
		_, resp, err := liveDial(t, srv, "", c.h)
		if err == nil || resp == nil || resp.StatusCode != c.want {
			t.Fatalf("%s: err=%v resp=%v", c.name, err, resp)
		}
	}
	s.publicURL = "https://mail.example"
	c, _, err := liveDial(t, srv, "", adminHeaders("https://mail.example"))
	if err != nil {
		t.Fatalf("public url origin: %v", err)
	}
	if m := liveRead(t, c); m.Type != "hello" || m.Boot == "" {
		t.Fatalf("hello: %+v", m)
	}
}

func TestLiveSocketPushesChangesAndResumes(t *testing.T) {
	s, srv := liveServer(t)
	c, _, err := liveDial(t, srv, "", adminHeaders(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	hello := liveRead(t, c)
	if hello.Type != "hello" || hello.Reset || len(hello.Kinds) != 0 {
		t.Fatalf("hello: %+v", hello)
	}

	if code := liveWrite(t, srv, "POST", "/api/messages", `{"recipient":"b","subject":"hi","body":"x"}`, asA); code != 201 && code != 200 {
		t.Fatalf("send: %d", code)
	}
	m := liveRead(t, c)
	if m.Type != "change" || m.Seq != hello.Seq+1 || !slices.Contains(m.Kinds, "mail") || !slices.Contains(m.Kinds, "progress") {
		t.Fatalf("change after send: %+v", m)
	}
	// A refused write changes nothing and publishes nothing.
	if code := liveWrite(t, srv, "POST", "/api/messages", `{"recipient":"nobody"}`, asA); code < 400 {
		t.Fatalf("bad send: %d", code)
	}
	if code := liveWrite(t, srv, "PUT", "/api/admin/question-settings/default", `{"can_ask":true}`, map[string]string{"X-Switchboard-Admin": "proxy-secret", "X-Switchboard-Dashboard": "1"}); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	if m = liveRead(t, c); m.Seq != hello.Seq+2 || !slices.Equal(m.Kinds, []string{"settings"}) {
		t.Fatalf("change after settings: %+v", m)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// Missed while away: the kinds after the cursor.
	s.live.Publish("permissions")
	c2, _, err := liveDial(t, srv, "?boot="+hello.Boot+"&after="+itoa(int64(hello.Seq+1)), adminHeaders(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if m = liveRead(t, c2); m.Type != "hello" || m.Reset || !slices.Equal(m.Kinds, []string{"permissions", "settings"}) || m.Seq != hello.Seq+3 {
		t.Fatalf("resume: %+v", m)
	}
	// Another boot (a restarted server): reload everything.
	c3, _, err := liveDial(t, srv, "?boot=0000&after=1", adminHeaders(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if m = liveRead(t, c3); !m.Reset {
		t.Fatalf("other boot: %+v", m)
	}
}

func TestLiveKindsPerRoute(t *testing.T) {
	for _, c := range []struct {
		path  string
		admin bool
		want  string
	}{
		{"messages/4/ack", false, "mail"},
		{"permissions/perm_x/answer", false, "permissions"},
		{"questions", false, "questions"},
		{"slots/heartbeat", false, "sessions"},
		{"permissions/perm_x/answer", true, "permissions"},
		{"questions/q_x/answer", true, "questions"},
		{"roles/a/revoke", true, "settings"},
	} {
		if !slices.Contains(liveKinds(c.path, c.admin), c.want) {
			t.Fatalf("%s admin=%v: %v", c.path, c.admin, liveKinds(c.path, c.admin))
		}
	}
	if liveKinds("whoami", false) != nil || liveKinds("me", true) != nil {
		t.Fatal("read-only routes publish")
	}
}
