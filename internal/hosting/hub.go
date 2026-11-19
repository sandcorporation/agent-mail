package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// StateNodeOffline is the runtime state of a record whose node is not
// attached; the node keeps enforcing its last assign meanwhile.
const StateNodeOffline = "node offline"

// ErrNodeOffline means the record's node is not attached, so it cannot be
// told to restart now.
var ErrNodeOffline = errors.New("the session's node is offline")

// SyncInterval is how often the host re-reads the records, so a change made
// by the CLI straight in the database reaches the node.
const SyncInterval = 2 * time.Second

const (
	nodeWriteTimeout = 10 * time.Second
	nodeReadLimit    = 8 << 20 // a snapshot carries history
	nodeQueue        = 32
)

// Hub is the host side of the node channel: the attached nodes, the runtime
// state they report, and the assign lists sent to them. publish announces a
// change of the Terminals tab's data (the live kind "terminals").
type Hub struct {
	svc     *Service
	publish func(kinds ...string)

	mu      sync.Mutex
	conns   map[string]*nodeConn            // attached nodes by name
	runtime map[string]wire.HostedRuntime   // last report by record id
	away    map[string]int64                // when a node detached, by name
	owner   map[string]string               // node by record id, as of the last Sync
	digest  string                          // the records as of the last Sync
	viewers map[string]map[*viewer]struct{} // open browser terminal sockets by record id
	sizeBy  map[string]*viewer              // the viewer whose size a terminal has, by record id
	admin   Admin

	tickets tickets
}

type nodeConn struct {
	name   string
	out    chan wire.NodeFrame
	cancel context.CancelFunc
	sent   string // the last assign list sent
}

// NewHub returns the hub over svc. publish may be nil.
func NewHub(svc *Service, publish func(kinds ...string)) *Hub {
	if publish == nil {
		publish = func(...string) {}
	}
	return &Hub{svc: svc, publish: publish, conns: map[string]*nodeConn{}, runtime: map[string]wire.HostedRuntime{},
		away: map[string]int64{}, owner: map[string]string{}, viewers: map[string]map[*viewer]struct{}{}, sizeBy: map[string]*viewer{}}
}

// Run re-reads the records every interval until ctx is done.
func (h *Hub) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := h.Sync(); err != nil {
			log.Printf("hosting: sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync re-reads the records, publishes "terminals" when they changed and
// sends each attached node its list when that changed.
func (h *Hub) Sync() error {
	recs, err := h.svc.List()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(recs)
	byNode := map[string][]wire.HostedSession{}
	owner := map[string]string{}
	for _, r := range recs {
		byNode[r.Node] = append(byNode[r.Node], r)
		owner[r.ID] = r.Node
	}
	revoked, err := h.svc.revokedNodes()
	if err != nil {
		return err
	}
	h.mu.Lock()
	for name, c := range h.conns {
		if revoked[name] {
			c.cancel()
		}
	}
	changed := h.digest != "" && h.digest != string(b)
	h.digest = string(b)
	h.owner = owner
	for id := range h.runtime {
		if _, ok := owner[id]; !ok {
			delete(h.runtime, id)
		}
	}
	for name, c := range h.conns {
		h.assignLocked(c, byNode[name])
	}
	h.mu.Unlock()
	if changed {
		h.changed()
	}
	return nil
}

// assignLocked sends c its records when they differ from the last list it
// got. A node too slow to take a frame is dropped and reconnects.
func (h *Hub) assignLocked(c *nodeConn, recs []wire.HostedSession) {
	if recs == nil {
		recs = []wire.HostedSession{}
	}
	b, _ := json.Marshal(recs)
	if c.sent == string(b) {
		return
	}
	c.sent = string(b)
	h.sendLocked(c, wire.NodeFrame{Type: wire.NodeAssign, Sessions: recs})
}

func (h *Hub) sendLocked(c *nodeConn, f wire.NodeFrame) bool {
	select {
	case c.out <- f:
		return true
	default:
		c.cancel()
		return false
	}
}

// List is every record with the runtime its node reported: "unknown" until
// it reports, "node offline" while a known node is detached.
func (h *Hub) List() ([]wire.HostedSession, error) {
	recs, err := h.svc.List()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, r := range recs {
		rt, reported := h.runtime[r.ID]
		_, attached := h.conns[r.Node]
		switch {
		case attached && reported:
			recs[i].Runtime = rt
		case attached:
		default:
			k, ok := known[r.Node]
			if !ok {
				k = h.svc.nodeKnown(r.Node)
				known[r.Node] = k
			}
			if !k {
				continue
			}
			rt.State, rt.PID, rt.NextAttemptAt = StateNodeOffline, 0, nil
			if at, ok := h.away[r.Node]; ok {
				rt.Since = &at
			} else {
				rt.Since = nil
			}
			recs[i].Runtime = rt
		}
	}
	return recs, nil
}

// Restart restarts a running record with resume through its node; a stopped
// record is set to run instead.
func (h *Hub) Restart(id string) (wire.HostedSession, error) {
	r, err := h.svc.Get(id)
	if err != nil {
		return r, err
	}
	if r.Desired == DesiredStopped {
		if r, err = h.svc.SetDesired(id, DesiredRunning); err == nil {
			err = h.Sync()
		}
		return r, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.conns[r.Node]
	if !ok || !h.sendLocked(c, wire.NodeFrame{Type: wire.NodeRestart, ID: id}) {
		return r, ErrNodeOffline
	}
	return r, nil
}

// NodeAPI serves /api/node/ paths (prefix removed): join for a node
// presenting an invite code, the socket for a node presenting its token.
func (h *Hub) NodeAPI(w http.ResponseWriter, r *http.Request, path string) {
	if path == "join" {
		h.svc.joinAPI(w, r)
		return
	}
	name, err := h.svc.NodeByToken(bearer(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if path != "socket" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.nodeSocket(w, r, name)
}

func bearer(r *http.Request) string {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// nodeSocket serves the node channel for node name until either side closes.
// A second socket for the same node replaces the first.
func (h *Hub) nodeSocket(w http.ResponseWriter, r *http.Request, name string) {
	// No browser can set the Authorization header on a WebSocket, so the
	// token is the guard; Accept still refuses a cross-origin browser.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(nodeReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &nodeConn{name: name, out: make(chan wire.NodeFrame, nodeQueue), cancel: cancel}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-c.out:
				wctx, done := context.WithTimeout(ctx, nodeWriteTimeout)
				err := wsjson.Write(wctx, ws, f)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	h.attach(c)
	defer h.detach(c)
	for {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			h.relay(c, b)
			continue
		}
		var f wire.NodeFrame
		if json.Unmarshal(b, &f) != nil {
			return
		}
		h.receive(c, f)
	}
}

// attach registers c, replacing an older socket of the same node, and sends
// it the node's records.
func (h *Hub) attach(c *nodeConn) {
	recs, err := h.svc.List()
	h.mu.Lock()
	if old, ok := h.conns[c.name]; ok {
		old.cancel()
	}
	h.conns[c.name] = c
	delete(h.away, c.name)
	if err == nil {
		var mine []wire.HostedSession
		for _, r := range recs {
			h.owner[r.ID] = r.Node
			if r.Node == c.name {
				mine = append(mine, r)
			}
		}
		h.assignLocked(c, mine)
		h.attachViewedLocked(c)
	} else {
		c.cancel()
	}
	h.mu.Unlock()
	h.changed()
}

func (h *Hub) detach(c *nodeConn) {
	h.mu.Lock()
	if h.conns[c.name] != c {
		h.mu.Unlock()
		return
	}
	delete(h.conns, c.name)
	h.away[c.name] = h.svc.clock().UnixMilli()
	h.mu.Unlock()
	if err := h.svc.nodeLeft(c.name); err != nil {
		log.Printf("hosting: node %s left: %v", c.name, err)
	}
	h.changed()
}

// Revoke revokes node name and closes its socket; its sessions show
// "node offline" and its token is refused from now on.
func (h *Hub) Revoke(name string) (wire.NodeView, error) {
	n, err := h.svc.RevokeNode(name)
	if err != nil {
		return n, err
	}
	h.mu.Lock()
	c, ok := h.conns[name]
	if ok {
		c.cancel()
	}
	h.mu.Unlock()
	h.changed()
	return n, nil
}

// receive handles one node frame: hello updates the registry, state the
// runtime of the node's own records.
func (h *Hub) receive(c *nodeConn, f wire.NodeFrame) {
	switch f.Type {
	case wire.NodeHelloType:
		if f.Hello != nil {
			if err := h.svc.nodeSeen(c.name, *f.Hello); err != nil {
				log.Printf("hosting: node %s hello: %v", c.name, err)
			}
			h.publish("terminals")
		}
	case wire.NodeStateType:
		now := h.svc.clock().UnixMilli()
		changed, launched := false, []string{}
		h.mu.Lock()
		for _, st := range f.States {
			if h.owner[st.ID] != c.name {
				continue
			}
			rt := st.Runtime
			prev, ok := h.runtime[st.ID]
			if ok && prev.State == rt.State {
				rt.Since = prev.Since
			} else {
				rt.Since = &now
			}
			if !ok || !sameRuntime(prev, rt) {
				changed = true
			}
			h.runtime[st.ID] = rt
			if st.Launched {
				launched = append(launched, st.ID)
			}
		}
		h.mu.Unlock()
		for _, id := range launched {
			if err := h.svc.markLaunched(id); err != nil {
				log.Printf("hosting: mark %s launched: %v", id, err)
			}
		}
		if changed {
			h.changed()
		}
	}
}

func sameRuntime(a, b wire.HostedRuntime) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// AdminTerminalsAPI serves /api/admin/terminals… (rest after it, without the
// slash): the list with runtime, restart, and the record writes of Service,
// after which the nodes get their new lists at once. It reports false for a
// path it does not serve.
func (h *Hub) AdminTerminalsAPI(w http.ResponseWriter, r *http.Request, rest string) bool {
	if rest == "" && r.Method == http.MethodGet {
		out, err := h.List()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return true
		}
		writeJSON(w, http.StatusOK, out)
		return true
	}
	for _, sub := range []string{"/ticket", "/socket", "/log"} {
		id, ok := strings.CutSuffix(rest, sub)
		if !ok {
			continue
		}
		switch {
		case !ValidID(id):
			http.Error(w, "no such hosted session", http.StatusNotFound)
		case sub == "/ticket":
			h.ticketAPI(w, r, id)
		case sub == "/socket":
			h.terminalSocket(w, r, id)
		default:
			h.logAPI(w, r, id)
		}
		return true
	}
	if id, ok := strings.CutSuffix(rest, "/restart"); ok {
		if !ValidID(id) {
			http.Error(w, "no such hosted session", http.StatusNotFound)
			return true
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		rec, err := h.Restart(id)
		if errors.Is(err, ErrNodeOffline) {
			http.Error(w, err.Error(), http.StatusConflict)
			return true
		}
		writeResult(w, http.StatusOK, rec, err)
		return true
	}
	if !h.svc.AdminTerminalsAPI(w, r, rest) {
		return false
	}
	if r.Method != http.MethodGet {
		if err := h.Sync(); err != nil {
			log.Printf("hosting: sync: %v", err)
		}
	}
	return true
}
