package term

import (
	"bytes"
	"strconv"
	"strings"
)

// Snapshot serialises the grid as VT bytes for a terminal of the same size:
// reset and clear, the history lines (which scroll off the top into the
// viewer's scrollback), the screen with attributes, then the cursor. A fresh
// Emulator of g.Cols x g.Rows fed these bytes renders the same Grid.
func (g Grid) Snapshot() []byte {
	var b bytes.Buffer
	// Reset attributes, show the cursor, home, clear the screen and the
	// scrollback.
	b.WriteString("\x1b[0m\x1b[?25h\x1b[H\x1b[2J\x1b[3J")
	cur := Attr{}
	lines := make([][]Cell, 0, len(g.History)+len(g.Screen))
	lines = append(lines, g.History...)
	lines = append(lines, g.Screen...)
	for i, l := range lines {
		if i > 0 {
			// Reset before the line feed so the line scrolled in is blank.
			if !cur.IsZero() {
				b.WriteString("\x1b[0m")
				cur = Attr{}
			}
			b.WriteString("\r\n")
		}
		for _, c := range l {
			if c.Width == 0 {
				continue
			}
			if c.Attr != cur {
				b.WriteString(sgr(c.Attr))
				cur = c.Attr
			}
			b.WriteString(c.Text)
		}
	}
	b.WriteString("\x1b[0m")
	b.WriteString("\x1b[" + strconv.Itoa(g.Cursor.Y+1) + ";" + strconv.Itoa(g.Cursor.X+1) + "H")
	if g.Cursor.Hidden {
		b.WriteString("\x1b[?25l")
	}
	return b.Bytes()
}

// sgr returns the full SGR sequence for a, starting from a reset.
func sgr(a Attr) string {
	params := []string{"0"}
	for _, f := range []struct {
		on   bool
		code string
	}{
		{a.Bold, "1"}, {a.Faint, "2"}, {a.Italic, "3"}, {a.Underline, "4"},
		{a.Blink, "5"}, {a.Reverse, "7"}, {a.Conceal, "8"}, {a.Strike, "9"},
	} {
		if f.on {
			params = append(params, f.code)
		}
	}
	if p := colorParams(a.Fg, 30, 90, "38"); p != "" {
		params = append(params, p)
	}
	if p := colorParams(a.Bg, 40, 100, "48"); p != "" {
		params = append(params, p)
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

func colorParams(c string, base, bright int, ext string) string {
	if c == "" {
		return ""
	}
	if strings.HasPrefix(c, "#") && len(c) == 7 {
		r, _ := strconv.ParseUint(c[1:3], 16, 8)
		g, _ := strconv.ParseUint(c[3:5], 16, 8)
		bl, _ := strconv.ParseUint(c[5:7], 16, 8)
		return ext + ";2;" + strconv.FormatUint(r, 10) + ";" + strconv.FormatUint(g, 10) + ";" + strconv.FormatUint(bl, 10)
	}
	n, err := strconv.Atoi(c)
	if err != nil || n < 0 || n > 255 {
		return ""
	}
	switch {
	case n < 8:
		return strconv.Itoa(base + n)
	case n < 16:
		return strconv.Itoa(bright + n - 8)
	default:
		return ext + ";5;" + strconv.Itoa(n)
	}
}
