package term

import (
	"errors"
	"fmt"
	"image/color"
	"io"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/sandcorporation/switchboard/internal/term/internal/vt"
)

// DefaultHistory is the scrollback kept when NewEmulator is given history <= 0.
const DefaultHistory = 10000

// Emulator is a VT terminal in memory: VT bytes in, Grid out. It is safe for
// concurrent use. Call Close when done with it.
//
// It wraps internal/vt, a patched copy of github.com/charmbracelet/x/vt at
// vt/v0.1.0 (see its README). No caller sees the library's types, so this
// file is the only place to change if it stops fitting.
type Emulator struct {
	mu     sync.Mutex
	vt     *vt.Emulator
	closed bool
	done   chan struct{}
}

// ErrClosed is returned by writes to a closed Emulator.
var ErrClosed = errors.New("term: emulator closed")

// NewEmulator returns an emulator of cols x rows keeping history lines of
// scrollback.
func NewEmulator(cols, rows, history int) *Emulator {
	if history <= 0 {
		history = DefaultHistory
	}
	v := vt.NewEmulator(max(cols, 1), max(rows, 1))
	v.SetScrollbackSize(history)
	e := &Emulator{vt: v, done: make(chan struct{})}
	// The library writes its replies to terminal queries (device attributes,
	// cursor reports) to a pipe and blocks until they are read. The real
	// terminal answers the program itself, so the replies are discarded.
	go func() {
		defer close(e.done)
		_, _ = io.Copy(io.Discard, v)
	}()
	return e
}

// Write feeds VT output into the terminal.
func (e *Emulator) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0, ErrClosed
	}
	n, err := e.vt.Write(p)
	if err != nil {
		return n, fmt.Errorf("term: emulator write: %w", err)
	}
	return n, nil
}

// Resize changes the terminal size.
func (e *Emulator) Resize(cols, rows int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vt.Resize(max(cols, 1), max(rows, 1))
}

// Close releases the emulator; later writes fail with ErrClosed.
func (e *Emulator) Close() error {
	e.mu.Lock()
	already := e.closed
	e.closed = true
	e.mu.Unlock()
	if already {
		return nil
	}
	// Close only the reply pipe: the library's own Close sets a flag its Read
	// checks without a lock, which races with the draining goroutine.
	if c, ok := e.vt.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
	<-e.done
	return nil
}

// Grid renders the current screen with at most history lines of scrollback
// (negative: all of it).
func (e *Emulator) Grid(history int) Grid {
	e.mu.Lock()
	defer e.mu.Unlock()
	cols, rows := e.vt.Width(), e.vt.Height()
	pos := e.vt.CursorPosition()
	g := Grid{
		Cols:   cols,
		Rows:   rows,
		Cursor: Cursor{X: pos.X, Y: pos.Y, Hidden: e.vt.CursorHidden()},
		Screen: make([][]Cell, rows),
	}
	if sb := e.vt.Scrollback(); sb != nil && history != 0 {
		lines := sb.Lines()
		if history > 0 && len(lines) > history {
			lines = lines[len(lines)-history:]
		}
		for _, l := range lines {
			g.History = append(g.History, historyLine(l, cols))
		}
	}
	for y := range rows {
		line := make([]Cell, cols)
		for x := range cols {
			line[x] = cellOf(e.vt.CellAt(x, y))
		}
		g.Screen[y] = continuations(line)
	}
	return g
}

// historyLine converts a scrollback line, clipped to the current width (a
// line scrolled off before a narrowing resize would otherwise wrap in a
// viewer) and without trailing blank cells.
func historyLine(l uv.Line, cols int) []Cell {
	line := make([]Cell, 0, min(len(l), cols))
	used, pending := 0, 0 // columns taken; continuation cells still owed
	for i := range l {
		c := cellOf(&l[i])
		if c.Width == 0 && pending > 0 {
			line = append(line, c)
			pending--
			continue
		}
		if c.Width == 0 {
			c = Cell{Text: " ", Width: 1} // a cell the library never wrote
		}
		if used+c.Width > cols {
			break
		}
		for ; pending > 0; pending-- {
			line = append(line, Cell{})
		}
		line = append(line, c)
		used += c.Width
		pending = c.Width - 1
	}
	// The library trims a wide character's trailing continuation cell.
	for ; pending > 0; pending-- {
		line = append(line, Cell{})
	}
	blank := Cell{Text: " ", Width: 1}
	for len(line) > 0 && line[len(line)-1] == blank {
		line = line[:len(line)-1]
	}
	return continuations(line)
}

// continuations normalises zero-width cells: right after a wide character
// they are its continuation (empty, with its style); anywhere else they are
// blanks the library never wrote.
func continuations(line []Cell) []Cell {
	owed := 0
	for i := range line {
		switch {
		case line[i].Width == 0 && owed > 0:
			line[i] = Cell{Attr: line[i-1].Attr}
			owed--
		case line[i].Width == 0:
			line[i] = Cell{Text: " ", Width: 1}
		default:
			owed = line[i].Width - 1
		}
	}
	return line
}

func cellOf(c *uv.Cell) Cell {
	if c == nil {
		return Cell{Text: " ", Width: 1}
	}
	out := Cell{Text: c.Content, Width: c.Width, Attr: attrOf(c.Style)}
	if out.Width == 0 {
		// Continuation of a wide character.
		out.Text = ""
	} else if out.Text == "" {
		out.Text = " "
	}
	return out
}

func attrOf(s uv.Style) Attr {
	return Attr{
		Fg:        colorOf(s.Fg),
		Bg:        colorOf(s.Bg),
		Bold:      s.Attrs&uv.AttrBold != 0,
		Faint:     s.Attrs&uv.AttrFaint != 0,
		Italic:    s.Attrs&uv.AttrItalic != 0,
		Underline: s.Underline != uv.UnderlineNone,
		Blink:     s.Attrs&(uv.AttrBlink|uv.AttrRapidBlink) != 0,
		Reverse:   s.Attrs&uv.AttrReverse != 0,
		Conceal:   s.Attrs&uv.AttrConceal != 0,
		Strike:    s.Attrs&uv.AttrStrikethrough != 0,
	}
}

func colorOf(c color.Color) string {
	switch v := c.(type) {
	case nil:
		return ""
	case ansi.BasicColor:
		return fmt.Sprint(uint8(v))
	case ansi.IndexedColor:
		return fmt.Sprint(uint8(v))
	default:
		r, g, b, _ := c.RGBA()
		return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	}
}
