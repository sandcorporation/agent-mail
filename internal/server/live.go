package server

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Live dashboard updates (GET /api/admin/live, a WebSocket).
//
// The router publishes a change on s.live after every successful write (and
// after a recipient opens a message, which records read): the kinds are the
// dashboard tabs the write can change. Domain packages know nothing of it.
// The background sweep publishes what its notices changed. The socket sends
//
//	{"type":"hello","boot":B,"seq":N,"reset":bool,"kinds":[…]}
//	{"type":"change","seq":N,"kinds":[…]}
//
// A client reconnects with ?boot=B&after=N; hello then lists the kinds it
// missed, or reset=true when the server restarted or the ring of recent
// events no longer reaches N (the client reloads everything).

const (
	liveWriteTimeout = 5 * time.Second
	liveCheckEvery   = 30 * time.Second // ping and re-check the admin session
)

type liveMessage struct {
	Type  string   `json:"type"`
	Boot  string   `json:"boot,omitempty"`
	Seq   uint64   `json:"seq"`
	Reset bool     `json:"reset,omitempty"`
	Kinds []string `json:"kinds"`
}

// liveKinds is the dashboard tabs a successful request on path (after /api/)
// can change; nil for none. admin is true for /api/admin/ paths (then path is
// after /api/admin/).
func liveKinds(path string, admin bool) []string {
	head, _, _ := strings.Cut(path, "/")
	if admin {
		switch head {
		case "question-settings":
			return []string{"settings"}
		case "questions":
			return []string{"questions", "mail", "progress"}
		case "permissions":
			return []string{"permissions", "mail", "progress"}
		case "roles", "groups", "invites":
			return []string{"settings", "sessions", "progress"}
		}
		return nil
	}
	switch head {
	case "register", "join":
		return []string{"sessions", "progress", "settings"}
	case "messages":
		return []string{"mail", "progress", "metrics"}
	case "permissions":
		return []string{"permissions", "mail", "progress"}
	case "questions":
		return []string{"questions", "mail", "progress"}
	case "slots", "presence":
		return []string{"sessions", "progress"}
	}
	return nil
}

// statusWriter remembers the status code of a write request.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// api serves /api/ and publishes the change of every successful write.
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	adminPath, admin := strings.CutPrefix(path, "admin/")
	if admin {
		path = adminPath
	}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	// GET /api/messages/{id} by the recipient records read.
	if !admin && r.Method == http.MethodGet && strings.HasPrefix(path, "messages/") && strings.Count(path, "/") == 1 {
		write = true
	}
	kinds := liveKinds(path, admin)
	if !write || kinds == nil {
		s.serveAPI(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	s.serveAPI(sw, r)
	if sw.code != 0 && sw.code < 400 {
		s.live.Publish(kinds...)
	}
}

// liveOriginAllowed: a browser always sends Origin on a WebSocket; it must be
// this host (as the request names it) or SWITCHBOARD_PUBLIC_URL.
func (s *Server) liveOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	u, err := url.Parse(origin)
	if origin == "" || err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if s.publicURL != "" {
		p, err := url.Parse(s.publicURL)
		return err == nil && strings.EqualFold(p.Scheme, u.Scheme) && strings.EqualFold(p.Host, u.Host)
	}
	return false
}

// liveSocket serves GET /api/admin/live for an identified admin.
func (s *Server) liveSocket(w http.ResponseWriter, r *http.Request) {
	if !s.liveOriginAllowed(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	sub, res, cancel, ok := s.live.Subscribe(q.Get("boot"), after)
	if !ok {
		http.Error(w, "too many live connections", http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	// The origin is checked above (also against SWITCHBOARD_PUBLIC_URL).
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // the client sends nothing; this answers pings and sees close
	send := func(m liveMessage) bool {
		wctx, done := context.WithTimeout(ctx, liveWriteTimeout)
		defer done()
		return wsjson.Write(wctx, c, m) == nil
	}
	if res.Kinds == nil {
		res.Kinds = []string{}
	}
	if !send(liveMessage{Type: "hello", Boot: s.live.Boot(), Seq: res.Seq, Reset: res.Reset, Kinds: res.Kinds}) {
		return
	}
	check := time.NewTicker(liveCheckEvery)
	defer check.Stop()
	for {
		select {
		case ev, open := <-sub.C:
			if !open { // dropped as too slow: the client reconnects with its cursor
				c.Close(websocket.StatusTryAgainLater, "too slow")
				return
			}
			if !send(liveMessage{Type: "change", Seq: ev.Seq, Kinds: ev.Kinds}) {
				return
			}
		case <-check.C:
			if _, ok := s.adminAuth().Identify(r); !ok {
				c.Close(websocket.StatusPolicyViolation, "signed out")
				return
			}
			pctx, done := context.WithTimeout(ctx, liveWriteTimeout)
			err := c.Ping(pctx)
			done()
			if err != nil {
				return
			}
		case <-s.done:
			c.Close(websocket.StatusGoingAway, "server stopping")
			return
		case <-ctx.Done():
			return
		}
	}
}
