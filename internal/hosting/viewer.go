package hosting

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Browser terminal sockets (#173): GET /api/admin/terminals/{id}/socket with
// a one-time ticket from POST /api/admin/terminals/{id}/ticket. The host asks
// the record's node to attach while at least one viewer is open; the node
// answers with a snapshot and then output frames, which the hub fans out.

// Ticket and viewer limits.
const (
	TicketTTL      = 30 * time.Second
	ViewerQueue    = 256              // frames a viewer may fall behind before it is dropped
	ViewerRecheck  = 30 * time.Second // ping and re-check the admin session
	ticketBytes    = 32
	maxTickets     = 1024
	viewerWriteTTL = 10 * time.Second
	viewerDropWait = time.Second // a dropped viewer's socket closes at the latest after this
)

// Ticket modes. A "view" socket only receives: its input and resize frames
// are ignored.
const (
	ModeView  = "view"
	ModeInput = "input"
)

// Input limits (#192).
const (
	// MaxInput is the most bytes one input frame may carry; a larger one is
	// refused and the client splits pastes.
	MaxInput = 64 << 10
	// MaxCols and MaxRows bound a resize.
	MaxCols = 500
	MaxRows = 200
	// viewerReadLimit bounds a client frame: JSON escapes control bytes
	// (six bytes each), so a refusable frame still fits.
	viewerReadLimit = 7 * MaxInput
	// nodeInputWait is how long input waits for room on a busy node channel
	// before it is refused (the node is not dropped for it).
	nodeInputWait = 2 * time.Second
)

// Admin is what the browser terminal routes need from admin sign-in; the
// server fills it in (hosting does not import adminauth).
type Admin struct {
	// Session names the signed-in admin of r and a key for that sign-in.
	Session func(r *http.Request) (user, key string, ok bool)
	// Origin reports whether a WebSocket's Origin is allowed: the rule of
	// /api/admin/live.
	Origin func(r *http.Request) bool
	// Recheck is how often an open socket re-checks the session (0:
	// ViewerRecheck). Queue is the viewer queue (0: ViewerQueue).
	Recheck time.Duration
	Queue   int
}

// SetAdmin installs the sign-in hooks; without them the ticket and socket
// routes answer 401.
func (h *Hub) SetAdmin(a Admin) {
	if a.Recheck <= 0 {
		a.Recheck = ViewerRecheck
	}
	if a.Queue <= 0 {
		a.Queue = ViewerQueue
	}
	h.mu.Lock()
	h.admin = a
	h.mu.Unlock()
}

func (h *Hub) adminHooks() Admin {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.admin
}

type ticket struct {
	id, mode, session string
	expires           time.Time
}

// tickets are kept in memory by hash: single use, valid TicketTTL.
type tickets struct {
	mu sync.Mutex
	m  map[string]ticket
}

func (t *tickets) issue(now time.Time, id, mode, session string) (string, time.Time, error) {
	raw := make([]byte, ticketBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	v := base64.RawURLEncoding.EncodeToString(raw)
	exp := now.Add(TicketTTL)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]ticket{}
	}
	for k, x := range t.m {
		if !now.Before(x.expires) {
			delete(t.m, k)
		}
	}
	if len(t.m) >= maxTickets {
		return "", time.Time{}, errors.New("too many open tickets")
	}
	t.m[store.TokenHash(v)] = ticket{id: id, mode: mode, session: session, expires: exp}
	return v, exp, nil
}

// redeem uses up a ticket. It is refused when unknown, used, expired, or
// issued for another terminal or another sign-in; a refused ticket is used up
// too.
func (t *tickets) redeem(now time.Time, raw, id, session string) (string, bool) {
	if raw == "" {
		return "", false
	}
	k := store.TokenHash(raw)
	t.mu.Lock()
	x, ok := t.m[k]
	delete(t.m, k)
	t.mu.Unlock()
	if !ok || !now.Before(x.expires) || x.id != id || x.session != session {
		return "", false
	}
	return x.mode, true
}

// viewer is one open browser terminal socket.
type viewer struct {
	id    string
	mode  string // ModeView or ModeInput
	user  string // the signed-in admin
	out   chan viewerFrame
	gone  chan struct{} // closed when the hub drops it as too slow
	state string        // the state last sent
	once  sync.Once

	cols, rows int // the size this viewer last asked for (0: none); under Hub.mu

	mu    sync.Mutex
	typed tally // input bytes of the current LogWindow
}

type viewerFrame struct {
	binary bool
	data   []byte
}

func (v *viewer) drop() { v.once.Do(func() { close(v.gone) }) }

// push queues f without blocking; a full queue drops the viewer.
func (v *viewer) push(f viewerFrame) {
	select {
	case <-v.gone:
	case v.out <- f:
	default:
		v.drop()
	}
}

func (v *viewer) pushJSON(m wire.TerminalMessage) {
	b, _ := json.Marshal(m)
	v.push(viewerFrame{data: b})
}

// addViewer registers v and asks the record's node for a fresh snapshot
// (attaching it when v is the first viewer).
func (h *Hub) addViewer(v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.viewers[v.id] == nil {
		h.viewers[v.id] = map[*viewer]struct{}{}
	}
	h.viewers[v.id][v] = struct{}{}
	if c, ok := h.conns[h.owner[v.id]]; ok {
		h.sendLocked(c, wire.NodeFrame{Type: wire.NodeAttach, ID: v.id})
	}
}

// removeViewer forgets v; the last viewer of a terminal detaches the node.
func (h *Hub) removeViewer(v *viewer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.viewers[v.id]
	if _, ok := set[v]; !ok {
		return
	}
	delete(set, v)
	if h.sizeBy[v.id] == v {
		delete(h.sizeBy, v.id)
	}
	if len(set) > 0 {
		return
	}
	delete(h.viewers, v.id)
	if c, ok := h.conns[h.owner[v.id]]; ok {
		h.sendLocked(c, wire.NodeFrame{Type: wire.NodeDetach, ID: v.id})
	}
}

// attachViewedLocked asks a node that just attached to stream its records
// that have viewers.
func (h *Hub) attachViewedLocked(c *nodeConn) {
	for id := range h.viewers {
		if h.owner[id] == c.name {
			h.sendLocked(c, wire.NodeFrame{Type: wire.NodeAttach, ID: id})
		}
	}
}

// toNode queues f for node c, waiting up to nodeInputWait for room.
func toNode(c *nodeConn, f wire.NodeFrame) bool {
	t := time.NewTimer(nodeInputWait)
	defer t.Stop()
	select {
	case c.out <- f:
		return true
	case <-t.C:
		return false
	}
}

// errNodeBusy refuses input a node channel had no room for.
var errNodeBusy = errors.New("the session's node is busy")

// input types b into v's terminal. The last viewer that sent input or a
// resize sets the terminal's size, so a viewer typing into a terminal last
// sized by another viewer resizes it to its own size first.
func (h *Hub) input(v *viewer, b []byte) error {
	h.mu.Lock()
	c, ok := h.conns[h.owner[v.id]]
	resize := ok && v.cols > 0 && h.sizeBy[v.id] != v
	if resize {
		h.sizeBy[v.id] = v
	}
	cols, rows := v.cols, v.rows
	h.mu.Unlock()
	if !ok {
		return ErrNodeOffline
	}
	if resize {
		h.applySize(v, c, cols, rows)
	}
	if !toNode(c, wire.NodeFrame{Type: wire.NodeInput, ID: v.id, Data: b}) {
		return errNodeBusy
	}
	return nil
}

// resize makes cols x rows the size of v's terminal: the node resizes it,
// the record keeps it for the next launch, and the other viewers are told.
func (h *Hub) resize(v *viewer, cols, rows int) {
	h.mu.Lock()
	v.cols, v.rows = cols, rows
	h.sizeBy[v.id] = v
	c := h.conns[h.owner[v.id]]
	h.mu.Unlock()
	h.applySize(v, c, cols, rows)
}

// applySize sends the resize to node c (nil: offline), stores it and, when
// the stored size changed, tells the other viewers and syncs the records.
func (h *Hub) applySize(v *viewer, c *nodeConn, cols, rows int) {
	if c != nil {
		toNode(c, wire.NodeFrame{Type: wire.NodeResize, ID: v.id, Cols: cols, Rows: rows})
	}
	changed, err := h.svc.SetSize(v.id, cols, rows)
	if err != nil {
		log.Printf("hosting: store size of %s: %v", v.id, err)
		return
	}
	if !changed {
		return
	}
	h.mu.Lock()
	for o := range h.viewers[v.id] {
		if o != v {
			o.pushJSON(wire.TerminalMessage{Type: "size", Cols: cols, Rows: rows})
		}
	}
	h.mu.Unlock()
	if err := h.Sync(); err != nil {
		log.Printf("hosting: sync: %v", err)
	}
}

// audit appends a terminal_log row for v; failures are logged, not fatal.
func (h *Hub) audit(v *viewer, kind string, at int64, bytes int) {
	if err := h.svc.logTerminal(v.id, v.user, kind, at, bytes); err != nil {
		log.Printf("hosting: terminal log: %v", err)
	}
}

// typed counts n input bytes of v, logging a finished minute.
func (h *Hub) typed(v *viewer, n int) {
	v.mu.Lock()
	done := v.typed.add(h.svc.clock().UnixMilli(), n)
	v.mu.Unlock()
	if done.bytes > 0 {
		h.audit(v, LogInput, done.start, done.bytes)
	}
}

// flushTyped logs v's input minute when it is over, or at once when force.
func (h *Hub) flushTyped(v *viewer, force bool) {
	v.mu.Lock()
	done := v.typed.due(h.svc.clock().UnixMilli(), force)
	v.mu.Unlock()
	if done.bytes > 0 {
		h.audit(v, LogInput, done.start, done.bytes)
	}
}

// readViewer takes the client frames of v's socket until it closes. Only an
// input socket is heard; a view socket's frames are read and dropped.
func (h *Hub) readViewer(ctx context.Context, ws *websocket.Conn, v *viewer) {
	refuse := func(msg string) { v.pushJSON(wire.TerminalMessage{Type: "refused", Error: msg}) }
	for {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText || v.mode != ModeInput {
			continue
		}
		var m wire.TerminalInput
		if json.Unmarshal(b, &m) != nil {
			refuse("invalid json")
			continue
		}
		switch m.Type {
		case "input":
			if len(m.Data) > MaxInput {
				refuse("input over 64 KiB")
				continue
			}
			if m.Data == "" {
				continue
			}
			if err := h.input(v, []byte(m.Data)); err != nil {
				refuse(err.Error())
				continue
			}
			h.typed(v, len(m.Data))
		case "resize":
			if m.Cols < 1 || m.Cols > MaxCols || m.Rows < 1 || m.Rows > MaxRows {
				refuse("size out of range")
				continue
			}
			h.resize(v, m.Cols, m.Rows)
		default:
			refuse("unknown type")
		}
	}
}

// relay fans a terminal frame from node c out to the viewers of its record.
func (h *Hub) relay(c *nodeConn, b []byte) {
	kind, id, payload, ok := wire.DecodeTermFrame(b)
	if !ok || (kind != wire.TermSnapshot && kind != wire.TermOutput) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.owner[id] != c.name {
		return
	}
	for v := range h.viewers[id] {
		// Viewers share the bytes; nobody writes to them.
		v.push(viewerFrame{binary: true, data: payload})
	}
}

// changed publishes the live kind "terminals" and sends each viewer its
// terminal's state when that changed.
func (h *Hub) changed() {
	h.publish("terminals")
	h.mu.Lock()
	n := len(h.viewers)
	h.mu.Unlock()
	if n == 0 {
		return
	}
	recs, err := h.List()
	if err != nil {
		return
	}
	state := map[string]string{}
	for _, r := range recs {
		state[r.ID] = r.Runtime.State
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, set := range h.viewers {
		st, ok := state[id]
		if !ok {
			st = StateGone
		}
		for v := range set {
			if v.state != st {
				v.state = st
				v.pushJSON(wire.TerminalMessage{Type: "state", State: st})
			}
		}
	}
}

// StateGone is a viewed terminal's state once its record is deleted.
const StateGone = "gone"

// viewState is the record's current state for a viewer's hello.
func (h *Hub) viewState(id string) (wire.HostedSession, error) {
	recs, err := h.List()
	if err != nil {
		return wire.HostedSession{}, err
	}
	for _, r := range recs {
		if r.ID == id {
			return r, nil
		}
	}
	return wire.HostedSession{}, ErrNotFound
}

// ticketAPI serves POST /api/admin/terminals/{id}/ticket.
func (h *Hub) ticketAPI(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a := h.adminHooks()
	if a.Session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_, key, ok := a.Session(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req wire.TerminalTicketRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Mode != ModeView && req.Mode != ModeInput {
		http.Error(w, `mode must be "view" or "input"`, http.StatusBadRequest)
		return
	}
	if _, err := h.svc.Get(id); err != nil {
		writeResult(w, http.StatusOK, wire.HostedSession{}, err)
		return
	}
	raw, exp, err := h.tickets.issue(h.svc.clock(), id, req.Mode, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, wire.TerminalTicket{Ticket: raw, Mode: req.Mode, ExpiresAt: exp.UnixMilli()})
}

// terminalSocket serves GET /api/admin/terminals/{id}/socket?ticket=….
func (h *Hub) terminalSocket(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a := h.adminHooks()
	if a.Session == nil || a.Origin == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	user, key, ok := a.Session(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !a.Origin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	mode, ok := h.tickets.redeem(h.svc.clock(), r.URL.Query().Get("ticket"), id, key)
	if !ok {
		http.Error(w, "invalid ticket", http.StatusForbidden)
		return
	}
	rec, err := h.viewState(id)
	if err != nil {
		writeResult(w, http.StatusOK, wire.HostedSession{}, err)
		return
	}
	// The origin is checked above (also against the public URL).
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(viewerReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	send := func(f viewerFrame) bool {
		wctx, done := context.WithTimeout(ctx, viewerWriteTTL)
		defer done()
		typ := websocket.MessageText
		if f.binary {
			typ = websocket.MessageBinary
		}
		return ws.Write(wctx, typ, f.data) == nil
	}
	v := &viewer{id: id, mode: mode, user: user, out: make(chan viewerFrame, a.Queue), gone: make(chan struct{}), state: rec.Runtime.State}
	hello := wire.TerminalMessage{Type: "hello", State: rec.Runtime.State, Cols: rec.Cols, Rows: rec.Rows}
	if wctx, done := context.WithTimeout(ctx, viewerWriteTTL); wsjson.Write(wctx, ws, hello) != nil {
		done()
		return
	} else {
		done()
	}
	h.audit(v, LogAttach, h.svc.clock().UnixMilli(), 0)
	h.addViewer(v)
	read := make(chan struct{})
	go func() {
		defer close(read)
		defer cancel() // the browser went away
		h.readViewer(ctx, ws, v)
	}()
	defer func() {
		h.removeViewer(v)
		cancel()
		_ = ws.CloseNow()
		<-read
		h.flushTyped(v, true)
		h.audit(v, LogDetach, h.svc.clock().UnixMilli(), 0)
	}()
	go func() {
		// A viewer stuck in a write is closed shortly after it is dropped.
		select {
		case <-v.gone:
			time.AfterFunc(viewerDropWait, func() { _ = ws.CloseNow() })
		case <-ctx.Done():
		}
	}()
	check := time.NewTicker(a.Recheck)
	defer check.Stop()
	for {
		select {
		case <-v.gone:
			ws.Close(websocket.StatusTryAgainLater, "too slow")
			return
		case f := <-v.out:
			if !send(f) {
				return
			}
		case <-check.C:
			if _, k, ok := a.Session(r); !ok || k != key {
				ws.Close(websocket.StatusPolicyViolation, "signed out")
				return
			}
			h.flushTyped(v, false)
			pctx, done := context.WithTimeout(ctx, viewerWriteTTL)
			err := ws.Ping(pctx)
			done()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
