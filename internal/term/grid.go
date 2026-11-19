package term

import (
	"fmt"
	"strings"
)

// Grid is a rendered terminal: the visible screen, the scrollback above it
// and the cursor. Both backends produce it through Emulator, so it is the one
// shape the node, host and dashboard see.
type Grid struct {
	Cols, Rows int
	Cursor     Cursor
	// History holds scrollback lines, oldest first. Lines carry no trailing
	// blank cells.
	History [][]Cell
	// Screen holds Rows lines of exactly Cols cells.
	Screen [][]Cell
}

// Cursor is the cursor position (zero based) and visibility.
type Cursor struct {
	X, Y   int
	Hidden bool
}

// Cell is one column of a line. A wide character occupies a cell of Width 2
// followed by a continuation cell of Width 0 and empty Text. A blank cell is
// a single space.
type Cell struct {
	Text  string
	Width int
	Attr  Attr
}

// Attr is a cell's normalised style. Colours are "" (the terminal default),
// a palette index "0".."255" (SGR 30-37, 90-97 and 38;5;n alike) or "#rrggbb".
type Attr struct {
	Fg, Bg    string
	Bold      bool
	Faint     bool
	Italic    bool
	Underline bool
	Blink     bool
	Reverse   bool
	Conceal   bool
	Strike    bool
}

// IsZero reports whether the attribute is the terminal default.
func (a Attr) IsZero() bool { return a == Attr{} }

func (a Attr) String() string {
	var parts []string
	if a.Fg != "" {
		parts = append(parts, "fg="+a.Fg)
	}
	if a.Bg != "" {
		parts = append(parts, "bg="+a.Bg)
	}
	for _, f := range []struct {
		on   bool
		name string
	}{
		{a.Bold, "bold"}, {a.Faint, "faint"}, {a.Italic, "italic"}, {a.Underline, "underline"},
		{a.Blink, "blink"}, {a.Reverse, "reverse"}, {a.Conceal, "conceal"}, {a.Strike, "strike"},
	} {
		if f.on {
			parts = append(parts, f.name)
		}
	}
	return strings.Join(parts, " ")
}

// Dump renders the grid as stable, readable text: size and cursor, the lines
// between bars, then the styled runs per line ("h<n>" history, "s<n>"
// screen, columns as cell indexes). Golden files store this form.
func (g Grid) Dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "size %dx%d\n", g.Cols, g.Rows)
	vis := "visible"
	if g.Cursor.Hidden {
		vis = "hidden"
	}
	fmt.Fprintf(&b, "cursor %d,%d %s\n", g.Cursor.X, g.Cursor.Y, vis)
	fmt.Fprintf(&b, "history %d\n", len(g.History))
	for _, l := range g.History {
		fmt.Fprintf(&b, "|%s|\n", lineText(l))
	}
	b.WriteString("screen\n")
	for _, l := range g.Screen {
		fmt.Fprintf(&b, "|%s|\n", lineText(l))
	}
	b.WriteString("styles\n")
	writeRuns(&b, "h", g.History)
	writeRuns(&b, "s", g.Screen)
	return b.String()
}

func lineText(l []Cell) string {
	var b strings.Builder
	for _, c := range l {
		b.WriteString(c.Text)
	}
	return b.String()
}

func writeRuns(b *strings.Builder, prefix string, lines [][]Cell) {
	for y, l := range lines {
		for x := 0; x < len(l); {
			a := l[x].Attr
			end := x + 1
			for end < len(l) && l[end].Attr == a {
				end++
			}
			if !a.IsZero() {
				fmt.Fprintf(b, "%s%d %d-%d %s\n", prefix, y, x, end-1, a)
			}
			x = end
		}
	}
}

// ScreenText is the visible screen's rows as text, trailing spaces trimmed.
func (g Grid) ScreenText() []string {
	out := make([]string, len(g.Screen))
	for i, l := range g.Screen {
		out[i] = strings.TrimRight(lineText(l), " ")
	}
	return out
}
