// Package conpty is the Windows terminal backend: ConPTY sessions held by a
// keeper process (switchboard __keeper) so they outlive the node, the
// keeper's pipe protocol, and the client the node uses as its term.Backend.
//
// The keeper, the protocol and the client are OS independent and run on
// every OS over any net.Listener; only the pseudo-console processes and the
// named pipe (files ending in _windows.go) are Windows code. The ConPTY
// library (github.com/charmbracelet/x/conpty) is used only in
// proc_windows.go, so callers never see it.
package conpty

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sandcorporation/switchboard/internal/term"
)

// Protocol is the keeper pipe protocol version, reported by "version" next
// to the switchboard version.
const Protocol = 1

// Operations a request names.
const (
	OpLaunch   = "launch"
	OpList     = "list"
	OpScreen   = "screen"
	OpStream   = "stream"
	OpInput    = "input"
	OpResize   = "resize"
	OpStop     = "stop"
	OpForget   = "forget"
	OpVersion  = "version"
	OpShutdown = "shutdown"
)

// Frame kinds. A frame is a 4-byte big-endian length, then the kind byte,
// then length-1 bytes of payload.
const (
	frameJSON   = 'J' // a request or a response
	frameOutput = 'O' // stream: VT bytes
	frameEnd    = 'E' // stream: ended; payload is endExited or endDropped
)

// Why a stream ended.
const (
	endExited  = "exited"
	endDropped = "dropped"
	endClosed  = "closed"
)

// maxFrame bounds a frame. A screen with full scrollback is the largest.
const maxFrame = 64 << 20

// request is one control message from the client. Each carries an id the
// response repeats; the keeper answers the requests of one connection in
// order.
type request struct {
	ID      uint64     `json:"id"`
	Op      string     `json:"op"`
	Name    string     `json:"name,omitempty"`
	Spec    *term.Spec `json:"spec,omitempty"`
	History int        `json:"history,omitempty"`
	Data    []byte     `json:"data,omitempty"`
	Cols    int        `json:"cols,omitempty"`
	Rows    int        `json:"rows,omitempty"`
	// GraceMS is stop's and shutdown's grace in milliseconds.
	GraceMS int64 `json:"grace_ms,omitempty"`
}

// response answers one request. Code carries a term error kind so the
// client returns the same sentinel errors as every other backend.
type response struct {
	ID       uint64      `json:"id"`
	Error    string      `json:"error,omitempty"`
	Code     string      `json:"code,omitempty"`
	Sessions []term.Info `json:"sessions,omitempty"`
	// Grid is the gob-encoded term.Grid of "screen": a cell per column is
	// far smaller in gob than in JSON.
	Grid     []byte `json:"grid,omitempty"`
	Version  string `json:"version,omitempty"`
	Protocol int    `json:"protocol,omitempty"`
	PID      int    `json:"pid,omitempty"`
}

// Error codes.
const (
	codeNoSession  = "no_session"
	codeExists     = "exists"
	codeNotRunning = "not_running"
)

var codeErrors = map[string]error{
	codeNoSession:  term.ErrNoSession,
	codeExists:     term.ErrSessionExists,
	codeNotRunning: term.ErrNotRunning,
}

func errorResponse(id uint64, err error) response {
	r := response{ID: id, Error: err.Error()}
	for code, e := range codeErrors {
		if errors.Is(err, e) {
			r.Code = code
		}
	}
	return r
}

// err rebuilds the keeper's error on the client side.
func (r response) err() error {
	if r.Error == "" {
		return nil
	}
	if e, ok := codeErrors[r.Code]; ok {
		return fmt.Errorf("keeper: %s: %w", r.Error, e)
	}
	return fmt.Errorf("keeper: %s", r.Error)
}

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload)+1 > maxFrame {
		return fmt.Errorf("conpty: frame of %d bytes exceeds %d", len(payload)+1, maxFrame)
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)+1))
	buf[4] = kind
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrame {
		return 0, nil, fmt.Errorf("conpty: bad frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func writeJSON(w io.Writer, v any) error { // v: request or response
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, frameJSON, b)
}

func readJSON(r *bufio.Reader, v any) error { // v: *request or *response
	kind, b, err := readFrame(r)
	if err != nil {
		return err
	}
	if kind != frameJSON {
		return fmt.Errorf("conpty: frame %q where a control message was expected", kind)
	}
	return json.Unmarshal(b, v)
}

func encodeGrid(g term.Grid) ([]byte, error) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(g); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodeGrid(b []byte) (term.Grid, error) {
	var g term.Grid
	err := gob.NewDecoder(bytes.NewReader(b)).Decode(&g)
	return g, err
}
