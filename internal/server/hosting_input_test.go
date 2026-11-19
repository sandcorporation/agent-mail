package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sandcorporation/switchboard/internal/hosting"
	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// openMode dials with a fresh ticket of mode and reads the hello and the
// snapshot into a new emulator of the hello's size.
func (r *termRig) openMode(t *testing.T, mode string) (*websocket.Conn, *term.Emulator) {
	t.Helper()
	got := adminCall(t, r.base, "POST", "/api/admin/terminals/"+r.id+"/ticket", r.cookie, true, `{"mode":"`+mode+`"}`)
	var tk wire.TerminalTicket
	if err := json.Unmarshal([]byte(got.body), &tk); err != nil || tk.Mode != mode {
		t.Fatalf("ticket = %d %s", got.code, got.body)
	}
	c, code := r.dial(t, r.id, tk.Ticket, r.cookie, r.base)
	if c == nil {
		t.Fatalf("socket refused: %d", code)
	}
	var hello wire.TerminalMessage
	if err := json.Unmarshal(readFrame(t, c, websocket.MessageText), &hello); err != nil || hello.Type != "hello" {
		t.Fatalf("hello %+v: %v", hello, err)
	}
	emu := term.NewEmulator(hello.Cols, hello.Rows, 100)
	_, _ = emu.Write(readFrame(t, c, websocket.MessageBinary))
	return c, emu
}

func clientSend(t *testing.T, c *websocket.Conn, m wire.TerminalInput) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, m); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func typeInto(t *testing.T, c *websocket.Conn, data string) {
	t.Helper()
	clientSend(t, c, wire.TerminalInput{Type: "input", Data: data})
}

func hasText(texts []wire.TerminalMessage, typ string) (wire.TerminalMessage, bool) {
	for _, m := range texts {
		if m.Type == typ {
			return m, true
		}
	}
	return wire.TerminalMessage{}, false
}

type termLogRow struct {
	user, kind string
	at         int64
	bytes      int
}

func terminalLog(t *testing.T, s *Server, id string) []termLogRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT admin_user,kind,at,bytes FROM terminal_log WHERE session=? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []termLogRow
	for rows.Next() {
		var l termLogRow
		if err := rows.Scan(&l.user, &l.kind, &l.at, &l.bytes); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func kinds(rows []termLogRow) []string {
	var out []string
	for _, l := range rows {
		out = append(out, l.kind)
	}
	return out
}

// dbContains reports every table.column of s's database holding needle.
func dbContains(t *testing.T, s *Server, needle string) []string {
	t.Helper()
	tables, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	var hits []string
	for _, n := range names {
		rows, err := s.db.Query(`SELECT * FROM "` + n + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				var b []byte
				switch x := v.(type) {
				case string:
					b = []byte(x)
				case []byte:
					b = x
				}
				if bytes.Contains(b, []byte(needle)) {
					hits = append(hits, n+"."+cols[i])
				}
			}
		}
		rows.Close()
	}
	return hits
}

func TestTerminalInputReachesTheSession(t *testing.T) {
	r := newTermRig(t, hosting.Admin{})
	r.fake.SetResponder(func(_ string, b []byte) []byte {
		if bytes.HasPrefix(b, []byte("echo ")) {
			return append([]byte("\r\n"), bytes.TrimSuffix(b[5:], []byte("\r"))...)
		}
		return nil
	})
	in, emu := r.openMode(t, hosting.ModeInput)
	view, _ := r.openMode(t, hosting.ModeView)
	eventually(t, "the node streaming", func() bool { return r.fake.Streams(r.name()) == 1 })

	// A view socket's input and resize are ignored.
	typeInto(t, view, "view-typed")
	clientSend(t, view, wire.TerminalInput{Type: "resize", Cols: 30, Rows: 9})

	// A line, then Ctrl-C, Escape and the arrows, byte for byte.
	keys := "\x03\x1b\x1b[A\x1b[B\x1b[C\x1b[D"
	typeInto(t, in, "echo seen-by-the-agent\r")
	typeInto(t, in, keys)
	want := "echo seen-by-the-agent\r" + keys
	eventually(t, "the input typed", func() bool { return string(r.fake.Typed(r.name())) == want })
	readUntil(t, in, emu, func([]wire.TerminalMessage) bool { return strings.Contains(emu.Grid(0).Dump(), "seen-by-the-agent") })
	if got := string(r.fake.Typed(r.name())); got != want {
		t.Errorf("typed %q, want %q", got, want)
	}
	if rs := r.fake.Resizes(r.name()); slices.Contains(rs, termtest.Size{Cols: 30, Rows: 9}) {
		t.Errorf("a view socket resized the terminal: %v", rs)
	}

	// Over 64 KiB is refused; exactly 64 KiB is typed.
	typeInto(t, in, strings.Repeat("x", hosting.MaxInput+1))
	texts := readUntil(t, in, emu, func(texts []wire.TerminalMessage) bool { _, ok := hasText(texts, "refused"); return ok })
	if m, _ := hasText(texts, "refused"); !strings.Contains(m.Error, "64 KiB") {
		t.Errorf("refusal %+v", m)
	}
	typeInto(t, in, strings.Repeat("y", hosting.MaxInput))
	eventually(t, "64 KiB typed", func() bool { return len(r.fake.Typed(r.name())) == len(want)+hosting.MaxInput })
	if typed := string(r.fake.Typed(r.name())); strings.Contains(typed, "x") || strings.Contains(typed, "view-typed") {
		t.Errorf("refused or view input typed: %d bytes", len(typed))
	}
}

func TestTerminalResizeLastWriterWins(t *testing.T) {
	r := newTermRig(t, hosting.Admin{})
	a, emuA := r.openMode(t, hosting.ModeInput)
	b, _ := r.openMode(t, hosting.ModeInput)
	view, emuV := r.openMode(t, hosting.ModeView)
	size := func() (int, int) {
		var c, w int
		if err := r.s.db.QueryRow(`SELECT cols, rows FROM hosted_sessions WHERE id=?`, r.id).Scan(&c, &w); err != nil {
			t.Fatal(err)
		}
		return c, w
	}
	last := func() termtest.Size {
		rs := r.fake.Resizes(r.name())
		if len(rs) == 0 {
			return termtest.Size{}
		}
		return rs[len(rs)-1]
	}

	clientSend(t, a, wire.TerminalInput{Type: "resize", Cols: 30, Rows: 8})
	eventually(t, "resized to a's size", func() bool { return last() == termtest.Size{Cols: 30, Rows: 8} })
	if c, w := size(); c != 30 || w != 8 {
		t.Errorf("stored %dx%d", c, w)
	}
	texts := readUntil(t, view, emuV, func(texts []wire.TerminalMessage) bool { _, ok := hasText(texts, "size"); return ok })
	if m, _ := hasText(texts, "size"); m.Cols != 30 || m.Rows != 8 {
		t.Errorf("viewer told %+v", m)
	}

	clientSend(t, b, wire.TerminalInput{Type: "resize", Cols: 40, Rows: 10})
	eventually(t, "resized to b's size", func() bool { return last() == termtest.Size{Cols: 40, Rows: 10} })

	// a types: the terminal takes a's size again before the input.
	typeInto(t, a, "z")
	eventually(t, "a's input", func() bool { return string(r.fake.Typed(r.name())) == "z" })
	if got := last(); got != (termtest.Size{Cols: 30, Rows: 8}) {
		t.Errorf("after a typed the size is %v", got)
	}
	if c, w := size(); c != 30 || w != 8 {
		t.Errorf("stored %dx%d after a typed", c, w)
	}
	n := len(r.fake.Resizes(r.name()))
	typeInto(t, a, "z")
	eventually(t, "a's second input", func() bool { return string(r.fake.Typed(r.name())) == "zz" })
	if got := len(r.fake.Resizes(r.name())); got != n {
		t.Errorf("typing again resized again: %d -> %d", n, got)
	}

	// Out of range is refused.
	clientSend(t, a, wire.TerminalInput{Type: "resize", Cols: hosting.MaxCols + 1, Rows: 8})
	texts = readUntil(t, a, emuA, func(texts []wire.TerminalMessage) bool { _, ok := hasText(texts, "refused"); return ok })
	if m, _ := hasText(texts, "refused"); m.Error == "" {
		t.Errorf("refusal %+v", m)
	}

	// The next launch uses the stored size.
	eventually(t, "the node has the new size", func() bool {
		recs := terminals(t, r.base, r.cookie)
		return recs[r.id].Cols == 30 && recs[r.id].Rows == 8
	})
}

func TestTerminalAuditLogKeepsNoContent(t *testing.T) {
	r := newTermRig(t, hosting.Admin{})
	const secret = "s3cret-typed-text"
	c, _ := r.openMode(t, hosting.ModeInput)
	typeInto(t, c, "echo "+secret)
	typeInto(t, c, "\r")
	want := len("echo "+secret) + 1
	eventually(t, "typed", func() bool { return len(r.fake.Typed(r.name())) == want })

	// Input is summed per socket per minute (see TestTally in hosting).
	c.Close(websocket.StatusNormalClosure, "")
	eventually(t, "detach logged", func() bool { return slices.Contains(kinds(terminalLog(t, r.s, r.id)), "detach") })

	rows := terminalLog(t, r.s, r.id)
	if got := strings.Join(kinds(rows), ","); got != "attach,input,detach" {
		t.Fatalf("log kinds %s", got)
	}
	if rows[1].bytes != want || rows[0].bytes != 0 || rows[2].bytes != 0 {
		t.Errorf("bytes %+v; want %d for input", rows, want)
	}
	for _, l := range rows {
		if l.user != "alice" {
			t.Errorf("row %+v: admin user", l)
		}
	}
	if hits := dbContains(t, r.s, secret); len(hits) > 0 {
		t.Errorf("input text stored in %v", hits)
	}

	// The terminal view's log, newest first.
	got := adminCall(t, r.base, "GET", "/api/admin/terminals/"+r.id+"/log", r.cookie, false, "")
	var entries []wire.TerminalLogEntry
	if err := json.Unmarshal([]byte(got.body), &entries); err != nil || len(entries) != 3 {
		t.Fatalf("log = %d %s", got.code, got.body)
	}
	if entries[0].Kind != "detach" || entries[0].AdminUser != "alice" || strings.Contains(got.body, secret) {
		t.Errorf("log %s", got.body)
	}
	if got := adminCall(t, r.base, "GET", "/api/admin/terminals/hs_00000000000000000000000000000000/log", r.cookie, false, ""); got.code != http.StatusNotFound {
		t.Errorf("log of no record = %d", got.code)
	}
}

func TestSweepPrunesTerminalLog(t *testing.T) {
	s := testServer(t)
	now := time.Now()
	for i, age := range []time.Duration{31 * 24 * time.Hour, 29 * 24 * time.Hour} {
		if _, err := s.db.Exec(`INSERT INTO terminal_log(session,admin_user,kind,at,bytes) VALUES(?,?,?,?,0)`, fmt.Sprint("hs_", i), "alice", "attach", now.Add(-age).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left []string
	rows, err := s.db.Query(`SELECT session FROM terminal_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	if !slices.Equal(left, []string{"hs_1"}) {
		t.Errorf("left %v, want the 29-day-old row only", left)
	}
}
