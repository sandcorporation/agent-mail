package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/config"
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
	notify  Notify
	bosses  []string
	// autoMail is the mail of scheduled runs (autorun.go).
	autoMail AutoMail
	// autoMailDone tells whether a scheduled run's mail was completed.
	autoMailDone AutoMailDone
	// nodeLimit is the last usage limit each node reported; limited and
	// limitKnown are the subscription limit as last published (#190).
	nodeLimit  map[string]wire.NodeLimit
	limited    bool
	limitKnown bool
	slotOf     SlotOf
	runSeen    map[string]int64 // host time the current run was first reported, by record id

	tickets tickets
}

type nodeConn struct {
	name   string
	out    chan wire.NodeFrame
	cancel context.CancelFunc
	sent   string // the last assign list sent
	// takes is whether the node asked for role credentials (#343); held
	// is the credential ids it holds.
	takes bool
	held  map[string]bool
}

// NewHub returns the hub over svc. publish may be nil.
func NewHub(svc *Service, publish func(kinds ...string)) *Hub {
	if publish == nil {
		publish = func(...string) {}
	}
	return &Hub{svc: svc, publish: publish, conns: map[string]*nodeConn{}, runtime: map[string]wire.HostedRuntime{},
		away: map[string]int64{}, owner: map[string]string{}, viewers: map[string]map[*viewer]struct{}{}, sizeBy: map[string]*viewer{}, nodeLimit: map[string]wire.NodeLimit{},
		runSeen: map[string]int64{}}
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
	if h.follow(recs) {
		if recs, err = h.svc.List(); err != nil {
			return err
		}
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
	live, err := h.svc.liveCredentials(recs, revoked)
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
			delete(h.runSeen, id)
		}
	}
	for name, c := range h.conns {
		h.deliverLocked(c, byNode[name], live)
		h.assignLocked(c, h.withLastRunLocked(byNode[name]))
	}
	h.mu.Unlock()
	if changed {
		h.changed()
	}
	return h.checkLimit()
}

// checkLimit publishes a change of the subscription limit: "terminals",
// and LimitClearedKind once when it clears (recorded as ClearedAt).
func (h *Hub) checkLimit() error {
	l, err := h.svc.Limit()
	if err != nil {
		return err
	}
	h.mu.Lock()
	was, known := h.limited, h.limitKnown
	h.limited, h.limitKnown = l.Limited, true
	h.mu.Unlock()
	if !known || was == l.Limited {
		return nil
	}
	if !l.Limited {
		if err := h.svc.markCleared(h.svc.clock().UnixMilli()); err != nil {
			return err
		}
		h.publish("terminals", LimitClearedKind)
		return nil
	}
	h.publish("terminals")
	return nil
}

// SetLimit is the operator's switch through the hub, published at once.
func (h *Hub) SetLimit(on bool) (wire.HostingLimit, error) {
	l, err := h.svc.SetLimit(on)
	if err != nil {
		return l, err
	}
	if err := h.checkLimit(); err != nil {
		return l, err
	}
	return h.svc.Limit()
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

// withLastRunLocked copies recs with the run the host last saw for each,
// so a node that starts over continues the count (#348).
func (h *Hub) withLastRunLocked(recs []wire.HostedSession) []wire.HostedSession {
	out := make([]wire.HostedSession, len(recs))
	for i, r := range recs {
		r.LastRun = h.runtime[r.ID].Run
		out[i] = r
	}
	return out
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
	h.withRuntime(recs)
	h.withSlots(recs)
	return recs, nil
}

// withRuntime fills each record's Runtime (Hub.List).
func (h *Hub) withRuntime(recs []wire.HostedSession) {
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
}

// Restart restarts a running record with resume through its node (the
// record's restart counter, so a node that reconnects still gets it); a
// stopped record is set to run instead. A running record's node must be
// attached.
func (h *Hub) Restart(id string) (wire.HostedSession, error) {
	r, err := h.svc.Get(id)
	if err != nil {
		return r, err
	}
	if r.Desired != DesiredStopped {
		h.mu.Lock()
		_, ok := h.conns[r.Node]
		h.mu.Unlock()
		if !ok {
			return r, ErrNodeOffline
		}
	}
	if r, err = h.svc.Restart(id); err == nil {
		err = h.Sync()
	}
	return r, err
}

// NewConversation gives the record a new conversation and sends it to its
// node, which stops the session and starts it with --session-id.
func (h *Hub) NewConversation(id string) (wire.HostedSession, error) {
	r, err := h.svc.NewConversation(id)
	if err == nil {
		err = h.Sync()
	}
	return r, err
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
	c.held, c.takes = parseHeld(r.Header.Values(config.HeaderPrefix + config.NodeCredentialsHeaderName))
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
	var live map[string]string
	if err == nil {
		var revoked map[string]bool
		if revoked, err = h.svc.revokedNodes(); err == nil {
			live, err = h.svc.liveCredentials(recs, revoked)
		}
	}
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
		h.deliverLocked(c, mine, live)
		h.assignLocked(c, h.withLastRunLocked(mine))
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
		changed, launched := false, []wire.HostedNodeState{}
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
			if !ok || prev.Run != rt.Run || !sameTime(prev.StartedAt, rt.StartedAt) {
				h.runSeen[st.ID] = now
			}
			if !ok || !sameRuntime(prev, rt) {
				changed = true
			}
			h.runtime[st.ID] = rt
			if st.Launched {
				launched = append(launched, st)
			}
		}
		limitChanged := false
		if f.Limit != nil {
			if prev, ok := h.nodeLimit[c.name]; !ok || !sameLimit(prev, *f.Limit) {
				h.nodeLimit[c.name], limitChanged = *f.Limit, true
			}
		}
		h.mu.Unlock()
		if limitChanged {
			if err := h.svc.setNodeLimit(c.name, *f.Limit); err != nil {
				log.Printf("hosting: node %s limit: %v", c.name, err)
			} else if err := h.checkLimit(); err != nil {
				log.Printf("hosting: limit: %v", err)
			}
		}
		for _, st := range launched {
			if err := h.svc.markLaunched(st.ID, st.AgentSessionID); err != nil {
				log.Printf("hosting: mark %s launched: %v", st.ID, err)
			}
		}
		if changed {
			h.changed()
		}
	case wire.NodeNoticeType:
		if f.Notice == nil {
			return
		}
		h.mu.Lock()
		own := h.owner[f.Notice.ID] == c.name
		h.mu.Unlock()
		if own {
			h.notice(*f.Notice)
		}
	case wire.NodeEventType:
		if f.Event == nil {
			return
		}
		h.mu.Lock()
		own := h.owner[f.Event.ID] == c.name
		h.mu.Unlock()
		if !own {
			return
		}
		if err := h.svc.recordEvent(*f.Event); err != nil {
			log.Printf("hosting: event %s %s: %v", f.Event.ID, f.Event.Kind, err)
		}
	}
}

func sameLimit(a, b wire.NodeLimit) bool {
	return a.Limited == b.Limited && a.Since == b.Since && (a.ResetAt == nil) == (b.ResetAt == nil) && (a.ResetAt == nil || *a.ResetAt == *b.ResetAt)
}

func sameTime(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

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
	if id, sub, ok := strings.Cut(rest, "/autos"); ok && (sub == "" || strings.HasPrefix(sub, "/")) {
		h.autosAPI(w, r, id, sub)
		return true
	}
	if rest == "" && r.Method == http.MethodGet {
		out, err := h.List()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return true
		}
		writeJSON(w, http.StatusOK, out)
		return true
	}
	if rest == "limit" {
		h.limitAPI(w, r)
		return true
	}
	for _, sub := range []string{"/ticket", "/socket", "/log", "/events"} {
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
		case sub == "/events":
			h.eventsAPI(w, r, id)
		default:
			h.logAPI(w, r, id)
		}
		return true
	}
	for _, sub := range []string{"/restart", "/new-conversation"} {
		id, ok := strings.CutSuffix(rest, sub)
		if !ok {
			continue
		}
		if !ValidID(id) {
			http.Error(w, "no such hosted session", http.StatusNotFound)
			return true
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		act := h.Restart
		if sub == "/new-conversation" {
			act = h.NewConversation
		}
		rec, err := act(id)
		if errors.Is(err, ErrNodeOffline) || errors.As(err, new(CapacityError)) {
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

// limitAPI serves /api/admin/terminals/limit: GET the subscription limit,
// PUT {"limited": bool} to set or clear it by hand.
func (h *Hub) limitAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		l, err := h.svc.Limit()
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, l)
	case http.MethodPut:
		var c wire.HostingLimitChange
		dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		l, err := h.SetLimit(c.Limited)
		if err != nil {
			http.Error(w, "update failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, l)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
